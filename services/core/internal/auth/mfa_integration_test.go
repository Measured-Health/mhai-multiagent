//go:build integration

package auth_test

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
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
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/delivery"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/logx"
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
	return e.challengeAt(t, mfaToken, code, trust, false)
}

func (e *env) challengeAt(t *testing.T, mfaToken, code string, trust, platform bool) resp {
	t.Helper()
	body := map[string]any{"mfa_token": mfaToken, "code": code}
	if trust {
		body["trust_device"] = true
	}
	return e.do(t, req{method: "POST", path: "/auth/mfa/challenge", body: body, ip: newIP(), platform: platform})
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
	if len(codes) != 1 {
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
	if usr["mfa_enabled"] != true || usr["recovery_codes_left"].(float64) != 1 {
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

// storedBackup returns the account's stored backup code hashes.
func storedBackup(t *testing.T, id uuid.UUID) []string {
	t.Helper()
	var stored []string
	if err := super.QueryRow(context.Background(), `SELECT backup_code_hashes FROM user_mfa WHERE user_id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

// backupHash is an independent copy of the stored form of a backup code:
// hex(HMAC-SHA256(HKDF(key, "mhai-mfa-backup-hmac-v1"), normalised code)).
func backupHash(t *testing.T, code string) string {
	t.Helper()
	k, err := hkdf.Key(sha256.New, []byte(testMFAKey), nil, "mhai-mfa-backup-hmac-v1", 32)
	if err != nil {
		t.Fatal(err)
	}
	m := hmac.New(sha256.New, k)
	m.Write([]byte(strings.ToLower(strings.ReplaceAll(code, "-", ""))))
	return hex.EncodeToString(m.Sum(nil))
}

// backupAuditPayload returns the payload of the request's audit row action.
func backupAuditPayload(t *testing.T, r resp, action string) map[string]any {
	t.Helper()
	var raw []byte
	if err := super.QueryRow(context.Background(), `SELECT payload::text FROM audit_log WHERE request_id = $1 AND action = $2`,
		r.hdr.Get("X-Request-ID"), action).Scan(&raw); err != nil {
		t.Fatalf("audit %s: %v", action, err)
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// renewedCode checks the reply's new_backup_code and returns it.
func renewedCode(t *testing.T, r resp, old string) string {
	t.Helper()
	nb, _ := r.body["new_backup_code"].(string)
	if len(nb) != 11 || nb[5] != '-' || nb == old {
		t.Fatalf("new_backup_code %q: %s", nb, r.raw)
	}
	if h := r.hdr.Get(httpx.HeaderNewBackupCode); h != "" {
		t.Fatal("the sign-in reply also sent the step-up header")
	}
	return nb
}

// The account has one backup code; signing in with it replaces it, and the
// reply carries the new one.
func TestBackupCodeRenewedAtSignIn(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)

	stored := storedBackup(t, u.id)
	if len(stored) != 1 || stored[0] != backupHash(t, en.codes[0]) {
		t.Fatalf("stored %v", stored)
	}
	if strings.Contains(stored[0], en.codes[0]) || strings.Contains(stored[0], strings.ReplaceAll(en.codes[0], "-", "")) {
		t.Fatal("backup code stored in plain")
	}

	r := e.challenge(t, e.mfaToken(t, u), en.codes[0], false)
	if r.code != 200 {
		t.Fatalf("backup code: %d %s", r.code, r.raw)
	}
	if cc := r.hdr.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q", cc)
	}
	if strings.Join(keys(r.body), ",") != "access_token,expires_in,new_backup_code,refresh_token,user" {
		t.Fatalf("keys %v", keys(r.body))
	}
	c, _ := token.Verify(r.body["access_token"].(string), issuer.PublicKeys())
	if !c.MFA || strings.Join(c.AMR, ",") != "pwd,backup" {
		t.Fatalf("claims %+v", c)
	}
	nb := renewedCode(t, r, en.codes[0])
	if left := r.body["user"].(map[string]any)["recovery_codes_left"].(float64); left != 1 {
		t.Fatalf("recovery_codes_left %v", left)
	}
	if p := backupAuditPayload(t, r, "auth.mfa.backup_used"); p["left"] != float64(1) || p["renewed"] != true || len(p) != 2 {
		t.Fatalf("backup_used payload %v", p)
	}
	if s := storedBackup(t, u.id); len(s) != 1 || s[0] != backupHash(t, nb) {
		t.Fatal("the stored hash is not the new code's")
	}
	// The renewal keeps the security notice: exactly one for the use.
	oneNotice(t, u.id, delivery.NoticeBackupCodeUsed)
	if rf := e.refresh(t, r.body["refresh_token"].(string)); rf.code != 200 {
		t.Fatalf("refresh of a backup-code session: %d", rf.code)
	}

	// Replay of the used code: refused, and nothing renewed.
	again := e.challenge(t, e.mfaToken(t, u), en.codes[0], false)
	mustCode(t, "backup code twice", again, 403, "mfa_invalid")
	if again.hdr.Get(httpx.HeaderNewBackupCode) != "" || again.body["new_backup_code"] != nil {
		t.Fatal("a refused code got a new one")
	}
	// The new code works (case and the dash don't matter) and renews again.
	loose := strings.ToUpper(strings.ReplaceAll(nb, "-", ""))
	r2 := e.challenge(t, e.mfaToken(t, u), loose, false)
	if r2.code != 200 {
		t.Fatalf("new backup code: %d %s", r2.code, r2.raw)
	}
	nb2 := renewedCode(t, r2, nb)
	mustCode(t, "the replaced code", e.challenge(t, e.mfaToken(t, u), nb, false), 403, "mfa_invalid")

	// A TOTP sign-in never carries a code and leaves the backup code alone.
	tr := e.challenge(t, e.mfaToken(t, u), totpAt(en.secret, e.clock.Now()), false)
	if tr.code != 200 || tr.body["new_backup_code"] != nil || tr.hdr.Get(httpx.HeaderNewBackupCode) != "" {
		t.Fatalf("TOTP sign-in: %d %s", tr.code, tr.raw)
	}
	if s := storedBackup(t, u.id); len(s) != 1 || s[0] != backupHash(t, nb2) {
		t.Fatal("a TOTP sign-in changed the backup code")
	}
}

// An account from before the one-code rule holds several codes; the first
// use leaves it with exactly one, the new one.
func TestLegacyBackupCodesCollapseToOne(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	e.enrol(t, u)
	legacy := make([]string, 10)
	hashes := make([]string, 10)
	for i := range legacy {
		legacy[i] = fmt.Sprintf("synth-%05d", i)
		hashes[i] = backupHash(t, legacy[i])
	}
	if _, err := super.Exec(context.Background(), `UPDATE user_mfa SET backup_code_hashes = $2 WHERE user_id = $1`, u.id, hashes); err != nil {
		t.Fatal(err)
	}
	r := e.challenge(t, e.mfaToken(t, u), legacy[3], false)
	if r.code != 200 {
		t.Fatalf("legacy code: %d %s", r.code, r.raw)
	}
	nb := renewedCode(t, r, legacy[3])
	if s := storedBackup(t, u.id); len(s) != 1 || s[0] != backupHash(t, nb) {
		t.Fatalf("%d stored hashes after a use", len(s))
	}
	if left := r.body["user"].(map[string]any)["recovery_codes_left"].(float64); left != 1 {
		t.Fatalf("recovery_codes_left %v", left)
	}
	mustCode(t, "another legacy code", e.challenge(t, e.mfaToken(t, u), legacy[4], false), 403, "mfa_invalid")
	if r := e.challenge(t, e.mfaToken(t, u), nb, false); r.code != 200 {
		t.Fatalf("new code: %d %s", r.code, r.raw)
	}
}

// Two sign-ins racing with the same backup code: exactly one wins, and the
// stored code is the winner's new one.
func TestBackupCodeRenewalConcurrent(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	toks := []string{e.mfaToken(t, u), e.mfaToken(t, u), e.mfaToken(t, u)}
	out := make([]resp, len(toks))
	var wg sync.WaitGroup
	for i := range toks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = e.challenge(t, toks[i], en.codes[0], false)
		}(i)
	}
	wg.Wait()
	var won []string
	for _, r := range out {
		switch r.code {
		case 200:
			won = append(won, renewedCode(t, r, en.codes[0]))
		case 403:
			if r.body["new_backup_code"] != nil || r.hdr.Get(httpx.HeaderNewBackupCode) != "" {
				t.Fatal("a losing request got a code")
			}
		default:
			t.Fatalf("unexpected %d %s", r.code, r.raw)
		}
	}
	if len(won) != 1 {
		t.Fatalf("%d winners", len(won))
	}
	if s := storedBackup(t, u.id); len(s) != 1 || s[0] != backupHash(t, won[0]) {
		t.Fatal("stored code is not the winner's")
	}
}

func (e *env) stepUpProbe(t *testing.T, path, at string, body map[string]any) resp {
	t.Helper()
	return e.do(t, req{method: "POST", path: path, bearer: at, body: body})
}

// A backup-code step-up renews the code; the reply carries the new one in
// X-MHAI-New-Backup-Code exactly when the burn stands.
func TestStepUpBackupCodeRenewal(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	at := e.fullSession(t, u, en).body["access_token"].(string)
	code := en.codes[0]
	header := func(name string, r resp, status int, want bool) string {
		t.Helper()
		if r.code != status {
			t.Fatalf("%s: %d %s", name, r.code, r.raw)
		}
		h := r.hdr.Get(httpx.HeaderNewBackupCode)
		if !want {
			if h != "" {
				t.Fatalf("%s: header sent", name)
			}
			return ""
		}
		if len(h) != 11 || h[5] != '-' || r.hdr.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: header %q, Cache-Control %q", name, h, r.hdr.Get("Cache-Control"))
		}
		if s := storedBackup(t, u.id); len(s) != 1 || s[0] != backupHash(t, h) {
			t.Fatalf("%s: the header's code is not the stored one", name)
		}
		return h
	}
	const inTx, audited = "/probe/step-up-in-tx", "/probe/step-up-audited"

	// In the write's transaction, the write fails after the code was
	// accepted: rolled back, so no header and the old code still works.
	header("in-tx rollback", e.stepUpProbe(t, inTx, at, map[string]any{"backup_code": code, "fail": true}), 500, false)
	if s := storedBackup(t, u.id); len(s) != 1 || s[0] != backupHash(t, code) {
		t.Fatal("a rolled-back step-up changed the backup code")
	}
	if n := notices(t, u.id, delivery.NoticeBackupCodeUsed); n != 0 {
		t.Fatalf("a rolled-back step-up left %d backup-code notices", n)
	}
	// It succeeds: 2xx with the new code; the old one is refused (no header).
	code2 := header("in-tx", e.stepUpProbe(t, inTx, at, map[string]any{"backup_code": code}), 204, true)
	oneNotice(t, u.id, delivery.NoticeBackupCodeUsed)
	header("replay", e.stepUpProbe(t, inTx, at, map[string]any{"backup_code": code}), 403, false)
	// In its own transaction (StepUpAudited): the burn stands even when the
	// write fails, so the error reply carries the new code.
	code3 := header("audited", e.stepUpProbe(t, audited, at, map[string]any{"backup_code": code2}), 204, true)
	code4 := header("audited, write fails", e.stepUpProbe(t, audited, at, map[string]any{"backup_code": code3, "fail": true}), 500, true)
	header("audited replay", e.stepUpProbe(t, audited, at, map[string]any{"backup_code": code3}), 403, false)
	// The renewed code also signs in.
	if r := e.challenge(t, e.mfaToken(t, u), code4, false); r.code != 200 {
		t.Fatalf("sign-in with the step-up's new code: %d %s", r.code, r.raw)
	} else {
		code4 = renewedCode(t, r, code4)
	}
	// TOTP step-ups never send the header.
	e.clock.Add(30 * time.Second)
	header("in-tx TOTP", e.stepUpProbe(t, inTx, at, map[string]any{"mfa_code": totpAt(en.secret, e.clock.Now())}), 204, false)
	e.clock.Add(30 * time.Second)
	header("audited TOTP", e.stepUpProbe(t, audited, at, map[string]any{"mfa_code": totpAt(en.secret, e.clock.Now())}), 204, false)
	if s := storedBackup(t, u.id); len(s) != 1 || s[0] != backupHash(t, code4) {
		t.Fatal("a TOTP step-up changed the backup code")
	}

	// Lockout counting is unchanged: 5 wrong backup codes lock, and then
	// even the right one gets 429 with no header and is not spent.
	for i := 0; i < auth.MaxMFAFailures; i++ {
		header("wrong backup", e.stepUpProbe(t, audited, at, map[string]any{"backup_code": "zzzzz-zzzz" + fmt.Sprint(i)}), 403, false)
	}
	header("locked", e.stepUpProbe(t, audited, at, map[string]any{"backup_code": code4}), 429, false)
	if s := storedBackup(t, u.id); len(s) != 1 || s[0] != backupHash(t, code4) {
		t.Fatal("a locked step-up spent the code")
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
	if len(fresh) != 1 {
		t.Fatalf("%d codes", len(fresh))
	}
	if r.hdr.Get(httpx.HeaderNewBackupCode) != "" {
		t.Fatal("a TOTP step-up sent the header")
	}
	if p := backupAuditPayload(t, r, "auth.mfa.codes_regenerated"); p["count"] != float64(1) {
		t.Fatalf("codes_regenerated payload %v", p)
	}
	e.clock.Add(30 * time.Second)

	mustCode(t, "old code", e.challenge(t, e.mfaToken(t, u), en.codes[0], false), 403, "mfa_invalid")
	sr := e.challenge(t, e.mfaToken(t, u), fresh[0], false)
	if sr.code != 200 {
		t.Fatalf("new code: %d %s", sr.code, sr.raw)
	}
	renewed := renewedCode(t, sr, fresh[0])
	// A backup code also works as the step-up. The regenerated code
	// replaces the step-up's renewal in the same transaction, so only the
	// body carries a code: no header.
	r = e.do(t, req{method: "POST", path: "/auth/mfa/recovery-codes", bearer: at, body: map[string]any{"backup_code": renewed}})
	if r.code != 200 {
		t.Fatalf("regenerate with a backup code: %d %s", r.code, r.raw)
	}
	if r.hdr.Get(httpx.HeaderNewBackupCode) != "" {
		t.Fatal("regenerate sent a superseded code in the header")
	}
	again := r.body["recovery_codes"].([]any)[0].(string)
	if s := storedBackup(t, u.id); len(s) != 1 || s[0] != backupHash(t, again) {
		t.Fatal("the stored code is not the regenerated one")
	}
	mustCode(t, "step-up code after regenerate", e.do(t, req{method: "POST", path: "/auth/mfa/recovery-codes", bearer: at,
		body: map[string]any{"backup_code": renewed}}), 403, "mfa_invalid")
	if r := e.challenge(t, e.mfaToken(t, u), again, false); r.code != 200 {
		t.Fatalf("regenerated code: %d %s", r.code, r.raw)
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
	for _, role := range []string{"hospital_admin", "analyst"} {
		u, dev := e.trustedClinician(t, hid)
		// The clinician gains the role: the device no longer counts.
		if _, err := super.Exec(context.Background(), `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, u.id, role); err != nil {
			t.Fatal(err)
		}
		r := e.loginDevice(t, u.email, dev)
		if r.code != 200 || r.body["mfa_required"] != true || r.body["access_token"] != nil {
			t.Fatalf("%s with a device: %d %s", role, r.code, r.raw)
		}
		// And a device row seeded for such an account directly doesn't count either.
		adm := seedUser(t, hid, true, role)
		r = e.loginDevice(t, adm.email, seedDevice(t, adm.id, time.Now().Add(time.Hour)))
		if r.body["mfa_required"] != true {
			t.Fatalf("seeded %s device: %d %s", role, r.code, r.raw)
		}
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
	ctx, slot := httpx.WithNewBackupCodeSlot(context.Background())
	u := admin(t)
	en := e.enrol(t, u)
	if err := e.svc.StepUp(ctx, nil, u.id.String(), totpAt(en.secret, e.clock.Now()), ""); err != nil {
		t.Fatalf("good code: %v", err)
	}
	if slot.Take() != "" {
		t.Fatal("a TOTP step-up offered a backup code")
	}
	if err := e.svc.StepUp(ctx, nil, u.id.String(), totpAt(en.secret, e.clock.Now()), ""); !errors.Is(err, auth.ErrMFAInvalid) {
		t.Fatalf("reused code: %v", err)
	}
	// Outside a request (no slot) a backup code is refused before it is
	// spent: its replacement could not reach the person.
	if err := e.svc.StepUp(context.Background(), nil, u.id.String(), "", en.codes[0]); err == nil ||
		errors.Is(err, auth.ErrMFAInvalid) || errors.Is(err, auth.ErrMFALocked) {
		t.Fatalf("backup code with no slot: %v", err)
	}
	if err := e.svc.StepUp(ctx, nil, u.id.String(), "", en.codes[0]); err != nil {
		t.Fatalf("backup code: %v", err)
	}
	renewed := slot.Take()
	if renewed == "" || renewed == en.codes[0] {
		t.Fatalf("renewed code %q", renewed)
	}
	if err := e.svc.StepUp(ctx, nil, u.id.String(), "", en.codes[0]); !errors.Is(err, auth.ErrMFAInvalid) {
		t.Fatalf("backup code twice: %v", err)
	}
	if slot.Take() != "" {
		t.Fatal("a refused code offered a backup code")
	}
	if err := e.svc.StepUp(ctx, nil, u.id.String(), "", renewed); err != nil {
		t.Fatalf("renewed code: %v", err)
	}
	if slot.Take() == "" {
		t.Fatal("the renewed code was not renewed again")
	}
	if err := e.svc.StepUp(ctx, nil, u.id.String(), "", ""); !errors.Is(err, auth.ErrMFAInvalid) {
		t.Fatalf("no code: %v", err)
	}
	if err := e.svc.StepUp(ctx, nil, u.id.String(), "123456", en.codes[0]); !errors.Is(err, auth.ErrMFAInvalid) {
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
		{"recovery wrong backup", req{method: "POST", path: "/auth/mfa/recovery-codes", bearer: at, body: map[string]any{"backup_code": en.codes[0] + "x"}}},
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

// --- fix round 1 ----------------------------------------------------------------

// A clinician who turned two-step on is always challenged, even where the
// hospital doesn't require it, and a trusted device skips the code only
// where the hospital allows trusted devices.
func TestMFAEnabledAlwaysChallenged(t *testing.T) {
	e := newEnv(t)
	for _, allowTrust := range []bool{false, true} {
		hid := seedHospital(t, hospitalOpts{requireMFAClinicians: false, allowTrust: allowTrust})
		u := seedUser(t, hid, false, "clinician")
		l := e.login(t, u.email, testPassword, newIP())
		other := e.login(t, u.email, testPassword, newIP())
		if l.code != 200 || l.body["access_token"] == nil || other.code != 200 {
			t.Fatalf("voluntary clinician before enrolment: %d %s", l.code, l.raw)
		}
		at := l.body["access_token"].(string)
		en := e.do(t, req{method: "POST", path: "/auth/mfa/enroll", bearer: at})
		if en.code != 200 {
			t.Fatalf("enroll %d %s", en.code, en.raw)
		}
		secret := secretFromURI(t, en.body["otpauth_uri"].(string))
		if v := e.do(t, req{method: "POST", path: "/auth/mfa/verify", bearer: at,
			body: map[string]any{"code": totpAt(secret, e.clock.Now())}}); v.code != 204 {
			t.Fatalf("verify %d %s", v.code, v.raw)
		}
		e.clock.Add(30 * time.Second)

		// The password alone no longer opens a session.
		tok := e.mfaToken(t, u)
		r := e.challenge(t, tok, totpAt(secret, e.clock.Now()), false)
		c, _ := token.VerifyAt(fmt.Sprint(r.body["access_token"]), issuer.PublicKeys(), e.clock.Now())
		if r.code != 200 || !c.MFA || strings.Join(c.AMR, ",") != "pwd,otp" {
			t.Fatalf("challenge: %d %s", r.code, r.raw)
		}
		e.clock.Add(30 * time.Second)
		// A session opened before two-step was on can't be refreshed into a
		// full token, and its access token is refused.
		if rf := e.refresh(t, other.body["refresh_token"].(string)); rf.code != 401 {
			t.Fatalf("pre-enrolment session refresh: %d %s", rf.code, rf.raw)
		}
		// A trusted device skips the code only where the hospital allows it.
		dr := e.loginDevice(t, u.email, seedDevice(t, u.id, time.Now().Add(time.Hour)))
		if allowTrust {
			if dr.code != 200 || dr.body["access_token"] == nil {
				t.Fatalf("device, trust on: %d %s", dr.code, dr.raw)
			}
		} else if dr.code != 200 || dr.body["mfa_required"] != true || dr.body["access_token"] != nil {
			t.Fatalf("device, trust off: %d %s", dr.code, dr.raw)
		}
	}
	// An old mfa=false access token of an account now enrolled is refused.
	hid := seedHospital(t, hospitalOpts{})
	u := seedUser(t, hid, false, "clinician")
	l := e.login(t, u.email, testPassword, newIP())
	_, _ = super.Exec(context.Background(), `INSERT INTO user_mfa (user_id, secret_enc, enabled) VALUES ($1, '\x00', true)`, u.id)
	mustCode(t, "mfa=false token after enabling", e.do(t, req{method: "GET", path: "/probe", bearer: l.body["access_token"].(string)}),
		403, "mfa_required")
}

// A password change voids mfa_tokens issued before it, enrolled or not.
func TestPasswordChangeVoidsPendingMFAToken(t *testing.T) {
	e := newEnv(t)
	const newPW = "tangerine-orbit-77-quill"

	// Enrolled account.
	u := admin(t)
	en := e.enrol(t, u)
	pending := e.mfaToken(t, u)
	at := e.fullSession(t, u, en).body["access_token"].(string)
	if r := e.do(t, req{method: "POST", path: "/auth/password", bearer: at,
		body: map[string]any{"current_password": testPassword, "new_password": newPW}}); r.code != 204 {
		t.Fatalf("change: %d %s", r.code, r.raw)
	}
	mustCode(t, "enrolled, token from before the change", e.challenge(t, pending, totpAt(en.secret, e.clock.Now()), false),
		403, "mfa_invalid")

	// Unenrolled account: no limited session for the old token.
	v := admin(t)
	stale := e.mfaToken(t, v)
	lim := e.limitedSession(t, v).body["access_token"].(string)
	if r := e.do(t, req{method: "POST", path: "/auth/password", bearer: lim,
		body: map[string]any{"current_password": testPassword, "new_password": newPW}}); r.code != 204 {
		t.Fatalf("change from limited session: %d %s", r.code, r.raw)
	}
	mustCode(t, "unenrolled, token from before the change", e.challenge(t, stale, "", false), 403, "mfa_invalid")
	// A token issued after the change works.
	r := e.login(t, v.email, newPW, newIP())
	if c := e.challenge(t, fmt.Sprint(r.body["mfa_token"]), "", false); c.code != 200 {
		t.Fatalf("fresh token: %d %s", c.code, c.raw)
	}
}

// An mfa_token is redeemable only through the audience it was issued on.
func TestChallengeChecksAudience(t *testing.T) {
	e := newEnv(t)
	p := seedUser(t, uuid.Nil, false, "platform_admin")
	l := e.do(t, req{method: "POST", path: "/auth/login", platform: true, ip: newIP(),
		body: map[string]any{"username": p.email, "password": testPassword}})
	ptok, _ := l.body["mfa_token"].(string)
	if l.code != 200 || ptok == "" {
		t.Fatalf("platform login: %d %s", l.code, l.raw)
	}
	mustCode(t, "platform token, clinical audience", e.challengeAt(t, ptok, "", false, false), 403, "mfa_invalid")

	ctok := e.mfaToken(t, admin(t))
	mustCode(t, "clinical token, platform audience", e.challengeAt(t, ctok, "", false, true), 403, "mfa_invalid")

	// Not consumed by the refused tries: the right audience still works.
	if r := e.challengeAt(t, ptok, "", false, true); r.code != 200 {
		t.Fatalf("platform token, platform audience: %d %s", r.code, r.raw)
	}
	if r := e.challengeAt(t, ctok, "", false, false); r.code != 200 {
		t.Fatalf("clinical token, clinical audience: %d %s", r.code, r.raw)
	}
	// (A role change across the platform/clinical line between login and
	// challenge can't happen: the user_roles check constraint refuses
	// platform roles on an account with a hospital and the reverse.)
}

// Wrong codes at verify count toward the same per-account lock as the
// challenge.
func TestMFALockSpansVerifyAndChallenge(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	at := e.fullSession(t, u, en).body["access_token"].(string)
	bad := wrongCode(en.secret, e.clock.Now())
	for i := 0; i < 5; i++ {
		mustCode(t, "verify wrong", e.do(t, req{method: "POST", path: "/auth/mfa/verify", bearer: at, body: map[string]any{"code": bad}}),
			403, "mfa_invalid")
	}
	mustCode(t, "challenge after five wrong verifies", e.challenge(t, e.mfaToken(t, u), totpAt(en.secret, e.clock.Now()), false),
		429, "mfa_locked")
}
