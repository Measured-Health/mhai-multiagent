//go:build integration

package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// mint makes a session row for u and an access token for it, as sign-in
// would, without the sign-in flow: hid "" for platform accounts.
func (e *env) mint(t *testing.T, u user, hid string, roles []string, mfa bool, amr ...string) (string, uuid.UUID) {
	t.Helper()
	if len(amr) == 0 {
		amr = []string{"pwd"}
	}
	sid := uuid.New()
	if _, err := super.Exec(context.Background(), `INSERT INTO sessions (id, user_id, mfa, amr) VALUES ($1, $2, $3, $4)`,
		sid, u.id, mfa, amr); err != nil {
		t.Fatal(err)
	}
	iss := issuer
	iss.Now = e.clock.Now
	at, _, err := iss.Access(token.Claims{Sub: u.id.String(), Hid: hid, Roles: roles, MFA: mfa, AMR: amr,
		Sid: sid.String(), Jti: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	return at, sid
}

func (e *env) sessions(t *testing.T, at string) resp {
	t.Helper()
	return e.do(t, req{method: "GET", path: "/auth/sessions", bearer: at})
}

// Review Focus 3: a user deactivated mid-session keeps working only until
// the 30-second state cache expires, then gets 401 unauthenticated. The
// cache runs on the env's clock.
func TestDeactivatedUserNextRequest401(t *testing.T) {
	e := newEnv(t)
	u, l := loggedIn(t, e)
	at := l.body["access_token"].(string)
	mustOK(t, "before", e.sessions(t, at))
	if _, err := super.Exec(context.Background(), `UPDATE users SET is_active = false WHERE id = $1`, u.id); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(authz.MaxCacheTTL - time.Second)
	mustOK(t, "cached", e.sessions(t, at))
	e.clock.Add(2 * time.Second)
	mustCode(t, "after the cache", e.sessions(t, at), 401, "unauthenticated")
}

func TestDeactivatedHospital401(t *testing.T) {
	for _, status := range []string{"deactivated", "removal_pending", "removed"} {
		e := newEnv(t)
		hid := seedHospital(t, hospitalOpts{})
		u := seedUser(t, hid, false, "clinician")
		at, _ := e.mint(t, u, hid.String(), []string{"clinician"}, false)
		mustOK(t, status+" before", e.sessions(t, at))
		if _, err := super.Exec(context.Background(), `UPDATE hospitals SET status = $2 WHERE id = $1`, hid, status); err != nil {
			t.Fatal(err)
		}
		e.clock.Add(authz.MaxCacheTTL)
		mustCode(t, status+" after", e.sessions(t, at), 401, "unauthenticated")
	}
}

// A session revoked in the database stops within 30 seconds even when its
// denylist entry was never written (the M3 gap after a password change).
func TestRevokedSessionWithoutDenylist401(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	var hid uuid.UUID
	_ = super.QueryRow(context.Background(), `SELECT hospital_id FROM users WHERE id = $1`, u.id).Scan(&hid)
	at, sid := e.mint(t, u, hid.String(), []string{"clinician"}, false)
	mustOK(t, "before", e.sessions(t, at))
	if _, err := super.Exec(context.Background(), `UPDATE sessions SET revoked_at = now() WHERE id = $1`, sid); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(authz.MaxCacheTTL)
	mustCode(t, "after", e.sessions(t, at), 401, "unauthenticated")
}

// The token's hospital must be the account's; a token naming another
// hospital (or a platform token for a clinical account) is refused.
func TestTokenHospitalMustMatchAccount(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	other := seedHospital(t, hospitalOpts{})
	at, _ := e.mint(t, u, other.String(), []string{"clinician"}, false)
	mustCode(t, "other hospital", e.sessions(t, at), 401, "unauthenticated")
	at, _ = e.mint(t, u, "", []string{"clinician"}, false)
	mustCode(t, "no hospital", e.sessions(t, at), 401, "unauthenticated")
}

// A refresh rechecks the account at once, with no cache: a deactivated user
// or hospital can't renew its token.
func TestRefreshFailsForDeactivated(t *testing.T) {
	for name, stmt := range map[string]string{
		"user":     `UPDATE users SET is_active = false WHERE id = $1`,
		"hospital": `UPDATE hospitals SET status = 'deactivated' WHERE id = (SELECT hospital_id FROM users WHERE id = $1)`,
	} {
		e := newEnv(t)
		u, l := loggedIn(t, e)
		at := l.body["access_token"].(string)
		mustOK(t, name+" warm the cache", e.sessions(t, at))
		if _, err := super.Exec(context.Background(), stmt, u.id); err != nil {
			t.Fatal(err)
		}
		mustCode(t, name+" refresh", e.refresh(t, l.body["refresh_token"].(string)), 401, "unauthenticated")
	}
}

// M8: a refresh is honoured only on its account's audience, and the
// refused one changes nothing (the token still refreshes on the right side).
func TestRefreshChecksAudience(t *testing.T) {
	e := newEnv(t)
	_, l := loggedIn(t, e)
	rt := l.body["refresh_token"].(string)
	r := e.do(t, req{method: "POST", path: "/auth/refresh", platform: true, body: map[string]any{"refresh_token": rt}})
	if r.code != 403 || r.raw != `{"code":"not_platform"}` {
		t.Fatalf("clinical refresh on platform: %d %s", r.code, r.raw)
	}
	mustOK(t, "clinical refresh on clinical", e.refresh(t, rt))

	p := seedUser(t, uuid.Nil, false, "platform_admin")
	_, sid := e.mint(t, p, "", []string{"platform_admin"}, true, "pwd", "otp")
	prt, hash := token.NewRefresh()
	if _, err := super.Exec(context.Background(), `INSERT INTO refresh_tokens (session_id, token_hash) VALUES ($1, $2)`, sid, hash); err != nil {
		t.Fatal(err)
	}
	mustCode(t, "platform refresh on clinical", e.refresh(t, prt), 403, "forbidden")
	r = e.do(t, req{method: "POST", path: "/auth/refresh", platform: true, body: map[string]any{"refresh_token": prt}})
	if r.code != 200 {
		t.Fatalf("platform refresh on platform: %d %s", r.code, r.raw)
	}
}

// An account on the other audience is refused on every authenticated
// route: a clinical account on the platform side is not_platform, a
// platform account on the clinical side forbidden.
func TestAudienceMustMatchAccount(t *testing.T) {
	e := newEnv(t)
	_, l := loggedIn(t, e)
	at := l.body["access_token"].(string)
	r := e.do(t, req{method: "GET", path: "/auth/sessions", bearer: at, platform: true})
	if r.code != 403 || r.raw != `{"code":"not_platform"}` {
		t.Fatalf("clinical on platform: %d %s", r.code, r.raw)
	}
	p := seedUser(t, uuid.Nil, false, "auditor")
	pat, _ := e.mint(t, p, "", []string{"auditor"}, true, "pwd", "otp")
	mustCode(t, "platform on clinical", e.sessions(t, pat), 403, "forbidden")
	if r := e.do(t, req{method: "GET", path: "/auth/sessions", bearer: pat, platform: true}); r.code != 200 {
		t.Fatalf("platform on platform: %d %s", r.code, r.raw)
	}
}

// The denylist still fails closed: the state cache never stands in for it.
func TestDenylistedTokenRefusedDespiteCache(t *testing.T) {
	e := newEnv(t)
	_, l := loggedIn(t, e)
	at := l.body["access_token"].(string)
	mustOK(t, "warm", e.sessions(t, at))
	c, _ := token.Verify(at, issuer.PublicKeys())
	if _, err := buckets.Denylist.Put(context.Background(), authz.JTIKey(c.Jti), []byte("1")); err != nil {
		t.Fatal(err)
	}
	mustCode(t, "denylisted", e.sessions(t, at), 401, "unauthenticated")
}

// A role taken away stops working once the cache expires, even though the
// token still lists it.
func TestRemovedRoleDropsFromPrincipal(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t, hospitalOpts{})
	u := seedUser(t, hid, false, "clinician", "analyst")
	at, _ := e.mint(t, u, hid.String(), []string{"clinician", "analyst"}, true, "pwd", "otp")
	p := e.do(t, req{method: "GET", path: "/probe", bearer: at})
	mustOK(t, "probe", p)
	if _, err := super.Exec(context.Background(), `DELETE FROM user_roles WHERE user_id = $1 AND role = 'analyst'`, u.id); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(authz.MaxCacheTTL)
	got := e.guardRoles(t, at)
	if len(got) != 1 || got[0] != "clinician" {
		t.Fatalf("roles after removal %v", got)
	}
}

func mustOK(t *testing.T, name string, r resp) {
	t.Helper()
	if r.code != 200 {
		t.Fatalf("%s: want 200, got %d %s", name, r.code, r.raw)
	}
}

// guardRoles returns the principal's roles as the probe route sees them.
func (e *env) guardRoles(t *testing.T, at string) []string {
	t.Helper()
	r := e.do(t, req{method: "GET", path: "/probe", bearer: at})
	mustOK(t, "probe", r)
	var out []string
	for _, x := range r.body["roles"].([]any) {
		out = append(out, x.(string))
	}
	return out
}

// I1: a platform account can't call a clinical route that any signed-in
// clinical account may use (the probe route, Rule{}), on either audience.
func TestPlatformRefusedOnClinicalAnyRoute(t *testing.T) {
	e := newEnv(t)
	p := seedUser(t, uuid.Nil, true, "platform_admin")
	at, _ := e.mint(t, p, "", []string{"platform_admin"}, true, "pwd", "otp")
	for _, aud := range []bool{true, false} {
		r := e.do(t, req{method: "GET", path: "/probe", bearer: at, platform: aud})
		if r.code != 403 || r.body["code"] != "forbidden" {
			t.Fatalf("platform audience %v: %d %s", aud, r.code, r.raw)
		}
	}
}

// M1: a token whose roles the account no longer holds at all (demoted from
// platform_admin to auditor) is refused, so it can't slip past role checks
// until it expires.
func TestNoRemainingRoles401(t *testing.T) {
	e := newEnv(t)
	p := seedUser(t, uuid.Nil, true, "auditor")
	at, _ := e.mint(t, p, "", []string{"platform_admin"}, true, "pwd", "otp")
	r := e.do(t, req{method: "GET", path: "/auth/sessions", bearer: at, platform: true})
	if r.code != 401 {
		t.Fatalf("demoted token: %d %s", r.code, r.raw)
	}
}

// M7: the enrolment-limited path is only for accounts that haven't turned
// two-step on. An enrolled account's password-only session is refused even
// on the enrolment routes.
func TestEnrolRoutesRefuseEnrolledWithoutCode(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t, hospitalOpts{})
	u := seedUser(t, hid, true, "clinician")
	at, _ := e.mint(t, u, hid.String(), []string{"clinician"}, false)
	mustCode(t, "password", e.do(t, req{method: "POST", path: "/auth/password", bearer: at,
		body: map[string]any{"current_password": testPassword, "new_password": "tangerine-orbit-77-quill"}}), 403, "mfa_required")
	mustCode(t, "logout", e.do(t, req{method: "POST", path: "/auth/logout", bearer: at}), 403, "mfa_required")
}

// M3: a policy tightened mid-session ends a password-only session at the
// middleware once the cache expires (not only at the next refresh).
func TestPolicyTightenedMidSession(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t, hospitalOpts{})
	u := seedUser(t, hid, false, "clinician")
	at, _ := e.mint(t, u, hid.String(), []string{"clinician"}, false)
	mustOK(t, "before", e.sessions(t, at))
	if _, err := super.Exec(context.Background(), `UPDATE hospital_settings SET require_mfa_for_clinicians = true WHERE hospital_id = $1`, hid); err != nil {
		t.Fatal(err)
	}
	mustOK(t, "cached", e.sessions(t, at))
	e.clock.Add(authz.MaxCacheTTL)
	mustCode(t, "after", e.sessions(t, at), 403, "mfa_required")
}
