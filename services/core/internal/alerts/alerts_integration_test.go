//go:build integration

package alerts_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/alerts"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// The alert routes on a migrated database, through the handlers with a
// verified principal in the context (the guard's role and two-step checks
// are authz's, pinned by TestAlertRouteRules). Every name, MRN and value
// is synthetic.

var (
	sharedSuper, sharedApp *pgxpool.Pool
)

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

var masterKey = bytes.Repeat([]byte{7}, 32)

const canaryNote = "SYNTHETIC-CANARY-NOTE-7d1f"

type world struct {
	t            *testing.T
	h, conn      uuid.UUID
	admin        uuid.UUID
	u, v         uuid.UUID // clinicians
	patient, obs uuid.UUID
	finding      uuid.UUID
	logs         *bytes.Buffer
	mux          *http.ServeMux
}

func exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := sharedSuper.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q[:min(len(q), 60)], err)
	}
}

func user(t *testing.T, h uuid.UUID, roles ...string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := sharedSuper.QueryRow(context.Background(), `INSERT INTO users (hospital_id, email, password_hash, display_name, specialty)
		VALUES ($1, $2, 'synthetic-hash', 'Synthetic Clinician', 'Synthetic medicine') RETURNING id`,
		h, uuid.NewString()[:12]+"@example.test").Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		exec(t, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, id, r)
	}
	return id
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, h: uuid.New(), conn: uuid.New(), logs: &bytes.Buffer{}}
	exec(t, `INSERT INTO hospitals (id, name, short_code, status) VALUES ($1, 'Synthetic Hospital', $2, 'active')`,
		w.h, "AL"+w.h.String()[:6])
	exec(t, `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url, settings, enabled, poll_seconds)
		VALUES ($1, $2, 'fhir_r4', 'Main record', 'https://fhir.example.org/r4',
		        '{"record_link": "https://fhir.example.org/chart/{id}"}', true, 120)`, w.conn, w.h)
	exec(t, `INSERT INTO emr.sync_runs (hospital_id, connection_id, started_at, finished_at, ok, records, outcome)
		VALUES ($1, $2, now(), now(), true, 1, 'ok')`, w.h, w.conn)
	w.admin = user(t, w.h, "hospital_admin")
	w.u = user(t, w.h, "clinician")
	w.v = user(t, w.h, "clinician")
	w.patient, w.obs, w.finding = w.newPatient(t, "src-1", "SYN-0001")
	log := slog.New(slog.NewJSONHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := alerts.WithStepUpCheck(alerts.Deps{Pool: sharedApp, Log: log, MasterKey: masterKey},
		func(_ *http.Request, code, _ string) error {
			if code != "123456" {
				return errors.New("wrong code")
			}
			return nil
		})
	w.mux = http.NewServeMux()
	for k, h := range alerts.Handlers(d) {
		method, path, _ := strings.Cut(k, " ")
		w.mux.Handle(method+" "+httpx.BasePath+path, h)
	}
	return w
}

// newPatient makes a patient linked to the connection, with one potassium
// observation and an open critical finding on it.
func (w *world) newPatient(t *testing.T, src, mrn string) (patient, obs, finding uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := sharedSuper.QueryRow(ctx, `INSERT INTO emr.patients (hospital_id, display_name, mrn, birth_date, sex)
		VALUES ($1, 'Synthetic Patient', $2, '1970-01-01', 'female') RETURNING id`, w.h, mrn).Scan(&patient); err != nil {
		t.Fatal(err)
	}
	exec(t, `INSERT INTO emr.patient_links (hospital_id, connection_id, source_patient_id, patient_id) VALUES ($1, $2, $3, $4)`,
		w.h, w.conn, src, patient)
	sum := sha256.Sum256([]byte(src))
	var raw uuid.UUID
	if err := sharedSuper.QueryRow(ctx, `INSERT INTO emr.raw_records (hospital_id, source_kind, connection_id, resource_kind,
		source_id, source_version, payload, payload_sha256, connector_type, connector_version)
		VALUES ($1, 'connector', $2, 'observation', $3, '1', '{}', $4, 'fhir_r4', 1) RETURNING id`,
		w.h, w.conn, "obs-"+src, sum[:]).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := sharedSuper.QueryRow(ctx, `INSERT INTO emr.observations (hospital_id, patient_id, category, loinc, local_display,
		value_num, unit_ucum, value_si, ref_low, ref_high, source_flag, status, effective_at, standardised, raw_id, source_kind,
		connection_id, connector_type, connector_version, source_id, source_version, received_at, mapping_version,
		normaliser_version)
		VALUES ($1, $2, 'laboratory', '99901-1', 'Potassium', 6.8, 'mmol/L', 6.8, 3.5, 5.1, 'HH', 'final', now(), true, $3,
		        'connector', $4, 'fhir_r4', 1, $5, '1', now(), 'synthetic-map', 'n1') RETURNING id`,
		w.h, patient, raw, w.conn, "obs-"+src).Scan(&obs); err != nil {
		t.Fatal(err)
	}
	causes := `[{"id": "c1", "label": "Synthetic cause", "confidence": "caution", "observation_ids": [], "medication_ids": []},
		{"id": "c2", "label": "Never shown", "confidence": "high", "observation_ids": [], "medication_ids": []}]`
	if err := sharedSuper.QueryRow(ctx, `INSERT INTO emr.findings (hospital_id, patient_id, connection_id, rule_set_version,
		rule_set_sha256, rule_id, kind, category, severity, primary_observation_id, causes, effective_at, received_at, status,
		shadow, preliminary, arrived_late, standardised, dedup_key)
		VALUES ($1, $2, $3, '1', $4, 'potassium-critical-high', 'threshold', 'blood_chemistry', 'critical', $5, $6::jsonb,
		        now(), now(), 'open', false, false, false, true, $7) RETURNING id`,
		w.h, patient, w.conn, sum[:], obs, causes, "k:"+obs.String()).Scan(&finding); err != nil {
		t.Fatal(err)
	}
	return patient, obs, finding
}

// alert inserts a new alert of the world's patient (rule distinguishes
// alerts), on the queue or not, delivered to each of to.
func (w *world) alert(rule string, queue bool, to ...uuid.UUID) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := sharedSuper.QueryRow(context.Background(), `INSERT INTO alerts (hospital_id, finding_id, patient_id, connection_id,
		rule_id, loinc, category, severity, routed_queue) VALUES ($1, $2, $3, $4, $5, '99901-1', 'blood_chemistry', 'critical', $6)
		RETURNING id`, w.h, w.finding, w.patient, w.conn, rule, queue).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	for _, u := range to {
		exec(w.t, `INSERT INTO alert_deliveries (alert_id, hospital_id, user_id) VALUES ($1, $2, $3)`, id, w.h, u)
	}
	return id
}

// as serves a request as user u of hospital h (roles as given).
func (w *world) as(h, u uuid.UUID, roles []string, method, path, body string) (int, map[string]any) {
	w.t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, httpx.BasePath+path, rd)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	ctx := authz.WithPrincipal(r.Context(), authz.Principal{UserID: u, HospitalID: h, Roles: roles, MFA: true,
		SessionID: "s-" + u.String()})
	rec := httptest.NewRecorder()
	w.mux.ServeHTTP(rec, r.WithContext(ctx))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (w *world) clin(u uuid.UUID, method, path, body string) (int, map[string]any) {
	return w.as(w.h, u, []string{authz.RoleClinician}, method, path, body)
}

func (w *world) adm(method, path, body string) (int, map[string]any) {
	return w.as(w.h, w.admin, []string{authz.RoleHospitalAdmin}, method, path, body)
}

func ids(page map[string]any) []string {
	var out []string
	list, _ := page["alerts"].([]any)
	for _, a := range list {
		out = append(out, a.(map[string]any)["id"].(string))
	}
	return out
}

func auditCount(t *testing.T, h uuid.UUID, action string) int {
	t.Helper()
	var n int
	if err := sharedSuper.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE hospital_id = $1 AND action = $2`,
		h, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func auditPayloads(t *testing.T, h uuid.UUID) string {
	t.Helper()
	var s string
	if err := sharedSuper.QueryRow(context.Background(), `SELECT coalesce(string_agg(payload::text, ' '), '') FROM audit_log
		WHERE hospital_id = $1`, h).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// --- Settings (Task 32) ----------------------------------------------------

// No connection: not_connected, every default filled in, version 0.
func TestNotConnected(t *testing.T) {
	w := newWorld(t)
	other := uuid.New()
	exec(t, `INSERT INTO hospitals (id, name, short_code, status) VALUES ($1, 'Synthetic Empty', $2, 'active')`, other, "AE"+other.String()[:6])
	admin := user(t, other, "hospital_admin")
	code, out := w.as(other, admin, []string{authz.RoleHospitalAdmin}, "GET", "/hospital/alert-settings", "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	conn := out["connection"].(map[string]any)
	if conn["status"] != "not_connected" || len(conn["sources"].([]any)) != 0 || out["version"] != float64(0) {
		t.Fatalf("settings %v", out)
	}
	rec := out["recipients"].(map[string]any)
	if rec["queue"] != true || len(out["categories"].([]any)) != 8 {
		t.Fatalf("defaults %v", out)
	}
	for _, c := range out["categories"].([]any) {
		c := c.(map[string]any)
		want := "critical_urgent"
		if c["id"] == "vital_signs" {
			want = "critical"
		}
		if c["level"] != want {
			t.Errorf("%v level %v", c["id"], c["level"])
		}
	}
	// A connected hospital: status only, by label; no address or type.
	code, out = w.adm("GET", "/hospital/alert-settings", "")
	b, _ := json.Marshal(out)
	if code != 200 || out["connection"].(map[string]any)["status"] != "connected" || strings.Contains(string(b), "fhir") {
		t.Fatalf("%d %s", code, b)
	}
}

// A15: a recipient of another hospital, inactive, or not a clinician is
// 422, and nothing is saved.
func TestRecipientsOtherHospitalInactiveOrNonClinician422(t *testing.T) {
	w := newWorld(t)
	other := newWorld(t)
	inactive := user(t, w.h, "clinician")
	exec(t, `UPDATE users SET is_active = false WHERE id = $1`, inactive)
	for name, id := range map[string]uuid.UUID{"other hospital": other.u, "inactive": inactive, "admin": w.admin,
		"unknown": uuid.New()} {
		code, out := w.adm("PATCH", "/hospital/alert-settings",
			`{"version": 0, "recipients": {"queue": true, "clinician_ids": ["`+id.String()+`"]}}`)
		if code != 422 || out["code"] != "validation_failed" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if n := auditCount(t, w.h, "alert_settings.recipients_changed"); n != 0 {
		t.Fatalf("%d saves audited", n)
	}
	code, out := w.adm("PATCH", "/hospital/alert-settings",
		`{"version": 0, "recipients": {"queue": false, "clinician_ids": ["`+w.u.String()+`"]}}`)
	if code != 200 || out["version"] != float64(1) {
		t.Fatalf("%d %v", code, out)
	}
	cl := out["recipients"].(map[string]any)["clinicians"].([]any)
	if len(cl) != 1 || cl[0].(map[string]any)["id"] != w.u.String() {
		t.Fatalf("clinicians %v", cl)
	}
}

func TestNoRecipients422(t *testing.T) {
	w := newWorld(t)
	code, out := w.adm("PATCH", "/hospital/alert-settings", `{"version": 0, "recipients": {"queue": false, "clinician_ids": []}}`)
	if code != 422 || out["code"] != "no_recipients" {
		t.Fatalf("%d %v", code, out)
	}
}

func TestSettingsVersionConflict(t *testing.T) {
	w := newWorld(t)
	body := `{"version": 0, "rules": {"levels": {"blood_count": "all"}}}`
	if code, _ := w.adm("PATCH", "/hospital/alert-settings", body); code != 200 {
		t.Fatal(code)
	}
	if code, out := w.adm("PATCH", "/hospital/alert-settings", body); code != 409 || out["code"] != "conflict" {
		t.Fatalf("stale version: %d %v", code, out)
	}
	for _, bad := range []string{
		`{"version": 1, "rules": {"levels": {"blood_count": "all"}}, "units": {"covered": []}}`,
		`{"version": 1, "rules": {"levels": {"nonsense": "all"}}}`,
		`{"version": 1, "rules": {"levels": {"blood_count": "loud"}}}`,
		`{"version": 1, "units": {"covered": ["` + uuid.NewString() + `"]}}`,
		`{"rules": {"levels": {"blood_count": "all"}}}`,
		`{"version": 1, "rules": {"levels": {}}, "extra": 1}`,
	} {
		if code, out := w.adm("PATCH", "/hospital/alert-settings", bad); code != 422 {
			t.Errorf("%s: %d %v", bad, code, out)
		}
	}
}

// A21: a settings save is audited with counts only: no name, id list or
// level value.
func TestSettingsAuditCountsOnly(t *testing.T) {
	w := newWorld(t)
	var unit uuid.UUID
	if err := sharedSuper.QueryRow(context.Background(), `INSERT INTO emr.units (hospital_id, connection_id, source_location_id,
		label, kind) VALUES ($1, $2, 'w3', 'Synthetic Ward 3', 'ward') RETURNING id`, w.h, w.conn).Scan(&unit); err != nil {
		t.Fatal(err)
	}
	steps := []string{
		`{"version": 0, "units": {"covered": []}}`,
		`{"version": 1, "rules": {"levels": {"vital_signs": "all", "blood_count": "critical_urgent"}}}`,
		`{"version": 2, "recipients": {"queue": true, "clinician_ids": ["` + w.u.String() + `"]}}`,
	}
	for _, s := range steps {
		if code, out := w.adm("PATCH", "/hospital/alert-settings", s); code != 200 {
			t.Fatalf("%s: %d %v", s, code, out)
		}
	}
	_, out := w.adm("GET", "/hospital/alert-settings", "")
	units := out["units"].([]any)
	if len(units) != 1 || units[0].(map[string]any)["covered"] != false || units[0].(map[string]any)["source_label"] != "Main record" {
		t.Fatalf("units %v", units)
	}
	var payloads []string
	rows, err := sharedSuper.Query(context.Background(), `SELECT action, payload::text FROM audit_log WHERE hospital_id = $1
		AND action LIKE 'alert_settings.%' ORDER BY id`, w.h)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a, p string
		if err := rows.Scan(&a, &p); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, a+" "+p)
	}
	if len(payloads) != 3 {
		t.Fatalf("audit rows %v", payloads)
	}
	all := strings.Join(payloads, "\n")
	for _, leak := range []string{w.u.String(), unit.String(), "Synthetic", "vital_signs", "critical_urgent"} {
		if strings.Contains(all, leak) {
			t.Errorf("an audit payload carries %q: %s", leak, all)
		}
	}
	if !strings.Contains(all, `"levels_changed": 1`) && !strings.Contains(all, `"levels_changed":1`) {
		t.Errorf("levels_changed: %s", all)
	}
}

// Design §12.2: taking a clinician off the named list moves their open
// alerts (sent to nobody else) to the queue, with an alert.requeued fact.
func TestRecipientRemovedRequeues(t *testing.T) {
	w := newWorld(t)
	if code, _ := w.adm("PATCH", "/hospital/alert-settings",
		`{"version": 0, "recipients": {"queue": false, "clinician_ids": ["`+w.u.String()+`"]}}`); code != 200 {
		t.Fatal(code)
	}
	a := w.alert("r1", false, w.u)
	code, out := w.adm("PATCH", "/hospital/alert-settings",
		`{"version": 1, "recipients": {"queue": false, "clinician_ids": ["`+w.v.String()+`"]}}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	var q bool
	if err := sharedSuper.QueryRow(context.Background(), `SELECT routed_queue FROM alerts WHERE id = $1`, a).Scan(&q); err != nil || !q {
		t.Fatalf("not re-queued: %v %v", q, err)
	}
	var n int
	if err := sharedSuper.QueryRow(context.Background(), `SELECT count(*) FROM intelligence_audit_outbox WHERE hospital_id = $1
		AND action = 'alert.requeued' AND payload ->> 'reason' = 'recipient_removed'`, w.h).Scan(&n); err != nil || n != 1 {
		t.Fatalf("facts %d %v", n, err)
	}
}

// --- Alerts (Task 33) ------------------------------------------------------

// A14: another hospital's alert is 404 everywhere, and never listed.
func TestOtherHospitalAlert404(t *testing.T) {
	w, other := newWorld(t), newWorld(t)
	a := other.alert("r1", true)
	for _, p := range []string{"/alerts/" + a.String() + "/acknowledge", "/alerts/" + a.String() + "/dismiss"} {
		body := `{"reason": "other"}`
		if strings.HasSuffix(p, "/acknowledge") {
			body = ""
		}
		if code, out := w.clin(w.u, "POST", p, body); code != 404 {
			t.Errorf("%s: %d %v", p, code, out)
		}
	}
	if _, page := w.clin(w.u, "GET", "/alerts", ""); len(ids(page)) != 0 {
		t.Fatalf("listed: %v", page)
	}
	if _, c := w.clin(w.u, "GET", "/alerts/unread-count", ""); c["count"] != float64(0) {
		t.Fatalf("count %v", c)
	}
}

// A17: an alert sent to another clinician by name (not on the queue) is
// invisible to this one: not listed, not counted, 404 to handle.
func TestNotRoutedAlert404ListCountAck(t *testing.T) {
	w := newWorld(t)
	a := w.alert("r1", false, w.v)
	if _, page := w.clin(w.u, "GET", "/alerts", ""); len(ids(page)) != 0 {
		t.Fatalf("listed to u: %v", page)
	}
	if _, c := w.clin(w.u, "GET", "/alerts/unread-count", ""); c["count"] != float64(0) {
		t.Fatalf("counted for u: %v", c)
	}
	if code, _ := w.clin(w.u, "POST", "/alerts/"+a.String()+"/acknowledge", ""); code != 404 {
		t.Fatalf("u acknowledged it: %d", code)
	}
	if _, page := w.clin(w.v, "GET", "/alerts?sent_to=you", ""); len(ids(page)) != 1 {
		t.Fatalf("v doesn't see it: %v", page)
	}
	if _, c := w.clin(w.v, "GET", "/alerts/unread-count", ""); c["count"] != float64(1) {
		t.Fatalf("v count %v", c)
	}
}

// Review Focus 4: a queue alert stays visible to every clinician after the
// admin turns the queue off; visibility is never recomputed.
func TestQueueAlertStaysVisibleAfterQueueOff(t *testing.T) {
	w := newWorld(t)
	a := w.alert("r1", true)
	if code, _ := w.adm("PATCH", "/hospital/alert-settings",
		`{"version": 0, "recipients": {"queue": false, "clinician_ids": ["`+w.v.String()+`"]}}`); code != 200 {
		t.Fatal(code)
	}
	code, page := w.clin(w.u, "GET", "/alerts", "")
	if code != 200 || len(ids(page)) != 1 || ids(page)[0] != a.String() || page["queue"] != false {
		t.Fatalf("%d %v", code, page)
	}
	al := page["alerts"].([]any)[0].(map[string]any)
	if r := al["routing"].(map[string]any); r["queue"] != true || r["to_you"] != false {
		t.Fatalf("routing %v", r)
	}
	// The evidence: the finding, the patient, the link, causes caution and
	// review only (never high).
	f := al["finding"].(map[string]any)
	if f["name"] != "Potassium" || f["value"] != "6.8" || f["flag"] != "high" || f["code_system"] != "loinc" ||
		f["reference_range"] != "3.5–5.1" {
		t.Fatalf("finding %v", f)
	}
	if p := al["patient"].(map[string]any); p["mrn"] != "SYN-0001" || p["location"] != nil {
		t.Fatalf("patient %v", p)
	}
	if al["emr_link"] != "https://fhir.example.org/chart/src-1" || al["source_label"] != "Main record" {
		t.Fatalf("link %v label %v", al["emr_link"], al["source_label"])
	}
	causes := al["likely_causes"].([]any)
	if len(causes) != 1 || causes[0].(map[string]any)["confidence"] != "caution" {
		t.Fatalf("causes %v", causes)
	}
}

// Twenty clinicians acknowledge at once: exactly one wins; the rest get
// 409 already_handled naming the winner.
func TestAcknowledgeFirstWins(t *testing.T) {
	w := newWorld(t)
	a := w.alert("r1", true)
	var clins []uuid.UUID
	for range 20 {
		clins = append(clins, user(t, w.h, "clinician"))
	}
	var wg sync.WaitGroup
	codes := make([]int, len(clins))
	bodies := make([]map[string]any, len(clins))
	for i, c := range clins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], bodies[i] = w.clin(c, "POST", "/alerts/"+a.String()+"/acknowledge", "")
		}()
	}
	wg.Wait()
	won := 0
	for i, c := range codes {
		switch c {
		case 200:
			won++
		case 409:
			b := bodies[i]
			if b["code"] != "already_handled" || b["status"] != "handled" || b["handled_by"] == nil || b["handled_at"] == nil {
				t.Errorf("409 body %v", b)
			}
		default:
			t.Errorf("code %d %v", c, bodies[i])
		}
	}
	if won != 1 || auditCount(t, w.h, "alert.acknowledged") != 1 {
		t.Fatalf("%d won, %d audited", won, auditCount(t, w.h, "alert.acknowledged"))
	}
}

// A21, E22: a page of alerts served is audited with the ids served.
func TestAlertsReadAudited(t *testing.T) {
	w := newWorld(t)
	a := w.alert("r1", true)
	b := w.alert("r2", false, w.u)
	if _, page := w.clin(w.u, "GET", "/alerts?status=new&limit=1", ""); len(ids(page)) != 1 || page["next_cursor"] == nil {
		t.Fatalf("page %v", page)
	} else {
		_, page2 := w.clin(w.u, "GET", "/alerts?status=new&limit=1&cursor="+page["next_cursor"].(string), "")
		if len(ids(page2)) != 1 || ids(page2)[0] == ids(page)[0] {
			t.Fatalf("second page %v", page2)
		}
		// Another session's, or another filter's, cursor is refused.
		if code, _ := w.clin(w.u, "GET", "/alerts?status=handled&limit=1&cursor="+page["next_cursor"].(string), ""); code != 422 {
			t.Fatalf("cursor with other filters: %d", code)
		}
		if code, _ := w.clin(w.v, "GET", "/alerts?status=new&limit=1&cursor="+page["next_cursor"].(string), ""); code != 422 {
			t.Fatalf("another session's cursor: %d", code)
		}
	}
	var payloads []string
	rows, err := sharedSuper.Query(context.Background(), `SELECT payload::text FROM audit_log WHERE hospital_id = $1 AND
		action = 'alerts.read' AND actor_id = $2 ORDER BY id`, w.h, w.u)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		_ = rows.Scan(&p)
		payloads = append(payloads, p)
	}
	all := strings.Join(payloads, " ")
	if len(payloads) != 2 || !strings.Contains(all, a.String()) || !strings.Contains(all, b.String()) ||
		strings.Contains(all, "SYN-0001") || strings.Contains(all, "Synthetic Patient") {
		t.Fatalf("alerts.read rows %v", payloads)
	}
	for _, bad := range []string{"/alerts?limit=0", "/alerts?limit=101", "/alerts?status=open", "/alerts?x=1",
		"/alerts?status=new&status=handled", "/alerts?sent_to=them", "/alerts?cursor=00"} {
		if code, _ := w.clin(w.u, "GET", bad, ""); code != 422 {
			t.Errorf("%s: %d", bad, code)
		}
	}
}

// A23, A42, §15.3: sources_status is on every page, empty list included;
// a source silent for three poll intervals, or paused, is behind.
func TestSourcesStatusBehindWhenStaleOrPaused(t *testing.T) {
	w := newWorld(t)
	status := func() string {
		_, page := w.clin(w.u, "GET", "/alerts", "")
		ss := page["sources_status"].([]any)
		if len(ss) != 1 || len(ids(page)) != 0 {
			t.Fatalf("page %v", page)
		}
		return ss[0].(map[string]any)["status"].(string)
	}
	if s := status(); s != "current" {
		t.Fatalf("fresh source %s", s)
	}
	exec(t, `UPDATE emr_connections SET paused = true WHERE id = $1`, w.conn)
	if s := status(); s != "behind" {
		t.Fatalf("paused source %s", s)
	}
	exec(t, `UPDATE emr_connections SET paused = false WHERE id = $1`, w.conn)
	exec(t, `UPDATE emr.sync_runs SET finished_at = now() - interval '7 minutes' WHERE connection_id = $1`, w.conn)
	if s := status(); s != "behind" {
		t.Fatalf("stale source %s", s)
	}
	_, out := w.adm("GET", "/hospital/alert-settings", "")
	if out["connection"].(map[string]any)["status"] != "problems" {
		t.Fatalf("settings status %v", out["connection"])
	}
}

func TestEmptyListCarriesSourcesStatus(t *testing.T) {
	w := newWorld(t)
	code, page := w.clin(w.u, "GET", "/alerts", "")
	if code != 200 || page["sources_status"] == nil || len(page["sources_status"].([]any)) != 1 ||
		len(page["alerts"].([]any)) != 0 || page["grouped"] != nil {
		t.Fatalf("%d %v", code, page)
	}
	if auditCount(t, w.h, "alerts.read") != 0 {
		t.Fatal("an empty page was audited")
	}
}

// §12.5: the dismissal note is stored on the alert, never logged or
// audited; the reason is audited.
func TestDismissNoteNeverAudited(t *testing.T) {
	w := newWorld(t)
	a := w.alert("r1", true)
	code, out := w.clin(w.u, "POST", "/alerts/"+a.String()+"/dismiss", `{"reason": "already_known", "note": " `+canaryNote+` "}`)
	if code != 200 || out["status"] != "dismissed" || out["dismiss_reason"] != "already_known" {
		t.Fatalf("%d %v", code, out)
	}
	var note string
	if err := sharedSuper.QueryRow(context.Background(), `SELECT dismiss_note FROM alerts WHERE id = $1`, a).Scan(&note); err != nil ||
		note != canaryNote {
		t.Fatalf("note %q %v", note, err)
	}
	if strings.Contains(auditPayloads(t, w.h), canaryNote) || strings.Contains(w.logs.String(), canaryNote) {
		t.Fatal("the note reached the audit log or a log line")
	}
	if auditCount(t, w.h, "alert.dismissed") != 1 {
		t.Fatal("dismissal not audited")
	}
	long := strings.Repeat("x", 501)
	b := w.alert("r2", true)
	for _, bad := range []string{`{"reason": "bored"}`, `{}`, `{"reason": "other", "note": "` + long + `"}`} {
		if code, _ := w.clin(w.u, "POST", "/alerts/"+b.String()+"/dismiss", bad); code != 422 {
			t.Errorf("%s: %d", bad[:min(len(bad), 40)], code)
		}
	}
}

// The notification switches: on by default, saved per user, audited.
func TestNotificationSettings(t *testing.T) {
	w := newWorld(t)
	if _, out := w.clin(w.u, "GET", "/me/notification-settings", ""); out["alert_popups"] != true || out["correction_popups"] != true {
		t.Fatalf("defaults %v", out)
	}
	if code, out := w.clin(w.u, "PATCH", "/me/notification-settings", `{"alert_popups": false}`); code != 200 ||
		out["alert_popups"] != false || out["correction_popups"] != true {
		t.Fatalf("%d %v", code, out)
	}
	if _, out := w.clin(w.v, "GET", "/me/notification-settings", ""); out["alert_popups"] != true {
		t.Fatalf("another user's switches changed: %v", out)
	}
	if code, _ := w.clin(w.u, "PATCH", "/me/notification-settings", `{}`); code != 422 {
		t.Fatal("an empty patch was accepted")
	}
	if auditCount(t, w.h, "notification_settings.changed") != 1 {
		t.Fatal("not audited")
	}
}

// --- Matches and exclusions (Task 34) ---------------------------------------

func (w *world) match(a, b uuid.UUID) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := sharedSuper.QueryRow(context.Background(), `INSERT INTO emr.possible_matches (hospital_id, patient_a, patient_b, reason,
		connection_id) VALUES ($1, $2, $3, 'mrn_birth_sex', $4) RETURNING id`, w.h, a, b, w.conn).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// A31: another hospital's match is 404 to list, confirm and reject.
func TestMatchOtherHospital404(t *testing.T) {
	w, other := newWorld(t), newWorld(t)
	b, _, _ := other.newPatient(t, "src-2", "SYN-0001")
	m := other.match(other.patient, b)
	if _, out := w.adm("GET", "/hospital/patient-matches", ""); len(out["matches"].([]any)) != 0 {
		t.Fatalf("listed %v", out)
	}
	if code, out := w.adm("POST", "/hospital/patient-matches/"+m.String()+"/confirm", `{"keep": "a", "mfa_code": "123456"}`); code != 404 {
		t.Fatalf("confirm: %d %v", code, out)
	}
	if code, _ := w.adm("POST", "/hospital/patient-matches/"+m.String()+"/reject", `{"mfa_code": "123456"}`); code != 404 {
		t.Fatalf("reject: %d", code)
	}
	if code, _ := w.adm("POST", "/hospital/patient-matches/not-a-uuid/reject", `{"mfa_code": "123456"}`); code != 404 {
		t.Fatalf("bad id: %d", code)
	}
}

// A12, A31: a confirm needs the code, merges, and is audited with ids
// only; the merged patient's alert moves to the kept one.
func TestConfirmAuditedMergeOnly(t *testing.T) {
	w := newWorld(t)
	b, _, _ := w.newPatient(t, "src-2", "SYN-0001")
	m := w.match(w.patient, b)
	code, out := w.adm("GET", "/hospital/patient-matches", "")
	list := out["matches"].([]any)
	if code != 200 || len(list) != 1 || list[0].(map[string]any)["a"].(map[string]any)["records"] != float64(1) {
		t.Fatalf("%d %v", code, out)
	}
	if auditCount(t, w.h, "emr.patient_matches_read") != 1 {
		t.Fatal("the cards served weren't audited")
	}
	al := w.alert("r1", true)
	path := "/hospital/patient-matches/" + m.String() + "/confirm"
	if code, _ := w.adm("POST", path, `{"keep": "b"}`); code != 403 {
		t.Fatalf("no code: %d", code)
	}
	if code, _ := w.adm("POST", path, `{"keep": "b", "mfa_code": "000000"}`); code != 403 {
		t.Fatalf("wrong code: %d", code)
	}
	if code, _ := w.adm("POST", path, `{"keep": "c", "mfa_code": "123456"}`); code != 422 {
		t.Fatalf("keep c: %d", code)
	}
	if auditCount(t, w.h, "emr.patients_merged") != 0 {
		t.Fatal("merged before the code")
	}
	if code, out := w.adm("POST", path, `{"keep": "b", "mfa_code": "123456"}`); code != 204 {
		t.Fatalf("%d %v", code, out)
	}
	if auditCount(t, w.h, "emr.patients_merged") != 1 || strings.Contains(auditPayloads(t, w.h), "SYN-0001") {
		t.Fatal("merge audit")
	}
	var moved uuid.UUID
	if err := sharedSuper.QueryRow(context.Background(), `SELECT patient_id FROM alerts WHERE id = $1`, al).Scan(&moved); err != nil ||
		moved != b {
		t.Fatalf("alert patient %v %v", moved, err)
	}
	if code, _ := w.adm("POST", path, `{"keep": "b", "mfa_code": "123456"}`); code != 404 {
		t.Fatalf("confirmed twice: %d", code)
	}
}

func TestRejectNotProposedAgain(t *testing.T) {
	w := newWorld(t)
	b, _, _ := w.newPatient(t, "src-2", "SYN-0001")
	m := w.match(w.patient, b)
	if code, _ := w.adm("POST", "/hospital/patient-matches/"+m.String()+"/reject", `{"mfa_code": "123456"}`); code != 204 {
		t.Fatal(code)
	}
	if _, out := w.adm("GET", "/hospital/patient-matches", ""); len(out["matches"].([]any)) != 0 {
		t.Fatalf("still listed %v", out)
	}
	// The pair can't be proposed again for the same reason.
	if _, err := sharedSuper.Exec(context.Background(), `INSERT INTO emr.possible_matches (hospital_id, patient_a, patient_b, reason)
		VALUES ($1, $2, $3, 'mrn_birth_sex')`, w.h, b, w.patient); err == nil {
		t.Fatal("re-proposed")
	}
	if auditCount(t, w.h, "emr.patient_match_rejected") != 1 {
		t.Fatal("not audited")
	}
}

// A34: an exclusion withdraws the patient's open alerts at once (gone from
// every list and count), purges the patient, and is audited with counts.
func TestExclusionWithdrawsAlertsAtOnce(t *testing.T) {
	w := newWorld(t)
	a := w.alert("r1", true)
	if _, c := w.clin(w.u, "GET", "/alerts/unread-count", ""); c["count"] != float64(1) {
		t.Fatalf("before: %v", c)
	}
	if code, _ := w.adm("POST", "/hospital/patient-exclusions", `{"mrn": "SYN-NONE", "reason": "objection", "mfa_code": "123456"}`); code != 404 {
		t.Fatalf("unknown mrn: %d", code)
	}
	if code, _ := w.adm("POST", "/hospital/patient-exclusions", `{"mrn": "SYN-0001", "patient_id": "`+w.patient.String()+`", "reason": "objection"}`); code != 422 {
		t.Fatalf("both keys: %d", code)
	}
	code, out := w.adm("POST", "/hospital/patient-exclusions", `{"mrn": "SYN-0001", "reason": "objection", "mfa_code": "123456"}`)
	if code != 202 || out["patient_id"] != w.patient.String() || out["purge_by"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	if _, c := w.clin(w.u, "GET", "/alerts/unread-count", ""); c["count"] != float64(0) {
		t.Fatalf("after: %v", c)
	}
	var withdrawn *string
	if err := sharedSuper.QueryRow(context.Background(), `SELECT withdrawn FROM alerts WHERE id = $1`, a).Scan(&withdrawn); err != nil ||
		withdrawn == nil || *withdrawn != "excluded" {
		t.Fatalf("withdrawn %v %v", withdrawn, err)
	}
	if auditCount(t, w.h, "emr.patient_excluded") != 1 || auditCount(t, w.h, "alert.withdrawn") != 1 ||
		strings.Contains(auditPayloads(t, w.h), "SYN-0001") {
		t.Fatal("exclusion audit")
	}
	// Two active patients with one MRN: the caller must name the patient.
	w.newPatient(t, "src-3", "SYN-0002")
	w.newPatient(t, "src-4", "SYN-0002")
	if code, out := w.adm("POST", "/hospital/patient-exclusions", `{"mrn": "SYN-0002", "reason": "other", "mfa_code": "123456"}`); code != 409 {
		t.Fatalf("ambiguous mrn: %d %v", code, out)
	}
	// Another hospital's patient is 404.
	other := newWorld(t)
	if code, _ := w.adm("POST", "/hospital/patient-exclusions", `{"patient_id": "`+other.patient.String()+`", "reason": "other", "mfa_code": "123456"}`); code != 404 {
		t.Fatalf("other hospital: %d", code)
	}
}

// No log line names a patient, an MRN or a value.
func TestLogsCarryNoPatientData(t *testing.T) {
	w := newWorld(t)
	w.alert("r1", true)
	w.clin(w.u, "GET", "/alerts", "")
	w.adm("POST", "/hospital/patient-exclusions", `{"mrn": "SYN-NONE", "reason": "objection", "mfa_code": "123456"}`)
	w.clin(w.u, "POST", "/alerts/"+uuid.NewString()+"/acknowledge", "")
	for _, s := range []string{"SYN-0001", "SYN-NONE", "Synthetic Patient", "6.8"} {
		if strings.Contains(w.logs.String(), s) {
			t.Errorf("a log line carries %q", s)
		}
	}
}
