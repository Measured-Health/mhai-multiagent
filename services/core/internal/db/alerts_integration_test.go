//go:build integration

package db_test

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00029 (EMR plan Task 31; design §12, §13.2): alerts, their
// settings and deliveries, the recipient check, the re-queue when a
// recipient leaves, and the merge and exclusion steps for alerts. Every
// name and value is synthetic.

// alertFixture is a hospital with a connection, a patient with one
// observation and finding, and two clinicians.
type alertFixture struct {
	h, conn, patient, obs, finding uuid.UUID
	u, v                           uuid.UUID
}

func newAlertFixture(t *testing.T, pool *pgxpool.Pool) alertFixture {
	t.Helper()
	var f alertFixture
	f.h, _ = seedHospital(t, pool, uniqueCode("AL"))
	f.conn = seedConn(t, pool, f.h)
	f.patient = seedPatient(t, pool, f.h, f.conn, "p-"+uuid.NewString()[:8], "MRN-AL")
	f.obs = seedObservation(t, pool, f.h, f.conn, f.patient, "p")
	f.finding = seedFinding(t, pool, f.h, f.patient, f.obs, "al:"+uuid.NewString())
	f.u = seedClinician(t, pool, f.h)
	f.v = seedClinician(t, pool, f.h)
	return f
}

// seedAlert inserts a new alert of the fixture's patient as the superuser,
// with a delivery to each of to. rule distinguishes alerts (one open per
// patient, rule and analyte).
func seedAlert(t *testing.T, pool *pgxpool.Pool, f alertFixture, rule, severity string, queue bool, to ...uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO alerts (hospital_id, finding_id, patient_id, connection_id, rule_id, loinc,
		category, severity, routed_queue, last_notified_at) VALUES ($1, $2, $3, $4, $5, '99901-1', 'blood_chemistry', $6, $7, now())
		RETURNING id`, f.h, f.finding, f.patient, f.conn, rule, severity, queue).Scan(&id); err != nil {
		t.Fatalf("seed alert: %v", err)
	}
	for _, u := range to {
		if _, err := pool.Exec(ctx, `INSERT INTO alert_deliveries (alert_id, hospital_id, user_id) VALUES ($1, $2, $3)`,
			id, f.h, u); err != nil {
			t.Fatalf("seed delivery: %v", err)
		}
	}
	return id
}

type alertState struct {
	queue    bool
	notified bool
	status   string
	withdraw *string
	patient  *uuid.UUID
}

func alertNow(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) alertState {
	t.Helper()
	var s alertState
	if err := pool.QueryRow(context.Background(), `SELECT routed_queue, last_notified_at IS NOT NULL, status, withdrawn, patient_id
		FROM alerts WHERE id = $1`, id).Scan(&s.queue, &s.notified, &s.status, &s.withdraw, &s.patient); err != nil {
		t.Fatal(err)
	}
	return s
}

// requeueFacts are the hospital's alert.requeued facts in the outbox:
// each one's ids (sorted) and reason.
func requeueFacts(t *testing.T, pool *pgxpool.Pool, h uuid.UUID) []map[string]any {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT payload FROM intelligence_audit_outbox
		WHERE hospital_id = $1 AND action = 'alert.requeued' AND query_id IS NULL AND job_id IS NULL ORDER BY id`, h)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func idStrings(ids ...uuid.UUID) []any {
	s := make([]string, 0, len(ids))
	for _, id := range ids {
		s = append(s, id.String())
	}
	sort.Strings(s)
	out := make([]any, len(s))
	for i := range s {
		out[i] = s[i]
	}
	return out
}

// A33: a recipient deactivated: every open alert delivered only to them
// moves to the queue in the same transaction (a critical is notified
// again), with one alert.requeued fact; an alert another active clinician
// was also sent, an alert already on the queue, and a handled alert stay.
func TestRequeueOnDeactivation(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	f := newAlertFixture(t, pool)
	a1 := seedAlert(t, pool, f, "r1", "critical", false, f.u)
	a2 := seedAlert(t, pool, f, "r2", "urgent", false, f.u)
	a3 := seedAlert(t, pool, f, "r3", "info", false, f.u)
	both := seedAlert(t, pool, f, "r4", "critical", false, f.u, f.v)
	handled := seedAlert(t, pool, f, "r5", "critical", false, f.u)
	if _, err := pool.Exec(ctx, `UPDATE alerts SET status = 'handled', handled_by = $2, handled_at = now() WHERE id = $1`,
		handled, f.u); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1`, f.u); err != nil {
		t.Fatal(err)
	}
	for _, a := range []uuid.UUID{a1, a2, a3} {
		if s := alertNow(t, pool, a); !s.queue {
			t.Errorf("alert %v not re-queued", a)
		}
	}
	if s := alertNow(t, pool, a1); s.notified {
		t.Error("the critical alert keeps its notification: it must be notified again as a queue critical")
	}
	if s := alertNow(t, pool, a2); !s.notified {
		t.Error("an urgent alert lost its notification time")
	}
	if alertNow(t, pool, both).queue || alertNow(t, pool, handled).queue {
		t.Error("an alert another clinician sees, or a handled one, moved to the queue")
	}
	facts := requeueFacts(t, pool, f.h)
	if len(facts) != 1 || facts[0]["reason"] != "user_deactivated" {
		t.Fatalf("facts %v", facts)
	}
	got := facts[0]["alert_ids"].([]any)
	sort.Slice(got, func(i, j int) bool { return got[i].(string) < got[j].(string) })
	if !slices.Equal(got, idStrings(a1, a2, a3)) {
		t.Fatalf("fact ids %v", got)
	}
}

// A33, Review Focus 4: losing the clinician role re-queues too, in the
// same transaction as the role change, and the hospital setting of the
// caller's transaction is put back.
func TestRoleRemovalRequeues(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	f := newAlertFixture(t, pool)
	a := seedAlert(t, pool, f, "r1", "urgent", false, f.u)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, f.h.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_roles WHERE user_id = $1 AND role = 'clinician'`, f.u); err != nil {
		t.Fatal(err)
	}
	var setting string
	if err := tx.QueryRow(ctx, `SELECT current_setting('mhai.hospital_id', true)`).Scan(&setting); err != nil || setting != f.h.String() {
		t.Fatalf("the caller's hospital setting changed: %q %v", setting, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !alertNow(t, pool, a).queue {
		t.Fatal("not re-queued on role removal")
	}
	if facts := requeueFacts(t, pool, f.h); len(facts) != 1 || facts[0]["reason"] != "role_removed" {
		t.Fatalf("facts %v", facts)
	}
}

// A15 (database): a named recipient and a delivery must be an active
// clinician of the same hospital, whoever writes the row.
func TestRecipientMustBeActiveClinicianSameHospital(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	f := newAlertFixture(t, pool)
	other, _ := seedHospital(t, pool, uniqueCode("AO"))
	foreign := seedClinician(t, pool, other)
	inactive := seedClinician(t, pool, f.h)
	if _, err := pool.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1`, inactive); err != nil {
		t.Fatal(err)
	}
	admin := seedUser(t, pool, &f.h)
	if _, err := pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'hospital_admin')`, admin); err != nil {
		t.Fatal(err)
	}
	a := seedAlert(t, pool, f, "r1", "critical", true)
	for name, u := range map[string]uuid.UUID{"other hospital": foreign, "inactive": inactive, "not a clinician": admin} {
		if _, err := pool.Exec(ctx, `INSERT INTO alert_recipients (hospital_id, user_id) VALUES ($1, $2)`, f.h, u); pgCode(err) != checkViolation {
			t.Errorf("recipient %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO alert_deliveries (alert_id, hospital_id, user_id) VALUES ($1, $2, $3)`,
			a, f.h, u); pgCode(err) != checkViolation {
			t.Errorf("delivery %s: %v", name, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO alert_recipients (hospital_id, user_id) VALUES ($1, $2)`, f.h, f.u); err != nil {
		t.Fatalf("an active clinician refused: %v", err)
	}
}

// The first handler wins at the database: the gateway moves an alert out
// of new once, as the caller, and never back.
func TestAlertUpdateOnlyFromNew(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	f := newAlertFixture(t, pool)
	a := seedAlert(t, pool, f, "r1", "critical", true)
	handle := func(user uuid.UUID, by uuid.UUID, status string) (int64, error) {
		return inTx(ctx, pool, []string{`SET LOCAL ROLE mhai_gateway`,
			`SELECT set_config('mhai.user_id', '` + user.String() + `', true)`}, &f.h,
			`UPDATE alerts SET status = $2, handled_by = $3, handled_at = now() WHERE id = $1 AND status = 'new'`, a, status, by)
	}
	if n, err := handle(f.u, f.v, "handled"); pgCode(err) != rlsViolation {
		t.Fatalf("handled in another user's name: %d %v", n, err)
	}
	if n, err := handle(f.u, f.u, "handled"); err != nil || n != 1 {
		t.Fatalf("first handler: %d %v", n, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE alerts SET status = 'handled', handled_by = $2, handled_at = now() WHERE id = $1`, a, f.u); err != nil {
		t.Fatal(err)
	}
	// Even without the status guard in the statement, the restrictive
	// policy refuses to touch a handled alert.
	n, err := inTx(ctx, pool, []string{`SET LOCAL ROLE mhai_gateway`,
		`SELECT set_config('mhai.user_id', '` + f.v.String() + `', true)`}, &f.h,
		`UPDATE alerts SET status = 'dismissed', dismiss_reason = 'other', handled_by = $2 WHERE id = $1`, a, f.v)
	if err != nil || n != 0 {
		t.Fatalf("a handled alert was changed: %d %v", n, err)
	}
	// Routing holds at the database too: an alert sent only to v can't be
	// handled as u.
	c := seedAlert(t, pool, f, "r3", "critical", false, f.v)
	if n, err := handle(f.u, f.u, "handled"); err != nil || n != 0 {
		_ = c
		t.Fatalf("handled the first alert again: %d %v", n, err)
	}
	if n, err := inTx(ctx, pool, []string{`SET LOCAL ROLE mhai_gateway`,
		`SELECT set_config('mhai.user_id', '` + f.u.String() + `', true)`}, &f.h,
		`UPDATE alerts SET status = 'handled', handled_by = $2, handled_at = now() WHERE id = $1`, c, f.u); err != nil || n != 0 {
		t.Fatalf("u handled an alert sent only to v: %d %v", n, err)
	}
	// And never back to new.
	b := seedAlert(t, pool, f, "r2", "critical", true)
	if _, err := inTx(ctx, pool, []string{`SET LOCAL ROLE mhai_gateway`,
		`SELECT set_config('mhai.user_id', '` + f.u.String() + `', true)`}, &f.h,
		`UPDATE alerts SET status = 'new' WHERE id = $1`, b); pgCode(err) != rlsViolation {
		t.Fatalf("reset to new: %v", err)
	}
}

// A13, A14 (database): another hospital's alerts, deliveries and settings
// are invisible to the gateway and to core's consumer role.
func TestAlertsRLS(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	f := newAlertFixture(t, pool)
	seedAlert(t, pool, f, "r1", "critical", true, f.u)
	for _, q := range []string{`INSERT INTO alert_settings (hospital_id, queue) VALUES ($1, true)`,
		`INSERT INTO alert_levels (hospital_id, category, level) VALUES ($1, 'blood_count', 'all')`} {
		if _, err := pool.Exec(ctx, q, f.h); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO alert_recipients (hospital_id, user_id) VALUES ($1, $2)`, f.h, f.u); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_notification_settings (user_id, hospital_id) VALUES ($1, $2)`, f.u, f.h); err != nil {
		t.Fatal(err)
	}
	other, _ := seedHospital(t, pool, uniqueCode("AR"))
	for _, role := range []string{"mhai_gateway", "mhai_emr_core"} {
		for _, table := range []string{"alerts", "alert_deliveries", "alert_settings", "alert_levels", "alert_recipients",
			"alert_units", "user_notification_settings"} {
			if err := asRoleIn(ctx, pool, role, other, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SELECT set_config('mhai.user_id', $1, true)`, f.u.String()); err != nil {
					return err
				}
				var n int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE hospital_id IS NOT NULL`).Scan(&n); err != nil {
					return err
				}
				if n != 0 {
					t.Errorf("%s sees %d of another hospital's %s", role, n, table)
				}
				return nil
			}); err != nil {
				t.Fatalf("%s %s: %v", role, table, err)
			}
		}
	}
	// An insert naming another hospital fails the policy.
	if err := asRoleIn(ctx, pool, "mhai_gateway", other, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alert_levels (hospital_id, category, level) VALUES ($1, 'blood_sugar', 'off')`, f.h)
		return err
	}); pgCode(err) != rlsViolation {
		t.Fatalf("cross-hospital settings insert: %v", err)
	}
}

// A27: the only recipient column is a user; there is no patient or address
// recipient anywhere in the alert tables.
func TestNoPatientRecipientType(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'alert_deliveries' ORDER BY column_name`)
	if err != nil {
		t.Fatal(err)
	}
	cols, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cols, []string{"alert_id", "hospital_id", "notified_at", "read_at", "user_id"}) {
		t.Fatalf("alert_deliveries columns %v", cols)
	}
	var ref string
	if err := pool.QueryRow(ctx, `SELECT confrelid::regclass::text FROM pg_constraint
		WHERE conrelid = 'alert_deliveries'::regclass AND contype = 'f' AND conkey = ARRAY[(SELECT attnum FROM pg_attribute
		  WHERE attrelid = 'alert_deliveries'::regclass AND attname = 'user_id')]`).Scan(&ref); err != nil || ref != "users" {
		t.Fatalf("user_id references %q: %v", ref, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public'
		AND table_name IN ('alerts', 'alert_deliveries', 'alert_recipients')
		AND column_name ~ '(phone|email|address|recipient_patient|sms)'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d contact columns: %v", n, err)
	}
}

// Only the owner's definers write alert facts: the worker may not, and an
// alert fact names no question or job.
func TestWorkerCannotWriteAlertFacts(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	f := newAlertFixture(t, pool)
	if _, err := asWorker(ctx, pool, f.h, `INSERT INTO intelligence_audit_outbox (hospital_id, action, entity, payload)
		VALUES ($1, 'alert.requeued', 'alert', '{"alert_ids": [], "reason": "user_deactivated"}')`, f.h); pgCode(err) != rlsViolation &&
		pgCode(err) != checkViolation {
		t.Fatalf("worker wrote an alert fact: %v", err)
	}
	q := seedQueryIn(t, pool, f.h)
	if _, err := pool.Exec(ctx, `INSERT INTO intelligence_audit_outbox (hospital_id, query_id, action, payload)
		VALUES ($1, $2, 'alert.requeued', '{}')`, f.h, q); pgCode(err) != checkViolation {
		t.Fatalf("an alert fact with a question: %v", err)
	}
}

// TODO.md PR 5 follow-ups: the exclusion withdraws the patient's open
// alerts at once (A34) and clears every alert's patient, finding and note;
// the merge moves the merged patient's alerts to the kept one, except an
// open alert the kept patient already has for the same rule and analyte.
func TestExclusionAndMergeAlertSteps(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	f := newAlertFixture(t, pool)
	open := seedAlert(t, pool, f, "r1", "critical", true)
	done := seedAlert(t, pool, f, "r2", "urgent", true)
	if _, err := pool.Exec(ctx, `UPDATE alerts SET status = 'dismissed', dismiss_reason = 'other', dismiss_note = 'synthetic note',
		handled_by = $2, handled_at = now() WHERE id = $1`, done, f.u); err != nil {
		t.Fatal(err)
	}
	var withdrawn []uuid.UUID
	if err := asRoleIn(ctx, pool, "mhai_gateway", f.h, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT alert_ids FROM emr_exclude_patient($1, 'objection')`, f.patient).Scan(&withdrawn)
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(withdrawn, []uuid.UUID{open}) {
		t.Fatalf("withdrawn %v, want the open alert", withdrawn)
	}
	s := alertNow(t, pool, open)
	if s.status != "dismissed" || s.withdraw == nil || *s.withdraw != "excluded" || s.patient != nil {
		t.Fatalf("open alert after the exclusion: %+v", s)
	}
	var note *string
	var patient *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT dismiss_note, patient_id FROM alerts WHERE id = $1`, done).Scan(&note, &patient); err != nil ||
		note != nil || patient != nil {
		t.Fatalf("handled alert keeps note %v patient %v: %v", note, patient, err)
	}

	// The merge.
	g := newAlertFixture(t, pool)
	kept := seedPatient(t, pool, g.h, g.conn, "p-kept", "MRN-AL")
	keptObs := seedObservation(t, pool, g.h, g.conn, kept, "p-kept")
	keptFinding := seedFinding(t, pool, g.h, kept, keptObs, "al:kept")
	var clash uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO alerts (hospital_id, finding_id, patient_id, connection_id, rule_id, loinc,
		category, severity, routed_queue) VALUES ($1, $2, $3, $4, 'r1', '99901-1', 'blood_chemistry', 'critical', true)
		RETURNING id`, g.h, keptFinding, kept, g.conn).Scan(&clash); err != nil {
		t.Fatal(err)
	}
	moves := seedAlert(t, pool, g, "r2", "urgent", true)
	stays := seedAlert(t, pool, g, "r1", "critical", true)
	m := seedMatch(t, pool, g.h, kept, g.patient)
	if err := asRoleIn(ctx, pool, "mhai_gateway", g.h, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT emr_merge_patients($1, 'a')`, m)
		return err
	}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if s := alertNow(t, pool, moves); s.patient == nil || *s.patient != kept {
		t.Fatalf("alert not moved: %+v", s)
	}
	if s := alertNow(t, pool, stays); s.patient == nil || *s.patient != g.patient || s.status != "new" {
		t.Fatalf("clashing open alert: %+v", s)
	}
}

// Rejecting a match is the caller's hospital's only, once.
func TestRejectMatch(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	f := newAlertFixture(t, pool)
	b := seedPatient(t, pool, f.h, f.conn, "p-b", "MRN-AL")
	m := seedMatch(t, pool, f.h, f.patient, b)
	other, _ := seedHospital(t, pool, uniqueCode("RJ"))
	reject := func(h uuid.UUID) error {
		return asRoleIn(ctx, pool, "mhai_gateway", h, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT emr_reject_match($1)`, m)
			return err
		})
	}
	if err := reject(other); pgCode(err) != noDataFound {
		t.Fatalf("another hospital rejected it: %v", err)
	}
	if err := reject(f.h); err != nil {
		t.Fatal(err)
	}
	if err := reject(f.h); pgCode(err) != noDataFound {
		t.Fatalf("rejected twice: %v", err)
	}
}

func TestMigration00029DownUp(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.MigrateDownTo(ctx, pg.URL, 28); err != nil {
		t.Fatalf("down to 28: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_class WHERE relname IN ('alerts', 'alert_settings',
			'alert_units', 'alert_levels', 'alert_recipients', 'alert_deliveries', 'user_notification_settings'))
		+ (SELECT count(*) FROM pg_proc WHERE proname IN ('mhai_alerts_requeue', 'alerts_requeue_removed', 'emr_hospital_sources',
			'emr_patient_sources', 'emr_alert_links', 'emr_reject_match', 'mhai_alert_recipient_check'))
		+ (SELECT count(*) FROM pg_policy WHERE polname = 'worker_no_alert_facts')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("after down: %d objects left", left)
	}
	var cols int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_proc p WHERE p.proname = 'emr_exclude_patient'
		AND 'alert_ids' = ANY (p.proargnames)`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("the exclusion still returns alert ids: %d %v", cols, err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
