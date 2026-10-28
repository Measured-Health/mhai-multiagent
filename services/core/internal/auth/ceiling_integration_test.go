//go:build integration

package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
)

// fillCeiling spends the login's per-login ceiling with wrong passwords,
// each from a fresh client IP.
func (e *env) fillCeiling(t *testing.T, email string) {
	t.Helper()
	e.fillCeilingAud(t, email, false)
}

func (e *env) fillCeilingAud(t *testing.T, email string, platform bool) {
	t.Helper()
	for i := 0; i < auth.MaxLoginFailuresPerLogin; i++ {
		r := e.do(t, req{method: "POST", path: "/auth/login", ip: newIP(), platform: platform,
			body: map[string]any{"username": email, "password": "wrong-password-" + uniq()}})
		if r.code != 401 {
			t.Fatalf("failure %d: %d %s", i+1, r.code, r.raw)
		}
	}
}

// counter reads a KV counter ("" when the key is absent).
func counter(t *testing.T, name string, key string, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	kv := buckets.LoginAttempts
	if name == "ceiling" {
		kv = buckets.LoginCeiling
	}
	v, err := kv.Get(context.Background(), key)
	if bus.IsNotFound(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(v.Value())
}

func mustLocked(t *testing.T, name string, r resp) {
	t.Helper()
	if r.code != 429 || r.body["code"] != "login_locked" {
		t.Fatalf("%s: want 429 login_locked, got %d %s", name, r.code, r.raw)
	}
}

// I1: at the ceiling, a right password with the user's own trusted-device
// token still signs in, where the hospital allows trusted devices for a
// clinician. Another user's device, or a wrong password with the right
// device, stays locked. The escape doesn't hand the ceiling a slot back,
// and the request is audited as auth.login.ceiling.
func TestLoginCeilingTrustedDeviceSignsIn(t *testing.T) {
	e := newEnv(t)
	trusting := seedHospital(t, hospitalOpts{requireMFAClinicians: true, allowTrust: true})
	u, dev := e.trustedClinician(t, trusting)
	_, otherDev := e.trustedClinician(t, trusting)
	e.fillCeiling(t, u.email)

	mustLocked(t, "no device", e.login(t, u.email, testPassword, newIP()))
	mustLocked(t, "another user's device", e.loginDevice(t, u.email, otherDev))
	mustLocked(t, "wrong password, right device", e.do(t, req{method: "POST", path: "/auth/login", ip: newIP(),
		body: map[string]any{"username": u.email, "password": "wrong-password-" + uniq(), "device_token": dev}}))

	r := e.loginDevice(t, u.email, dev)
	if r.code != 200 || r.body["access_token"] == nil || r.body["mfa_token"] != nil {
		t.Fatalf("own device at the ceiling: %d %s", r.code, r.raw)
	}
	hasAction(t, r, "auth.login.ceiling")
	hasAction(t, r, "auth.login.succeeded")
	ck, err := e.svc.CeilingKey(u.email)
	if got := counter(t, "ceiling", ck, err); got != "20" {
		t.Fatalf("ceiling after the escape: %q, want 20", got)
	}
}

// I1: a device token that may not stand in for the code never passes the
// ceiling, even with the right password: an admin's old device, a
// clinician's device where the hospital has trusted devices off, and a
// clinician's device on the platform audience. Each gets exactly the wrong
// password's reply, and no mfa_token.
func TestLoginCeilingDeviceThatMayNotStandIn(t *testing.T) {
	e := newEnv(t)
	trusting := seedHospital(t, hospitalOpts{requireMFAClinicians: true, allowTrust: true})
	strict := seedHospital(t, hospitalOpts{requireMFAClinicians: true, allowTrust: false})
	exp := e.clock.Now().Add(24 * time.Hour)

	adm := seedUser(t, trusting, true, "clinician", "hospital_admin")
	admDev := seedDevice(t, adm.id, exp)
	clin := seedUser(t, strict, true, "clinician")
	clinDev := seedDevice(t, clin.id, exp)
	plat, platDev := e.trustedClinician(t, trusting)

	for _, c := range []struct {
		name     string
		u        user
		dev      string
		platform bool
	}{
		{"admin's old device", adm, admDev, false},
		{"trusted devices off", clin, clinDev, false},
		{"platform audience", plat, platDev, true},
	} {
		e.fillCeilingAud(t, c.u.email, c.platform)
		right := e.do(t, req{method: "POST", path: "/auth/login", ip: newIP(), platform: c.platform,
			body: map[string]any{"username": c.u.email, "password": testPassword, "device_token": c.dev}})
		wrong := e.do(t, req{method: "POST", path: "/auth/login", ip: newIP(), platform: c.platform,
			body: map[string]any{"username": c.u.email, "password": "wrong-password-" + uniq(), "device_token": c.dev}})
		mustLocked(t, c.name, right)
		if right.body["mfa_token"] != nil {
			t.Fatalf("%s: mfa_token at the ceiling", c.name)
		}
		sameReply(t, right, wrong)
		assertAudit(t, right, "auth.login.ceiling", c.u.email)
	}
}

// I1 (revised): at the ceiling, an account with two-step verification
// does not move on to the code step: its right password gets exactly the
// wrong password's reply.
func TestLoginCeilingMFADoesNotReachCode(t *testing.T) {
	e := newEnv(t)
	u := seedUser(t, seedHospital(t, hospitalOpts{}), true, "clinician")
	e.fillCeiling(t, u.email)

	right := e.login(t, u.email, testPassword, newIP())
	wrong := e.login(t, u.email, "wrong-password-"+uniq(), newIP())
	mustLocked(t, "right password", right)
	if right.body["mfa_token"] != nil {
		t.Fatalf("mfa_token at the ceiling: %s", right.raw)
	}
	sameReply(t, right, wrong)
	assertAudit(t, right, "auth.login.ceiling", u.email)
}

// I1 (revised): at the ceiling, a right password without a device gets the
// same reply as a wrong one and as an unknown login, its (login, IP) slot
// stays counted, and each request writes one auth.login.ceiling row with no
// email.
func TestLoginCeilingRightPasswordSameAsWrong(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	e.fillCeiling(t, u.email)

	ip := newIP()
	right := e.login(t, u.email, testPassword, ip)
	mustLocked(t, "right password, no device", right)
	assertAudit(t, right, "auth.login.ceiling", u.email)
	k, err := e.svc.LockoutKey(u.email, ip)
	if got := counter(t, "ip", k, err); got != "1" {
		t.Fatalf("(login, IP) counter after a right password at the ceiling: %q, want 1", got)
	}
	ip2 := newIP()
	wrong := e.login(t, u.email, "wrong-password-"+uniq(), ip2)
	assertAudit(t, wrong, "auth.login.ceiling", u.email)
	k2, err := e.svc.LockoutKey(u.email, ip2)
	if got := counter(t, "ip", k2, err); got != "1" {
		t.Fatalf("(login, IP) counter after a wrong password at the ceiling: %q, want 1", got)
	}
	sameReply(t, right, wrong)

	ghost := "synthetic.nobody." + uniq() + "@havana.example"
	e.fillCeiling(t, ghost)
	g := e.login(t, ghost, testPassword, newIP())
	sameReply(t, right, g)
	assertAudit(t, g, "auth.login.ceiling", "")
}

// I1 (revised): at the ceiling, right and wrong passwords, with or without
// a (bogus) device token, and an unknown login, all take the same time.
func TestLoginCeilingTimingSame(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	e := newEnv(t)
	u := seedUser(t, seedHospital(t, hospitalOpts{}), true, "clinician")
	ghost := "synthetic.nobody." + uniq() + "@havana.example"
	e.fillCeiling(t, u.email)
	e.fillCeiling(t, ghost)
	login := func(email, pw, dev string) {
		body := map[string]any{"username": email, "password": pw}
		if dev != "" {
			body["device_token"] = dev
		}
		r := e.do(t, req{method: "POST", path: "/auth/login", ip: newIP(), body: body})
		mustLocked(t, "timing", r)
	}
	bogus := "bogus-device-token-" + uniq()
	cases := map[string]func(){
		"right":             func() { login(u.email, testPassword, "") },
		"wrong":             func() { login(u.email, "wrong-password-"+uniq(), "") },
		"unknown":           func() { login(ghost, "wrong-password-"+uniq(), "") },
		"right, bad device": func() { login(u.email, testPassword, bogus) },
		"wrong, bad device": func() { login(u.email, "wrong-password-"+uniq(), bogus) },
	}
	// Round-robin, so load from other packages hits every case alike.
	samples := map[string][]time.Duration{}
	for i := 0; i < 9; i++ {
		for name, f := range cases {
			start := time.Now()
			f()
			samples[name] = append(samples[name], time.Since(start))
		}
	}
	med := map[string]time.Duration{}
	for name, ds := range samples {
		med[name] = median(ds)
	}
	within := func(a, b string) {
		x, y := med[a], med[b]
		if x > y*13/10 || y > x*13/10 {
			t.Errorf("median %s %v vs %s %v", a, x, b, y)
		}
	}
	within("right", "wrong")
	within("right", "unknown")
	within("right, bad device", "wrong, bad device")
}
