// Package authz decides who a request acts for and what it may do.
//
//   - The principal middleware (Guard) verifies the access token, checks the
//     denylist, and checks the session, user and hospital against the
//     database (cached for at most 30 seconds). It then builds the Principal.
//   - The permission table (Route, Rule, Table) holds every route with the
//     roles it needs. A route can only be served by registering it here.
//   - Tx runs a request's queries under the principal's hospital (or as the
//     platform), so row-level security applies on every request.
//
// Identity comes only from the verified token and the database, never from
// the request body, query or headers.
package authz

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// MaxCacheTTL is the longest the session, user and hospital state may be
// cached (Review Focus 3).
const MaxCacheTTL = 30 * time.Second

// Principal is who a request acts for. HospitalID is uuid.Nil for platform
// accounts. Roles are the token's roles that the account still holds.
type Principal struct {
	UserID     uuid.UUID
	HospitalID uuid.UUID
	Roles      []string
	MFA        bool
	AMR        []string
	SessionID  string
	// TokenID is the access token's jti (logout denylists it).
	TokenID  string
	Platform bool
}

// HasRole reports whether p holds role r.
func (p Principal) HasRole(r string) bool { return slices.Contains(p.Roles, NormaliseRole(r)) }

type principalKey struct{}

// FromContext returns the request's principal, set by the middleware.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok && p.UserID != uuid.Nil
}

// WithPrincipal returns ctx carrying p, and the httpx principal the
// idempotency store scopes by. Only the middleware and tests call it.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	ctx = context.WithValue(ctx, principalKey{}, p)
	return httpx.WithPrincipal(ctx, httpx.Principal{UserID: p.UserID.String(), HospitalID: p.HospitalID})
}

// Denylist keys. jti and sid are server-made uuids, never personal data.
func JTIKey(jti string) string { return "jti." + jti }
func SIDKey(sid string) string { return "sid." + sid }

// ErrNoPrincipal is returned by Tx outside an authenticated request.
var ErrNoPrincipal = errors.New("authz: no principal")

// Tx runs fn in a transaction scoped to the request's principal: as
// mhai_gateway with the principal's hospital set (row-level security shows
// only that hospital's rows) for a clinical principal, or as mhai_platform
// for a platform principal. Without a principal it fails. Handlers use
// this, never db.With* directly.
func Tx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	p, ok := FromContext(ctx)
	if !ok {
		return ErrNoPrincipal
	}
	if p.Platform {
		if p.HospitalID != uuid.Nil {
			return ErrNoPrincipal
		}
		return db.WithPlatform(ctx, pool, fn)
	}
	return db.WithHospital(ctx, pool, p.HospitalID, fn)
}

// Guard is the principal middleware. Build one per process and share it;
// it must not be copied after first use.
type Guard struct {
	// Keys verify access tokens (by kid).
	Keys map[string]ed25519.PublicKey
	// Denylist is the jti/sid denylist bucket.
	Denylist jetstream.KeyValue
	// Pool reads the session, user and hospital state (as mhai_auth).
	Pool *pgxpool.Pool
	Log  *slog.Logger
	// Now is the token clock; defaults to time.Now.
	Now func() time.Time
	// CacheNow is the state cache's clock; defaults to Now. Tests move it
	// to expire the cache.
	CacheNow func() time.Time
	// CacheTTL is how long state is cached, at most MaxCacheTTL (the
	// default).
	CacheTTL time.Duration
	// Idem is the Idempotency-Key middleware for authenticated writes
	// (httpx.Idempotent: scoped by the principal). IdemAnon is the one for
	// public writes such as sign-in (httpx.IdempotentAnon).
	Idem, IdemAnon func(http.Handler) http.Handler

	mu    sync.Mutex
	cache map[string]cached
}

// accountState is what the database says about a session's account.
type accountState struct {
	UserID         uuid.UUID
	Revoked        bool
	Active         bool
	HospitalID     *uuid.UUID
	HospitalStatus *string
	Policy         MFAPolicy
	MFAEnabled     bool
	Roles          []string
}

type cached struct {
	at    time.Time
	state *accountState // nil: no such session
}

func (g *Guard) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *Guard) cacheNow() time.Time {
	if g.CacheNow != nil {
		return g.CacheNow()
	}
	return g.now()
}

func (g *Guard) ttl() time.Duration {
	if g.CacheTTL > 0 && g.CacheTTL < MaxCacheTTL {
		return g.CacheTTL
	}
	return MaxCacheTTL
}

func (g *Guard) log() *slog.Logger {
	if g.Log != nil {
		return g.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// maxCacheEntries bounds the cache; past it, expired entries are swept.
const maxCacheEntries = 10000

// state returns the session's account state, from the cache when it is
// younger than the TTL. A clock that went backwards refetches.
func (g *Guard) state(ctx context.Context, sid uuid.UUID) (*accountState, error) {
	key := sid.String()
	now := g.cacheNow()
	ttl := g.ttl()
	g.mu.Lock()
	if c, ok := g.cache[key]; ok && !now.Before(c.at) && now.Sub(c.at) < ttl {
		g.mu.Unlock()
		return c.state, nil
	}
	g.mu.Unlock()

	st, err := loadState(ctx, g.Pool, sid)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.cache == nil {
		g.cache = map[string]cached{}
	}
	if len(g.cache) >= maxCacheEntries {
		for k, c := range g.cache {
			if now.Before(c.at) || now.Sub(c.at) >= ttl {
				delete(g.cache, k)
			}
		}
		if len(g.cache) >= maxCacheEntries {
			g.cache = map[string]cached{}
		}
	}
	g.cache[key] = cached{at: now, state: st}
	g.mu.Unlock()
	return st, nil
}

// stateSelect reads a session's account state (mhai_auth). A missing
// hospital_settings row means two-step is required for clinicians, as at
// sign-in.
const stateSelect = `
SELECT s.user_id, s.revoked_at IS NOT NULL, u.is_active, u.hospital_id, h.status,
       COALESCE(hs.require_mfa_for_clinicians, true), COALESCE(hs.allow_trust_device, false),
       COALESCE(m.enabled, false),
       ARRAY(SELECT r.role FROM user_roles r WHERE r.user_id = u.id)
FROM sessions s
JOIN users u                   ON u.id = s.user_id
LEFT JOIN hospitals h          ON h.id = u.hospital_id
LEFT JOIN hospital_settings hs ON hs.hospital_id = u.hospital_id
LEFT JOIN user_mfa m           ON m.user_id = u.id
WHERE s.id = $1`

var errStateLookup = errors.New("authz: account state lookup failed")

func loadState(ctx context.Context, pool *pgxpool.Pool, sid uuid.UUID) (*accountState, error) {
	if pool == nil {
		return nil, errStateLookup
	}
	var st *accountState
	err := db.WithAuth(ctx, pool, func(tx pgx.Tx) error {
		a := &accountState{}
		err := tx.QueryRow(ctx, stateSelect, sid).Scan(&a.UserID, &a.Revoked, &a.Active, &a.HospitalID, &a.HospitalStatus,
			&a.Policy.RequireForClinicians, &a.Policy.AllowTrustDevice, &a.MFAEnabled, &a.Roles)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return errStateLookup
		}
		a.Roles = SortRoles(a.Roles)
		st = a
		return nil
	})
	if err != nil {
		// Driver errors can carry row values (R2): classify, never wrap.
		return nil, errStateLookup
	}
	return st, nil
}

// denied reports whether the token's jti or session is on the denylist.
func (g *Guard) denied(ctx context.Context, c token.Claims) (bool, error) {
	if g.Denylist == nil {
		return false, errors.New("authz: no denylist")
	}
	for _, k := range []string{JTIKey(c.Jti), SIDKey(c.Sid)} {
		_, err := g.Denylist.Get(ctx, k)
		if err == nil {
			return true, nil
		}
		if !bus.IsNotFound(err) {
			return false, err
		}
	}
	return false, nil
}

// authenticate checks the request's access token and the account behind it
// and returns the principal, or the error code to send:
//   - no, malformed, forged or wrong-audience token: unauthenticated;
//     expired: token_expired;
//   - jti or session on the denylist: unauthenticated; denylist unreadable:
//     not_ready (fail closed);
//   - session unknown or revoked, user inactive or not the token's subject,
//     hospital not the token's or not running, roles inconsistent:
//     unauthenticated (from the database, cached for at most 30 s, so this
//     holds even when the denylist is down); lookup failed: not_ready;
//   - unless allowEnrol, a session that doesn't meet the account's current
//     two-step policy (a session limited to enrolment): mfa_required.
func (g *Guard) authenticate(r *http.Request, allowEnrol bool) (Principal, string) {
	raw, ok := bearer(r.Header.Values("Authorization"))
	if !ok {
		return Principal{}, "unauthenticated"
	}
	c, err := token.VerifyAt(raw, g.Keys, g.now())
	if errors.Is(err, token.ErrExpired) {
		return Principal{}, "token_expired"
	}
	if err != nil {
		return Principal{}, "unauthenticated"
	}
	uid, err1 := uuid.Parse(c.Sub)
	sid, err2 := uuid.Parse(c.Sid)
	if err1 != nil || err2 != nil || uid == uuid.Nil || c.Jti == "" {
		return Principal{}, "unauthenticated"
	}
	var hid uuid.UUID
	if c.Hid != "" {
		if hid, err = uuid.Parse(c.Hid); err != nil || hid == uuid.Nil {
			return Principal{}, "unauthenticated"
		}
	}
	ctx := r.Context()
	rid := httpx.RequestIDFrom(ctx)
	denied, err := g.denied(ctx, c)
	if err != nil {
		g.log().Error("authz: denylist unavailable", "request_id", rid)
		return Principal{}, "not_ready"
	}
	if denied {
		return Principal{}, "unauthenticated"
	}
	st, err := g.state(ctx, sid)
	if err != nil {
		g.log().Error("authz: account state unavailable", "request_id", rid, "user_id", uid.String())
		return Principal{}, "not_ready"
	}
	if st == nil || st.Revoked || st.UserID != uid ||
		!AccountUsable(st.Active, st.HospitalID, st.HospitalStatus, st.Roles) {
		return Principal{}, "unauthenticated"
	}
	switch {
	case st.HospitalID == nil && hid != uuid.Nil,
		st.HospitalID != nil && *st.HospitalID != hid:
		return Principal{}, "unauthenticated"
	}
	// The enrolment-limited path is only for an account that hasn't turned
	// two-step on: an enrolled account always needs its code.
	if !SessionSatisfiesMFA(st.Roles, st.Policy, st.MFAEnabled, c.MFA, c.AMR) && (!allowEnrol || st.MFAEnabled) {
		return Principal{}, "mfa_required"
	}
	// Roles: those in the token that the account still holds. A role taken
	// away stops working within the cache TTL; a role added waits for the
	// next token.
	var roles []string
	for _, rl := range SortRoles(c.Roles) {
		if slices.Contains(st.Roles, rl) {
			roles = append(roles, rl)
		}
	}
	if len(roles) == 0 {
		// None of the token's roles is held any more: refuse, so the client
		// refreshes and gets a token with the current roles.
		return Principal{}, "unauthenticated"
	}
	return Principal{
		UserID: uid, HospitalID: hid, Roles: roles, MFA: c.MFA, AMR: slices.Clone(c.AMR),
		SessionID: sid.String(), TokenID: c.Jti, Platform: st.HospitalID == nil,
	}, ""
}

func bearer(vals []string) (string, bool) {
	if len(vals) != 1 {
		return "", false
	}
	scheme, tok, ok := strings.Cut(vals[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" || strings.ContainsAny(tok, " \t") {
		return "", false
	}
	return tok, true
}

// check applies the route's rule to an authenticated principal and returns
// the error code to send, or "".
//
// Audience and account kind:
//   - Platform routes need the platform audience and a platform account
//     (else not_platform).
//   - SelfService routes take either kind of account on its own audience.
//   - Every other route is clinical: a platform account is forbidden on
//     either audience, so a clinical handler never runs as mhai_platform.
//
// Then AnyRole, then auditors: refused on every Write and on every unsafe
// method (anything but GET, HEAD, OPTIONS) outside SelfService routes, so a
// forgotten Write mark can't open a write to them.
func check(r *http.Request, rule Rule, p Principal) string {
	platformAud := httpx.Audience(r.Context()) == httpx.AudiencePlatform
	switch {
	case rule.Platform:
		if !platformAud || !p.Platform {
			return "not_platform"
		}
	default:
		if platformAud && !p.Platform {
			return "not_platform"
		}
		if p.Platform && (!rule.SelfService || !platformAud) {
			return "forbidden"
		}
	}
	if len(rule.AnyRole) > 0 && !slices.ContainsFunc(rule.AnyRole, p.HasRole) {
		return "forbidden"
	}
	if p.HasRole(RoleAuditor) && (rule.Write || (!rule.SelfService && !safeMethod(r.Method))) {
		return "forbidden"
	}
	if rule.NeedsMFA && !p.MFA {
		return "mfa_required"
	}
	return ""
}

// safeMethod reports whether method only reads.
func safeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// wrap puts h behind the rule: public routes get only the anonymous
// idempotency store; every other route is authenticated, checked against
// the rule, and then idempotent per principal.
func (g *Guard) wrap(rule Rule, h http.Handler) http.Handler {
	// NewMux refuses a guard without both middlewares.
	if rule.Public {
		return g.IdemAnon(h)
	}
	h = g.Idem(h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, code := g.authenticate(r, rule.MFAEnrol)
		if code != "" {
			httpx.Error(w, r, code)
			return
		}
		if code := check(r, rule, p); code != "" {
			httpx.Error(w, r, code)
			return
		}
		h.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}
