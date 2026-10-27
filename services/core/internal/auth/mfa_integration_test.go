//go:build integration

package auth_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// --- helpers -----------------------------------------------------------------

// totpAt is an independent RFC 6238 implementation (SHA-1, 30 s, 6 digits).
func totpAt(secret []byte, at time.Time) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(at.Unix()/30))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1000000)
}

// wrongCode is a six-digit code valid for none of the accepted steps.
func wrongCode(secret []byte, at time.Time) string {
	valid := map[string]bool{}
	for d := -2; d <= 2; d++ {
		valid[totpAt(secret, at.Add(time.Duration(d)*30*time.Second))] = true
	}
	for i := 0; ; i++ {
		c := fmt.Sprintf("%06d", (i*7919)%1000000)
		if !valid[c] {
			return c
		}
	}
}

func secretFromURI(t *testing.T, uri string) []byte {
	t.Helper()
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "otpauth" || u.Host != "totp" {
		t.Fatalf("otpauth uri %q", uri)
	}
	q := u.Query()
	if q.Get("issuer") == "" || q.Get("digits") != "6" || q.Get("period") != "30" || q.Get("algorithm") != "SHA1" {
		t.Fatalf("otpauth parameters %v", q)
	}
	s, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(q.Get("secret"))
	if err != nil || len(s) < 20 {
		t.Fatalf("secret: %v (%d bytes)", err, len(s))
	}
	return s
}

func (e *env) challenge(t *testing.T, mfaToken, code string, trust bool) resp {
	t.Helper()
	body := map[string]any{"mfa_token": mfaToken, "code": code}
	if trust {
		body["trust_device"] = true
	}
	return e.do(t, req{method: "POST", path: "/auth/mfa/challenge", body: body, ip: newIP()})
}

// mfaToken signs in and expects the two-step branch.
func (e *env) mfaToken(t *testing.T, u user) string {
	t.Helper()
	r := e.login(t, u.email, testPassword, newIP())
	tok, _ := r.body["mfa_token"].(string)
	if r.code != 200 || r.body["mfa_required"] != true || tok == "" {
		t.Fatalf("want the two-step branch: %d %s", r.code, r.raw)
	}
	return tok
}

// limitedSession signs in an account that must use two-step but hasn't set
// it up, and returns the challenge reply (a session limited to enrolment).
func (e *env) limitedSession(t *testing.T, u user) resp {
	t.Helper()
	r := e.challenge(t, e.mfaToken(t, u), "", false)
	if r.code != 200 || r.body["access_token"] == nil {
		t.Fatalf("unenrolled challenge: %d %s", r.code, r.raw)
	}
	return r
}

type enrolment struct {
	secret []byte
	codes  []string
	uri    string
}

// enrol takes u (who must use two-step, not yet set up) through first
// sign-in, enrolment and verification, then moves the clock to a fresh step.
func (e *env) enrol(t *testing.T, u user) enrolment {
	t.Helper()
	l := e.limitedSession(t, u)
	at := l.body["access_token"].(string)
	en := e.do(t, req{method: "POST", path: "/auth/mfa/enroll", bearer: at})
	if en.code != 200 {
		t.Fatalf("enroll: %d %s", en.code, en.raw)
	}
	if cc := en.hdr.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("enroll Cache-Control %q", cc)
	}
	if strings.Join(keys(en.body), ",") != "otpauth_uri,recovery_codes" {
		t.Fatalf("enroll keys %v", keys(en.body))
	}
	uri := en.body["otpauth_uri"].(string)
	var codes []string
	for _, c := range en.body["recovery_codes"].([]any) {
		codes = append(codes, c.(string))
	}
	if len(codes) != 10 {
		t.Fatalf("%d recovery codes", len(codes))
	}
	secret := secretFromURI(t, uri)
	v := e.do(t, req{method: "POST", path: "/auth/mfa/verify", bearer: at, body: map[string]any{"code": totpAt(secret, e.clock.Now())}})
	if v.code != 204 {
		t.Fatalf("verify: %d %s", v.code, v.raw)
	}
	e.clock.Add(30 * time.Second)
	return enrolment{secret: secret, codes: codes, uri: uri}
}

// fullSession signs u in with a code and returns the challenge reply.
func (e *env) fullSession(t *testing.T, u user, en enrolment) resp {
	t.Helper()
	r := e.challenge(t, e.mfaToken(t, u), totpAt(en.secret, e.clock.Now()), false)
	if r.code != 200 {
		t.Fatalf("challenge: %d %s", r.code, r.raw)
	}
	e.clock.Add(30 * time.Second)
	return r
}

func admin(t *testing.T) user {
	return seedUser(t, seedHospital(t, hospitalOpts{}), false, "hospital_admin")
}

// actions lists the audit actions a request wrote, and fails on a payload
// with an email in it.
func actions(t *testing.T, r resp) []string {
	t.Helper()
	rows, err := super.Query(context.Background(), `SELECT action, payload::text FROM audit_log WHERE request_id = $1 ORDER BY id`,
		r.hdr.Get("X-Request-ID"))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a, p string
		if err := rows.Scan(&a, &p); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(p, "@") {
			t.Fatalf("audit payload has an email: %s", p)
		}
		out = append(out, a)
	}
	return out
}

func hasAction(t *testing.T, r resp, action string) {
	t.Helper()
	got := actions(t, r)
	for _, a := range got {
		if a == action {
			return
		}
	}
	t.Fatalf("audit for request %v, want %s", got, action)
}

func mustCode(t *testing.T, name string, r resp, status int, code string) {
	t.Helper()
	if r.code != status || r.body["code"] != code {
		t.Fatalf("%s: want %d %s, got %d %s", name, status, code, r.code, r.raw)
	}
}

// --- challenge ---------------------------------------------------------------

func TestChallengeValidCode(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)

	tok := e.mfaToken(t, u)
	r := e.challenge(t, tok, totpAt(en.secret, e.clock.Now()), false)
	if r.code != 200 {
		t.Fatalf("challenge: %d %s", r.code, r.raw)
	}
	if cc := r.hdr.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q", cc)
	}
	if strings.Join(keys(r.body), ",") != "access_token,expires_in,refresh_token,user" {
		t.Fatalf("keys %v", keys(r.body))
	}
	c, err := token.Verify(r.body["access_token"].(string), issuer.PublicKeys())
	if err != nil || !c.MFA || strings.Join(c.AMR, ",") != "pwd,otp" || c.Sub != u.id.String() {
		t.Fatalf("claims %+v (%v)", c, err)
	}
	usr := r.body["user"].(map[string]any)
	if usr["mfa_enabled"] != true || usr["recovery_codes_left"].(float64) != 10 {
		t.Fatalf("user %v", usr)
	}
	hasAction(t, r, "auth.mfa.verified")
	hasAction(t, r, "auth.login.succeeded")

	// The session works and survives a refresh (amr shows the code).
	if pr := e.do(t, req{method: "GET", path: "/probe", bearer: r.body["access_token"].(string)}); pr.code != 200 {
		t.Fatalf("probe %d %s", pr.code, pr.raw)
	}
	if rf := e.refresh(t, r.body["refresh_token"].(string)); rf.code != 200 {
		t.Fatalf("refresh %d %s", rf.code, rf.raw)
	}
	// The token is single use.
	mustCode(t, "token reused", e.challenge(t, tok, totpAt(en.secret, e.clock.Now()), false), 403, "mfa_invalid")
	// An enrolled account can't skip the code.
	mustCode(t, "empty code", e.challenge(t, e.mfaToken(t, u), "", false), 403, "mfa_invalid")
	mustCode(t, "unknown token", e.challenge(t, token.NewOpaque(), "123456", false), 403, "mfa_invalid")
	if v := e.do(t, req{method: "POST", path: "/auth/mfa/challenge", body: `{"mfa_token":"x","code":"1","extra":1}`}); v.code != 422 {
		t.Fatalf("unknown field: %d", v.code)
	}
}

func TestCodeReuseSameStepRejected(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	code := totpAt(en.secret, e.clock.Now())
	if r := e.challenge(t, e.mfaToken(t, u), code, false); r.code != 200 {
		t.Fatalf("first use: %d %s", r.code, r.raw)
	}
	mustCode(t, "same code again", e.challenge(t, e.mfaToken(t, u), code, false), 403, "mfa_invalid")
	// Nor may a code from an earlier step follow a later one.
	mustCode(t, "earlier step", e.challenge(t, e.mfaToken(t, u), totpAt(en.secret, e.clock.Now().Add(-30*time.Second)), false),
		403, "mfa_invalid")
}

func TestCodeReuseConcurrent(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	code := totpAt(en.secret, e.clock.Now())
	toks := []string{e.mfaToken(t, u), e.mfaToken(t, u), e.mfaToken(t, u), e.mfaToken(t, u)}
	var ok atomic.Int32
	var wg sync.WaitGroup
	for _, tok := range toks {
		wg.Add(1)
		go func(tok string) {
			defer wg.Done()
			if r := e.challenge(t, tok, code, false); r.code == 200 {
				ok.Add(1)
			}
		}(tok)
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("one code signed in %d times", ok.Load())
	}
}

func TestStepBoundaryTolerance(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	// Clear of the step verify used, the step before now passes.
	e.clock.Add(30 * time.Second)
	if r := e.challenge(t, e.mfaToken(t, u), totpAt(en.secret, e.clock.Now().Add(-30*time.Second)), false); r.code != 200 {
		t.Fatalf("previous step: %d %s", r.code, r.raw)
	}
	e.clock.Add(90 * time.Second)
	// Two steps before fails (and is after the last used step, so only the
	// tolerance refuses it).
	mustCode(t, "two steps before", e.challenge(t, e.mfaToken(t, u), totpAt(en.secret, e.clock.Now().Add(-60*time.Second)), false),
		403, "mfa_invalid")
	mustCode(t, "two steps ahead", e.challenge(t, e.mfaToken(t, u), totpAt(en.secret, e.clock.Now().Add(60*time.Second)), false),
		403, "mfa_invalid")
	// The step after now passes.
	if r := e.challenge(t, e.mfaToken(t, u), totpAt(en.secret, e.clock.Now().Add(30*time.Second)), false); r.code != 200 {
		t.Fatalf("next step: %d %s", r.code, r.raw)
	}
}

func TestFiveWrongCodesLocks(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	tok := e.mfaToken(t, u)
	bad := wrongCode(en.secret, e.clock.Now())
	var last resp
	for i := 0; i < 5; i++ {
		last = e.challenge(t, tok, bad, false)
		mustCode(t, fmt.Sprintf("wrong code %d", i+1), last, 403, "mfa_invalid")
	}
	hasAction(t, last, "auth.mfa.failed")
	good := totpAt(en.secret, e.clock.Now())
	r := e.challenge(t, tok, good, false)
	mustCode(t, "after five wrong codes", r, 429, "mfa_locked")
	// The lock is per account, not per token: a fresh sign-in is locked too.
	r = e.challenge(t, e.mfaToken(t, u), good, false)
	mustCode(t, "fresh token", r, 429, "mfa_locked")
	hasAction(t, r, "auth.mfa.locked")
	// And so is step-up.
	if err := e.svc.StepUp(context.Background(), nil, u.id.String(), good, ""); !errors.Is(err, auth.ErrMFALocked) {
		t.Fatalf("step-up while locked: %v", err)
	}
}

func TestMfaTokenExpires(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	tok := e.mfaToken(t, u)
	e.clock.Add(5*time.Minute + time.Second)
	mustCode(t, "expired token", e.challenge(t, tok, totpAt(en.secret, e.clock.Now()), false), 403, "mfa_invalid")
}

// --- backup codes ------------------------------------------------------------

func TestBackupCodeOnce(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)

	var stored []string
	_ = super.QueryRow(context.Background(), `SELECT backup_code_hashes FROM user_mfa WHERE user_id = $1`, u.id).Scan(&stored)
	if len(stored) != 10 {
		t.Fatalf("%d stored hashes", len(stored))
	}
	for _, h := range stored {
		for _, c := range en.codes {
			if strings.Contains(h, c) || strings.Contains(h, strings.ReplaceAll(c, "-", "")) {
				t.Fatal("backup code stored in plain")
			}
		}
	}

	r := e.challenge(t, e.mfaToken(t, u), en.codes[0], false)
	if r.code != 200 {
		t.Fatalf("backup code: %d %s", r.code, r.raw)
	}
	c, _ := token.Verify(r.body["access_token"].(string), issuer.PublicKeys())
	if !c.MFA || strings.Join(c.AMR, ",") != "pwd,backup" {
		t.Fatalf("claims %+v", c)
	}
	if left := r.body["user"].(map[string]any)["recovery_codes_left"].(float64); left != 9 {
		t.Fatalf("recovery_codes_left %v", left)
	}
	hasAction(t, r, "auth.mfa.backup_used")
	if rf := e.refresh(t, r.body["refresh_token"].(string)); rf.code != 200 {
		t.Fatalf("refresh of a backup-code session: %d", rf.code)
	}
	mustCode(t, "backup code twice", e.challenge(t, e.mfaToken(t, u), en.codes[0], false), 403, "mfa_invalid")
	// Case and the dash don't matter.
	loose := strings.ToUpper(strings.ReplaceAll(en.codes[1], "-", ""))
	if r := e.challenge(t, e.mfaToken(t, u), loose, false); r.code != 200 {
		t.Fatalf("normalised backup code: %d %s", r.code, r.raw)
	}
}

func TestRegenerateInvalidatesOld(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	at := e.fullSession(t, u, en).body["access_token"].(string)

	mustCode(t, "no step-up", e.do(t, req{method: "POST", path: "/auth/mfa/recovery-codes", bearer: at, body: map[string]any{}}),
		403, "mfa_invalid")
	mustCode(t, "wrong step-up", e.do(t, req{method: "POST", path: "/auth/mfa/recovery-codes", bearer: at,
		body: map[string]any{"mfa_code": wrongCode(en.secret, e.clock.Now())}}), 403, "mfa_invalid")

	r := e.do(t, req{method: "POST", path: "/auth/mfa/recovery-codes", bearer: at,
		body: map[string]any{"mfa_code": totpAt(en.secret, e.clock.Now())}})
	if r.code != 200 {
		t.Fatalf("regenerate: %d %s", r.code, r.raw)
	}
	if cc := r.hdr.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q", cc)
	}
	if strings.Join(keys(r.body), ",") != "recovery_codes" {
		t.Fatalf("keys %v", keys(r.body))
	}
	var fresh []string
	for _, c := range r.body["recovery_codes"].([]any) {
		fresh = append(fresh, c.(string))
	}
	if len(fresh) != 10 {
		t.Fatalf("%d codes", len(fresh))
	}
	hasAction(t, r, "auth.mfa.codes_regenerated")
	e.clock.Add(30 * time.Second)

	mustCode(t, "old code", e.challenge(t, e.mfaToken(t, u), en.codes[2], false), 403, "mfa_invalid")
	if r := e.challenge(t, e.mfaToken(t, u), fresh[0], false); r.code != 200 {
		t.Fatalf("new code: %d %s", r.code, r.raw)
	}
	// A backup code also works as the step-up.
	r = e.do(t, req{method: "POST", path: "/auth/mfa/recovery-codes", bearer: at, body: map[string]any{"backup_code": fresh[1]}})
	if r.code != 200 {
		t.Fatalf("regenerate with a backup code: %d %s", r.code, r.raw)
	}
}

// --- trusted devices ---------------------------------------------------------

func TestTrustDeviceOnlyWhenHospitalAllows(t *testing.T) {
	e := newEnv(t)
	countDevices := func(id uuid.UUID) int {
		var n int
		_ = super.QueryRow(context.Background(), `SELECT count(*) FROM trusted_devices WHERE user_id = $1`, id).Scan(&n)
		return n
	}

	strict := seedHospital(t, hospitalOpts{requireMFAClinicians: true, allowTrust: false})
	a := seedUser(t, strict, false, "clinician")
	enA := e.enrol(t, a)
	r := e.challenge(t, e.mfaToken(t, a), totpAt(enA.secret, e.clock.Now()), true)
	if r.code != 200 || r.body["device_token"] != nil || countDevices(a.id) != 0 {
		t.Fatalf("trust off: %d %s (%d devices)", r.code, r.raw, countDevices(a.id))
	}

	trusting := seedHospital(t, hospitalOpts{requireMFAClinicians: true, allowTrust: true})
	b := seedUser(t, trusting, false, "clinician")
	enB := e.enrol(t, b)
	// Without trust_device, nothing is issued.
	r = e.challenge(t, e.mfaToken(t, b), totpAt(enB.secret, e.clock.Now()), false)
	if r.code != 200 || r.body["device_token"] != nil {
		t.Fatalf("not asked: %d %s", r.code, r.raw)
	}
	e.clock.Add(30 * time.Second)
	r = e.challenge(t, e.mfaToken(t, b), totpAt(enB.secret, e.clock.Now()), true)
	dev, _ := r.body["device_token"].(string)
	if r.code != 200 || len(dev) < 32 {
		t.Fatalf("trust on: %d %s", r.code, r.raw)
	}
	hasAction(t, r, "auth.device.trusted")
	var plain int
	var expires time.Time
	_ = super.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE token_hash = $2), max(expires_at)
		FROM trusted_devices WHERE user_id = $1`, b.id, dev).Scan(&plain, &expires)
	if countDevices(b.id) != 1 || plain != 0 {
		t.Fatalf("device rows %d, stored in plain %d", countDevices(b.id), plain)
	}
	if d := expires.Sub(e.clock.Now()); d < 30*24*time.Hour-time.Minute || d > 30*24*time.Hour+time.Minute {
		t.Fatalf("device lifetime %v", d)
	}

	// A hospital admin in the same hospital is never issued one.
	adm := seedUser(t, trusting, false, "clinician", "hospital_admin")
	enAdm := e.enrol(t, adm)
	r = e.challenge(t, e.mfaToken(t, adm), totpAt(enAdm.secret, e.clock.Now()), true)
	if r.code != 200 || r.body["device_token"] != nil || countDevices(adm.id) != 0 {
		t.Fatalf("admin: %d %s", r.code, r.raw)
	}
}

// trustedClinician enrols a clinician at a hospital that allows trusted
// devices and returns a device token issued by the challenge.
func (e *env) trustedClinician(t *testing.T, hid uuid.UUID) (user, string) {
	t.Helper()
	u := seedUser(t, hid, false, "clinician")
	en := e.enrol(t, u)
	r := e.challenge(t, e.mfaToken(t, u), totpAt(en.secret, e.clock.Now()), true)
	dev, _ := r.body["device_token"].(string)
	if r.code != 200 || dev == "" {
		t.Fatalf("trust: %d %s", r.code, r.raw)
	}
	e.clock.Add(30 * time.Second)
	return u, dev
}

func TestTrustedDeviceSkipsForClinician(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t, hospitalOpts{requireMFAClinicians: true, allowTrust: true})
	u, dev := e.trustedClinician(t, hid)
	r := e.loginDevice(t, u.email, dev)
	if r.code != 200 || r.body["access_token"] == nil {
		t.Fatalf("device sign-in: %d %s", r.code, r.raw)
	}
	c, _ := token.Verify(r.body["access_token"].(string), issuer.PublicKeys())
	if !c.MFA || strings.Join(c.AMR, ",") != "pwd,device" {
		t.Fatalf("claims %+v", c)
	}
	if pr := e.do(t, req{method: "GET", path: "/probe", bearer: r.body["access_token"].(string)}); pr.code != 200 {
		t.Fatalf("probe %d", pr.code)
	}
}

func TestTrustedDeviceNeverSkipsForAdmin(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t, hospitalOpts{requireMFAClinicians: true, allowTrust: true})
	u, dev := e.trustedClinician(t, hid)
	// The clinician becomes a hospital admin: the device no longer counts.
	if _, err := super.Exec(context.Background(), `INSERT INTO user_roles (user_id, role) VALUES ($1, 'hospital_admin')`, u.id); err != nil {
		t.Fatal(err)
	}
	r := e.loginDevice(t, u.email, dev)
	if r.code != 200 || r.body["mfa_required"] != true || r.body["access_token"] != nil {
		t.Fatalf("hospital_admin with a device: %d %s", r.code, r.raw)
	}
	// And a device row seeded for an admin directly doesn't count either.
	adm := seedUser(t, hid, true, "hospital_admin")
	r = e.loginDevice(t, adm.email, seedDevice(t, adm.id, time.Now().Add(time.Hour)))
	if r.body["mfa_required"] != true {
		t.Fatalf("seeded admin device: %d %s", r.code, r.raw)
	}
}

func TestDeviceTokenBoundToUser(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t, hospitalOpts{requireMFAClinicians: true, allowTrust: true})
	_, dev := e.trustedClinician(t, hid)
	other := seedUser(t, hid, false, "clinician")
	e.enrol(t, other)
	r := e.loginDevice(t, other.email, dev)
	if r.code != 200 || r.body["mfa_required"] != true || r.body["access_token"] != nil {
		t.Fatalf("another user's device token: %d %s", r.code, r.raw)
	}
}

// --- unenrolled accounts -----------------------------------------------------

func TestUnenrolledAdminLimitedRoutes(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	l := e.limitedSession(t, u)
	at := l.body["access_token"].(string)
	c, _ := token.Verify(at, issuer.PublicKeys())
	if c.MFA || strings.Join(c.AMR, ",") != "pwd" {
		t.Fatalf("limited claims %+v", c)
	}
	if usr := l.body["user"].(map[string]any); usr["mfa_enabled"] != false {
		t.Fatalf("user %v", usr)
	}
	for name, rq := range map[string]req{
		"probe":          {method: "GET", path: "/probe"},
		"sessions":       {method: "GET", path: "/auth/sessions"},
		"revoke-all":     {method: "POST", path: "/auth/sessions/revoke-all"},
		"terms":          {method: "POST", path: "/auth/terms", body: map[string]any{"version": "2026-10"}},
		"recovery-codes": {method: "POST", path: "/auth/mfa/recovery-codes", body: map[string]any{"mfa_code": "123456"}},
	} {
		rq.bearer = at
		mustCode(t, name, e.do(t, rq), 403, "mfa_required")
	}
	// Password change is reachable (wrong current password: refused, but
	// not for want of two-step).
	pw := e.do(t, req{method: "POST", path: "/auth/password", bearer: at,
		body: map[string]any{"current_password": "not-the-password-at-all", "new_password": "tangerine-orbit-77-quill"}})
	mustCode(t, "password", pw, 403, "forbidden")

	// Enrol and verify are reachable; after verify the session meets the
	// policy, and a refresh gives a full token.
	en := e.do(t, req{method: "POST", path: "/auth/mfa/enroll", bearer: at})
	if en.code != 200 {
		t.Fatalf("enroll %d %s", en.code, en.raw)
	}
	secret := secretFromURI(t, en.body["otpauth_uri"].(string))
	mustCode(t, "verify wrong", e.do(t, req{method: "POST", path: "/auth/mfa/verify", bearer: at,
		body: map[string]any{"code": wrongCode(secret, e.clock.Now())}}), 403, "mfa_invalid")
	var enabled bool
	_ = super.QueryRow(context.Background(), `SELECT enabled FROM user_mfa WHERE user_id = $1`, u.id).Scan(&enabled)
	if enabled {
		t.Fatal("enabled by a wrong code")
	}
	v := e.do(t, req{method: "POST", path: "/auth/mfa/verify", bearer: at, body: map[string]any{"code": totpAt(secret, e.clock.Now())}})
	if v.code != 204 {
		t.Fatalf("verify %d %s", v.code, v.raw)
	}
	hasAction(t, v, "auth.mfa.enrolled")
	rf := e.refresh(t, l.body["refresh_token"].(string))
	if rf.code != 200 {
		t.Fatalf("refresh after verify: %d %s", rf.code, rf.raw)
	}
	full := rf.body["access_token"].(string)
	c, _ = token.Verify(full, issuer.PublicKeys())
	if !c.MFA || strings.Join(c.AMR, ",") != "pwd,otp" {
		t.Fatalf("upgraded claims %+v", c)
	}
	if pr := e.do(t, req{method: "GET", path: "/probe", bearer: full}); pr.code != 200 {
		t.Fatalf("probe after verify: %d", pr.code)
	}
	// Enrolling again once enabled is refused.
	mustCode(t, "enroll again", e.do(t, req{method: "POST", path: "/auth/mfa/enroll", bearer: full}), 409, "conflict")
	// Logout is reachable from a limited session.
	l2 := e.limitedSession(t, admin(t))
	if lo := e.do(t, req{method: "POST", path: "/auth/logout", bearer: l2.body["access_token"].(string)}); lo.code != 204 {
		t.Fatalf("logout from a limited session: %d %s", lo.code, lo.raw)
	}
}

func TestSecretEncryptedAtRest(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	var raw []byte
	if err := super.QueryRow(context.Background(), `SELECT secret_enc FROM user_mfa WHERE user_id = $1`, u.id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	b32 := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(en.secret)
	if bytes.Contains(raw, en.secret) || strings.Contains(string(raw), b32) || strings.Contains(strings.ToUpper(string(raw)), b32) {
		t.Fatal("secret_enc contains the secret")
	}
	if len(raw) != 12+len(en.secret)+16 {
		t.Fatalf("secret_enc is %d bytes, want nonce + secret + tag", len(raw))
	}
}

// --- step-up -------------------------------------------------------------------

func TestStepUp(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	u := admin(t)
	en := e.enrol(t, u)
	if err := e.svc.StepUp(ctx, nil, u.id.String(), totpAt(en.secret, e.clock.Now()), ""); err != nil {
		t.Fatalf("good code: %v", err)
	}
	if err := e.svc.StepUp(ctx, nil, u.id.String(), totpAt(en.secret, e.clock.Now()), ""); !errors.Is(err, auth.ErrMFAInvalid) {
		t.Fatalf("reused code: %v", err)
	}
	if err := e.svc.StepUp(ctx, nil, u.id.String(), "", en.codes[0]); err != nil {
		t.Fatalf("backup code: %v", err)
	}
	if err := e.svc.StepUp(ctx, nil, u.id.String(), "", en.codes[0]); !errors.Is(err, auth.ErrMFAInvalid) {
		t.Fatalf("backup code twice: %v", err)
	}
	if err := e.svc.StepUp(ctx, nil, u.id.String(), "", ""); !errors.Is(err, auth.ErrMFAInvalid) {
		t.Fatalf("no code: %v", err)
	}
	if err := e.svc.StepUp(ctx, nil, u.id.String(), "123456", en.codes[1]); !errors.Is(err, auth.ErrMFAInvalid) {
		t.Fatalf("both codes: %v", err)
	}
	// Running inside a caller's mhai_auth transaction works too.
	e.clock.Add(30 * time.Second)
	if err := db.WithAuth(ctx, app, func(tx pgx.Tx) error {
		return e.svc.StepUp(ctx, tx, u.id.String(), totpAt(en.secret, e.clock.Now()), "")
	}); err != nil {
		t.Fatalf("in a transaction: %v", err)
	}
	// 5 wrong codes in 15 minutes lock it (the bad attempts above count).
	bad := wrongCode(en.secret, e.clock.Now())
	var err error
	for i := 0; i < 6; i++ {
		err = e.svc.StepUp(ctx, nil, u.id.String(), bad, "")
	}
	if !errors.Is(err, auth.ErrMFALocked) {
		t.Fatalf("after repeated wrong codes: %v", err)
	}
	// An account without two-step.
	plain := clinician(t)
	if err := e.svc.StepUp(ctx, nil, plain.id.String(), "123456", ""); !errors.Is(err, auth.ErrMFARequired) {
		t.Fatalf("no two-step: %v", err)
	}
}

// --- password change -----------------------------------------------------------

func TestPasswordChangeRequiresCurrent(t *testing.T) {
	e := newEnv(t)
	u, l := loggedIn(t, e)
	at := l.body["access_token"].(string)
	const newPW = "tangerine-orbit-77-quill"
	r := e.do(t, req{method: "POST", path: "/auth/password", bearer: at,
		body: map[string]any{"current_password": "not-the-password-at-all", "new_password": newPW}})
	mustCode(t, "wrong current", r, 403, "forbidden")
	mustCode(t, "missing current", e.do(t, req{method: "POST", path: "/auth/password", bearer: at,
		body: map[string]any{"new_password": newPW}}), 422, "validation_failed")
	if x := e.login(t, u.email, testPassword, newIP()); x.code != 200 {
		t.Fatalf("old password stopped working after a refused change: %d", x.code)
	}
	r = e.do(t, req{method: "POST", path: "/auth/password", bearer: at,
		body: map[string]any{"current_password": testPassword, "new_password": newPW}})
	if r.code != 204 {
		t.Fatalf("change: %d %s", r.code, r.raw)
	}
	hasAction(t, r, "auth.password.changed")
	if x := e.login(t, u.email, newPW, newIP()); x.code != 200 {
		t.Fatalf("new password: %d", x.code)
	}
	if x := e.login(t, u.email, testPassword, newIP()); x.code != 401 {
		t.Fatalf("old password: %d", x.code)
	}
	var h string
	_ = super.QueryRow(context.Background(), `SELECT password_hash FROM users WHERE id = $1`, u.id).Scan(&h)
	if ok, _ := password.Verify(newPW, h); !ok || strings.Contains(h, newPW) {
		t.Fatal("stored hash")
	}
}

func TestPasswordChangeWrongCurrentLocks(t *testing.T) {
	e := newEnv(t)
	_, l := loggedIn(t, e)
	at := l.body["access_token"].(string)
	var r resp
	for i := 0; i < 6; i++ {
		r = e.do(t, req{method: "POST", path: "/auth/password", bearer: at, ip: "198.51.100.77",
			body: map[string]any{"current_password": "not-the-password-at-all", "new_password": "tangerine-orbit-77-quill"}})
	}
	mustCode(t, "sixth wrong current password", r, 429, "login_locked")
}

func TestPasswordChangeSignsOutOthers(t *testing.T) {
	e := newEnv(t)
	u, a := loggedIn(t, e)
	b := e.login(t, u.email, testPassword, newIP())
	if b.code != 200 {
		t.Fatal(b.raw)
	}
	_, _ = super.Exec(context.Background(), `UPDATE users SET must_change_password = true WHERE id = $1`, u.id)
	r := e.do(t, req{method: "POST", path: "/auth/password", bearer: a.body["access_token"].(string),
		body: map[string]any{"current_password": testPassword, "new_password": "tangerine-orbit-77-quill"}})
	if r.code != 204 {
		t.Fatalf("change: %d %s", r.code, r.raw)
	}
	// The other session is gone, at once and at refresh.
	if pr := e.do(t, req{method: "GET", path: "/probe", bearer: b.body["access_token"].(string)}); pr.code != 401 {
		t.Fatalf("other session's access token: %d", pr.code)
	}
	if rf := e.refresh(t, b.body["refresh_token"].(string)); rf.code != 401 {
		t.Fatalf("other session's refresh: %d", rf.code)
	}
	cb, _ := token.Verify(b.body["access_token"].(string), issuer.PublicKeys())
	if !e.svc.Denylisted(context.Background(), cb) {
		t.Fatal("other session not denylisted")
	}
	// This session stays.
	if pr := e.do(t, req{method: "GET", path: "/probe", bearer: a.body["access_token"].(string)}); pr.code != 200 {
		t.Fatalf("current session: %d", pr.code)
	}
	if rf := e.refresh(t, a.body["refresh_token"].(string)); rf.code != 200 {
		t.Fatalf("current session refresh: %d", rf.code)
	}
	var must bool
	_ = super.QueryRow(context.Background(), `SELECT must_change_password FROM users WHERE id = $1`, u.id).Scan(&must)
	if must {
		t.Fatal("must_change_password not cleared")
	}
}

func TestPasswordChangeBreachedRejected(t *testing.T) {
	e := newEnv(t)
	u, l := loggedIn(t, e)
	at := l.body["access_token"].(string)
	local := strings.SplitN(u.email, "@", 2)[0]
	for _, pw := range []string{"password1234", "short-pw", local + "-extra-words", testPassword} {
		r := e.do(t, req{method: "POST", path: "/auth/password", bearer: at,
			body: map[string]any{"current_password": testPassword, "new_password": pw}})
		mustCode(t, "weak "+pw[:4], r, 422, "weak_password")
	}
}

// --- R4: no code, secret, password or email in the logs ---------------------------

func TestMFANoCredentialsInLogs(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	full := e.fullSession(t, u, en)
	at := full.body["access_token"].(string)
	tok := e.mfaToken(t, u)
	limited := e.limitedSession(t, admin(t)).body["access_token"].(string)
	bad := wrongCode(en.secret, e.clock.Now())
	good := totpAt(en.secret, e.clock.Now())
	b32 := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(en.secret)
	const badPW = "wrong-secret-password-9931"
	const weakPW = "password1234"

	steps := []struct {
		name string
		r    req
	}{
		{"challenge wrong code", req{method: "POST", path: "/auth/mfa/challenge", body: map[string]any{"mfa_token": tok, "code": bad}}},
		{"challenge wrong backup", req{method: "POST", path: "/auth/mfa/challenge", body: map[string]any{"mfa_token": tok, "code": "zzzzz-zzzzz"}}},
		{"challenge bad token", req{method: "POST", path: "/auth/mfa/challenge", body: map[string]any{"mfa_token": tok + "x", "code": good}}},
		{"challenge malformed", req{method: "POST", path: "/auth/mfa/challenge", body: `{"mfa_token":"` + tok + `","code":"` + good + `","x":1}`}},
		{"enroll when enabled", req{method: "POST", path: "/auth/mfa/enroll", bearer: at}},
		{"enroll bad token", req{method: "POST", path: "/auth/mfa/enroll", bearer: at + "x"}},
		{"verify wrong code", req{method: "POST", path: "/auth/mfa/verify", bearer: at, body: map[string]any{"code": bad}}},
		{"verify not enrolled", req{method: "POST", path: "/auth/mfa/verify", bearer: limited, body: map[string]any{"code": good}}},
		{"recovery wrong code", req{method: "POST", path: "/auth/mfa/recovery-codes", bearer: at, body: map[string]any{"mfa_code": bad}}},
		{"recovery wrong backup", req{method: "POST", path: "/auth/mfa/recovery-codes", bearer: at, body: map[string]any{"backup_code": en.codes[9] + "x"}}},
		{"recovery limited", req{method: "POST", path: "/auth/mfa/recovery-codes", bearer: limited, body: map[string]any{"mfa_code": good}}},
		{"password wrong current", req{method: "POST", path: "/auth/password", bearer: at, body: map[string]any{"current_password": badPW, "new_password": "tangerine-orbit-77-quill"}}},
		{"password weak", req{method: "POST", path: "/auth/password", bearer: at, body: map[string]any{"current_password": testPassword, "new_password": weakPW}}},
		{"password malformed", req{method: "POST", path: "/auth/password", bearer: at, body: `{"current_password":"` + badPW + `","new_password":"` + weakPW + `","x":1}`}},
	}
	for _, s := range steps {
		e.logs = &syncBuffer{}
		e.svc.Log = logx.New(e.logs)
		r := e.do(t, s.r)
		if r.code < 400 {
			t.Fatalf("%s: expected a failure, got %d %s", s.name, r.code, r.raw)
		}
		out := e.logs.String()
		for _, secret := range append([]string{u.email, strings.SplitN(u.email, "@", 2)[0], badPW, weakPW, testPassword,
			bad, good, b32, tok, at, at[len(at)-20:]}, en.codes...) {
			if strings.Contains(out, secret) {
				t.Fatalf("%s: log contains a code, secret, password or email:\n%s", s.name, out)
			}
		}
	}
	// A lockout run logs nothing sensitive either.
	e.logs = &syncBuffer{}
	e.svc.Log = logx.New(e.logs)
	tok = e.mfaToken(t, u)
	for i := 0; i < 7; i++ {
		e.challenge(t, tok, bad, false)
	}
	out := e.logs.String()
	for _, secret := range []string{u.email, bad, tok, b32} {
		if strings.Contains(out, secret) {
			t.Fatalf("lockout logs contain a credential:\n%s", out)
		}
	}
}
