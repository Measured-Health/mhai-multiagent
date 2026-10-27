//go:build integration

package auth_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// seedDevice stores a trusted device for user (hash only) and returns the
// plain device token.
func seedDevice(t *testing.T, userID uuid.UUID, expires time.Time) string {
	t.Helper()
	plain := token.NewOpaque()
	if _, err := super.Exec(context.Background(),
		`INSERT INTO trusted_devices (user_id, token_hash, expires_at) VALUES ($1, $2, $3)`,
		userID, token.HashRefresh(plain), expires); err != nil {
		t.Fatal(err)
	}
	return plain
}

func (e *env) loginDevice(t *testing.T, email, device string) resp {
	t.Helper()
	return e.do(t, req{method: "POST", path: "/auth/login", ip: newIP(),
		body: map[string]any{"username": email, "password": testPassword, "device_token": device}})
}

func TestLockoutBurst(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	ip := newIP()
	const n = 20
	var wg sync.WaitGroup
	var unauth, locked, other atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := e.login(t, u.email, "not-the-password-at-all", ip)
			switch {
			case r.code == 401:
				unauth.Add(1)
			case r.code == 429 && r.body["code"] == "login_locked":
				locked.Add(1)
			default:
				other.Add(1)
			}
		}()
	}
	wg.Wait()
	t.Logf("burst of %d: %d password checks (401), %d locked, %d other", n, unauth.Load(), locked.Load(), other.Load())
	if unauth.Load() != auth.MaxLoginFailures || other.Load() != 0 || unauth.Load()+locked.Load() != n {
		t.Fatalf("burst: %d checked, %d locked, %d other", unauth.Load(), locked.Load(), other.Load())
	}
	if r := e.login(t, u.email, testPassword, ip); r.code != 429 {
		t.Fatalf("correct password after burst: %d", r.code)
	}
}

func TestLoginMissingClientIP(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	hr := httptest.NewRequest("POST", httpx.BasePath+"/auth/login",
		strings.NewReader(`{"username":"`+u.email+`","password":"`+testPassword+`"}`))
	hr.Header.Set("X-MHAI-Service", serviceKey)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, hr)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), `"not_ready"`) {
		t.Fatalf("missing client IP: %d %s", rec.Code, rec.Body.String())
	}
}

func TestTrustedDeviceSkip(t *testing.T) {
	e := newEnv(t)
	trusting := seedHospital(t, hospitalOpts{requireMFAClinicians: true, allowTrust: true})
	future := time.Now().Add(24 * time.Hour)

	// 1. A clinician-only account with a valid device token skips the code.
	a := seedUser(t, trusting, true, "clinician")
	devA := seedDevice(t, a.id, future)
	r := e.loginDevice(t, a.email, devA)
	if r.code != 200 || r.body["access_token"] == nil {
		t.Fatalf("valid device: %d %s", r.code, r.raw)
	}
	c, err := token.Verify(r.body["access_token"].(string), issuer.PublicKeys())
	if err != nil || !c.MFA || strings.Join(c.AMR, ",") != "pwd,device" {
		t.Fatalf("device claims %+v (%v)", c, err)
	}
	var mfa bool
	var amr []string
	_ = super.QueryRow(context.Background(), `SELECT mfa, amr FROM sessions WHERE id = $1`, c.Sid).Scan(&mfa, &amr)
	if !mfa || strings.Join(amr, ",") != "pwd,device" {
		t.Fatalf("session row mfa=%v amr=%v", mfa, amr)
	}

	mustMFA := func(name string, r resp) {
		t.Helper()
		if r.code != 200 || r.body["mfa_required"] != true || r.body["access_token"] != nil {
			t.Fatalf("%s: want mfa_required, got %d %s", name, r.code, r.raw)
		}
	}
	// 2. Another user's device token.
	b := seedUser(t, trusting, true, "clinician")
	mustMFA("other user's device", e.loginDevice(t, b.email, devA))
	// 3. An expired device.
	cx := seedUser(t, trusting, true, "clinician")
	mustMFA("expired device", e.loginDevice(t, cx.email, seedDevice(t, cx.id, time.Now().Add(-time.Minute))))
	// 4. The hospital doesn't allow trusted devices.
	strict := seedHospital(t, hospitalOpts{requireMFAClinicians: true, allowTrust: false})
	d := seedUser(t, strict, true, "clinician")
	mustMFA("trust off", e.loginDevice(t, d.email, seedDevice(t, d.id, future)))
	// 5. A hospital admin never skips, even as a clinician with a valid device.
	adm := seedUser(t, trusting, true, "clinician", "hospital_admin")
	mustMFA("hospital_admin", e.loginDevice(t, adm.email, seedDevice(t, adm.id, future)))
	an := seedUser(t, trusting, true, "clinician", "analyst")
	mustMFA("analyst", e.loginDevice(t, an.email, seedDevice(t, an.id, future)))
}

// deviceSession signs a clinician in on a trusted device.
func deviceSession(t *testing.T, e *env) (user, uuid.UUID, resp) {
	t.Helper()
	hid := seedHospital(t, hospitalOpts{requireMFAClinicians: true, allowTrust: true})
	u := seedUser(t, hid, true, "clinician")
	r := e.loginDevice(t, u.email, seedDevice(t, u.id, time.Now().Add(24*time.Hour)))
	if r.code != 200 || r.body["access_token"] == nil {
		t.Fatalf("device login: %d %s", r.code, r.raw)
	}
	return u, hid, r
}

// assertEnded checks that refresh now fails and the session's access token
// is refused.
func assertEnded(t *testing.T, e *env, l resp) {
	t.Helper()
	if r := e.refresh(t, l.body["refresh_token"].(string)); r.code != 401 {
		t.Fatalf("refresh: want 401, got %d %s", r.code, r.raw)
	}
	c, _ := token.Verify(l.body["access_token"].(string), issuer.PublicKeys())
	var revoked bool
	_ = super.QueryRow(context.Background(), `SELECT revoked_at IS NOT NULL FROM sessions WHERE id = $1`, c.Sid).Scan(&revoked)
	if !revoked {
		t.Fatal("session not revoked")
	}
	if pr := e.do(t, req{method: "GET", path: "/probe", bearer: l.body["access_token"].(string)}); pr.code != 401 {
		t.Fatalf("access token still accepted: %d", pr.code)
	}
}

func TestRefreshEndsDeviceSessionOnNewRole(t *testing.T) {
	e := newEnv(t)
	u, _, l := deviceSession(t, e)
	if _, err := super.Exec(context.Background(), `INSERT INTO user_roles (user_id, role) VALUES ($1, 'hospital_admin')`, u.id); err != nil {
		t.Fatal(err)
	}
	assertEnded(t, e, l)
}

func TestRefreshEndsDeviceSessionWhenTrustOff(t *testing.T) {
	e := newEnv(t)
	_, hid, l := deviceSession(t, e)
	_, _ = super.Exec(context.Background(), `UPDATE hospital_settings SET allow_trust_device = false WHERE hospital_id = $1`, hid)
	assertEnded(t, e, l)
}

func TestRefreshEndsSessionWhenClinicianPolicyOn(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t, hospitalOpts{})
	u := seedUser(t, hid, false, "clinician")
	l := e.login(t, u.email, testPassword, newIP())
	if l.code != 200 || l.body["access_token"] == nil {
		t.Fatalf("login %d %s", l.code, l.raw)
	}
	_, _ = super.Exec(context.Background(), `UPDATE hospital_settings SET require_mfa_for_clinicians = true WHERE hospital_id = $1`, hid)
	assertEnded(t, e, l)
}

func TestRefreshKeepsSatisfiedSession(t *testing.T) {
	e := newEnv(t)
	_, _, dev := deviceSession(t, e)
	_, plain := loggedIn(t, e)
	for name, l := range map[string]resp{"device session": dev, "clinician without policy": plain} {
		rt := l.body["refresh_token"].(string)
		for i := 0; i < 3; i++ {
			e.clock.Add(time.Minute)
			r := e.refresh(t, rt)
			if r.code != 200 {
				t.Fatalf("%s refresh %d: %d %s", name, i+1, r.code, r.raw)
			}
			rt = r.body["refresh_token"].(string)
		}
	}
}

func TestMFAAttemptBurst(t *testing.T) {
	e := newEnv(t)
	u := seedUser(t, seedHospital(t, hospitalOpts{}), true, "analyst")
	tok := e.login(t, u.email, testPassword, newIP()).body["mfa_token"].(string)
	store := e.svc.MFATokens()
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.Attempt(context.Background(), tok); err == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 5 {
		t.Fatalf("%d attempts allowed, want exactly 5", ok.Load())
	}
	// A correct code on the last attempt can still be consumed, once.
	tok = e.login(t, u.email, testPassword, newIP()).body["mfa_token"].(string)
	for i := 0; i < 5; i++ {
		if _, err := store.Attempt(context.Background(), tok); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Consume(context.Background(), tok); err != nil {
		t.Fatalf("consume after the 5th attempt: %v", err)
	}
	if _, err := store.Consume(context.Background(), tok); err == nil {
		t.Fatal("consumed twice")
	}
}

func TestReuseRetryStillDenylists(t *testing.T) {
	e := newEnv(t)
	_, l := loggedIn(t, e)
	old := l.body["refresh_token"].(string)
	r := e.refresh(t, old)
	live, _ := token.Verify(r.body["access_token"].(string), issuer.PublicKeys())
	e.clock.Add(11 * time.Second)
	if x := e.refresh(t, old); x.code != 401 {
		t.Fatalf("reuse: %d", x.code)
	}
	// Simulate a lost denylist write, then a retry of the stolen token.
	ctx := context.Background()
	_ = buckets.Denylist.Delete(ctx, "jti."+live.Jti)
	_ = buckets.Denylist.Delete(ctx, "sid."+live.Sid)
	if x := e.refresh(t, old); x.code != 401 {
		t.Fatalf("retry: %d", x.code)
	}
	if !e.svc.Denylisted(ctx, live) {
		t.Fatal("retry did not restore the denylist")
	}
}

func TestMissingSettingsRequiresMFA(t *testing.T) {
	e := newEnv(t)
	var hid uuid.UUID
	if err := super.QueryRow(context.Background(), `INSERT INTO hospitals (name, short_code, status)
		VALUES ('Synthetic General', $1, 'active') RETURNING id`, "H"+uniq()).Scan(&hid); err != nil {
		t.Fatal(err)
	}
	u := seedUser(t, hid, true, "clinician")
	if r := e.login(t, u.email, testPassword, newIP()); r.body["mfa_required"] != true {
		t.Fatalf("clinician without a settings row: %d %s", r.code, r.raw)
	}
}
