package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/gateway/internal/account"
	"github.com/Measured-Health/mhai-multiagent/gateway/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/gateway/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/gateway/internal/password"
	"github.com/Measured-Health/mhai-multiagent/gateway/internal/token"
)

// Input bounds. A longer value is a validation failure, not a sign-in
// attempt.
const (
	maxUsernameLen = 320
	maxPasswordLen = 1024
	maxUALen       = 120
)

type loginRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DeviceToken string `json:"device_token,omitempty"`
}

type loginResponse struct {
	AccessToken  string   `json:"access_token"`
	ExpiresIn    int      `json:"expires_in"`
	RefreshToken string   `json:"refresh_token"`
	User         userView `json:"user"`
}

type mfaResponse struct {
	MFARequired bool   `json:"mfa_required"`
	MFAToken    string `json:"mfa_token"`
}

// LockoutKey is the login_attempts key for (login, client IP):
// hex(sha256(lower(trimmed email) | ip)) (Ruling 3). The email is never a key.
func LockoutKey(email, ip string) string {
	sum := sha256.Sum256([]byte(account.NormaliseEmail(email) + "|" + ip))
	return hex.EncodeToString(sum[:])
}

func clientIP(r *http.Request) string {
	ip := httpx.ClientIP(r.Context())
	if !ip.IsValid() {
		return ""
	}
	return ip.String()
}

// login: POST /auth/login. Unknown, inactive and wrong-password logins get
// the same 401 after the same argon2 work. Each attempt first reserves a
// slot in the (login, IP) counter atomically; one that finds the counter
// already at 5 gets 429 login_locked without verifying and without writing.
// If the counter can't be read or written, sign-in fails closed (503).
func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	var in loginRequest
	if !httpx.Decode(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Username) == "" || in.Password == "" ||
		len(in.Username) > maxUsernameLen || len(in.Password) > maxPasswordLen || len(in.DeviceToken) > 256 {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	rid := httpx.RequestIDFrom(ctx)
	ip := clientIP(r)
	if ip == "" {
		// The BFF must send X-MHAI-Client-IP (contract §2.1); without it every
		// request would share one counter per login, which anyone could trip.
		s.fail(w, r, "not_ready", "auth: login without a client IP; check the BFF configuration")
		return
	}
	email := account.NormaliseEmail(in.Username)
	key := LockoutKey(email, ip)

	// Before the account lookup, so the reply never depends on the account.
	locked, err := s.reserveAttempt(ctx, key)
	if err != nil {
		s.fail(w, r, "not_ready", "auth: login attempts unavailable")
		return
	}

	var acct *acctRec
	err = s.withAuth(ctx, func(tx pgx.Tx) error {
		a, err := accountByEmail(ctx, tx, email)
		if err != nil && !errors.Is(err, errNoAccount) {
			return err
		}
		acct = a
		return nil
	})
	if err != nil {
		s.fail(w, r, "internal_error", "auth: login lookup failed")
		return
	}
	actor, hospital := "", ""
	if acct != nil {
		actor, hospital = acct.ID.String(), acct.hospitalID()
	}

	if locked {
		s.auditOnly(w, r, "auth.login.locked", actor, hospital, map[string]any{"reason": "locked"})
		s.log().Info("auth: login locked", "request_id", rid, "user_id", actor)
		httpx.Error(w, r, "login_locked")
		return
	}

	// The argon2 work is the same whether or not the account exists.
	ok := false
	if acct != nil && acct.PasswordHash != nil && *acct.PasswordHash != "" {
		ok, _ = password.Verify(in.Password, *acct.PasswordHash)
	} else {
		password.DummyVerify(in.Password)
	}
	if !ok || !acct.usable() {
		// The attempt was already counted by reserveAttempt.
		s.auditOnly(w, r, "auth.login.failed", actor, hospital, map[string]any{"reason": "bad_credentials"})
		s.log().Info("auth: login failed", "request_id", rid, "user_id", actor)
		httpx.Error(w, r, "unauthenticated")
		return
	}
	if err := s.Buckets.LoginAttempts.Delete(ctx, key); err != nil && !bus.IsNotFound(err) {
		s.log().Error("auth: login attempts reset failed", "request_id", rid, "user_id", actor)
	}

	// The audience check comes only after the password, so it reveals
	// nothing about accounts the caller can't sign in to.
	platform := httpx.Audience(ctx) == httpx.AudiencePlatform
	if platform != hasPlatformRole(acct.Roles) {
		reason, code := "not_platform", "not_platform"
		if !platform {
			reason, code = "platform_account", "forbidden"
		}
		s.auditOnly(w, r, "auth.login.failed", actor, hospital, map[string]any{"reason": reason})
		httpx.Error(w, r, code)
		return
	}

	rehash := password.NeedsRehash(*acct.PasswordHash)
	mfa, amr := false, []string{"pwd"}
	if !sessionSatisfiesMFA(acct.Roles, acct.policy(), mfa, amr) {
		// A trusted device may stand in for the code only where the shared
		// check accepts it (clinician-only account, hospital allows it).
		if in.DeviceToken != "" && sessionSatisfiesMFA(acct.Roles, acct.policy(), true, []string{"pwd", "device"}) {
			trusted, err := s.trustedDevice(ctx, acct.ID, in.DeviceToken)
			if err != nil {
				s.fail(w, r, "internal_error", "auth: trusted device lookup failed", "user_id", actor)
				return
			}
			if trusted {
				mfa, amr = true, []string{"pwd", "device"}
			}
		}
		if !sessionSatisfiesMFA(acct.Roles, acct.policy(), mfa, amr) {
			s.mfaPending(w, r, acct, in.Password, rehash)
			return
		}
	}
	s.startSession(w, r, acct, mfa, amr, func(ctx context.Context, tx pgx.Tx) error {
		return s.maybeRehash(ctx, tx, acct.ID, in.Password, rehash)
	})
}

// mfaPending answers the two-step branch: a short-lived single-use
// mfa_token and no access token. No session exists until the challenge.
func (s *Service) mfaPending(w http.ResponseWriter, r *http.Request, acct *acctRec, pw string, rehash bool) {
	ctx := r.Context()
	tok, err := s.MFATokens().Issue(ctx, MFAToken{UserID: acct.ID.String(), Enrolled: acct.MFAEnabled,
		DeviceAllowed: acct.AllowTrustDevice && deviceMaySkip(acct.Roles)})
	if err != nil {
		s.fail(w, r, "not_ready", "auth: mfa token store failed", "user_id", acct.ID.String())
		return
	}
	err = s.withAuth(ctx, func(tx pgx.Tx) error {
		if err := s.maybeRehash(ctx, tx, acct.ID, pw, rehash); err != nil {
			return err
		}
		return writeAudit(ctx, tx, r, "auth.login.succeeded", acct.ID.String(), acct.hospitalID(), "user", acct.ID.String(),
			map[string]any{"mfa_required": true})
	})
	if err != nil {
		s.fail(w, r, "internal_error", "auth: login audit failed", "user_id", acct.ID.String())
		return
	}
	httpx.JSON(w, http.StatusOK, mfaResponse{MFARequired: true, MFAToken: tok})
}

// startSession opens a session with its first refresh token, mints the
// access token, records the sign-in and replies with the login body. extra
// runs in the same transaction. Task 7's challenge finishes through here.
func (s *Service) startSession(w http.ResponseWriter, r *http.Request, acct *acctRec, mfa bool, amr []string,
	extra func(context.Context, pgx.Tx) error) {
	ctx := r.Context()
	now := s.now()
	sid, jti := uuid.New(), uuid.New()
	access, exp, err := s.issuer().Access(token.Claims{Sub: acct.ID.String(), Hid: acct.hospitalID(), Roles: acct.Roles,
		MFA: mfa, AMR: amr, Sid: sid.String(), Jti: jti.String()})
	if err != nil {
		s.fail(w, r, "internal_error", "auth: sign access token failed", "user_id", acct.ID.String())
		return
	}
	refresh, refreshHash := token.NewRefresh()
	err = s.withAuth(ctx, func(tx pgx.Tx) error {
		if extra != nil {
			if err := extra(ctx, tx); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO sessions (id, user_id, created_at, last_seen_at, ua_summary, ip_prefix, access_jti, access_expires_at, mfa, amr)
			VALUES ($1, $2, $3, $3, $4, $5, $6, $7, $8, $9)`,
			sid, acct.ID, now, uaSummary(r.UserAgent()), nullIfEmpty(ipPrefix(clientIP(r))), jti, exp, mfa, amr); err != nil {
			return errors.New("insert session failed")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO refresh_tokens (session_id, token_hash, issued_at) VALUES ($1, $2, $3)`,
			sid, refreshHash, now); err != nil {
			return errors.New("insert refresh token failed")
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET last_login_at = $2 WHERE id = $1`, acct.ID, now); err != nil {
			return errors.New("update last login failed")
		}
		return writeAudit(ctx, tx, r, "auth.login.succeeded", acct.ID.String(), acct.hospitalID(), "session", sid.String(),
			map[string]any{"mfa": mfa, "amr": amr})
	})
	if err != nil {
		s.fail(w, r, "internal_error", "auth: open session failed", "user_id", acct.ID.String())
		return
	}
	httpx.JSON(w, http.StatusOK, loginResponse{AccessToken: access, ExpiresIn: int(token.AccessTTL / time.Second),
		RefreshToken: refresh, User: acct.view()})
}

func (s *Service) maybeRehash(ctx context.Context, tx pgx.Tx, id uuid.UUID, pw string, rehash bool) error {
	if !rehash {
		return nil
	}
	h, err := password.Hash(pw)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, id, h); err != nil {
		return errors.New("rehash failed")
	}
	return nil
}

// trustedDevice reports whether deviceToken is a live trusted device of
// user id (bound to that user; anyone else's token doesn't match).
func (s *Service) trustedDevice(ctx context.Context, id uuid.UUID, deviceToken string) (bool, error) {
	var ok bool
	err := s.withAuth(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM trusted_devices
			WHERE user_id = $1 AND token_hash = $2 AND expires_at > $3)`,
			id, token.HashRefresh(deviceToken), s.now()).Scan(&ok)
	})
	return ok, err
}

// auditOnly writes one audit row in its own transaction. A failure is
// logged; the reply to the client doesn't change.
func (s *Service) auditOnly(_ http.ResponseWriter, r *http.Request, action, actor, hospital string, payload map[string]any) {
	ctx := r.Context()
	entity, entityID := "", ""
	if actor != "" {
		entity, entityID = "user", actor
	}
	if err := s.withAuth(ctx, func(tx pgx.Tx) error {
		return writeAudit(ctx, tx, r, action, actor, hospital, entity, entityID, payload)
	}); err != nil {
		s.log().Error("auth: audit write failed", "request_id", httpx.RequestIDFrom(ctx), "action", action)
	}
}

// reserveAttempt counts this attempt against key before the password is
// checked, by compare-and-set, so a concurrent burst gets at most
// MaxLoginFailures checks. locked is true when the counter was already at
// the limit; then nothing is written (the bucket's lifetime runs from the
// last write, so locked requests must not extend it). An error (KV failure,
// or contention that outlasts the retries) means the caller must fail closed.
func (s *Service) reserveAttempt(ctx context.Context, key string) (locked bool, err error) {
	kv := s.Buckets.LoginAttempts
	for i := 0; i < 16; i++ {
		e, err := kv.Get(ctx, key)
		if bus.IsNotFound(err) {
			_, err := kv.Create(ctx, key, []byte("1"))
			if err == nil {
				return false, nil
			}
			if !errors.Is(err, jetstream.ErrKeyExists) && !isRevisionConflict(err) {
				return false, err
			}
			continue
		}
		if err != nil {
			return false, err
		}
		n, err := strconv.Atoi(string(e.Value()))
		if err != nil || n >= MaxLoginFailures {
			return true, nil // an unreadable counter counts as locked
		}
		_, err = kv.Update(ctx, key, []byte(strconv.Itoa(n+1)), e.Revision())
		if err == nil {
			return false, nil
		}
		if !isRevisionConflict(err) {
			return false, err
		}
	}
	return false, errors.New("auth: login attempts contention")
}

// isRevisionConflict reports a lost compare-and-set (another writer won).
func isRevisionConflict(err error) bool {
	var apiErr *jetstream.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence
}

// ipPrefix is the client's /24 (IPv4) or /48 (IPv6), for the device list.
func ipPrefix(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	a = a.Unmap()
	bits := 48
	if a.Is4() {
		bits = 24
	}
	p, err := a.Prefix(bits)
	if err != nil {
		return ""
	}
	return p.String()
}

// uaSummary keeps a short printable prefix of the user agent.
func uaSummary(ua string) any {
	ua = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, ua)
	if rs := []rune(ua); len(rs) > maxUALen {
		ua = string(rs[:maxUALen])
	}
	return nullIfEmpty(strings.TrimSpace(ua))
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
