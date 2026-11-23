//go:build integration

package auth_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// POST /auth/password/reset: a hospital admin's reset link and an invite
// (migration 00050). Tokens are seeded as the admin routes store them:
// only their hash.

const resetPW = "synthetic-quartz-meadow-8841-lark"

// seedToken stores a token for user u of hospital h and returns the plain
// one. ttl is a Postgres interval; a negative one seeds an expired token.
func seedToken(t *testing.T, h, u uuid.UUID, purpose, ttl string) string {
	t.Helper()
	plain, hash := token.NewRefresh()
	if _, err := super.Exec(context.Background(), `INSERT INTO account_tokens (hospital_id, user_id, purpose, token_hash, created_by, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $2, now() - interval '1 hour', now() + $5::interval)`, h, u, purpose, hash, ttl); err != nil {
		t.Fatal(err)
	}
	return plain
}

func hospitalOf(t *testing.T, u uuid.UUID) uuid.UUID {
	t.Helper()
	var h uuid.UUID
	if err := super.QueryRow(context.Background(), `SELECT hospital_id FROM users WHERE id = $1`, u).Scan(&h); err != nil {
		t.Fatal(err)
	}
	return h
}

func (e *env) reset(t *testing.T, tok, pw, ip string) resp {
	t.Helper()
	return e.do(t, req{method: "POST", path: "/auth/password/reset", body: map[string]any{"token": tok, "new_password": pw}, ip: ip})
}

func TestPasswordResetLink(t *testing.T) {
	e := newEnv(t)
	u, _ := loggedIn(t, e)
	h := hospitalOf(t, u.id)
	plain := seedToken(t, h, u.id, "password_reset", "29 minutes")

	mustCode(t, "unknown token", e.reset(t, "synthetic-not-a-token", resetPW, newIP()), 410, "invalid_token")
	mustCode(t, "missing password", e.do(t, req{method: "POST", path: "/auth/password/reset", body: map[string]any{"token": plain}}), 422, "validation_failed")
	mustCode(t, "weak password", e.reset(t, plain, "short", newIP()), 422, "weak_password")
	r := e.reset(t, plain, resetPW, newIP())
	if r.code != 204 {
		t.Fatalf("reset %d %s", r.code, r.raw)
	}
	hasAction(t, r, "auth.password.reset")
	var live int
	_ = super.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, u.id).Scan(&live)
	if live != 0 {
		t.Errorf("%d sessions from before the reset are live", live)
	}
	if x := e.login(t, u.email, resetPW, newIP()); x.code != 200 {
		t.Fatalf("new password %d", x.code)
	}
	if x := e.login(t, u.email, testPassword, newIP()); x.code != 401 {
		t.Fatalf("old password %d", x.code)
	}
	// Used: never again.
	mustCode(t, "reuse", e.reset(t, plain, resetPW+"-again", newIP()), 410, "invalid_token")
	// Expired, or the account deactivated: the same reply.
	mustCode(t, "expired", e.reset(t, seedToken(t, h, u.id, "invite", "-1 minute"), resetPW, newIP()), 410, "invalid_token")
	dead := seedToken(t, h, u.id, "password_reset", "20 minutes")
	if _, err := super.Exec(context.Background(), `UPDATE users SET is_active = false WHERE id = $1`, u.id); err != nil {
		t.Fatal(err)
	}
	mustCode(t, "deactivated", e.reset(t, dead, resetPW+"-x", newIP()), 410, "invalid_token")
	for _, s := range []string{plain, dead, resetPW, u.email} {
		if strings.Contains(e.logs.String(), s) {
			t.Errorf("logs carry a secret or an address")
		}
	}
}

// An invite: the invited account has no password until its link is used;
// then the person signs in (and enrols two-step as any new account).
func TestInviteLinkSetsFirstPassword(t *testing.T) {
	e := newEnv(t)
	h := seedHospital(t, hospitalOpts{})
	email := "synthetic.invitee." + uniq() + "@pilot-hospital.example"
	var id uuid.UUID
	if err := super.QueryRow(context.Background(), `INSERT INTO users (hospital_id, email, display_name, invited_at) VALUES ($1, $2, 'synthetic', now()) RETURNING id`,
		h, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(context.Background(), `INSERT INTO user_roles (user_id, role) VALUES ($1, 'clinician')`, id); err != nil {
		t.Fatal(err)
	}
	if x := e.login(t, email, resetPW, newIP()); x.code != 401 {
		t.Fatalf("an invitee signed in before choosing a password: %d", x.code)
	}
	plain := seedToken(t, h, id, "invite", "6 days")
	if r := e.reset(t, plain, resetPW, newIP()); r.code != 204 {
		t.Fatalf("accept %d %s", r.code, r.raw)
	}
	if x := e.login(t, email, resetPW, newIP()); x.code != 200 {
		t.Fatalf("sign-in after the invite %d %s", x.code, x.raw)
	}
}

// Two uses of one token at once: one wins.
func TestResetTokenSingleUseUnderRace(t *testing.T) {
	e := newEnv(t)
	u, _ := loggedIn(t, e)
	plain := seedToken(t, hospitalOf(t, u.id), u.id, "password_reset", "20 minutes")
	codes := make([]int, 4)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = e.reset(t, plain, resetPW+"-race", newIP()).code
		}(i)
	}
	wg.Wait()
	won := 0
	for _, c := range codes {
		switch c {
		case 204:
			won++
		case 410:
		default:
			t.Errorf("code %d", c)
		}
	}
	if won != 1 {
		t.Fatalf("%d uses won (%v)", won, codes)
	}
}

// Guessing from one address is cut off; the counter isn't spent by a
// success.
func TestResetRateLimitedPerIP(t *testing.T) {
	e := newEnv(t)
	ip := newIP()
	var r resp
	for range 11 {
		r = e.reset(t, "synthetic-guess-"+uniq(), resetPW, ip)
	}
	mustCode(t, "eleventh guess", r, 429, "rate_limited")
	// Another address is unaffected.
	mustCode(t, "another address", e.reset(t, "synthetic-guess-"+uniq(), resetPW, newIP()), 410, "invalid_token")
}
