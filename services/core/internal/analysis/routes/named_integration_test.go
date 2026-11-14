//go:build integration

package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// Named access through the routes (plan Task 22): the route env's demo
// hospital (60 synthetic subjects in October's first block, frozen) with
// named access on (demo references), each subject's reference sealed with
// the env's keys, and an active demo letter covering the env's admin
// (seeded by the superuser: only the platform's verify, Plan 11b, makes a
// letter active). Names come from the demo fixture behind a counter.

type countingResolver struct {
	mu     sync.Mutex
	calls  int
	inner  analysis.Resolver
	tamper func(map[string]analysis.Identity)
}

func (c *countingResolver) Resolve(ctx context.Context, r analysis.IdentityRequest) (map[string]analysis.Identity, error) {
	c.mu.Lock()
	c.calls++
	tamper := c.tamper
	c.mu.Unlock()
	if c.inner == nil {
		return nil, analysis.ErrIdentityUnavailable
	}
	got, err := c.inner.Resolve(ctx, r)
	if err == nil && tamper != nil {
		tamper(got)
	}
	return got, err
}

func (c *countingResolver) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type namedEnv struct {
	*env
	letter uuid.UUID
	other  uuid.UUID // a second admin, covered by the letter too
	res    *countingResolver
}

func setNamedSwitch(t *testing.T, e *env, scope *uuid.UUID, position string) {
	t.Helper()
	run(t, e.super, `INSERT INTO named_access_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		VALUES ($1, $2, 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC') ON CONFLICT ON CONSTRAINT named_access_switch_one
		DO UPDATE SET position = EXCLUDED.position`, scope, position)
}

func addAdmin(t *testing.T, e *env) uuid.UUID {
	t.Helper()
	u := uuid.New()
	run(t, e.super, `INSERT INTO users (id, hospital_id, email, display_name) VALUES ($1, $2, $3, 'Synthetic Admin Two')`,
		u, e.h, "admin-"+u.String()[:8]+"@demo.example.test")
	run(t, e.super, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'hospital_admin')`, u)
	run(t, e.super, `INSERT INTO user_mfa (user_id, secret_enc, enabled) VALUES ($1, '\x00', true)`, u)
	return u
}

// seedActive is an active, verified demo letter of the env's hospital
// ending endIn days from the hospital's today, covering people.
func seedActive(t *testing.T, e *env, endIn int, people ...uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	run(t, e.super, `INSERT INTO clearances (id, hospital_id, status, signed_by, signer_title, letter_date, end_date, uploaded_by,
			verified_by, verified_at, pages, sha256, demo)
		VALUES ($1, $2, 'active', 'Synthetic Signer', 'Synthetic Title', (now() AT TIME ZONE 'Africa/Lagos')::date - 40,
			(now() AT TIME ZONE 'Africa/Lagos')::date + $3::int, $4, $4, now(), 1, sha256('synthetic'), true)`, id, e.h, endIn, people[0])
	for _, p := range people {
		run(t, e.super, `INSERT INTO clearance_covered (hospital_id, clearance_id, user_id, side) VALUES ($1, $2, $3, 'hospital')`, e.h, id, p)
	}
	return id
}

func newNamedEnv(t *testing.T) *namedEnv {
	t.Helper()
	super, app := pools(t)
	e := &namedEnv{env: newEnv(t, super, app, true)}
	setNamedSwitch(t, e.env, nil, "on")
	setNamedSwitch(t, e.env, &e.h, "on")
	rows, err := super.Query(context.Background(), `SELECT subject_id FROM analysis_subjects WHERE hospital_id = $1`, e.h)
	if err != nil {
		t.Fatal(err)
	}
	var subjects []uuid.UUID
	for rows.Next() {
		var s uuid.UUID
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		subjects = append(subjects, s)
	}
	rows.Close()
	for i, s := range subjects {
		ct, err := e.hd.d.Keys.SealRef(e.h, fmt.Sprintf("SYNTH-PIX-%05d", i))
		if err != nil {
			t.Fatal(err)
		}
		run(t, super, `UPDATE analysis_subjects SET ref_enc = $2 WHERE subject_id = $1`, s, ct)
	}
	e.other = addAdmin(t, e.env)
	e.letter = seedActive(t, e.env, 30, e.user, e.other)
	demo, err := analysis.NewDemoIdentity(false)
	if err != nil {
		t.Fatal(err)
	}
	e.res = &countingResolver{inner: demo}
	e.hd.d.Identity = e.res
	e.hd.meLim = newLimiter(ClearanceMePerMinute, time.Minute, nil)
	e.mux.Handle("POST "+httpx.BasePath+"/analysis/patients/named", httpx.NoStore(http.HandlerFunc(e.hd.named)))
	e.mux.Handle("GET "+httpx.BasePath+"/analysis/clearance/me", http.HandlerFunc(e.hd.clearanceMe))
	e.mux.Handle("GET "+httpx.BasePath+"/analysis/named-access", http.HandlerFunc(e.hd.namedAccess))
	return e
}

// do serves method path as user (the env's admin when uuid.Nil), with an
// Idempotency-Key when key isn't empty.
func (e *namedEnv) do(t *testing.T, user uuid.UUID, method, path, key, body string) (int, []byte) {
	t.Helper()
	if user == uuid.Nil {
		user = e.user
	}
	r := httptest.NewRequest(method, httpx.BasePath+path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	ctx := authz.WithPrincipal(r.Context(), authz.Principal{UserID: user, HospitalID: e.h,
		Roles: []string{authz.RoleHospitalAdmin}, MFA: true, SessionID: "session-" + user.String()})
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r.WithContext(ctx))
	if w.Code == 200 && w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("%s %s: Cache-Control %q", method, path, w.Header().Get("Cache-Control"))
	}
	return w.Code, w.Body.Bytes()
}

func (e *namedEnv) namedBody(t *testing.T, purpose, note string) string {
	t.Helper()
	b := map[string]any{"drill": e.drillFor(t, weekQ, "by_sex", "sex", "female"), "purpose": purpose, "filters": map[string]string{},
		"period": map[string]string{"level": "week", "start": "2026-10-01"}}
	if note != "" {
		b["note"] = note
	}
	out, _ := json.Marshal(b)
	return string(out)
}

type namedReplyT struct {
	Level      string
	Count      int
	Patients   []map[string]any
	NamedUntil string `json:"named_until"`
	Clearance  struct {
		ID      string
		EndDate string `json:"end_date"`
	}
}

func (e *namedEnv) post(t *testing.T, user uuid.UUID, key, body string) (int, []byte) {
	t.Helper()
	return e.do(t, user, http.MethodPost, "/analysis/patients/named", key, body)
}

func (e *namedEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.super.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A named list and its replay: names from the fixture after the entry;
// the replay keeps named_until and the rows, and is audited with its
// number; another person's key or another body is 422.
func TestNamedViewAndReplay(t *testing.T) {
	e := newNamedEnv(t)
	body := e.namedBody(t, "audit", "")
	c, b := e.post(t, uuid.Nil, "synthetic-key-0001", body)
	if c != 200 {
		t.Fatalf("named: %d %s", c, b)
	}
	var r namedReplyT
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if r.Level != "named" || r.Count != 20 || len(r.Patients) != 20 || r.Clearance.ID != e.letter.String() || r.NamedUntil == "" {
		t.Fatalf("reply: %s", b)
	}
	for _, p := range r.Patients {
		name, _ := p["name"].(string)
		mrn, _ := p["mrn"].(string)
		if !strings.HasSuffix(name, "(synthetic)") || !strings.HasPrefix(mrn, "SYN-") || !pidRe.MatchString(p["pid"].(string)) || p["sex"] != "female" {
			t.Fatalf("row %v", p)
		}
	}
	c, b2 := e.post(t, uuid.Nil, "synthetic-key-0001", body)
	if c != 200 {
		t.Fatalf("replay: %d %s", c, b2)
	}
	var r2 namedReplyT
	_ = json.Unmarshal(b2, &r2)
	if r2.NamedUntil != r.NamedUntil || len(r2.Patients) != 20 || r2.Patients[0]["pid"] != r.Patients[0]["pid"] {
		t.Errorf("the replay changed: %s", b2)
	}
	if n := e.count(t, `SELECT count(*) FROM analysis_views WHERE hospital_id = $1 AND level = 'named'`, e.h); n != 1 {
		t.Errorf("%d named lists, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'analysis.named_viewed' AND actor_id = $1
		AND payload->>'replays' IN ('0', '1') AND payload->>'clearance_id' = $2`, e.user, e.letter.String()); n != 2 {
		t.Errorf("%d named entries, want the list and its replay", n)
	}
	if c, b := e.post(t, e.other, "synthetic-key-0001", body); c != 422 || code(b) != "idempotency_mismatch" {
		t.Errorf("another person's key: %d %s", c, b)
	}
	if c, b := e.post(t, uuid.Nil, "synthetic-key-0001", e.namedBody(t, "quality_improvement", "")); c != 422 || code(b) != "idempotency_mismatch" {
		t.Errorf("another body: %d %s", c, b)
	}
	if c, b := e.post(t, uuid.Nil, "", body); c != 422 || code(b) != "validation_failed" {
		t.Errorf("no key: %d %s", c, b)
	}
	// After named_until, the key has expired.
	run(t, e.super, `UPDATE named_view_keys SET named_until = now() - interval '1 second'`)
	if c, b := e.post(t, uuid.Nil, "synthetic-key-0001", body); c != 409 || code(b) != "expired" {
		t.Errorf("after named_until: %d %s", c, b)
	}
}

// T11: below on, the gate answers first.
func TestT11LettersOnlyRefused(t *testing.T) {
	e := newNamedEnv(t)
	body := e.namedBody(t, "audit", "")
	setNamedSwitch(t, e.env, &e.h, "letters_only")
	if c, b := e.post(t, uuid.Nil, "synthetic-key-0011", body); c != 403 || code(b) != "named_access_off" {
		t.Errorf("letters_only: %d %s", c, b)
	}
	if c, b := e.do(t, uuid.Nil, http.MethodGet, "/analysis/clearance/me", "", ""); c != 200 || !strings.Contains(string(b), `"reason":"switched_off"`) {
		t.Errorf("clearance/me at letters_only: %d %s", c, b)
	}
	if c, b := e.do(t, uuid.Nil, http.MethodGet, "/analysis/named-access", "", ""); c != 200 || !strings.Contains(string(b), `"position":"letters_only"`) {
		t.Errorf("named-access: %d %s", c, b)
	}
	setNamedSwitch(t, e.env, &e.h, "off")
	for _, p := range []string{"/analysis/clearance/me", "/analysis/named-access"} {
		if c, b := e.do(t, uuid.Nil, http.MethodGet, p, "", ""); c != 403 || code(b) != "named_access_off" {
			t.Errorf("%s off: %d %s", p, c, b)
		}
	}
	if e.res.n() != 0 {
		t.Error("identity was called")
	}
	// Analysis off: 404 before anything.
	run(t, e.super, `UPDATE analysis_switch SET position = 'off' WHERE hospital_id = $1`, e.h)
	for _, p := range []string{"/analysis/clearance/me", "/analysis/named-access"} {
		if c, _ := e.do(t, uuid.Nil, http.MethodGet, p, "", ""); c != 404 {
			t.Errorf("%s with analysis off: %d", p, c)
		}
	}
	if c, _ := e.post(t, uuid.Nil, "synthetic-key-0012", body); c != 404 {
		t.Errorf("named with analysis off: %d", c)
	}
}

// T12: a revocation elsewhere refuses the next request at once
// (not_cleared, reason revoked); a letter whose last day has passed at the
// hospital is clearance_ended.
func TestT12RevokedAndEndedAtOnce(t *testing.T) {
	e := newNamedEnv(t)
	body := e.namedBody(t, "audit", "")
	if c, b := e.post(t, uuid.Nil, "synthetic-key-0021", body); c != 200 {
		t.Fatalf("before: %d %s", c, b)
	}
	run(t, e.super, `UPDATE clearances SET status = 'revoked', revoked_at = now(), revoke_reason = 'security_concern',
		revoked_side = 'platform' WHERE id = $1`, e.letter)
	if c, b := e.post(t, uuid.Nil, "synthetic-key-0022", body); c != 403 || code(b) != "not_cleared" {
		t.Errorf("after the revocation: %d %s", c, b)
	}
	// The replay of the earlier list is refused too.
	if c, b := e.post(t, uuid.Nil, "synthetic-key-0021", body); c != 403 || code(b) != "not_cleared" {
		t.Errorf("replay after the revocation: %d %s", c, b)
	}
	if c, b := e.do(t, uuid.Nil, http.MethodGet, "/analysis/clearance/me", "", ""); c != 200 || !strings.Contains(string(b), `"reason":"revoked"`) ||
		!strings.Contains(string(b), `"named":false`) {
		t.Errorf("clearance/me: %d %s", c, b)
	}
	third := addAdmin(t, e.env)
	seedActive(t, e.env, -1, third)
	if c, b := e.post(t, third, "synthetic-key-0023", body); c != 403 || code(b) != "clearance_ended" {
		t.Errorf("ended yesterday at the hospital: %d %s", c, b)
	}
}

// T13: the role removed is refused before the 30-second cache expires.
func TestT13RoleRemovedRefusedAtOnce(t *testing.T) {
	e := newNamedEnv(t)
	body := e.namedBody(t, "audit", "")
	run(t, e.super, `DELETE FROM user_roles WHERE user_id = $1`, e.user)
	if c, b := e.post(t, uuid.Nil, "synthetic-key-0031", body); c != 403 || code(b) != "forbidden" {
		t.Errorf("role removed: %d %s", c, b)
	}
	if c, b := e.do(t, uuid.Nil, http.MethodGet, "/analysis/clearance/me", "", ""); c != 200 || !strings.Contains(string(b), `"reason":"role_or_account"`) {
		t.Errorf("clearance/me: %d %s", c, b)
	}
}

// T14: two-step reset, or the account deactivated, at once.
func TestT14MFAResetRefusedAtOnce(t *testing.T) {
	e := newNamedEnv(t)
	body := e.namedBody(t, "audit", "")
	run(t, e.super, `UPDATE user_mfa SET enabled = false WHERE user_id = $1`, e.user)
	if c, b := e.post(t, uuid.Nil, "synthetic-key-0041", body); c != 403 || code(b) != "mfa_required" {
		t.Errorf("two-step reset: %d %s", c, b)
	}
	if c, b := e.do(t, uuid.Nil, http.MethodGet, "/analysis/clearance/me", "", ""); c != 200 || !strings.Contains(string(b), `"reason":"role_or_account"`) {
		t.Errorf("clearance/me: %d %s", c, b)
	}
	run(t, e.super, `UPDATE users SET is_active = false WHERE id = $1`, e.other)
	if c, b := e.post(t, e.other, "synthetic-key-0042", body); c != 403 || code(b) != "forbidden" {
		t.Errorf("deactivated: %d %s", c, b)
	}
}

// T16, T17: after a list and its replay, no stored row in any table and
// no log line holds a synthetic name, MRN or the note; no log line holds a
// pid.
func TestT16T17NoNamesStoredOrLogged(t *testing.T) {
	e := newNamedEnv(t)
	note := "Synthetic note Quokka-7731"
	body := e.namedBody(t, "other", note)
	var r namedReplyT
	for i := 0; i < 2; i++ {
		c, b := e.post(t, uuid.Nil, "synthetic-key-0051", body)
		if c != 200 {
			t.Fatalf("named %d: %d %s", i, c, b)
		}
		_ = json.Unmarshal(b, &r)
	}
	needles := []string{note, "Quokka"}
	for _, p := range r.Patients {
		needles = append(needles, p["name"].(string), p["mrn"].(string))
	}
	rows, err := e.super.Query(context.Background(), `SELECT quote_ident(table_schema) || '.' || quote_ident(table_name)
		FROM information_schema.tables WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema')`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		tables = append(tables, s)
	}
	rows.Close()
	for _, tb := range tables {
		for _, n := range needles {
			if k := e.count(t, `SELECT count(*) FROM `+tb+` x WHERE strpos(row_to_json(x)::text, $1) > 0`, n); k != 0 {
				t.Errorf("%s holds %q", tb, n)
			}
		}
	}
	logs := e.logs.String()
	for _, n := range append(needles, r.Patients[0]["pid"].(string)) {
		if strings.Contains(logs, n) {
			t.Errorf("a log line holds %q", n)
		}
	}
	// The note is kept sealed, once, with its list.
	if n := e.count(t, `SELECT count(*) FROM named_view_notes WHERE hospital_id = $1`, e.h); n != 1 {
		t.Errorf("%d notes, want 1", n)
	}
}

// T18: the audit append fails: no rows are sent, identity is never called,
// and nothing is kept.
func TestT18AuditFailNoIdentityCall(t *testing.T) {
	e := newNamedEnv(t)
	body := e.namedBody(t, "audit", "")
	e.hd.afterOpen = func() error { return errors.New("synthetic audit failure") }
	if c, b := e.post(t, uuid.Nil, "synthetic-key-0061", body); c != 503 || strings.Contains(string(b), "patients") {
		t.Errorf("audit failed: %d %s", c, b)
	}
	if e.res.n() != 0 {
		t.Error("identity was called")
	}
	if n := e.count(t, `SELECT count(*) FROM analysis_views WHERE level = 'named'`); n != 0 {
		t.Errorf("%d named lists kept", n)
	}
	if n := e.count(t, `SELECT count(*) FROM named_view_keys`); n != 0 {
		t.Errorf("%d keys kept", n)
	}
}

// T19: a hospital, viewer or clearance id in the body is refused.
func TestT19NamedBodyFieldsRefused(t *testing.T) {
	e := newNamedEnv(t)
	body := e.namedBody(t, "audit", "")
	for _, f := range []string{"hospital_id", "viewer", "clearance_id"} {
		bad := strings.Replace(body, `{`, `{"`+f+`":"`+uuid.NewString()+`",`, 1)
		if c, b := e.post(t, uuid.Nil, "synthetic-key-0071-"+f, bad); c != 422 || code(b) != "validation_failed" {
			t.Errorf("%s: %d %s", f, c, b)
		}
	}
}

// T20 through the route: an extra reference or one on another subject
// fails the list with 503 and a flagged entry; the list stays recorded.
func TestT20ReplyMismatchFails(t *testing.T) {
	e := newNamedEnv(t)
	body := e.namedBody(t, "audit", "")
	e.res.tamper = func(m map[string]analysis.Identity) {
		m["SYNTH-PIX-99999"] = analysis.NewIdentity("Extra Person (synthetic)", "SYN-9")
	}
	if c, b := e.post(t, uuid.Nil, "synthetic-key-0081", body); c != 503 || code(b) != "unavailable" || strings.Contains(string(b), "synthetic)") {
		t.Errorf("extra reference: %d %s", c, b)
	}
	e.res.tamper = func(m map[string]analysis.Identity) {
		for k, v := range m {
			delete(m, k)
			m[k+"-other"] = v
			return
		}
	}
	if c, b := e.post(t, uuid.Nil, "synthetic-key-0082", body); c != 503 {
		t.Errorf("another subject: %d %s", c, b)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'analysis.named_view_failed' AND payload->>'clearance_id' = $1`, e.letter.String()); n != 2 {
		t.Errorf("%d failed entries, want 2", n)
	}
	// The live source (no identity yet): 503 as well.
	e.res.tamper, e.res.inner = nil, nil
	if c, _ := e.post(t, uuid.Nil, "synthetic-key-0083", body); c != 503 {
		t.Errorf("live: %d", c)
	}
}

// M7: replays count toward 5 a day; past it, 429 and one flagged entry.
func TestNamedBudgetWithReplays(t *testing.T) {
	e := newNamedEnv(t)
	body := e.namedBody(t, "audit", "")
	for i := 0; i < NamedPerDay; i++ {
		if c, b := e.post(t, uuid.Nil, "synthetic-key-0091", body); c != 200 {
			t.Fatalf("view %d: %d %s", i, c, b)
		}
	}
	for i := 0; i < 2; i++ {
		if c, b := e.post(t, uuid.Nil, "synthetic-key-0092", body); c != 429 || code(b) != "rate_limited" {
			t.Errorf("past the limit: %d %s", c, b)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'analysis.named_limit_reached' AND actor_id = $1`, e.user); n != 1 {
		t.Errorf("%d flagged entries, want 1", n)
	}
	// The other admin has their own budget.
	if c, b := e.post(t, e.other, "synthetic-key-0093", body); c != 200 {
		t.Errorf("another person: %d %s", c, b)
	}
}

// The distinct-patient cap is shared with de-identified lists: the same
// subjects opened by both cost once.
func TestNamedCapSharedWithLists(t *testing.T) {
	e := newNamedEnv(t)
	drill := e.drillFor(t, weekQ, "by_sex", "sex", "female")
	if c, b := e.list(t, weekQ, drill); c != 200 {
		t.Fatalf("list: %d %s", c, b)
	}
	if c, b := e.post(t, uuid.Nil, "synthetic-key-0101", e.namedBody(t, "audit", "")); c != 200 {
		t.Fatalf("named: %d %s", c, b)
	}
	if n := e.count(t, `SELECT opened FROM analysis_patient_budget WHERE hospital_id = $1 AND user_id = $2`, e.h, e.user); n != 20 {
		t.Errorf("opened %d, want 20", n)
	}
}

// clearance/me: 6 a minute per session, not audited.
func TestClearanceMeRateLimited(t *testing.T) {
	e := newNamedEnv(t)
	before := e.count(t, `SELECT count(*) FROM audit_log`)
	for i := 0; i < ClearanceMePerMinute; i++ {
		c, b := e.do(t, uuid.Nil, http.MethodGet, "/analysis/clearance/me", "", "")
		if c != 200 || !strings.Contains(string(b), `"named":true`) || !strings.Contains(string(b), e.letter.String()) {
			t.Fatalf("call %d: %d %s", i, c, b)
		}
	}
	if c, b := e.do(t, uuid.Nil, http.MethodGet, "/analysis/clearance/me", "", ""); c != 429 || code(b) != "rate_limited" {
		t.Errorf("past 6: %d %s", c, b)
	}
	if after := e.count(t, `SELECT count(*) FROM audit_log`); after != before {
		t.Errorf("clearance/me wrote %d entries", after-before)
	}
}
