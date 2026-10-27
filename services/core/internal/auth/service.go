// Package auth serves sign-in, two-step verification (challenge, enrol,
// verify, backup codes, trusted devices, step-up), password change,
// refresh, logout, the device (session) list and terms acceptance. Its
// routes register through authz.Route, so the principal middleware (authz)
// checks every access token.
//
// Nothing here logs or audits an email, password or token: logs carry the
// request id and, once known, the user id; audit payloads carry ids and
// fixed reason words only.
package auth

import (
	"context"
	"crypto/ed25519"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// Fixed values from the global constraints and Review Focus 2.
const (
	MaxLoginFailures   = 5                // per (login, client IP) in the bucket's 15 minutes
	DefaultReuseGrace  = 10 * time.Second // refresh reuse within this of a rotation is a race, not theft
	MFATokenTTL        = 5 * time.Minute
	MFATokenAttempts   = 5
	defaultIdleMinutes = 15 // platform accounts, and the contract's fallback
	defaultReviewHours = 4
)

// Service holds the dependencies of the auth routes.
type Service struct {
	Pool    *pgxpool.Pool
	Buckets bus.Buckets
	Issuer  token.Issuer
	// Keys verify access tokens (by kid); normally Issuer.PublicKeys().
	Keys map[string]ed25519.PublicKey
	Log  *slog.Logger
	// Now is the clock (tests inject one); defaults to time.Now.
	Now func() time.Time
	// ReuseGrace is the refresh race window; defaults to DefaultReuseGrace.
	ReuseGrace time.Duration
	// MFAKey is Config.MFAKey (at least 32 bytes). TOTP secrets are sealed
	// with AES-256-GCM and backup codes hashed with HMAC-SHA256, under keys
	// derived from it. Without it every two-step route fails closed (503).
	MFAKey []byte
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) grace() time.Duration {
	if s.ReuseGrace > 0 {
		return s.ReuseGrace
	}
	return DefaultReuseGrace
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (s *Service) issuer() token.Issuer {
	i := s.Issuer
	i.Now = s.now
	return i
}

// Register adds the auth routes to m. Every auth response carries
// Cache-Control: no-store, and every route that returns a token is wrapped
// in httpx.NoStore, so an Idempotency-Key replay never re-sends it.
// Idempotency itself comes from the mux: per principal on authenticated
// routes, per client IP on the public ones.
//
// The signed-in routes are SelfService: the account's own two-step,
// password and session housekeeping, open to clinical and platform
// accounts (auditors included) on their own audience. None is a Write.
func (s *Service) Register(m *authz.Mux) {
	h := func(method, pattern string, rule authz.Rule, next http.Handler) {
		authz.Route(m, method, pattern, rule, noCache(next))
	}
	public := authz.Rule{Public: true}
	// Enrol, verify, password change and logout also accept a session
	// limited to enrolment; every other route refuses it (mfa_required).
	enrol := authz.Rule{SelfService: true, MFAEnrol: true}
	signedIn := authz.Rule{SelfService: true}
	h(http.MethodPost, "/auth/login", public, httpx.NoStore(http.HandlerFunc(s.login)))
	h(http.MethodPost, "/auth/refresh", public, httpx.NoStore(http.HandlerFunc(s.refresh)))
	h(http.MethodPost, "/auth/mfa/challenge", public, httpx.NoStore(http.HandlerFunc(s.challenge)))
	h(http.MethodPost, "/auth/mfa/enroll", enrol, httpx.NoStore(http.HandlerFunc(s.enroll)))
	h(http.MethodPost, "/auth/mfa/verify", enrol, http.HandlerFunc(s.verify))
	h(http.MethodPost, "/auth/mfa/recovery-codes", authz.Rule{SelfService: true, StepUp: true}, httpx.NoStore(http.HandlerFunc(s.recoveryCodes)))
	h(http.MethodPost, "/auth/password", enrol, http.HandlerFunc(s.changePassword))
	h(http.MethodPost, "/auth/logout", enrol, http.HandlerFunc(s.logout))
	h(http.MethodPost, "/auth/terms", signedIn, http.HandlerFunc(s.terms))
	h(http.MethodGet, "/auth/sessions", signedIn, http.HandlerFunc(s.listSessions))
	h(http.MethodDelete, "/auth/sessions/{id}", signedIn, http.HandlerFunc(s.deleteSession))
	h(http.MethodPost, "/auth/sessions/revoke-all", signedIn, http.HandlerFunc(s.revokeAll))
}

// noCache sets Cache-Control: no-store before the handler runs.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// withAuth runs fn as mhai_auth in a READ COMMITTED transaction (the audit
// chain needs it; it is Postgres's default and the pool doesn't change it).
func (s *Service) withAuth(ctx context.Context, fn func(pgx.Tx) error) error {
	return db.WithAuth(ctx, s.Pool, fn)
}

// writeAudit appends an auth event. Payloads hold ids and fixed words only.
func writeAudit(ctx context.Context, tx pgx.Tx, r *http.Request, action, actor, hospital, entity, entityID string, payload map[string]any) error {
	return audit.Write(ctx, tx, audit.Event{
		ActorID: actor, HospitalID: hospital, RequestID: httpx.RequestIDFrom(r.Context()),
		Action: action, Entity: entity, EntityID: entityID, Payload: payload,
	})
}

// fail logs a server-side failure with only the request id and a fixed
// message, then sends code. err is classified, never printed, because
// driver errors can carry row values (R2).
func (s *Service) fail(w http.ResponseWriter, r *http.Request, code, msg string, attrs ...any) {
	attrs = append([]any{"request_id", httpx.RequestIDFrom(r.Context())}, attrs...)
	s.log().Error(msg, attrs...)
	httpx.Error(w, r, code)
}
