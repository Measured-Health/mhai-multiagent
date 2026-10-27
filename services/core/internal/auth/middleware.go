package auth

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

type claimsKey struct{}

// ClaimsFrom returns the verified access-token claims RequireToken stored.
func ClaimsFrom(ctx context.Context) (token.Claims, bool) {
	c, ok := ctx.Value(claimsKey{}).(token.Claims)
	return c, ok
}

// Denylist keys. jti and sid are server-made uuids, never personal data.
func jtiKey(jti string) string { return "jti." + jti }
func sidKey(sid string) string { return "sid." + sid }

// RequireToken is the interim access check (Ruling 2): a Bearer token whose
// signature, iss, aud = mhai-bff and exp verify, and whose jti and session
// are not on the denylist. It then sets the claims and httpx.WithPrincipal.
// A missing, bad or revoked token is 401 unauthenticated, an expired one
// 401 token_expired. If the denylist can't be read it fails closed (503).
//
// The token must also meet the account's current two-step policy: a token
// from a session limited to enrolment (an account that must use two-step
// but hasn't set it up) gets 403 mfa_required. Only enrol, verify,
// password change and logout accept such a token (requireTokenEnrol).
func (s *Service) RequireToken(next http.Handler) http.Handler {
	return s.requireToken(next, false)
}

// requireTokenEnrol is RequireToken that also accepts a session limited to
// enrolment.
func (s *Service) requireTokenEnrol(next http.Handler) http.Handler {
	return s.requireToken(next, true)
}

// tokenMeetsMFA reports whether c meets the account's current two-step
// policy. A token proven by a code does without a lookup; any other token
// is checked against the account's roles and hospital policy now (Task 8's
// principal middleware caches this).
func (s *Service) tokenMeetsMFA(ctx context.Context, c token.Claims) (ok, found bool, err error) {
	if c.MFA && (slices.Contains(c.AMR, "otp") || slices.Contains(c.AMR, "backup")) {
		return true, true, nil
	}
	id, err := uuid.Parse(c.Sub)
	if err != nil {
		return false, false, nil
	}
	var acct *acctRec
	err = s.withAuth(ctx, func(tx pgx.Tx) error {
		a, err := accountByID(ctx, tx, id)
		if err != nil && !errors.Is(err, errNoAccount) {
			return err
		}
		acct = a
		return nil
	})
	if err != nil {
		return false, false, err
	}
	if acct == nil || !acct.usable() {
		return false, false, nil
	}
	return sessionSatisfiesMFA(acct.Roles, acct.policy(), c.MFA, c.AMR), true, nil
}

func (s *Service) requireToken(next http.Handler, allowEnrol bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearer(r.Header.Values("Authorization"))
		if !ok {
			httpx.Error(w, r, "unauthenticated")
			return
		}
		c, err := token.VerifyAt(raw, s.Keys, s.now())
		if errors.Is(err, token.ErrExpired) {
			httpx.Error(w, r, "token_expired")
			return
		}
		if err != nil {
			httpx.Error(w, r, "unauthenticated")
			return
		}
		var hid uuid.UUID
		if c.Hid != "" {
			if hid, err = uuid.Parse(c.Hid); err != nil {
				httpx.Error(w, r, "unauthenticated")
				return
			}
		}
		denied, err := s.denied(r.Context(), c)
		if err != nil {
			s.fail(w, r, "not_ready", "auth: denylist unavailable")
			return
		}
		if denied {
			httpx.Error(w, r, "unauthenticated")
			return
		}
		if !allowEnrol {
			ok, found, err := s.tokenMeetsMFA(r.Context(), c)
			if err != nil {
				s.fail(w, r, "internal_error", "auth: account lookup failed", "user_id", c.Sub)
				return
			}
			if !found {
				httpx.Error(w, r, "unauthenticated")
				return
			}
			if !ok {
				httpx.Error(w, r, "mfa_required")
				return
			}
		}
		ctx := context.WithValue(r.Context(), claimsKey{}, c)
		ctx = httpx.WithPrincipal(ctx, httpx.Principal{UserID: c.Sub, HospitalID: hid})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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

// denied reports whether c's jti or session is on the denylist.
func (s *Service) denied(ctx context.Context, c token.Claims) (bool, error) {
	for _, k := range []string{jtiKey(c.Jti), sidKey(c.Sid)} {
		_, err := s.Buckets.Denylist.Get(ctx, k)
		if err == nil {
			return true, nil
		}
		if !bus.IsNotFound(err) {
			return false, err
		}
	}
	return false, nil
}

// Denylisted reports whether c is refused by the denylist (a read error
// counts as refused).
func (s *Service) Denylisted(ctx context.Context, c token.Claims) bool {
	d, err := s.denied(ctx, c)
	return d || err != nil
}

// denySession puts the session and the given access-token ids on the
// denylist. The bucket's 15-minute lifetime is at least the remaining life
// of any access token the session issued before it was revoked.
func (s *Service) denySession(ctx context.Context, sid string, jtis ...string) error {
	keys := []string{sidKey(sid)}
	for _, j := range jtis {
		if j != "" {
			keys = append(keys, jtiKey(j))
		}
	}
	var errs []error
	for _, k := range keys {
		if _, err := s.Buckets.Denylist.Put(ctx, k, []byte("1")); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.New("auth: denylist write failed")
	}
	return nil
}
