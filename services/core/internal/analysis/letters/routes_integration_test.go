//go:build integration

package letters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// The letter routes on a migrated database, through the handlers with a
// verified principal in the context (the guard's role and two-step checks
// are authz's; the routes re-read them in SQL). Synthetic hospitals,
// accounts and letters; the step-up code is the test's.

// Synthetic markers that must never reach a log line.
const (
	signerMark = "Synthetic Signer Zeta"
	noteMark   = "synthetic-note-omega"
	stepCode   = "123456"
)

type env struct {
	super, app     *pgxpool.Pool
	h              uuid.UUID
	admin, analyst uuid.UUID
	clin, pa       uuid.UUID
	paEmail        string
	mux            *http.ServeMux
	logs           *bytes.Buffer
	today, maxEnd  time.Time
}

func pools(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	pg, super := testutil.MigratedPostgres(t)
	app, err := testutil.AppLogin(context.Background(), pg, super)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return super, app
}

func run(t *testing.T, p *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	if _, err := p.Exec(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

func user(t *testing.T, p *pgxpool.Pool, h *uuid.UUID, email string, mfa bool, roles ...string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	run(t, p, `INSERT INTO users (id, hospital_id, email, display_name) VALUES ($1, $2, $3, 'Synthetic Person')`, id, h, email)
	for _, r := range roles {
		run(t, p, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, id, r)
	}
	run(t, p, `INSERT INTO user_mfa (user_id, secret_enc, enabled) VALUES ($1, '\x00', $2)`, id, mfa)
	return id
}

// newEnv is a hospital in zone, analysis on and named access letters_only
// (demo references on a demo hospital when demo, sign-off references
// otherwise), with an admin, an analyst, a clinician and a platform admin.
func newEnv(t *testing.T, super, app *pgxpool.Pool, demo bool, zone string) *env {
	t.Helper()
	e := &env{super: super, app: app, h: uuid.New(), logs: &bytes.Buffer{}}
	ref := "SO-2026-0001"
	if demo {
		ref = "DEMO-SYNTHETIC"
	}
	run(t, super, `INSERT INTO hospitals (id, name, short_code, status, time_zone, is_demo)
		VALUES ($1, 'Synthetic Hospital', $2, 'active', $3, $4)`, e.h, "Y"+e.h.String()[:8], zone, demo)
	for _, tbl := range []string{"analysis_switch", "named_access_switch"} {
		run(t, super, `INSERT INTO `+tbl+` (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
			VALUES (NULL, 'on', 'SO-2026-0009', 'SO-2026-0009', 'SO-2026-0009') ON CONFLICT DO NOTHING`)
	}
	run(t, super, `INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		VALUES ($1, 'on', $2, $2, $2)`, e.h, ref)
	run(t, super, `INSERT INTO named_access_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		VALUES ($1, 'letters_only', $2, $2, $2)`, e.h, ref)
	tag := e.h.String()[:8]
	e.admin = user(t, super, &e.h, "admin-"+tag+"@hospital.example.test", true, "hospital_admin")
	e.analyst = user(t, super, &e.h, "analyst-"+tag+"@hospital.example.test", true, "clinician", "analyst")
	e.clin = user(t, super, &e.h, "clin-"+tag+"@hospital.example.test", true, "clinician")
	e.paEmail = "pa-" + tag + "@measured-health.example"
	e.pa = user(t, super, nil, e.paEmail, true, "platform_admin")
	if err := super.QueryRow(context.Background(), `SELECT (now() AT TIME ZONE $1)::date, ((now() AT TIME ZONE $1)::date + interval '12 months')::date`,
		zone).Scan(&e.today, &e.maxEnd); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := Deps{Pool: app, Log: log, stepUpCheck: func(_ *http.Request, mfa, backup string) error {
		if mfa == stepCode {
			return nil
		}
		return errors.New("wrong code")
	}}
	e.mux = http.NewServeMux()
	for _, r := range []struct {
		pattern string
		h       http.HandlerFunc
	}{
		{"GET /hospital/clearances", d.list},
		{"POST /hospital/clearances", d.upload},
		{"GET /hospital/clearances/{id}", d.get},
		{"GET /hospital/clearances/{id}/pages/{n}", d.page},
		{"POST /hospital/clearances/{id}/revoke", d.revoke},
		{"POST /hospital/clearances/{id}/narrow", d.narrow},
	} {
		method, path, _ := strings.Cut(r.pattern, " ")
		e.mux.Handle(method+" "+httpx.BasePath+path, r.h)
	}
	return e
}

// do serves a request as user u of the env's hospital, with the API's
// body limit, and an Idempotency-Key when keyed.
func (e *env) do(t *testing.T, u uuid.UUID, method, path, ctype string, body []byte, keyed bool) (int, http.Header, []byte) {
	t.Helper()
	r := httptest.NewRequest(method, httpx.BasePath+path, bytes.NewReader(body))
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	if keyed {
		r.Header.Set("Idempotency-Key", uuid.NewString())
	}
	w := httptest.NewRecorder()
	r.Body = http.MaxBytesReader(w, r.Body, httpx.MaxBody)
	ctx := authz.WithPrincipal(r.Context(), authz.Principal{UserID: u, HospitalID: e.h,
		Roles: []string{authz.RoleHospitalAdmin}, MFA: true, SessionID: uuid.NewString()})
	e.mux.ServeHTTP(w, r.WithContext(ctx))
	return w.Code, w.Header(), w.Body.Bytes()
}

func (e *env) get(t *testing.T, path string) (int, []byte) {
	t.Helper()
	c, _, b := e.do(t, e.admin, http.MethodGet, path, "", nil, false)
	return c, b
}

func (e *env) post(t *testing.T, path string, v any) (int, []byte) {
	t.Helper()
	raw, _ := json.Marshal(v)
	c, _, b := e.do(t, e.admin, http.MethodPost, path, "application/json", raw, true)
	return c, b
}

// form is an upload's fields; file is the letter's bytes.
type form struct {
	fields [][2]string
	file   []byte
}

func (e *env) letterForm(staff ...uuid.UUID) form {
	f := form{file: []byte("%PDF-synthetic letter"), fields: [][2]string{
		{"signed_by", signerMark}, {"signer_title", "Synthetic Director"},
		{"letter_date", e.today.Format(time.DateOnly)}, {"end_date", e.today.AddDate(0, 6, 0).Format(time.DateOnly)},
		{"mfa_code", stepCode},
	}}
	for _, s := range staff {
		f.fields = append(f.fields, [2]string{"covered", s.String()})
	}
	return f
}

func (f form) with(k, v string) form {
	out := form{file: f.file}
	replaced := false
	for _, kv := range f.fields {
		if kv[0] == k && !replaced {
			out.fields = append(out.fields, [2]string{k, v})
			replaced = true
			continue
		}
		out.fields = append(out.fields, kv)
	}
	if !replaced {
		out.fields = append(out.fields, [2]string{k, v})
	}
	return out
}

func (f form) without(k string) form {
	out := form{file: f.file}
	for _, kv := range f.fields {
		if kv[0] != k {
			out.fields = append(out.fields, kv)
		}
	}
	return out
}

func (f form) encode(t *testing.T) ([]byte, string) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	if f.file != nil {
		fw, err := mw.CreateFormFile("file", "letter.pdf")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write(f.file)
	}
	for _, kv := range f.fields {
		if err := mw.WriteField(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	_ = mw.Close()
	return b.Bytes(), mw.FormDataContentType()
}

func (e *env) upload(t *testing.T, f form) (int, []byte) {
	t.Helper()
	body, ct := f.encode(t)
	c, _, b := e.do(t, e.admin, http.MethodPost, "/hospital/clearances", ct, body, true)
	return c, b
}

func code(body []byte) string {
	var p struct{ Code string }
	_ = json.Unmarshal(body, &p)
	return p.Code
}

func (e *env) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.super.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) letters(t *testing.T) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM clearances WHERE hospital_id = $1`, e.h)
}

// A demo letter, uploaded through the route.
func (e *env) demoLetter(t *testing.T, staff ...uuid.UUID) ClearanceOut {
	t.Helper()
	c, b := e.upload(t, e.letterForm(staff...))
	if c != http.StatusCreated {
		t.Fatalf("upload: %d %s", c, b)
	}
	var out ClearanceOut
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The gates: analysis off is 404, named access off 403 named_access_off,
// and a caller who isn't (any more) an active hospital admin with
// two-step 403 forbidden, re-read in the transaction.
func TestLetterGate(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true, "Africa/Lagos")
	if c, b := e.get(t, "/hospital/clearances"); c != http.StatusOK {
		t.Fatalf("gate open: %d %s", c, b)
	}
	c, _, b := e.do(t, e.clin, http.MethodGet, "/hospital/clearances", "", nil, false)
	if c != http.StatusForbidden || code(b) != "forbidden" {
		t.Errorf("a clinician: %d %s", c, b)
	}
	run(t, super, `UPDATE user_mfa SET enabled = false WHERE user_id = $1`, e.admin)
	if c, b := e.get(t, "/hospital/clearances"); c != http.StatusForbidden || code(b) != "forbidden" {
		t.Errorf("two-step reset: %d %s", c, b)
	}
	run(t, super, `UPDATE user_mfa SET enabled = true WHERE user_id = $1`, e.admin)
	run(t, super, `UPDATE named_access_switch SET position = 'off' WHERE hospital_id = $1`, e.h)
	if c, b := e.get(t, "/hospital/clearances"); c != http.StatusForbidden || code(b) != "named_access_off" {
		t.Errorf("named access off: %d %s", c, b)
	}
	run(t, super, `UPDATE analysis_switch SET position = 'off' WHERE hospital_id = $1`, e.h)
	for _, p := range []string{"/hospital/clearances", "/hospital/clearances/" + uuid.NewString(),
		"/hospital/clearances/" + uuid.NewString() + "/pages/1"} {
		if c, b := e.get(t, p); c != http.StatusNotFound || code(b) != "not_found" {
			t.Errorf("%s with analysis off: %d %s", p, c, b)
		}
	}
	if c, b := e.upload(t, e.letterForm(e.admin)); c != http.StatusNotFound || e.letters(t) != 0 {
		t.Errorf("upload with analysis off: %d %s", c, b)
	}
}

// T10: another hospital's letter, its page, revoke and narrow are 404.
func TestT10OtherHospitalsLetterIs404(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true, "Africa/Lagos")
	o := newEnv(t, super, app, true, "Africa/Lagos")
	l := o.demoLetter(t, o.admin, o.analyst)
	for _, p := range []string{"/hospital/clearances/" + l.ID, "/hospital/clearances/" + l.ID + "/pages/1"} {
		if c, b := e.get(t, p); c != http.StatusNotFound || code(b) != "not_found" {
			t.Errorf("%s: %d %s", p, c, b)
		}
	}
	if c, b := e.post(t, "/hospital/clearances/"+l.ID+"/revoke", map[string]any{"reason": "no_longer_needed", "version": 2,
		"mfa_code": stepCode}); c != http.StatusNotFound {
		t.Errorf("revoke: %d %s", c, b)
	}
	if c, b := e.post(t, "/hospital/clearances/"+l.ID+"/narrow", map[string]any{"remove": []string{o.analyst.String()},
		"reason": "person_left", "version": 2, "mfa_code": stepCode}); c != http.StatusNotFound {
		t.Errorf("narrow: %d %s", c, b)
	}
	_, b := e.get(t, "/hospital/clearances")
	if strings.Contains(string(b), l.ID) {
		t.Error("the list holds another hospital's letter")
	}
	if n := o.count(t, `SELECT count(*) FROM clearances WHERE id = $1 AND status = 'waiting' AND version = 2`, l.ID); n != 1 {
		t.Error("another hospital changed the letter")
	}
}

// T19: fields the server owns (the hospital, the uploader, the status) are
// refused, and nothing is stored.
func TestT19BodyFieldsRefused(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true, "Africa/Lagos")
	o := newEnv(t, super, app, true, "Africa/Lagos")
	for _, kv := range [][2]string{{"hospital_id", o.h.String()}, {"uploaded_by", o.admin.String()}, {"status", "active"},
		{"version", "9"}, {"signed_by", "Second Signer"}} {
		if c, b := e.upload(t, e.letterForm(e.admin).withExtra(kv)); c != http.StatusUnprocessableEntity ||
			code(b) != "validation_failed" {
			t.Errorf("%s in the body: %d %s", kv[0], c, b)
		}
	}
	// A JSON body with the same fields is not an upload at all.
	if c, _, b := e.do(t, e.admin, http.MethodPost, "/hospital/clearances", "application/json",
		[]byte(`{"hospital_id":"`+o.h.String()+`"}`), true); c != http.StatusUnsupportedMediaType || code(b) != "file_type" {
		t.Errorf("JSON upload: %d %s", c, b)
	}
	if e.letters(t) != 0 || o.letters(t) != 0 {
		t.Error("a refused upload stored a letter")
	}
}

// withExtra appends a field even when one of its name is present.
func (f form) withExtra(kv [2]string) form {
	out := form{file: f.file, fields: append([][2]string{}, f.fields...)}
	out.fields = append(out.fields, kv)
	return out
}

// The refusal table (design §8.1), in order; nothing is stored after any.
func TestUploadRefusals(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true, "Africa/Lagos")
	ok := e.letterForm(e.admin)
	big := ok
	big.file = bytes.Repeat([]byte("x"), httpx.MaxBody+1)
	var many []uuid.UUID
	for range MaxCovered + 1 {
		many = append(many, user(t, super, &e.h, uuid.NewString()[:8]+"@hospital.example.test", true, "hospital_admin"))
	}
	for _, c := range []struct {
		name   string
		f      form
		want   int
		code   string
		detail string
	}{
		{"no file", form{fields: ok.fields}, 422, "validation_failed", ""},
		{"too large", big, 413, "file_too_large", ""},
		{"no code", ok.without("mfa_code"), 403, "mfa_required", ""},
		{"a wrong code", ok.with("mfa_code", "654321"), 403, "mfa_invalid", ""},
		{"an end date of today", ok.with("end_date", e.today.Format(time.DateOnly)), 422, "end_date_too_late", ""},
		{"an end date past the cap", ok.with("end_date", e.maxEnd.AddDate(0, 0, 1).Format(time.DateOnly)), 422, "end_date_too_late", ""},
		{"a bad date", ok.with("end_date", "2027-02-30"), 422, "validation_failed", ""},
		{"a letter dated tomorrow", ok.with("letter_date", e.today.AddDate(0, 0, 1).Format(time.DateOnly)), 422, "validation_failed", ""},
		{"nobody", ok.without("covered"), 422, "validation_failed", ""},
		{"21 people", e.letterForm(many...), 422, "validation_failed", ""},
		{"a person twice", ok.withExtra([2]string{"covered", e.admin.String()}), 422, "validation_failed", ""},
		{"a clinician", e.letterForm(e.admin, e.clin), 422, "not_eligible", e.clin.String()},
		{"a platform email unknown", ok.withExtra([2]string{"covered_platform", "nobody@measured-health.example"}), 422,
			"not_eligible_platform", ""},
		{"renews unknown", ok.with("renews", uuid.NewString()), 422, "validation_failed", ""},
		{"a signer with a control character", ok.with("signed_by", "a\x01b"), 422, "validation_failed", ""},
	} {
		got, b := e.upload(t, c.f)
		if got != c.want || code(b) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, got, b, c.want, c.code)
		}
		if c.code == "not_eligible_platform" && strings.Contains(string(b), "nobody@") {
			t.Errorf("%s: the reply names the email: %s", c.name, b)
		}
		if c.detail != "" && !strings.Contains(string(b), c.detail) {
			t.Errorf("%s: the reply doesn't name %s: %s", c.name, c.detail, b)
		}
	}
	if c, _, b := e.do(t, e.admin, http.MethodPost, "/hospital/clearances", "", nil, true); c != http.StatusUnsupportedMediaType {
		t.Errorf("no content type: %d %s", c, b)
	}
	body, ct := ok.encode(t)
	if c, _, b := e.do(t, e.admin, http.MethodPost, "/hospital/clearances", ct, body, false); c != 422 {
		t.Errorf("no Idempotency-Key: %d %s", c, b)
	}
	if n := e.letters(t); n != 0 {
		t.Errorf("%d letters stored after refusals", n)
	}
}

// Outside the demo a valid upload is 503 unavailable, with nothing stored
// or audited.
func TestLiveUploadIs503AndStoresNothing(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, false, "Africa/Lagos")
	if c, b := e.upload(t, e.letterForm(e.admin).withExtra([2]string{"covered_platform", e.paEmail})); c != http.StatusServiceUnavailable ||
		code(b) != "unavailable" {
		t.Errorf("live upload: %d %s", c, b)
	}
	if e.letters(t) != 0 || e.count(t, `SELECT count(*) FROM audit_log WHERE hospital_id = $1 AND action LIKE 'clearance.%'`, e.h) != 0 {
		t.Error("a live upload stored or audited something")
	}
}

// The demo: the letter is waiting with the fixture's pages; each page view
// is audited before it is sent, with the page headers; no pages yet is
// 503 not_ready; past the last page 404.
func TestDemoUploadAndPages(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true, "Africa/Lagos")
	l := e.demoLetter(t, e.admin, e.analyst)
	if l.Status != "waiting" || l.Pages != DemoPages || l.Version != 2 || len(l.Covered) != 2 || l.SignedBy != signerMark ||
		len(l.History) != 2 || l.History[0].Kind != "uploaded" || l.History[1].Kind != "ready" {
		t.Errorf("letter: %+v", l)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE entity_id = $1 AND action IN ('clearance.uploaded', 'clearance.ready')
		AND hospital_id = $2`, l.ID, e.h); n != 2 {
		t.Errorf("%d upload entries, want 2", n)
	}
	// Covered platform admins, by email on upload, by id after.
	c, b := e.upload(t, e.letterForm(e.admin).withExtra([2]string{"covered_platform", strings.ToUpper(e.paEmail[:1]) + e.paEmail[1:]}))
	if c != http.StatusCreated || strings.Contains(string(b), e.pa.String()) || strings.Contains(string(b), e.paEmail) {
		t.Errorf("platform cover: %d %s", c, b)
	}
	st, h, page := e.do(t, e.admin, http.MethodGet, "/hospital/clearances/"+l.ID+"/pages/1", "", nil, false)
	if st != http.StatusOK || !bytes.HasPrefix(page, []byte("\x89PNG\r\n\x1a\n")) || h.Get("Content-Type") != "image/png" ||
		h.Get("Cache-Control") != "no-store" || h.Get("Content-Disposition") != "inline" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("page 1: %d %v", st, h)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'clearance.viewed' AND entity_id = $1 AND actor_id = $2
		AND payload->>'page' = '1'`, l.ID, e.admin); n != 1 {
		t.Errorf("%d view entries, want 1", n)
	}
	for _, p := range []string{"3", "0", "01", "21", "x"} {
		if c, _ := e.get(t, "/hospital/clearances/"+l.ID+"/pages/"+p); c != http.StatusNotFound {
			t.Errorf("page %s: %d", p, c)
		}
	}
	// A letter still checking has no pages yet.
	checking := uuid.New()
	run(t, super, `INSERT INTO clearances (id, hospital_id, signed_by, signer_title, letter_date, end_date, uploaded_by, demo)
		VALUES ($1, $2, 'S', 'T', $3, $4, $5, true)`, checking, e.h, e.today, e.today.AddDate(0, 1, 0), e.admin)
	if c, b := e.get(t, "/hospital/clearances/"+checking.String()+"/pages/1"); c != http.StatusServiceUnavailable || code(b) != "not_ready" {
		t.Errorf("checking letter's page: %d %s", c, b)
	}
	// A letter that isn't a demo letter: its pages are the documents service's.
	live := uuid.New()
	run(t, super, `INSERT INTO clearances (id, hospital_id, signed_by, signer_title, letter_date, end_date, uploaded_by, status, pages, sha256)
		VALUES ($1, $2, 'S', 'T', $3, $4, $5, 'waiting', 2, sha256('x'))`, live, e.h, e.today, e.today.AddDate(0, 1, 0), e.admin)
	if c, b := e.get(t, "/hospital/clearances/"+live.String()+"/pages/1"); c != http.StatusServiceUnavailable || code(b) != "unavailable" {
		t.Errorf("live letter's page: %d %s", c, b)
	}
	// The list.
	c, b = e.get(t, "/hospital/clearances")
	var list ClearancesOut
	if c != http.StatusOK || json.Unmarshal(b, &list) != nil || len(list.Clearances) != 4 ||
		list.Today != e.today.Format(time.DateOnly) || list.MaxEndDate != e.maxEnd.Format(time.DateOnly) {
		t.Errorf("list: %d %s", c, b)
	}
}

// Revoke and narrow: versions, would_empty, notes, and their entries.
func TestRevokeAndNarrow(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true, "Africa/Lagos")
	l := e.demoLetter(t, e.admin, e.analyst)
	path := "/hospital/clearances/" + l.ID
	cases := []struct {
		name, op string
		body     map[string]any
		want     int
		code     string
	}{
		{"narrow everyone", "narrow", map[string]any{"remove": []string{e.admin.String(), e.analyst.String()}, "reason": "person_left",
			"version": 2, "mfa_code": stepCode}, 422, "would_empty"},
		{"narrow at a stale version", "narrow", map[string]any{"remove": []string{e.analyst.String()}, "reason": "person_left",
			"version": 1, "mfa_code": stepCode}, 409, "conflict"},
		{"narrow without a version", "narrow", map[string]any{"remove": []string{e.analyst.String()}, "reason": "person_left",
			"mfa_code": stepCode}, 422, "validation_failed"},
		{"narrow other without a note", "narrow", map[string]any{"remove": []string{e.analyst.String()}, "reason": "other",
			"version": 2, "mfa_code": stepCode}, 422, "validation_failed"},
		{"narrow without a code", "narrow", map[string]any{"remove": []string{e.analyst.String()}, "reason": "person_left",
			"version": 2}, 403, "mfa_required"},
		{"narrow", "narrow", map[string]any{"remove": []string{e.analyst.String()}, "reason": "other", "note": noteMark,
			"version": 2, "mfa_code": stepCode}, 200, ""},
		{"revoke with an unknown field", "revoke", map[string]any{"reason": "other", "note": noteMark, "hospital_id": uuid.NewString(),
			"mfa_code": stepCode}, 422, "validation_failed"},
		{"revoke at a stale version", "revoke", map[string]any{"reason": "no_longer_needed", "version": 2, "mfa_code": stepCode}, 409, "conflict"},
		{"revoke with a wrong code", "revoke", map[string]any{"reason": "no_longer_needed", "version": 3, "mfa_code": "000000"}, 403, "mfa_invalid"},
		{"revoke", "revoke", map[string]any{"reason": "other", "note": noteMark, "version": 3, "mfa_code": stepCode}, 200, ""},
		{"revoke again", "revoke", map[string]any{"reason": "no_longer_needed", "mfa_code": stepCode}, 409, "conflict"},
		{"narrow a revoked letter", "narrow", map[string]any{"remove": []string{e.admin.String()}, "reason": "person_left",
			"version": 4, "mfa_code": stepCode}, 409, "conflict"},
	}
	for _, c := range cases {
		got, b := e.post(t, path+"/"+c.op, c.body)
		if got != c.want || (c.code != "" && code(b) != c.code) {
			t.Errorf("%s: %d %s (logs: %s)", c.name, got, b, e.logs.String())
		}
	}
	_, b := e.get(t, path)
	var out ClearanceOut
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "revoked" || out.Revocation == nil || out.Revocation.Side != "hospital" ||
		out.Revocation.Day != e.today.Format(time.DateOnly) || len(out.Covered) != 1 || out.Version != 4 {
		t.Errorf("after revoke: %s", b)
	}
	for _, c := range []struct {
		action string
		n      int
	}{{"clearance.narrowed", 1}, {"clearance.revoked", 1}} {
		if got := e.count(t, `SELECT count(*) FROM audit_log WHERE action = $1 AND entity_id = $2 AND actor_id = $3`, c.action, l.ID, e.admin); got != c.n {
			t.Errorf("%d %s entries, want %d", got, c.action, c.n)
		}
	}
	// No entry carries the note; no log line carries the note or the signer.
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE payload::text LIKE '%' || $1 || '%' OR payload::text LIKE '%' || $2 || '%'`,
		noteMark, signerMark); n != 0 {
		t.Errorf("%d entries carry a note or a signer", n)
	}
	if s := e.logs.String(); strings.Contains(s, noteMark) || strings.Contains(s, signerMark) || strings.Contains(s, e.paEmail) {
		t.Errorf("a log line carries free text: %s", s)
	}
}

// T12 (the midnight half) and reminders: in a far time zone, a letter
// whose last day is the hospital's today stays active; one whose last day
// was the hospital's yesterday is ended and audited; reminders are
// recorded once per letter and number.
func TestT12EndsAtHospitalMidnightAndReminders(t *testing.T) {
	super, app := pools(t)
	ctx := context.Background()
	for _, zone := range []string{"Pacific/Kiritimati", "Pacific/Pago_Pago"} {
		e := newEnv(t, super, app, true, zone)
		seed := func(end time.Time) uuid.UUID {
			id := uuid.New()
			run(t, super, `INSERT INTO clearances (id, hospital_id, signed_by, signer_title, letter_date, end_date, uploaded_by,
				status, pages, sha256, verified_by, verified_at, demo)
				VALUES ($1, $2, 'S', 'T', $3::date - 60, $3, $4, 'active', 1, sha256('x'), $5, now(), true)`, id, e.h, end, e.admin, e.pa)
			return id
		}
		yesterday, today, soon, later := seed(e.today.AddDate(0, 0, -1)), seed(e.today), seed(e.today.AddDate(0, 0, 5)), seed(e.today.AddDate(0, 2, 0))
		for i := range 2 {
			var now time.Time
			if err := super.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
				t.Fatal(err)
			}
			due, err := analysis.ClearancesDue(ctx, app, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Nightly(ctx, app, due, now, nil); err != nil {
				t.Fatalf("%s run %d: %v", zone, i, err)
			}
		}
		status := func(id uuid.UUID) string {
			var s string
			if err := super.QueryRow(ctx, `SELECT status FROM clearances WHERE id = $1`, id).Scan(&s); err != nil {
				t.Fatal(err)
			}
			return s
		}
		if s := status(yesterday); s != "ended" {
			t.Errorf("%s: yesterday's letter is %s", zone, s)
		}
		for _, id := range []uuid.UUID{today, soon, later} {
			if s := status(id); s != "active" {
				t.Errorf("%s: a current letter is %s", zone, s)
			}
		}
		if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'clearance.ended' AND entity_id = $1 AND actor_id IS NULL
			AND hospital_id = $2`, yesterday.String(), e.h); n != 1 {
			t.Errorf("%s: %d ended entries, want 1", zone, n)
		}
		for _, c := range []struct {
			id   uuid.UUID
			days int
		}{{today, 1}, {soon, 7}} {
			if n := e.count(t, `SELECT count(*) FROM clearance_events WHERE clearance_id = $1 AND kind = 'reminded' AND days = $2`, c.id, c.days); n != 1 {
				t.Errorf("%s: %d reminders of %d days, want 1", zone, n, c.days)
			}
			if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'clearance.reminder_sent' AND entity_id = $1
				AND payload->>'delivery' = 'pending' AND (payload->>'days')::int = $2`, c.id.String(), c.days); n != 1 {
				t.Errorf("%s: %d reminder entries of %d days, want 1", zone, n, c.days)
			}
		}
		if n := e.count(t, `SELECT count(*) FROM clearance_events WHERE clearance_id = $1`, later); n != 0 {
			t.Errorf("%s: a letter two months from its end has %d events", zone, n)
		}
		// The routes show the ended letter as ended.
		_, b := e.get(t, "/hospital/clearances/"+yesterday.String())
		if !strings.Contains(string(b), `"status":"ended"`) {
			t.Errorf("%s: %s", zone, b)
		}
	}
}

// Security review (1): outside the demo every upload is the same 503,
// whatever the body holds; nothing is checked, counted or stored.
func TestNonDemoUploadIs503WhateverTheBody(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, false, "Africa/Lagos")
	ok := e.letterForm(e.admin)
	big := ok
	big.file = bytes.Repeat([]byte("x"), httpx.MaxBody+1)
	var first []byte
	for i, f := range []form{
		ok.withExtra([2]string{"covered_platform", "nobody@measured-health.example"}),
		ok.withExtra([2]string{"covered_platform", e.paEmail}),
		e.letterForm(e.clin),
		ok.with("end_date", "2027-02-30"),
		ok.without("mfa_code"),
		big,
	} {
		c, b := e.upload(t, f)
		if c != http.StatusServiceUnavailable || code(b) != "unavailable" {
			t.Errorf("body %d: %d %s", i, c, b)
		}
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		delete(p, "request_id")
		got, _ := json.Marshal(p)
		if first == nil {
			first = got
		} else if !bytes.Equal(first, got) {
			t.Errorf("body %d: %s differs from %s", i, got, first)
		}
	}
	if c, _, b := e.do(t, e.admin, http.MethodPost, "/hospital/clearances", "application/json", []byte(`{}`), false); c != 503 {
		t.Errorf("JSON without a key: %d %s", c, b)
	}
	if e.letters(t) != 0 || e.count(t, `SELECT count(*) FROM clearance_check_refusals WHERE hospital_id = $1`, e.h) != 0 {
		t.Error("a non-demo upload stored or counted something")
	}
}

// Security review (2): not_eligible_platform is the same reply whether the
// address is unknown, an inactive platform admin, one without two-step,
// an auditor or a hospital account; it names none of them.
func TestNotEligiblePlatformReplyIdentical(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true, "Africa/Lagos")
	tag := e.h.String()[:8]
	inactive := "inactive-" + tag + "@measured-health.example"
	run(t, super, `UPDATE users SET is_active = false WHERE id = $1`, user(t, super, nil, inactive, true, "platform_admin"))
	noMFA := "nomfa-" + tag + "@measured-health.example"
	user(t, super, nil, noMFA, false, "platform_admin")
	auditor := "auditor-" + tag + "@measured-health.example"
	user(t, super, nil, auditor, true, "auditor")
	var first []byte
	for _, email := range []string{"unknown-" + tag + "@measured-health.example", inactive, noMFA, auditor,
		"admin-" + tag + "@hospital.example.test"} {
		c, b := e.upload(t, e.letterForm(e.admin).withExtra([2]string{"covered_platform", email}))
		if c != http.StatusUnprocessableEntity || code(b) != "not_eligible_platform" || strings.Contains(string(b), email) ||
			strings.Contains(string(b), "@") {
			t.Errorf("%s: %d %s", email, c, b)
		}
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		delete(p, "request_id")
		got, _ := json.Marshal(p)
		if first == nil {
			first = got
		} else if !bytes.Equal(first, got) {
			t.Errorf("%s: %s differs from %s", email, got, first)
		}
	}
	if e.letters(t) != 0 {
		t.Error("a refused upload stored a letter")
	}
}

// Security review (3): a platform admin named on a letter is not shown to
// the hospital (no id, no name) until Measured Health has verified it.
func TestPlatformAdminShownOnlyOnceVerified(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true, "Africa/Lagos")
	run(t, super, `UPDATE users SET display_name = 'Synthetic Platform Zeta' WHERE id = $1`, e.pa)
	c, b := e.upload(t, e.letterForm(e.admin).withExtra([2]string{"covered_platform", e.paEmail}))
	if c != http.StatusCreated {
		t.Fatalf("upload: %d %s", c, b)
	}
	var l ClearanceOut
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", "/hospital/clearances", "/hospital/clearances/" + l.ID} {
		body := b
		if p != "" {
			_, body = e.get(t, p)
		}
		if strings.Contains(string(body), e.pa.String()) || strings.Contains(string(body), "Platform Zeta") {
			t.Errorf("%q shows the platform admin before verification: %s", p, body)
		}
	}
	// Once a platform admin has verified it, the letter names them.
	verifier := user(t, super, nil, "verifier-"+e.h.String()[:8]+"@measured-health.example", true, "platform_admin")
	run(t, super, `UPDATE clearances SET status = 'active', verified_by = $2, verified_at = now() WHERE id = $1`, l.ID, verifier)
	_, body := e.get(t, "/hospital/clearances/"+l.ID)
	if !strings.Contains(string(body), e.pa.String()) || !strings.Contains(string(body), "Platform Zeta") {
		t.Errorf("a verified letter doesn't name its platform admin: %s", body)
	}
}

// Security review (4): eligibility refusals are counted per person and
// hospital day; past RefusalsPerDay the check is 429, whatever the body,
// with one flagged entry.
func TestEligibilityRefusalsRateLimited(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true, "Africa/Lagos")
	for i := range RefusalsPerDay {
		f := e.letterForm(e.clin)
		if i%2 == 1 {
			f = e.letterForm(e.admin).withExtra([2]string{"covered_platform", fmt.Sprintf("guess-%d@measured-health.example", i)})
		}
		if c, b := e.upload(t, f); c != http.StatusUnprocessableEntity {
			t.Fatalf("refusal %d: %d %s", i, c, b)
		}
	}
	for _, f := range []form{e.letterForm(e.clin), e.letterForm(e.admin)} {
		if c, b := e.upload(t, f); c != http.StatusTooManyRequests || code(b) != "rate_limited" {
			t.Errorf("past the limit: %d %s", c, b)
		}
	}
	if e.letters(t) != 0 {
		t.Error("a letter was stored past the limit")
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'clearance.check_limit_reached' AND actor_id = $1
		AND hospital_id = $2`, e.admin, e.h); n != 1 {
		t.Errorf("%d limit entries, want 1", n)
	}
	// Another admin of the hospital is not limited.
	other := user(t, super, &e.h, "other-"+e.h.String()[:8]+"@hospital.example.test", true, "hospital_admin")
	body, ct := e.letterForm(other).encode(t)
	if c, _, b := e.do(t, other, http.MethodPost, "/hospital/clearances", ct, body, true); c != http.StatusCreated {
		t.Errorf("another admin: %d %s", c, b)
	}
}

// Security review (4, under concurrency): the limit is reserved before the
// check, atomically, so 20 parallel bad checks get at most RefusalsPerDay
// not-eligible answers; the rest are 429.
func TestEligibilityRefusalsLimitUnderRace(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true, "Africa/Lagos")
	const n = 20
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		f := e.letterForm(e.admin).withExtra([2]string{"covered_platform", fmt.Sprintf("race-%d@measured-health.example", i)})
		body, ct := f.encode(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodPost, httpx.BasePath+"/hospital/clearances", bytes.NewReader(body))
			r.Header.Set("Content-Type", ct)
			r.Header.Set("Idempotency-Key", uuid.NewString())
			w := httptest.NewRecorder()
			r.Body = http.MaxBytesReader(w, r.Body, httpx.MaxBody)
			ctx := authz.WithPrincipal(r.Context(), authz.Principal{UserID: e.admin, HospitalID: e.h,
				Roles: []string{authz.RoleHospitalAdmin}, MFA: true, SessionID: uuid.NewString()})
			e.mux.ServeHTTP(w, r.WithContext(ctx))
			codes[i] = w.Code
		}()
	}
	wg.Wait()
	answered, limited := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusUnprocessableEntity:
			answered++
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if answered > RefusalsPerDay || answered+limited != n {
		t.Errorf("%d not-eligible answers and %d limited of %d parallel checks, want at most %d answers",
			answered, limited, n, RefusalsPerDay)
	}
	if got := e.count(t, `SELECT refused FROM clearance_check_refusals WHERE hospital_id = $1 AND user_id = $2`, e.h, e.admin); got != n {
		t.Errorf("%d slots taken, want %d", got, n)
	}
}
