//go:build integration

package auth_test

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/logx"
)

// Moving two-step to a new authenticator app (POST /auth/mfa/replace and
// /auth/mfa/replace/confirm).

// auditPayload returns the payload of the request's audit entry for action
// (failing when there is none).
func auditPayload(t *testing.T, r resp, action string) map[string]any {
	t.Helper()
	var raw []byte
	if err := super.QueryRow(context.Background(), `SELECT payload FROM audit_log WHERE request_id = $1 AND action = $2 ORDER BY id DESC LIMIT 1`,
		r.hdr.Get("X-Request-ID"), action).Scan(&raw); err != nil {
		t.Fatalf("audit %s for the request: %v (have %v)", action, err, actions(t, r))
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// pendingOf reports whether u has a pending secret, and the stored
// (sealed) secret_enc and pending_secret_enc.
func pendingOf(t *testing.T, id uuid.UUID) (current, pending []byte) {
	t.Helper()
	if err := super.QueryRow(context.Background(), `SELECT secret_enc, pending_secret_enc FROM user_mfa WHERE user_id = $1`, id).
		Scan(&current, &pending); err != nil {
		t.Fatal(err)
	}
	return current, pending
}

func (e *env) replaceStart(t *testing.T, at string, body any) resp {
	t.Helper()
	return e.do(t, req{method: "POST", path: "/auth/mfa/replace", bearer: at, body: body})
}

func (e *env) replaceConfirm(t *testing.T, at, code string) resp {
	t.Helper()
	return e.do(t, req{method: "POST", path: "/auth/mfa/replace/confirm", bearer: at, body: map[string]any{"code": code}})
}

// startReplace starts a move with a fresh code from en and returns the new
// app's secret.
func (e *env) startReplace(t *testing.T, at string, en enrolment) []byte {
	t.Helper()
	r := e.replaceStart(t, at, map[string]any{"mfa_code": totpAt(en.secret, e.clock.Now())})
	if r.code != 200 {
		t.Fatalf("replace: %d %s", r.code, r.raw)
	}
	e.clock.Add(30 * time.Second)
	return secretFromURI(t, r.body["otpauth_uri"].(string))
}

func TestReplaceAuthenticator(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	hid := seedHospital(t, hospitalOpts{requireMFAClinicians: true, allowTrust: true})
	u := seedUser(t, hid, false, "clinician")
	en := e.enrol(t, u)
	// A trusted device, standing in for the old app.
	tr := e.challenge(t, e.mfaToken(t, u), totpAt(en.secret, e.clock.Now()), true)
	dev, _ := tr.body["device_token"].(string)
	if tr.code != 200 || dev == "" {
		t.Fatalf("trust: %d %s", tr.code, tr.raw)
	}
	e.clock.Add(30 * time.Second)
	at := e.fullSession(t, u, en).body["access_token"].(string)
	before, _ := pendingOf(t, u.id)

	r := e.replaceStart(t, at, map[string]any{"mfa_code": totpAt(en.secret, e.clock.Now())})
	if r.code != 200 {
		t.Fatalf("replace: %d %s", r.code, r.raw)
	}
	if cc := r.hdr.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q", cc)
	}
	if strings.Join(keys(r.body), ",") != "otpauth_uri" {
		t.Fatalf("keys %v", keys(r.body))
	}
	uri := r.body["otpauth_uri"].(string)
	if !strings.HasPrefix(uri, "otpauth://totp/MHAI:") || !strings.Contains(uri, "issuer=MHAI") {
		t.Fatalf("otpauth uri %q", uri)
	}
	fresh := secretFromURI(t, uri)
	if string(fresh) == string(en.secret) {
		t.Fatal("the new secret is the old one")
	}
	if p := auditPayload(t, r, "auth.mfa.replace_started"); p["method"] != "otp" || len(p) != 1 {
		t.Fatalf("replace_started payload %v", p)
	}
	// Stored sealed, beside the current secret, which is unchanged.
	current, pending := pendingOf(t, u.id)
	b32 := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(fresh)
	if len(pending) != 12+len(fresh)+16 || strings.Contains(string(pending), string(fresh)) || strings.Contains(string(pending), b32) {
		t.Fatalf("pending secret is %d bytes or in plain", len(pending))
	}
	if string(current) != string(before) {
		t.Fatal("the current secret changed before confirmation")
	}
	e.clock.Add(30 * time.Second)

	// Until confirmed, the old app still signs in, and the new one doesn't.
	if c := e.challenge(t, e.mfaToken(t, u), totpAt(en.secret, e.clock.Now()), false); c.code != 200 {
		t.Fatalf("old app before confirm: %d %s", c.code, c.raw)
	}
	e.clock.Add(30 * time.Second)
	mustCode(t, "new app before confirm", e.challenge(t, e.mfaToken(t, u), totpAt(fresh, e.clock.Now()), false), 403, "mfa_invalid")
	e.clock.Add(30 * time.Second)
	if d := e.loginDevice(t, u.email, dev); d.code != 200 || d.body["access_token"] == nil {
		t.Fatalf("trusted device before confirm: %d %s", d.code, d.raw)
	}

	newCode := totpAt(fresh, e.clock.Now())
	c := e.replaceConfirm(t, at, newCode)
	if c.code != 204 {
		t.Fatalf("confirm: %d %s", c.code, c.raw)
	}
	if p := auditPayload(t, c, "auth.mfa.replaced"); p["trusted_devices_revoked"] != float64(1) || len(p) != 1 {
		t.Fatalf("replaced payload %v", p)
	}
	current, pending = pendingOf(t, u.id)
	if pending != nil || string(current) == string(before) {
		t.Fatalf("after confirm: pending %d bytes, secret changed %v", len(pending), string(current) != string(before))
	}
	var pendingAt *time.Time
	var devices int
	_ = super.QueryRow(ctx, `SELECT pending_created_at, (SELECT count(*) FROM trusted_devices WHERE user_id = $1)
		FROM user_mfa WHERE user_id = $1`, u.id).Scan(&pendingAt, &devices)
	if pendingAt != nil || devices != 0 {
		t.Fatalf("pending_created_at %v, %d trusted devices", pendingAt, devices)
	}

	// The confirming code is spent: no replay at sign-in or at confirm.
	mustCode(t, "confirming code at sign-in", e.challenge(t, e.mfaToken(t, u), newCode, false), 403, "mfa_invalid")
	mustCode(t, "confirming code again", e.replaceConfirm(t, at, newCode), 409, "conflict")
	e.clock.Add(30 * time.Second)

	// The old app is refused; the new one signs in.
	mustCode(t, "old app after confirm", e.challenge(t, e.mfaToken(t, u), totpAt(en.secret, e.clock.Now()), false), 403, "mfa_invalid")
	if s := e.challenge(t, e.mfaToken(t, u), totpAt(fresh, e.clock.Now()), false); s.code != 200 {
		t.Fatalf("new app: %d %s", s.code, s.raw)
	}
	// The trusted device is gone.
	if d := e.loginDevice(t, u.email, dev); d.code != 200 || d.body["mfa_required"] != true || d.body["access_token"] != nil {
		t.Fatalf("trusted device after confirm: %d %s", d.code, d.raw)
	}
	// The backup codes are unchanged.
	if s := e.challenge(t, e.mfaToken(t, u), en.codes[0], false); s.code != 200 {
		t.Fatalf("backup code after confirm: %d %s", s.code, s.raw)
	}
	// The caller's own session carries on.
	if pr := e.do(t, req{method: "GET", path: "/probe", bearer: at}); pr.code != 200 {
		t.Fatalf("own session after confirm: %d", pr.code)
	}
}

func TestReplaceStartRefusals(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	at := e.fullSession(t, u, en).body["access_token"].(string)

	r := e.replaceStart(t, at, map[string]any{})
	mustCode(t, "no step-up", r, 403, "mfa_invalid")
	if p := auditPayload(t, r, "auth.mfa.failed"); p["method"] != "step_up" {
		t.Fatalf("failed payload %v", p)
	}
	r = e.replaceStart(t, at, map[string]any{"mfa_code": wrongCode(en.secret, e.clock.Now())})
	mustCode(t, "wrong step-up", r, 403, "mfa_invalid")
	hasAction(t, r, "auth.mfa.failed")
	mustCode(t, "wrong backup code", e.replaceStart(t, at, map[string]any{"backup_code": "zzzzz-zzzzz"}), 403, "mfa_invalid")
	mustCode(t, "both codes", e.replaceStart(t, at, map[string]any{"mfa_code": totpAt(en.secret, e.clock.Now()), "backup_code": en.codes[0]}),
		403, "mfa_invalid")
	mustCode(t, "unknown field", e.replaceStart(t, at, `{"mfa_code":"123456","x":1}`), 422, "validation_failed")
	if _, pending := pendingOf(t, u.id); pending != nil {
		t.Fatal("a refused start stored a pending secret")
	}

	// A session limited to enrolment reaches neither route.
	lim := e.limitedSession(t, admin(t)).body["access_token"].(string)
	mustCode(t, "limited start", e.replaceStart(t, lim, map[string]any{"mfa_code": "123456"}), 403, "mfa_required")
	mustCode(t, "limited confirm", e.replaceConfirm(t, lim, "123456"), 403, "mfa_required")

	// An account with two-step off has nothing to move.
	plain, l := loggedIn(t, e)
	pat := l.body["access_token"].(string)
	mustCode(t, "two-step off, start", e.replaceStart(t, pat, map[string]any{"mfa_code": "123456"}), 409, "conflict")
	mustCode(t, "two-step off, confirm", e.replaceConfirm(t, pat, "123456"), 409, "conflict")
	var rows int
	_ = super.QueryRow(context.Background(), `SELECT count(*) FROM user_mfa WHERE user_id = $1`, plain.id).Scan(&rows)
	if rows != 0 {
		t.Fatal("start created a user_mfa row for an account without two-step")
	}

	// Five wrong step-ups lock the account, start included.
	e2 := newEnv(t)
	v := admin(t)
	enV := e2.enrol(t, v)
	vat := e2.fullSession(t, v, enV).body["access_token"].(string)
	for i := 0; i < 5; i++ {
		mustCode(t, fmt.Sprintf("wrong step-up %d", i+1), e2.replaceStart(t, vat, map[string]any{"mfa_code": wrongCode(enV.secret, e2.clock.Now())}),
			403, "mfa_invalid")
	}
	r = e2.replaceStart(t, vat, map[string]any{"mfa_code": totpAt(enV.secret, e2.clock.Now())})
	mustCode(t, "locked step-up", r, 429, "mfa_locked")
	hasAction(t, r, "auth.mfa.locked")
}

func TestReplaceStartWithBackupCode(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	at := e.fullSession(t, u, en).body["access_token"].(string)
	r := e.replaceStart(t, at, map[string]any{"backup_code": en.codes[0]})
	if r.code != 200 {
		t.Fatalf("replace with a backup code: %d %s", r.code, r.raw)
	}
	if p := auditPayload(t, r, "auth.mfa.replace_started"); p["method"] != "backup" {
		t.Fatalf("replace_started payload %v", p)
	}
	// The step-up ran in the replace's own transaction, which committed
	// before the 2xx: the reply carries the renewed backup code.
	renewed := r.hdr.Get(httpx.HeaderNewBackupCode)
	if len(renewed) != 11 || renewed[5] != '-' || renewed == en.codes[0] || r.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("header %q, Cache-Control %q", renewed, r.hdr.Get("Cache-Control"))
	}
	if strings.Contains(r.raw, renewed) {
		t.Fatal("the renewed code is in the body")
	}
	if s := storedBackup(t, u.id); len(s) != 1 || s[0] != backupHash(t, renewed) {
		t.Fatal("the stored code is not the header's")
	}
	// The backup code is spent, and the refusal brings no code.
	again := e.replaceStart(t, at, map[string]any{"backup_code": en.codes[0]})
	mustCode(t, "backup code again", again, 403, "mfa_invalid")
	if again.hdr.Get(httpx.HeaderNewBackupCode) != "" {
		t.Fatal("a refused code got a new one")
	}
	// The renewed code works as the next step-up, and renews again.
	r = e.replaceStart(t, at, map[string]any{"backup_code": renewed})
	if r.code != 200 {
		t.Fatalf("replace with the renewed code: %d %s", r.code, r.raw)
	}
	if h := r.hdr.Get(httpx.HeaderNewBackupCode); h == "" || h == renewed {
		t.Fatalf("renewed again: %q", h)
	}
	// A TOTP step-up never brings a code.
	if r := e.replaceStart(t, at, map[string]any{"mfa_code": totpAt(en.secret, e.clock.Now())}); r.code != 200 ||
		r.hdr.Get(httpx.HeaderNewBackupCode) != "" {
		t.Fatalf("TOTP replace: %d %v", r.code, r.hdr)
	}
}

func TestReplaceConfirmRefusals(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	full := e.fullSession(t, u, en)
	at, rt := full.body["access_token"].(string), full.body["refresh_token"].(string)
	// wait moves the clock on by d, refreshing the session on the way (the
	// access token lives for less than the pending secret).
	wait := func(d time.Duration) {
		t.Helper()
		e.clock.Add(d)
		rf := e.refresh(t, rt)
		if rf.code != 200 {
			t.Fatalf("refresh: %d %s", rf.code, rf.raw)
		}
		at, rt = rf.body["access_token"].(string), rf.body["refresh_token"].(string)
	}

	mustCode(t, "nothing pending", e.replaceConfirm(t, at, totpAt(en.secret, e.clock.Now())), 409, "conflict")
	mustCode(t, "empty code", e.replaceConfirm(t, at, ""), 422, "validation_failed")
	mustCode(t, "unknown field", e.do(t, req{method: "POST", path: "/auth/mfa/replace/confirm", bearer: at, body: `{"code":"123456","x":1}`}),
		422, "validation_failed")

	// Expired after 15 minutes: refused, and the pending secret cleared.
	fresh := e.startReplace(t, at, en)
	wait(10 * time.Minute)
	wait(5*time.Minute + time.Second)
	mustCode(t, "expired", e.replaceConfirm(t, at, totpAt(fresh, e.clock.Now())), 409, "conflict")
	if _, pending := pendingOf(t, u.id); pending != nil {
		t.Fatal("an expired pending secret was kept")
	}
	// Just inside 15 minutes is fine.
	fresh = e.startReplace(t, at, en)
	wait(10 * time.Minute)
	wait(4 * time.Minute)
	if c := e.replaceConfirm(t, at, totpAt(fresh, e.clock.Now())); c.code != 204 {
		t.Fatalf("confirm at 14 minutes: %d %s", c.code, c.raw)
	}
}

func TestReplaceConfirmCountsTowardLockout(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	at := e.fullSession(t, u, en).body["access_token"].(string)
	fresh := e.startReplace(t, at, en)
	// The old app's code isn't the new app's.
	r := e.replaceConfirm(t, at, totpAt(en.secret, e.clock.Now()))
	mustCode(t, "old app's code", r, 403, "mfa_invalid")
	if p := auditPayload(t, r, "auth.mfa.failed"); p["method"] != "replace" {
		t.Fatalf("failed payload %v", p)
	}
	for i := 0; i < 4; i++ {
		mustCode(t, fmt.Sprintf("wrong code %d", i+2), e.replaceConfirm(t, at, wrongCode(fresh, e.clock.Now())), 403, "mfa_invalid")
	}
	r = e.replaceConfirm(t, at, totpAt(fresh, e.clock.Now()))
	mustCode(t, "after five wrong codes", r, 429, "mfa_locked")
	hasAction(t, r, "auth.mfa.locked")
	// The same per-account lock as sign-in.
	mustCode(t, "sign-in while locked", e.challenge(t, e.mfaToken(t, u), totpAt(en.secret, e.clock.Now()), false), 429, "mfa_locked")
	current, pending := pendingOf(t, u.id)
	if pending == nil || len(current) == 0 {
		t.Fatal("a refused confirm changed the secrets")
	}
}

func TestReplaceRestartReplacesPending(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	at := e.fullSession(t, u, en).body["access_token"].(string)
	first := e.startReplace(t, at, en)
	second := e.startReplace(t, at, en)
	if string(first) == string(second) {
		t.Fatal("a second start reused the pending secret")
	}
	mustCode(t, "first pending", e.replaceConfirm(t, at, totpAt(first, e.clock.Now())), 403, "mfa_invalid")
	if c := e.replaceConfirm(t, at, totpAt(second, e.clock.Now())); c.code != 204 {
		t.Fatalf("second pending: %d %s", c.code, c.raw)
	}
}

func TestReplacePendingIsPerUser(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t, hospitalOpts{})
	a := seedUser(t, hid, false, "hospital_admin")
	b := seedUser(t, hid, false, "hospital_admin")
	enA, enB := e.enrol(t, a), e.enrol(t, b)
	atA := e.fullSession(t, a, enA).body["access_token"].(string)
	atB := e.fullSession(t, b, enB).body["access_token"].(string)

	freshA := e.startReplace(t, atA, enA)
	// B has nothing pending: A's new code does nothing for B.
	mustCode(t, "B confirms A's code", e.replaceConfirm(t, atB, totpAt(freshA, e.clock.Now())), 409, "conflict")
	if _, pending := pendingOf(t, b.id); pending != nil {
		t.Fatal("B has a pending secret")
	}
	// Nor once B has its own pending secret.
	e.startReplace(t, atB, enB)
	mustCode(t, "B confirms A's code against B's pending", e.replaceConfirm(t, atB, totpAt(freshA, e.clock.Now())), 403, "mfa_invalid")
	// A's move is untouched.
	if c := e.replaceConfirm(t, atA, totpAt(freshA, e.clock.Now())); c.code != 204 {
		t.Fatalf("A confirms: %d %s", c.code, c.raw)
	}
	currentB, pendingB := pendingOf(t, b.id)
	if pendingB == nil || len(currentB) == 0 {
		t.Fatal("A's confirm touched B's row")
	}
}

func TestReplaceConfirmConcurrent(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	at := e.fullSession(t, u, en).body["access_token"].(string)
	fresh := e.startReplace(t, at, en)
	code := totpAt(fresh, e.clock.Now())
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := e.replaceConfirm(t, at, code); r.code == 204 {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("one code confirmed %d times", ok.Load())
	}
}

// An Idempotency-Key retry of a start runs again (the reply carries a
// secret, so it is never stored): the spent step-up code is refused, and
// the earlier secret is never sent again.
func TestReplaceStartIdempotencyKey(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	at := e.fullSession(t, u, en).body["access_token"].(string)
	body := map[string]any{"mfa_code": totpAt(en.secret, e.clock.Now())}
	key := "replace-" + uniq()
	first := e.do(t, req{method: "POST", path: "/auth/mfa/replace", bearer: at, body: body, idem: key})
	if first.code != 200 {
		t.Fatalf("first: %d %s", first.code, first.raw)
	}
	again := e.do(t, req{method: "POST", path: "/auth/mfa/replace", bearer: at, body: body, idem: key})
	if again.code == 200 || again.hdr.Get("Idempotent-Replayed") != "" || strings.Contains(again.raw, "otpauth") {
		t.Fatalf("retry: %d %s", again.code, again.raw)
	}
}

// R4: no code or secret in the logs, failing or not.
func TestReplaceNoCredentialsInLogs(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	at := e.fullSession(t, u, en).body["access_token"].(string)
	bad := wrongCode(en.secret, e.clock.Now())
	e.logs = &syncBuffer{}
	e.svc.Log = logx.New(e.logs)
	e.replaceStart(t, at, map[string]any{"mfa_code": bad})
	good := totpAt(en.secret, e.clock.Now())
	r := e.replaceStart(t, at, map[string]any{"mfa_code": good})
	if r.code != 200 {
		t.Fatalf("replace: %d %s", r.code, r.raw)
	}
	fresh := secretFromURI(t, r.body["otpauth_uri"].(string))
	wrongNew := wrongCode(fresh, e.clock.Now())
	e.replaceConfirm(t, at, wrongNew)
	out := e.logs.String()
	b32 := base32.StdEncoding.WithPadding(base32.NoPadding)
	for _, secret := range []string{u.email, bad, good, wrongNew, b32.EncodeToString(fresh), b32.EncodeToString(en.secret), at} {
		if strings.Contains(out, secret) {
			t.Fatalf("log contains a code, secret or email:\n%s", out)
		}
	}
}
