//go:build integration

package patients_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/patients"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// The patient lookup on a migrated database, through the handler with a
// verified principal in the context. Every name and MRN is synthetic, and
// each MRN is distinctive so a leak into a log, an audit payload or a
// reply is found by a plain substring search.

var sharedSuper, sharedApp *pgxpool.Pool

func TestMain(m *testing.M) {
	code := func() int {
		ctx := context.Background()
		pg, stop, err := testutil.NewPostgres(ctx)
		if err != nil {
			panic(err)
		}
		defer stop()
		if err := db.MigrateUp(ctx, pg.URL); err != nil {
			panic(err)
		}
		super, err := db.Open(ctx, pg.URL)
		if err != nil {
			panic(err)
		}
		defer super.Close()
		app, err := testutil.AppLogin(ctx, pg, super)
		if err != nil {
			panic(err)
		}
		defer app.Close()
		sharedSuper, sharedApp = super, app
		return m.Run()
	}()
	os.Exit(code)
}

const (
	patientName = "Synthetic Odalys Brennecke"
	patientMRN  = "SYN-LKP-7Q4X-M2"
)

// roomy limits, so only the limit tests meet one.
var roomy = patients.Limits{PerMinute: 100, PerDay: 1000, HospitalPerDay: 100000}

type world struct {
	t    *testing.T
	h    uuid.UUID
	u    uuid.UUID // a clinician of h
	logs *syncBuf
	d    patients.Deps
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := sharedSuper.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q[:min(len(q), 60)], err)
	}
}

func hospital(t *testing.T) uuid.UUID {
	t.Helper()
	h := uuid.New()
	exec(t, `INSERT INTO hospitals (id, name, short_code, status, time_zone) VALUES ($1, 'Synthetic Hospital', $2, 'active', 'America/Havana')`,
		h, "PL"+h.String()[:6])
	return h
}

func clinician(t *testing.T, h uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := sharedSuper.QueryRow(context.Background(), `INSERT INTO users (hospital_id, email, password_hash, display_name)
		VALUES ($1, $2, 'synthetic-hash', 'Synthetic Clinician') RETURNING id`, h, uuid.NewString()[:12]+"@example.test").Scan(&id); err != nil {
		t.Fatal(err)
	}
	exec(t, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'clinician')`, id)
	exec(t, `INSERT INTO user_mfa (user_id, secret_enc, enabled, backup_code_hashes) VALUES ($1, '\x00', true, ARRAY['a'])`, id)
	return id
}

func newWorld(t *testing.T, on bool, lim patients.Limits) *world {
	t.Helper()
	w := &world{t: t, logs: &syncBuf{}}
	w.h = hospital(t)
	w.u = clinician(t, w.h)
	w.d = patients.Deps{Pool: sharedApp, On: on, Limits: lim,
		Log: slog.New(slog.NewJSONHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	return w
}

// patient adds an MHAI patient of h with mrn and status (a merged patient
// is merged into into).
func patient(t *testing.T, h uuid.UUID, mrn, status string, into uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := sharedSuper.QueryRow(context.Background(), `INSERT INTO emr.patients (hospital_id, display_name, mrn, birth_date, sex)
		VALUES ($1, $2, $3, '1966-02-19', 'female') RETURNING id`, h, patientName, mrn).Scan(&id); err != nil {
		t.Fatal(err)
	}
	switch status {
	case "excluded":
		exec(t, `UPDATE emr.patients SET status = 'excluded', excluded_at = now(), exclude_reason = 'objection' WHERE id = $1`, id)
	case "merged_into":
		exec(t, `UPDATE emr.patients SET status = 'merged_into', merged_into = $2 WHERE id = $1`, id, into)
	}
	return id
}

type reply struct {
	code int
	body map[string]any
	raw  string
	hdr  http.Header
}

// as looks mrn up as user u of hospital h (a clinician with two-step).
func (w *world) as(h, u uuid.UUID, body string) reply {
	w.t.Helper()
	return serve(w.d, h, u, "", body)
}

func serve(d patients.Deps, h, u uuid.UUID, query, body string) reply {
	r := httptest.NewRequest(http.MethodPost, httpx.BasePath+"/patients/lookup"+query, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	ctx := authz.WithPrincipal(r.Context(), authz.Principal{UserID: u, HospitalID: h, Roles: []string{authz.RoleClinician},
		MFA: true, SessionID: "s-" + u.String()})
	rec := httptest.NewRecorder()
	patients.Handler(d).ServeHTTP(rec, r.WithContext(ctx))
	out := reply{code: rec.Code, raw: rec.Body.String(), hdr: rec.Header()}
	_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
	return out
}

func mrnBody(mrn string) string {
	b, _ := json.Marshal(map[string]string{"mrn": mrn})
	return string(b)
}

func (w *world) lookup(mrn string) reply {
	w.t.Helper()
	return w.as(w.h, w.u, mrnBody(mrn))
}

type auditRow struct {
	actor, entity, entityID string
	payload                 map[string]any
}

func audits(t *testing.T, h uuid.UUID, action string) []auditRow {
	t.Helper()
	rows, err := sharedSuper.Query(context.Background(), `SELECT coalesce(actor_id::text, ''), coalesce(entity, ''),
		coalesce(entity_id, ''), payload FROM audit_log WHERE hospital_id = $1 AND action = $2 ORDER BY id`, h, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var a auditRow
		var raw []byte
		if err := rows.Scan(&a.actor, &a.entity, &a.entityID, &raw); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(raw, &a.payload)
		out = append(out, a)
	}
	return out
}

// noLeak: the hospital's audit rows and the logs never hold the patient's
// name or any of the MRNs typed.
func (w *world) noLeak(mrns ...string) {
	w.t.Helper()
	var all string
	if err := sharedSuper.QueryRow(context.Background(), `SELECT coalesce(string_agg(payload::text || coalesce(entity_id, ''), ' '), '')
		FROM audit_log WHERE hospital_id = $1`, w.h).Scan(&all); err != nil {
		w.t.Fatal(err)
	}
	logs := w.logs.String()
	for _, s := range append([]string{patientName}, mrns...) {
		for _, v := range []string{s, strings.ToUpper(s), strings.ToLower(s)} {
			if strings.Contains(all, v) {
				w.t.Errorf("the audit chain holds %q", v)
			}
			if strings.Contains(logs, v) {
				w.t.Errorf("the logs hold %q", v)
			}
		}
	}
}

// --- The lookup ---------------------------------------------------------

// A hit: 200 with the patient's id, name, MRN, age and sex only, never
// stored by a cache; the MRN as typed is normalised first (spaces, case).
// Audited patients.lookup {patient_id}, entity the patient;
// the MRN and the name in neither the chain nor the logs.
func TestLookupHit(t *testing.T) {
	w := newWorld(t, true, roomy)
	pid := patient(t, w.h, patientMRN, "active", uuid.Nil)
	r := w.lookup("  syn-lkp-7q4x-m2 ")
	if r.code != http.StatusOK {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	if r.hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q", r.hdr.Get("Cache-Control"))
	}
	if len(r.body) != 5 || r.body["id"] != pid.String() || r.body["full_name"] != patientName || r.body["mrn"] != patientMRN ||
		r.body["sex"] != "female" {
		t.Fatalf("%s", r.raw)
	}
	if age, ok := r.body["age"].(float64); !ok || age < 59 || age > 61 {
		t.Fatalf("age %v", r.body["age"])
	}
	got := audits(t, w.h, "patients.lookup")
	if len(got) != 1 || got[0].actor != w.u.String() || got[0].entity != "patient" || got[0].entityID != pid.String() ||
		len(got[0].payload) != 1 || got[0].payload["patient_id"] != pid.String() {
		t.Fatalf("%+v", got)
	}
	w.noLeak(patientMRN, "syn-lkp-7q4x-m2")
}

// A miss, another hospital's patient, an excluded patient and a merged
// patient are the same 404 not_found (only the request id differs), each
// audited as a miss with no patient.
func TestLookupMissesLookAlike(t *testing.T) {
	w := newWorld(t, true, roomy)
	other := hospital(t)
	patient(t, other, "SYN-LKP-OTHR-9K3", "active", uuid.Nil)
	kept := patient(t, w.h, "SYN-LKP-KEPT-5V8", "active", uuid.Nil)
	patient(t, w.h, "SYN-LKP-GONE-2H6", "merged_into", kept)
	// An exclusion clears the MRN (emr_exclude_patient); one set by hand
	// must not be found either.
	patient(t, w.h, "SYN-LKP-EXCL-4T1", "excluded", uuid.Nil)
	var bodies []string
	for _, mrn := range []string{"SYN-LKP-NONE-0Z0", "SYN-LKP-OTHR-9K3", "SYN-LKP-GONE-2H6", "SYN-LKP-EXCL-4T1"} {
		r := w.lookup(mrn)
		if r.code != http.StatusNotFound || r.body["code"] != "not_found" {
			t.Fatalf("%s: %d %s", mrn, r.code, r.raw)
		}
		delete(r.body, "request_id")
		b, _ := json.Marshal(r.body)
		bodies = append(bodies, string(b))
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Fatalf("the refusals differ: %v", bodies)
		}
	}
	got := audits(t, w.h, "patients.lookup")
	if len(got) != 4 {
		t.Fatalf("%d entries", len(got))
	}
	for _, a := range got {
		if a.payload["result"] != "miss" || len(a.payload) != 1 || a.entityID != "" {
			t.Fatalf("%+v", a)
		}
	}
	if n := len(audits(t, other, "patients.lookup")); n != 0 {
		t.Fatalf("the other hospital has %d entries", n)
	}
	w.noLeak("SYN-LKP-NONE-0Z0", "SYN-LKP-OTHR-9K3", "SYN-LKP-GONE-2H6", "SYN-LKP-EXCL-4T1")
}

// Exact match only: a prefix, a substring, a longer MRN, one with its
// leading zeros dropped or added, and SQL wildcards all miss.
func TestLookupExactOnly(t *testing.T) {
	w := newWorld(t, true, roomy)
	patient(t, w.h, "0072-SYN-LKP-EXACT", "active", uuid.Nil)
	for _, mrn := range []string{"0072-SYN-LKP", "SYN-LKP-EXACT", "72-SYN-LKP-EXACT", "00072-SYN-LKP-EXACT",
		"0072-SYN-LKP-EXACT1", "0072-SYN-LKP-EXAC_", "0072-SYN-LKP-%", "%", "_", ".*", "0072-SYN-LKP-EXACT'--"} {
		if r := w.lookup(mrn); r.code != http.StatusNotFound {
			t.Errorf("%q: %d %s", mrn, r.code, r.raw)
		}
	}
	if r := w.lookup("0072 syn lkp exact"); r.code != http.StatusNotFound {
		t.Errorf("spaces inside are removed, the dashes are not: %d", r.code)
	}
	if r := w.lookup("0072-syn-lkp-exact"); r.code != http.StatusOK {
		t.Errorf("the exact MRN, lower case: %d %s", r.code, r.raw)
	}
}

// Two active patients with one MRN: 409 conflict, never a list; audited
// ambiguous with no patient.
func TestLookupAmbiguous409(t *testing.T) {
	w := newWorld(t, true, roomy)
	patient(t, w.h, "SYN-LKP-TWIN-6R2", "active", uuid.Nil)
	patient(t, w.h, "SYN-LKP-TWIN-6R2", "active", uuid.Nil)
	r := w.lookup("SYN-LKP-TWIN-6R2")
	if r.code != http.StatusConflict || r.body["code"] != "conflict" || strings.Contains(r.raw, patientName) {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	got := audits(t, w.h, "patients.lookup")
	if len(got) != 1 || got[0].payload["result"] != "ambiguous" || len(got[0].payload) != 1 || got[0].entityID != "" {
		t.Fatalf("%+v", got)
	}
}

// With EMR off every lookup is 404 not_found, even a known MRN, and the
// database isn't asked: no count, no audit.
func TestLookupEMROff404(t *testing.T) {
	w := newWorld(t, false, roomy)
	patient(t, w.h, "SYN-LKP-OFF-3J7", "active", uuid.Nil)
	if r := w.lookup("SYN-LKP-OFF-3J7"); r.code != http.StatusNotFound || r.body["code"] != "not_found" {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	w.nothingCounted()
}

func (w *world) nothingCounted() {
	w.t.Helper()
	var counts, entries int
	if err := sharedSuper.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM patient_lookup_counts WHERE hospital_id = $1)
		+ (SELECT count(*) FROM patient_lookup_hospital_counts WHERE hospital_id = $1),
		(SELECT count(*) FROM audit_log WHERE hospital_id = $1)`, w.h).Scan(&counts, &entries); err != nil {
		w.t.Fatal(err)
	}
	if counts != 0 || entries != 0 {
		w.t.Fatalf("%d count rows, %d audit entries", counts, entries)
	}
}

// A body that isn't exactly {mrn}, an MRN that can't be one, or anything
// in the query string is 422 validation_failed before anything is
// counted or audited.
func TestLookupValidation422(t *testing.T) {
	w := newWorld(t, true, roomy)
	for _, body := range []string{``, `{}`, `{"mrn": null}`, `{"mrn": 7}`, `{"mrn": ""}`, `{"mrn": "   "}`,
		`{"mrn": "` + strings.Repeat("Q", 65) + `"}`, `{"mrn": "SYN\u0000LKP"}`, `{"mrn": "SYN\u0007LKP"}`,
		`{"mrn": "SYN-LKP-7Q4X-M2", "hospital_id": "` + uuid.NewString() + `"}`, `{"name": "Synthetic"}`,
		`{"mrn": "SYN-LKP-7Q4X-M2"} {}`, `["SYN-LKP-7Q4X-M2"]`} {
		if r := w.as(w.h, w.u, body); r.code != http.StatusUnprocessableEntity || r.body["code"] != "validation_failed" {
			t.Errorf("%s: %d %s", body, r.code, r.raw)
		}
	}
	if r := serve(w.d, w.h, w.u, "?mrn=SYN-LKP-7Q4X-M2", mrnBody("SYN-LKP-7Q4X-M2")); r.code != http.StatusUnprocessableEntity {
		t.Errorf("a query string: %d", r.code)
	}
	w.nothingCounted()
}

// The caller as they are now, not as the guard's cache saw them: a
// deactivated clinician, a deactivated hospital, or a clinician who lost the
// role is refused (401 unauthenticated, 403 forbidden), and nothing is
// audited as a lookup.
func TestLookupRechecksTheCaller(t *testing.T) {
	w := newWorld(t, true, roomy)
	patient(t, w.h, "SYN-LKP-CALL-8P5", "active", uuid.Nil)
	gone := clinician(t, w.h)
	exec(t, `UPDATE users SET is_active = false WHERE id = $1`, gone)
	if r := w.as(w.h, gone, mrnBody("SYN-LKP-CALL-8P5")); r.code != http.StatusUnauthorized {
		t.Errorf("deactivated: %d %s", r.code, r.raw)
	}
	nurse := clinician(t, w.h)
	exec(t, `DELETE FROM user_roles WHERE user_id = $1`, nurse)
	if r := w.as(w.h, nurse, mrnBody("SYN-LKP-CALL-8P5")); r.code != http.StatusForbidden {
		t.Errorf("no longer a clinician: %d %s", r.code, r.raw)
	}
	exec(t, `UPDATE hospitals SET status = 'deactivated' WHERE id = $1`, w.h)
	if r := w.lookup("SYN-LKP-CALL-8P5"); r.code != http.StatusUnauthorized {
		t.Errorf("deactivated hospital: %d %s", r.code, r.raw)
	}
	if n := len(audits(t, w.h, "patients.lookup")); n != 0 {
		t.Fatalf("%d lookups audited", n)
	}
}

// --- The limits ----------------------------------------------------------

// About 20 parallel lookups against a limit of 5 a minute: exactly 5 run
// (hits and misses alike), the rest are 429 rate_limited with
// Retry-After, and the clinician is flagged once (one
// patients.lookup_limit_reached entry), however many refusals follow.
func TestLookupMinuteLimitParallel(t *testing.T) {
	w := newWorld(t, true, patients.Limits{PerMinute: 5, PerDay: 1000, HospitalPerDay: 100000})
	patient(t, w.h, "SYN-LKP-PAR-1A1", "active", uuid.Nil)
	const n = 20
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			mrn := "SYN-LKP-PAR-1A1"
			if i%2 == 1 {
				mrn = "SYN-LKP-PAR-MISS"
			}
			r := serve(w.d, w.h, w.u, "", mrnBody(mrn))
			codes[i] = r.code
			if r.code == http.StatusTooManyRequests && (r.body["code"] != "rate_limited" || r.hdr.Get("Retry-After") != "60") {
				t.Errorf("429: %s %q", r.raw, r.hdr.Get("Retry-After"))
			}
		}()
	}
	close(start)
	wg.Wait()
	ran, refused := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK, http.StatusNotFound:
			ran++
		case http.StatusTooManyRequests:
			refused++
		default:
			t.Errorf("status %d", c)
		}
	}
	if ran != 5 || refused != n-5 {
		t.Fatalf("%d ran, %d refused: %v", ran, refused, codes)
	}
	if got := len(audits(t, w.h, "patients.lookup")); got != 5 {
		t.Fatalf("%d lookups audited, want 5", got)
	}
	if r := w.lookup("SYN-LKP-PAR-1A1"); r.code != http.StatusTooManyRequests {
		t.Fatalf("after: %d", r.code)
	}
	flags := audits(t, w.h, "patients.lookup_limit_reached")
	if len(flags) != 1 || flags[0].payload["window"] != "minute" || flags[0].entity != "user" || flags[0].entityID != w.u.String() ||
		flags[0].actor != w.u.String() {
		t.Fatalf("%+v", flags)
	}
	w.noLeak("SYN-LKP-PAR-1A1", "SYN-LKP-PAR-MISS")
}

// The day's limit counts misses: three misses, then even a hit is 429
// (no Retry-After: it lifts at the hospital's midnight), flagged once.
// Another clinician of the hospital is not limited by it.
func TestLookupDayLimitCountsMisses(t *testing.T) {
	w := newWorld(t, true, patients.Limits{PerMinute: 100, PerDay: 3, HospitalPerDay: 100000})
	patient(t, w.h, "SYN-LKP-DAY-7W3", "active", uuid.Nil)
	for _, mrn := range []string{"SYN-LKP-DAY-X01", "SYN-LKP-DAY-X02", "SYN-LKP-DAY-X03"} {
		if r := w.lookup(mrn); r.code != http.StatusNotFound {
			t.Fatalf("%s: %d", mrn, r.code)
		}
	}
	for range 3 {
		r := w.lookup("SYN-LKP-DAY-7W3")
		if r.code != http.StatusTooManyRequests || r.hdr.Get("Retry-After") != "" {
			t.Fatalf("%d %s %q", r.code, r.raw, r.hdr.Get("Retry-After"))
		}
	}
	flags := audits(t, w.h, "patients.lookup_limit_reached")
	if len(flags) != 1 || flags[0].payload["window"] != "day" {
		t.Fatalf("%+v", flags)
	}
	if r := w.as(w.h, clinician(t, w.h), mrnBody("SYN-LKP-DAY-7W3")); r.code != http.StatusOK {
		t.Fatalf("another clinician: %d", r.code)
	}
}

// The hospital's limit: lookups by any of its clinicians count; past it a
// clinician who has made none is refused (window hospital_day). Another
// hospital's clinicians are not.
func TestLookupHospitalLimit(t *testing.T) {
	w := newWorld(t, true, patients.Limits{PerMinute: 100, PerDay: 1000, HospitalPerDay: 2})
	v := clinician(t, w.h)
	if r := w.lookup("SYN-LKP-HOS-A01"); r.code != http.StatusNotFound {
		t.Fatal(r.code)
	}
	if r := w.as(w.h, v, mrnBody("SYN-LKP-HOS-A02")); r.code != http.StatusNotFound {
		t.Fatal(r.code)
	}
	third := clinician(t, w.h)
	if r := w.as(w.h, third, mrnBody("SYN-LKP-HOS-A03")); r.code != http.StatusTooManyRequests {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	flags := audits(t, w.h, "patients.lookup_limit_reached")
	if len(flags) != 1 || flags[0].payload["window"] != "hospital_day" || flags[0].entityID != third.String() {
		t.Fatalf("%+v", flags)
	}
	other := hospital(t)
	if r := w.as(other, clinician(t, other), mrnBody("SYN-LKP-HOS-A04")); r.code != http.StatusNotFound {
		t.Fatalf("another hospital: %d", r.code)
	}
}

// Limits that can't be (a wiring fault) fail closed: 503 not_ready, and
// the patient is not looked up.
func TestLookupFailsClosed(t *testing.T) {
	w := newWorld(t, true, patients.Limits{})
	patient(t, w.h, "SYN-LKP-SHUT-5N5", "active", uuid.Nil)
	if r := w.lookup("SYN-LKP-SHUT-5N5"); r.code != http.StatusServiceUnavailable || strings.Contains(r.raw, patientName) {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	w.nothingCounted()
}

// --- The database ----------------------------------------------------------

// The counts are reached only through the function: core's role reads and
// writes neither table, the function refuses a caller with no user, and
// refuses limits outside its bounds. The owner's rows of one hospital are
// invisible from another (RLS).
func TestLookupCountsOnlyThroughTheFunction(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, true, roomy)
	if r := w.lookup("SYN-LKP-DB-6Y6"); r.code != http.StatusNotFound {
		t.Fatal(r.code)
	}
	for _, q := range []string{`SELECT count(*) FROM patient_lookup_counts`, `SELECT count(*) FROM patient_lookup_hospital_counts`,
		`UPDATE patient_lookup_counts SET day_count = 0`, `DELETE FROM patient_lookup_hospital_counts`} {
		err := db.WithHospitalUser(ctx, sharedApp, w.h, w.u, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: %v", q, err)
		}
	}
	err := db.WithHospital(ctx, sharedApp, w.h, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT mhai_patient_lookup_reserve(5, 50, 1000, 'UTC')`)
		return err
	})
	if err == nil {
		t.Error("reserved with no user")
	}
	for _, args := range [][]any{{0, 50, 1000, "UTC"}, {101, 50, 1000, "UTC"}, {5, 0, 1000, "UTC"}, {5, 1001, 1000, "UTC"},
		{5, 50, 0, "UTC"}, {5, 50, 100001, "UTC"}, {5, 50, 1000, "Not/AZone"}, {5, 50, 1000, nil}} {
		err := db.WithHospitalUser(ctx, sharedApp, w.h, w.u, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT mhai_patient_lookup_reserve($1, $2, $3, $4)`, args...)
			return err
		})
		if err == nil {
			t.Errorf("%v: reserved", args)
		}
	}
	var day int
	if err := sharedSuper.QueryRow(ctx, `SELECT day_count FROM patient_lookup_counts WHERE hospital_id = $1 AND user_id = $2`,
		w.h, w.u).Scan(&day); err != nil || day != 1 {
		t.Fatalf("day_count %d %v: the refused calls took nothing", day, err)
	}
}
