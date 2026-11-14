//go:build integration

package db_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00032 (EMR plan Tasks 39-40; design §10.6, §14.2): purge
// requests, legal holds, the exclusion purge, the retention sweep and the
// hospital removal hook. Every value is synthetic, and distinctive
// (6.8371) so a search for it can't hit a timestamp or an id.

const (
	notInPrereqState = "55000"
	// purgeValue is the synthetic observation value the purge must leave
	// nowhere.
	purgeValue = "6.8371"
)

// retention is a hospital with a connection, for the tests below.
type retention struct {
	h, conn uuid.UUID
}

func newRetention(t *testing.T, pool *pgxpool.Pool, code string) retention {
	t.Helper()
	h, _ := seedHospital(t, pool, uniqueCode(code))
	return retention{h: h, conn: seedConn(t, pool, h)}
}

// patient inserts a patient linked to source id src.
func (r retention) patient(t *testing.T, pool *pgxpool.Pool, src string) uuid.UUID {
	t.Helper()
	return seedPatient(t, pool, r.h, r.conn, src, "SYN-"+uuid.NewString()[:6])
}

// obs inserts an observation of patient p (source patient src) with the
// distinctive value, effective ago (a Postgres interval) before now, from a
// raw record received at the same time.
func (r retention) obs(t *testing.T, pool *pgxpool.Pool, p uuid.UUID, src, ago string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	s := "obs-" + uuid.NewString()[:8]
	raw := seedRaw(t, pool, r.h, r.conn, "observation", s, "1", `{"patient": "`+src+`", "value_quantity": "`+purgeValue+`"}`)
	if _, err := pool.Exec(ctx, `UPDATE emr.raw_records SET received_at = now() - $2::interval, status = 'normalised' WHERE id = $1`,
		raw, ago); err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO emr.observations (hospital_id, patient_id, category, loinc, value_num, unit_ucum,
		value_si, status, effective_at, standardised, raw_id, source_kind, connection_id, connector_type, connector_version,
		source_id, source_version, received_at, mapping_version, normaliser_version)
		VALUES ($1, $2, 'laboratory', '99901-1', `+purgeValue+`, 'mmol/L', `+purgeValue+`, 'final', now() - $6::interval, true,
		        $3, 'connector', $4, 'fhir_r4', 1, $5, '1', now() - $6::interval, 'synthetic-map', 'n1') RETURNING id`,
		r.h, p, raw, r.conn, s, ago).Scan(&id); err != nil {
		t.Fatalf("seed observation: %v", err)
	}
	return id
}

// med inserts a medication of p started ago before now.
func (r retention) med(t *testing.T, pool *pgxpool.Pool, p uuid.UUID, src, ago string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	s := "med-" + uuid.NewString()[:8]
	raw := seedRaw(t, pool, r.h, r.conn, "medication", s, "1", `{"patient": "`+src+`"}`)
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO emr.medications (hospital_id, patient_id, kind, status, start_at, standardised,
		raw_id, source_kind, connection_id, connector_type, connector_version, source_id, source_version, received_at,
		mapping_version, normaliser_version)
		VALUES ($1, $2, 'request', 'active', now() - $6::interval, false, $3, 'connector', $4, 'fhir_r4', 1, $5, '1',
		        now() - $6::interval, 'synthetic-map', 'n1') RETURNING id`, r.h, p, raw, r.conn, s, ago).Scan(&id); err != nil {
		t.Fatalf("seed medication: %v", err)
	}
	return id
}

// enc inserts an encounter of p started ago before now.
func (r retention) enc(t *testing.T, pool *pgxpool.Pool, p uuid.UUID, src, ago string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	s := "enc-" + uuid.NewString()[:8]
	raw := seedRaw(t, pool, r.h, r.conn, "encounter", s, "1", `{"patient": "`+src+`"}`)
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO emr.encounters (hospital_id, patient_id, class, status, period_start, raw_id,
		source_kind, connection_id, connector_type, connector_version, source_id, source_version, received_at,
		mapping_version, normaliser_version)
		VALUES ($1, $2, 'inpatient', 'in-progress', now() - $6::interval, $3, 'connector', $4, 'fhir_r4', 1, $5, '1',
		        now() - $6::interval, 'synthetic-map', 'n1') RETURNING id`, r.h, p, raw, r.conn, s, ago).Scan(&id); err != nil {
		t.Fatalf("seed encounter: %v", err)
	}
	return id
}

// alert inserts an alert of p for finding f, made ago before now; status
// "dismissed" makes it dismissed by clinician by with a note.
func (r retention) alert(t *testing.T, pool *pgxpool.Pool, p, f uuid.UUID, rule, ago string, by *uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO alerts (hospital_id, finding_id, patient_id, connection_id, rule_id, loinc,
		category, severity, routed_queue) VALUES ($1, $2, $3, $4, $5, '99901-1', 'blood_chemistry', 'critical', true)
		RETURNING id`, r.h, f, p, r.conn, rule).Scan(&id); err != nil {
		t.Fatalf("seed alert: %v", err)
	}
	if by != nil {
		if _, err := pool.Exec(ctx, `UPDATE alerts SET status = 'dismissed', handled_by = $2, handled_at = now(),
			dismiss_reason = 'other', dismiss_note = 'Synthetic note 6.8371',
			amended = '{"previous_value": "6.8371"}' WHERE id = $1`, id, *by); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE alerts SET created_at = now() - $2::interval WHERE id = $1`, id, ago); err != nil {
		t.Fatal(err)
	}
	return id
}

func placeHold(t *testing.T, pool *pgxpool.Pool, h uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `INSERT INTO legal_holds (hospital_id, reference) VALUES ($1, 'LH-2026-0042')
		RETURNING id`, h).Scan(&id); err != nil {
		t.Fatalf("place hold: %v", err)
	}
	return id
}

func liftHold(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE legal_holds SET lifted_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

// exclusion is what emr_exclude_patient returned.
type exclusion struct {
	counts   [5]int64
	findings int64
	alerts   []uuid.UUID
	request  uuid.UUID
	held     bool
	purgeBy  time.Time
}

func excludeAs(ctx context.Context, pool *pgxpool.Pool, role string, h, p uuid.UUID) (exclusion, error) {
	var x exclusion
	err := asRoleIn(ctx, pool, role, h, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT observations, medications, encounters, raw_records, matches, alert_ids, findings,
			request_id, held, purge_by FROM emr_exclude_patient($1, 'objection')`, p).
			Scan(&x.counts[0], &x.counts[1], &x.counts[2], &x.counts[3], &x.counts[4], &x.alerts, &x.findings, &x.request,
				&x.held, &x.purgeBy)
	})
	return x, err
}

// maint runs q as core's maintenance role for hospital h and scans one value.
func maint(ctx context.Context, pool *pgxpool.Pool, h uuid.UUID, q string, dst any, args ...any) error {
	return asRoleIn(ctx, pool, "mhai_emr_maint", h, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, args...).Scan(dst)
	})
}

func countWhere(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// requirePurged checks everything the purge must remove for patient p.
func requirePurged(t *testing.T, pool *pgxpool.Pool, h, p uuid.UUID, alerts ...uuid.UUID) {
	t.Helper()
	for what, q := range map[string]string{
		"findings":      `SELECT count(*) FROM emr.findings WHERE hospital_id = $1 AND patient_id = $2`,
		"observations":  `SELECT count(*) FROM emr.observations WHERE hospital_id = $1 AND patient_id = $2`,
		"medications":   `SELECT count(*) FROM emr.medications WHERE hospital_id = $1 AND patient_id = $2`,
		"encounters":    `SELECT count(*) FROM emr.encounters WHERE hospital_id = $1 AND patient_id = $2`,
		"matches":       `SELECT count(*) FROM emr.possible_matches WHERE hospital_id = $1 AND (patient_a = $2 OR patient_b = $2)`,
		"alerts":        `SELECT count(*) FROM alerts WHERE hospital_id = $1 AND patient_id = $2`,
		"held alerts":   `SELECT count(*) FROM emr.purge_held_alerts WHERE hospital_id = $1 AND patient_id = $2`,
		"patient PII":   `SELECT count(*) FROM emr.patients WHERE hospital_id = $1 AND id = $2 AND (display_name IS NOT NULL OR mrn IS NOT NULL OR birth_date IS NOT NULL OR sex <> 'unknown')`,
		"raw (patient)": `SELECT count(*) FROM emr.raw_records r JOIN emr.patient_links l ON l.connection_id = r.connection_id WHERE r.hospital_id = $1 AND l.patient_id = $2 AND (r.source_id = l.source_patient_id OR r.payload ->> 'patient' = l.source_patient_id)`,
	} {
		if n := countWhere(t, pool, q, h, p); n != 0 {
			t.Errorf("after the purge, %s: %d rows left", what, n)
		}
	}
	for _, a := range alerts {
		var pid, fid *uuid.UUID
		var note, amended *string
		if err := pool.QueryRow(context.Background(), `SELECT patient_id, finding_id, dismiss_note, amended::text FROM alerts WHERE id = $1`, a).
			Scan(&pid, &fid, &note, &amended); err != nil {
			t.Fatalf("alert %s: %v (the alert stays for the counts)", a, err)
		}
		if pid != nil || fid != nil || note != nil || amended != nil {
			t.Errorf("alert %s keeps patient %v finding %v note %v", a, pid, fid, note)
		}
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM emr.patient_links WHERE hospital_id = $1 AND patient_id = $2`, h, p); n != 1 {
		t.Errorf("links of the excluded patient: %d, want the link kept", n)
	}
}

// A34: the purge removes the patient's findings, the consumer's records of
// them, alerts' patient columns, canonical rows, raw records and matches;
// the link stays. Another patient, and the same shapes in another
// hospital, are untouched.
func TestPurgeRemovesEverythingButLink(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	r, o := newRetention(t, pool, "PG"), newRetention(t, pool, "PO")
	build := func(r retention, src string) (p, f, open, handled uuid.UUID) {
		p = r.patient(t, pool, src)
		seedRaw(t, pool, r.h, r.conn, "patient", src, "1", `{"given": "Synthetic"}`)
		ob := r.obs(t, pool, p, src, "1 day")
		r.med(t, pool, p, src, "1 day")
		r.enc(t, pool, p, src, "1 day")
		f = seedFinding(t, pool, r.h, p, ob, "pg:"+uuid.NewString())
		if _, err := pool.Exec(ctx, `INSERT INTO emr_consumed (hospital_id, kind, ref_id, status, outcome)
			VALUES ($1, 'finding', $2, 'open', 'raised')`, r.h, f); err != nil {
			t.Fatal(err)
		}
		clin := seedClinician(t, pool, r.h)
		open = r.alert(t, pool, p, f, "r-open", "1 day", nil)
		handled = r.alert(t, pool, p, f, "r-done", "1 day", &clin)
		return
	}
	p, f, open, handled := build(r, "p-gone")
	keep := r.patient(t, pool, "p-keep")
	keepObs := r.obs(t, pool, keep, "p-keep", "1 day")
	seedMatch(t, pool, r.h, p, keep)
	op, of, _, _ := build(o, "p-gone")

	x, err := excludeAs(ctx, pool, "mhai_gateway", r.h, p)
	if err != nil {
		t.Fatal(err)
	}
	if x.held || x.counts != [5]int64{1, 1, 1, 4, 1} || x.findings != 1 || len(x.alerts) != 1 || x.alerts[0] != open {
		t.Fatalf("exclusion: %+v, want 1 observation, medication, encounter and match, 4 raw records, 1 finding, the open alert withdrawn", x)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM emr.purge_requests WHERE id = $1 AND hospital_id = $2 AND patient_id = $3
		AND done_at IS NOT NULL`, x.request, r.h, p).Scan(&state); err != nil || state != "done" {
		t.Fatalf("purge request: %q %v", state, err)
	}
	requirePurged(t, pool, r.h, p, open, handled)
	if n := countWhere(t, pool, `SELECT count(*) FROM emr_consumed WHERE ref_id = $1`, f); n != 0 {
		t.Errorf("the consumer's record of the purged finding stays (%d)", n)
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM emr.observations WHERE id = $1`, keepObs); n != 1 {
		t.Error("another patient's observation was purged")
	}
	// The other hospital: every row still there.
	for what, q := range map[string]string{
		"findings":     `SELECT count(*) FROM emr.findings WHERE hospital_id = $1 AND patient_id = $2`,
		"observations": `SELECT count(*) FROM emr.observations WHERE hospital_id = $1 AND patient_id = $2`,
		"alerts":       `SELECT count(*) FROM alerts WHERE hospital_id = $1 AND patient_id = $2`,
		"name":         `SELECT count(*) FROM emr.patients WHERE hospital_id = $1 AND id = $2 AND display_name IS NOT NULL`,
	} {
		if n := countWhere(t, pool, q, o.h, op); n == 0 {
			t.Errorf("another hospital's %s were touched", what)
		}
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM emr_consumed WHERE ref_id = $1`, of); n != 1 {
		t.Error("another hospital's consumer record was purged")
	}
	// Excluding again reuses nothing open and purges nothing more.
	if again, err := excludeAs(ctx, pool, "mhai_gateway", r.h, p); err != nil || again.counts != [5]int64{} || again.held {
		t.Errorf("exclude again: %+v %v", again, err)
	}
}

// A41: under a legal hold the patient is excluded at once and its alerts
// stop naming it, but nothing is deleted; the request is held, the loop
// can't purge it, and the release waits for the hold to be lifted. Then
// the purge removes everything.
func TestHoldDefersPurgeThenReleases(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	r := newRetention(t, pool, "HD")
	p := r.patient(t, pool, "p-held")
	ob := r.obs(t, pool, p, "p-held", "1 day")
	f := seedFinding(t, pool, r.h, p, ob, "hd:"+uuid.NewString())
	clin := seedClinician(t, pool, r.h)
	open := r.alert(t, pool, p, f, "r-open", "1 day", nil)
	handled := r.alert(t, pool, p, f, "r-done", "1 day", &clin)
	hold := placeHold(t, pool, r.h)

	x, err := excludeAs(ctx, pool, "mhai_gateway", r.h, p)
	if err != nil {
		t.Fatal(err)
	}
	if !x.held || x.counts != [5]int64{} || x.findings != 0 || len(x.alerts) != 1 {
		t.Fatalf("held exclusion: %+v", x)
	}
	if d := time.Until(x.purgeBy); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("purge_by %v, want the request's due time (24 hours)", x.purgeBy)
	}
	// Nothing deleted, nothing cleared on the patient, but no alert names it.
	if n := countWhere(t, pool, `SELECT count(*) FROM emr.observations WHERE id = $1`, ob); n != 1 {
		t.Fatal("an observation was deleted under the hold")
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM emr.patients WHERE id = $1 AND status = 'excluded' AND display_name IS NOT NULL`, p); n != 1 {
		t.Fatal("the patient was cleared under the hold, or isn't excluded")
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM alerts WHERE patient_id = $1`, p); n != 0 {
		t.Fatalf("%d alerts still name the excluded patient", n)
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM emr.purge_held_alerts WHERE request_id = $1 AND patient_id = $2
		AND finding_id = $3`, x.request, p, f); n != 2 {
		t.Fatalf("held alert columns: %d, want both alerts kept aside", n)
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM emr.purge_held_alerts WHERE alert_id = $1 AND dismiss_note IS NOT NULL`,
		handled); n != 1 {
		t.Fatal("the dismissed alert's note wasn't kept aside")
	}
	// The loop can neither purge nor release while the hold stands.
	var res json.RawMessage
	if err := maint(ctx, pool, r.h, `SELECT emr_purge_patient($1)`, &res, x.request); err != nil {
		t.Fatal(err)
	}
	if !jsonHas(res, "state", "held") {
		t.Fatalf("purge under the hold: %s", res)
	}
	var released []uuid.UUID
	if err := maint(ctx, pool, r.h, `SELECT emr_release_holds()`, &released); err != nil || len(released) != 0 {
		t.Fatalf("released under the hold: %v %v", released, err)
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM emr.observations WHERE id = $1`, ob); n != 1 {
		t.Fatal("deleted under the hold")
	}

	liftHold(t, pool, hold)
	if err := maint(ctx, pool, r.h, `SELECT emr_release_holds()`, &released); err != nil || len(released) != 1 || released[0] != x.request {
		t.Fatalf("release after the lift: %v %v", released, err)
	}
	var due time.Time
	if err := pool.QueryRow(ctx, `SELECT due_at FROM emr.purge_requests WHERE id = $1 AND state = 'pending'`, x.request).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if d := time.Until(due); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("released due_at %v, want 24 hours on", due)
	}
	var pending []uuid.UUID
	if err := asRoleIn(ctx, pool, "mhai_emr_maint", r.h, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT * FROM emr_pending_purges(10)`)
		if err != nil {
			return err
		}
		pending, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	}); err != nil || len(pending) != 1 || pending[0] != x.request {
		t.Fatalf("pending: %v %v", pending, err)
	}
	if err := maint(ctx, pool, r.h, `SELECT emr_purge_patient($1)`, &res, x.request); err != nil {
		t.Fatal(err)
	}
	if !jsonHas(res, "state", "done") || !jsonHas(res, "findings", float64(1)) || !jsonHas(res, "alerts", float64(2)) {
		t.Fatalf("purge after the release: %s", res)
	}
	requirePurged(t, pool, r.h, p, open, handled)
	if n := countWhere(t, pool, `SELECT count(*) FROM emr.purge_held_alerts WHERE request_id = $1`, x.request); n != 0 {
		t.Errorf("held alert columns left: %d", n)
	}
	// A done request is left as it is.
	if err := maint(ctx, pool, r.h, `SELECT emr_purge_patient($1)`, &res, x.request); err != nil || !jsonHas(res, "state", "done") {
		t.Fatalf("purge a done request: %s %v", res, err)
	}
}

// A request is purged only for its own hospital: another hospital's id is
// no_data_found, and nothing moves.
func TestPurgeOwnHospitalOnly(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	r, o := newRetention(t, pool, "PH"), newRetention(t, pool, "PX")
	p := r.patient(t, pool, "p-own")
	hold := placeHold(t, pool, r.h)
	x, err := excludeAs(ctx, pool, "mhai_gateway", r.h, p)
	if err != nil {
		t.Fatal(err)
	}
	liftHold(t, pool, hold)
	var res json.RawMessage
	if err := maint(ctx, pool, o.h, `SELECT emr_purge_patient($1)`, &res, x.request); pgCode(err) != noDataFound {
		t.Fatalf("another hospital's request: %v", err)
	}
	var released []uuid.UUID
	if err := maint(ctx, pool, o.h, `SELECT emr_release_holds()`, &released); err != nil || len(released) != 0 {
		t.Fatalf("another hospital released: %v %v", released, err)
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM emr.purge_requests WHERE id = $1 AND state = 'held'`, x.request); n != 1 {
		t.Fatal("the request moved")
	}
}

func jsonHas(raw json.RawMessage, key string, want any) bool {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	return m[key] == want
}

// Design §14.2 on aged rows: each rule removes what is past it and keeps
// what isn't.
func TestRetentionAges(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	r := newRetention(t, pool, "RA")
	p := r.patient(t, pool, "p-ra")
	clin := seedClinician(t, pool, r.h)

	// Raw payloads: 8 days normalised goes; 6 days and a pending 8 days stay.
	rawOld := seedRaw(t, pool, r.h, r.conn, "patient", "p-ra", "old", `{"given": "Synthetic"}`)
	rawPending := seedRaw(t, pool, r.h, r.conn, "patient", "p-ra", "pending", `{"given": "Synthetic"}`)
	rawYoung := seedRaw(t, pool, r.h, r.conn, "patient", "p-ra", "young", `{"given": "Synthetic"}`)
	for id, q := range map[uuid.UUID]string{
		rawOld:     `UPDATE emr.raw_records SET status = 'normalised', received_at = now() - interval '8 days' WHERE id = $1`,
		rawPending: `UPDATE emr.raw_records SET received_at = now() - interval '8 days' WHERE id = $1`,
		rawYoung:   `UPDATE emr.raw_records SET status = 'normalised', received_at = now() - interval '6 days' WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, q, id); err != nil {
			t.Fatal(err)
		}
	}
	// Canonical rows: 14 months go, 12 months stay.
	oldObs, youngObs := r.obs(t, pool, p, "p-ra", "14 months"), r.obs(t, pool, p, "p-ra", "12 months")
	oldMed, youngMed := r.med(t, pool, p, "p-ra", "14 months"), r.med(t, pool, p, "p-ra", "12 months")
	oldEnc, youngEnc := r.enc(t, pool, p, "p-ra", "14 months"), r.enc(t, pool, p, "p-ra", "12 months")
	var oldObsRaw uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT raw_id FROM emr.observations WHERE id = $1`, oldObs).Scan(&oldObsRaw); err != nil {
		t.Fatal(err)
	}
	// Findings (on the young observation): 91 days with no alert goes;
	// 91 days with an alert, and 89 days, stay.
	fOld := seedFinding(t, pool, r.h, p, youngObs, "ra1:"+uuid.NewString())
	fAlerted := seedFinding(t, pool, r.h, p, youngObs, "ra2:"+uuid.NewString())
	fYoung := seedFinding(t, pool, r.h, p, youngObs, "ra3:"+uuid.NewString())
	for f, ago := range map[uuid.UUID]string{fOld: "91 days", fAlerted: "91 days", fYoung: "89 days"} {
		if _, err := pool.Exec(ctx, `UPDATE emr.findings SET created_at = now() - $2::interval WHERE id = $1`, f, ago); err != nil {
			t.Fatal(err)
		}
	}
	// The consumer's records: 91 days for a deleted finding goes; for a
	// finding that stays, it stays.
	for _, f := range []uuid.UUID{fOld, fAlerted} {
		if _, err := pool.Exec(ctx, `INSERT INTO emr_consumed (hospital_id, kind, ref_id, status, outcome, created_at)
			VALUES ($1, 'finding', $2, 'open', 'raised', now() - interval '91 days')`, r.h, f); err != nil {
			t.Fatal(err)
		}
	}
	// Alerts: 25 months goes; 14 months loses the patient and the note;
	// 12 months stays as it is.
	aGone := r.alert(t, pool, p, fAlerted, "r-25", "25 months", &clin)
	aAged := r.alert(t, pool, p, fAlerted, "r-14", "14 months", &clin)
	aYoung := r.alert(t, pool, p, fAlerted, "r-12", "12 months", &clin)
	// Patients: untouched for 14 months with nothing left goes (with its
	// link); one with a young row stays; an excluded one stays.
	idle := r.patient(t, pool, "p-idle")
	excluded := r.patient(t, pool, "p-excl")
	if _, err := excludeAs(ctx, pool, "mhai_gateway", r.h, excluded); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE emr.patients SET created_at = now() - interval '14 months',
		updated_at = now() - interval '14 months' WHERE id = ANY ($1)`, []uuid.UUID{idle, excluded, p}); err != nil {
		t.Fatal(err)
	}

	var res json.RawMessage
	if err := maint(ctx, pool, r.h, `SELECT emr_retention_sweep(10000)`, &res); err != nil {
		t.Fatal(err)
	}
	if jsonHas(res, "more", true) {
		t.Errorf("sweep says more: %s", res)
	}
	exists := func(table string, id uuid.UUID) bool {
		return countWhere(t, pool, `SELECT count(*) FROM `+table+` WHERE id = $1`, id) == 1
	}
	for _, c := range []struct {
		table string
		id    uuid.UUID
		want  bool
	}{
		{"emr.observations", oldObs, false}, {"emr.observations", youngObs, true},
		{"emr.medications", oldMed, false}, {"emr.medications", youngMed, true},
		{"emr.encounters", oldEnc, false}, {"emr.encounters", youngEnc, true},
		{"emr.raw_records", oldObsRaw, false},
		{"emr.findings", fOld, false}, {"emr.findings", fAlerted, true}, {"emr.findings", fYoung, true},
		{"alerts", aGone, false}, {"alerts", aAged, true}, {"alerts", aYoung, true},
		{"emr.patients", idle, false}, {"emr.patients", p, true}, {"emr.patients", excluded, true},
	} {
		if got := exists(c.table, c.id); got != c.want {
			t.Errorf("%s %s exists %v, want %v", c.table, c.id, got, c.want)
		}
	}
	for id, want := range map[uuid.UUID]bool{rawOld: false, rawPending: true, rawYoung: true} {
		if got := countWhere(t, pool, `SELECT count(*) FROM emr.raw_records WHERE id = $1 AND payload IS NOT NULL`, id) == 1; got != want {
			t.Errorf("raw %s payload kept %v, want %v", id, got, want)
		}
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM emr.raw_records WHERE id = $1 AND payload_sha256 IS NOT NULL`, rawOld); n != 1 {
		t.Error("the aged raw record lost its row (the hash and provenance stay)")
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM alerts WHERE id = $1 AND patient_id IS NULL AND finding_id IS NULL
		AND dismiss_note IS NULL AND severity = 'critical' AND category = 'blood_chemistry'`, aAged); n != 1 {
		t.Error("the 14-month alert keeps its patient or note, or lost its codes")
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM alerts WHERE id = $1 AND patient_id IS NOT NULL AND dismiss_note IS NOT NULL`, aYoung); n != 1 {
		t.Error("the 12-month alert was cleared")
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM emr_consumed WHERE ref_id = ANY ($1)`, []uuid.UUID{fOld, fAlerted}); n != 1 {
		t.Errorf("consumer records: %d, want only the standing finding's", n)
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM emr.patient_links WHERE patient_id = $1`, idle); n != 0 {
		t.Error("the deleted patient's link stays")
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM emr.patient_links WHERE patient_id = $1`, excluded); n != 1 {
		t.Error("the excluded patient's link was deleted")
	}
}

// A hospital on hold is left out of the sweep entirely.
func TestRetentionSkipsHeldHospital(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	r := newRetention(t, pool, "RH")
	p := r.patient(t, pool, "p-rh")
	ob := r.obs(t, pool, p, "p-rh", "14 months")
	placeHold(t, pool, r.h)
	var res json.RawMessage
	if err := maint(ctx, pool, r.h, `SELECT emr_retention_sweep(10000)`, &res); err != nil {
		t.Fatal(err)
	}
	if !jsonHas(res, "held", true) {
		t.Fatalf("sweep on hold: %s", res)
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM emr.observations WHERE id = $1`, ob); n != 1 {
		t.Fatal("deleted under a hold")
	}
}

// The sweep's cap holds per step and says there is more.
func TestRetentionCapped(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	r := newRetention(t, pool, "RC")
	p := r.patient(t, pool, "p-rc")
	for i := 0; i < 3; i++ {
		r.obs(t, pool, p, "p-rc", "14 months")
	}
	var res json.RawMessage
	if err := maint(ctx, pool, r.h, `SELECT emr_retention_sweep(2)`, &res); err != nil {
		t.Fatal(err)
	}
	if !jsonHas(res, "observations", float64(2)) || !jsonHas(res, "more", true) {
		t.Fatalf("capped sweep: %s", res)
	}
	if err := maint(ctx, pool, r.h, `SELECT emr_retention_sweep(2)`, &res); err != nil {
		t.Fatal(err)
	}
	if !jsonHas(res, "observations", float64(1)) {
		t.Fatalf("second sweep: %s", res)
	}
}

// Plan departure 11: the removal hook is refused unless the hospital is
// removal_scheduled, not on hold and every credential wiped; then it
// removes the hospital's EMR rows and alert tables, and nothing of another
// hospital. Only the provisioning role may call it.
func TestRemoveHospitalDataNeedsRemovalScheduled(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	r, o := newRetention(t, pool, "RM"), newRetention(t, pool, "RO")
	for _, x := range []retention{r, o} {
		p := x.patient(t, pool, "p-rm")
		ob := x.obs(t, pool, p, "p-rm", "1 day")
		f := seedFinding(t, pool, x.h, p, ob, "rm:"+uuid.NewString())
		x.alert(t, pool, p, f, "r-rm", "1 day", nil)
		if _, err := pool.Exec(ctx, `INSERT INTO alert_settings (hospital_id) VALUES ($1)`, x.h); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO emr.credentials (connection_id, hospital_id, field, set_at, secret_version, origin)
		VALUES ($1, $2, 'private_key', now(), 'v1', '{https://fhir.example.org:443}')`, r.conn, r.h); err != nil {
		t.Fatal(err)
	}
	remove := func(role string) (json.RawMessage, error) {
		var res json.RawMessage
		err := asRoleIn(ctx, pool, role, r.h, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT emr_remove_hospital_data($1)`, r.h).Scan(&res)
		})
		return res, err
	}
	for _, role := range []string{"mhai_gateway", "mhai_platform", "mhai_emr", "mhai_emr_core", "mhai_emr_maint", "mhai_emr_admin"} {
		if _, err := remove(role); pgCode(err) != permissionDenied {
			t.Errorf("%s calling the removal: %v", role, err)
		}
	}
	if _, err := remove("mhai_provision"); pgCode(err) != notInPrereqState {
		t.Fatalf("active hospital: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hospitals SET status = 'removal_scheduled' WHERE id = $1`, r.h); err != nil {
		t.Fatal(err)
	}
	if _, err := remove("mhai_provision"); pgCode(err) != notInPrereqState {
		t.Fatalf("credentials not wiped: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM emr.credentials WHERE hospital_id = $1`, r.h); err != nil {
		t.Fatal(err)
	}
	hold := placeHold(t, pool, r.h)
	if _, err := remove("mhai_provision"); pgCode(err) != notInPrereqState {
		t.Fatalf("on hold: %v", err)
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM emr.observations WHERE hospital_id = $1`, r.h); n != 1 {
		t.Fatal("a refused removal deleted rows")
	}
	liftHold(t, pool, hold)
	res, err := remove("mhai_provision")
	if err != nil {
		t.Fatal(err)
	}
	if !jsonHas(res, "observations", float64(1)) || !jsonHas(res, "alerts", float64(1)) || !jsonHas(res, "emr_connections", float64(1)) {
		t.Errorf("removal counts: %s", res)
	}
	for _, table := range []string{"emr_connections", "emr.patients", "emr.raw_records", "emr.observations", "emr.findings",
		"emr.patient_links", "alerts", "alert_settings"} {
		if n := countWhere(t, pool, `SELECT count(*) FROM `+table+` WHERE hospital_id = $1`, r.h); n != 0 {
			t.Errorf("%s: %d rows left after the removal", table, n)
		}
		if n := countWhere(t, pool, `SELECT count(*) FROM `+table+` WHERE hospital_id = $1`, o.h); n == 0 {
			t.Errorf("%s: another hospital's rows removed", table)
		}
	}
}

// K8: the legal holds are the provisioning role's: it places and lifts
// them, a lifted hold never changes again, and nothing deletes one. No
// application role holds anything on them.
func TestLegalHoldsProvisionOnly(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	r := newRetention(t, pool, "LH")
	var id uuid.UUID
	if err := asRoleIn(ctx, pool, "mhai_provision", r.h, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO legal_holds (hospital_id, reference) VALUES ($1, 'LH-2026-0042') RETURNING id`, r.h).Scan(&id)
	}); err != nil {
		t.Fatalf("provision places: %v", err)
	}
	if err := asRoleIn(ctx, pool, "mhai_provision", r.h, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO legal_holds (hospital_id, reference) VALUES ($1, 'Synthetic Person')`, r.h)
		return err
	}); err == nil {
		t.Error("a free-text reference was accepted")
	}
	lift := func() (int64, error) {
		var n int64
		err := asRoleIn(ctx, pool, "mhai_provision", r.h, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE legal_holds SET lifted_at = now() WHERE id = $1`, id)
			n = tag.RowsAffected()
			return err
		})
		return n, err
	}
	if n, err := lift(); err != nil || n != 1 {
		t.Fatalf("provision lifts: %d %v", n, err)
	}
	if n, err := lift(); err != nil || n != 0 {
		t.Fatalf("a lifted hold changed again: %d %v", n, err)
	}
	if err := asRoleIn(ctx, pool, "mhai_provision", r.h, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM legal_holds WHERE id = $1`, id)
		return err
	}); pgCode(err) != permissionDenied {
		t.Errorf("provision deleting a hold: %v", err)
	}
	for _, role := range []string{"mhai_gateway", "mhai_platform", "mhai_emr", "mhai_emr_core", "mhai_emr_maint", "mhai_emr_admin",
		"mhai_worker", "mhai_jobs"} {
		for _, q := range []string{`SELECT count(*) FROM legal_holds`, `SELECT count(*) FROM emr.purge_requests`,
			`SELECT count(*) FROM emr.purge_held_alerts`} {
			err := asRoleIn(ctx, pool, role, r.h, func(tx pgx.Tx) error {
				var n int
				return tx.QueryRow(ctx, q).Scan(&n)
			})
			requireDenied(t, err, role+" "+q)
		}
	}
}

// The new definers: owner mhai_migrate, search_path pinned, and EXECUTE
// only for the roles named (the internal ones: none). mhai_emr_maint holds
// nothing but its functions.
func TestRetentionDefiners(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	want := map[string][]string{
		"mhai_hospital_on_hold":    {},
		"emr_hold_check":           {},
		"mhai_legal_hold_lock":     {},
		"emr_purge_ids":            {},
		"emr_purge_lock":           {},
		"emr_purge_patient_data":   {},
		"emr_purge_hold_alerts":    {},
		"emr_exclude_patient":      {"mhai_emr_admin", "mhai_gateway"},
		"emr_maint_hospitals":      {"mhai_emr_maint"},
		"emr_release_holds":        {"mhai_emr_maint"},
		"emr_pending_purges":       {"mhai_emr_maint"},
		"emr_purge_patient":        {"mhai_emr_maint"},
		"emr_held_overdue":         {"mhai_emr_maint"},
		"emr_retention_sweep":      {"mhai_emr_maint"},
		"emr_remove_hospital_data": {"mhai_provision"},
		"emr_patients_by_mrn":      {"mhai_emr_admin"},
	}
	for name, grantees := range want {
		var owner string
		var definer bool
		var config, got []string
		if err := pool.QueryRow(ctx, `
			SELECT pg_get_userbyid(p.proowner), p.prosecdef, COALESCE(p.proconfig, '{}'),
			       COALESCE((SELECT array_agg(CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END ORDER BY 1)
			                 FROM aclexplode(p.proacl) a
			                 WHERE a.privilege_type = 'EXECUTE' AND a.grantee <> p.proowner), '{}')
			FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'public' AND p.proname = $1`, name).Scan(&owner, &definer, &config, &got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if owner != "mhai_migrate" || !definer || len(config) != 1 || config[0] != "search_path=pg_catalog, pg_temp" {
			t.Errorf("%s: owner %s definer %v config %v", name, owner, definer, config)
		}
		slices.Sort(got)
		if !slices.Equal(got, grantees) {
			t.Errorf("%s: EXECUTE for %v, want %v", name, got, grantees)
		}
	}
	for _, table := range []string{"emr.patients", "emr.findings", "alerts", "emr.observations"} {
		err := asRoleIn(ctx, pool, "mhai_emr_maint", uuid.New(), func(tx pgx.Tx) error {
			var n int
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
		})
		requireDenied(t, err, "mhai_emr_maint reading "+table)
	}
	for _, table := range []string{"legal_holds", "emr.purge_requests", "emr.purge_held_alerts"} {
		var rls, force bool
		var owner string
		if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity, pg_get_userbyid(relowner) FROM pg_class
			WHERE oid = $1::regclass`, table).Scan(&rls, &force, &owner); err != nil || !rls || !force || owner != "mhai_migrate" {
			t.Errorf("%s: rls %v force %v owner %s %v", table, rls, force, owner, err)
		}
	}
}

// K2: a temporary table can't shadow what the purge reads.
func TestRetentionDefinersIgnoreTempTables(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	r := newRetention(t, pool, "TT")
	p := r.patient(t, pool, "p-tt")
	ob := r.obs(t, pool, p, "p-tt", "1 day")
	err := asRoleIn(ctx, pool, "mhai_gateway", r.h, func(tx pgx.Tx) error {
		for _, q := range []string{`CREATE TEMP TABLE legal_holds (hospital_id uuid, lifted_at timestamptz) ON COMMIT DROP`,
			`CREATE TEMP TABLE observations (hospital_id uuid, patient_id uuid)`} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `SELECT * FROM emr_exclude_patient($1, 'objection')`, p)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM emr.observations WHERE id = $1`, ob); n != 0 {
		t.Fatal("the purge read a temporary table")
	}
}

func TestMigration00032DownUp(t *testing.T) {
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
	// A held exclusion: the down is refused while its purge is owed.
	r := newRetention(t, pool, "DU")
	p := r.patient(t, pool, "p-du")
	ob := r.obs(t, pool, p, "p-du", "1 day")
	f := seedFinding(t, pool, r.h, p, ob, "du:"+uuid.NewString())
	r.alert(t, pool, p, f, "r-du", "1 day", nil)
	hold := placeHold(t, pool, r.h)
	x, err := excludeAs(ctx, pool, "mhai_gateway", r.h, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 31); err == nil {
		t.Fatal("down with a purge owed")
	}
	liftHold(t, pool, hold)
	var released []uuid.UUID
	var res json.RawMessage
	if err := maint(ctx, pool, r.h, `SELECT emr_release_holds()`, &released); err != nil {
		t.Fatal(err)
	}
	if err := maint(ctx, pool, r.h, `SELECT emr_purge_patient($1)`, &res, x.request); err != nil || !jsonHas(res, "state", "done") {
		t.Fatalf("purge: %s %v", res, err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 31); err != nil {
		t.Fatalf("down to 31: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_class WHERE relname IN ('legal_holds', 'purge_requests',
			'purge_held_alerts', 'raw_records_payload_age', 'alerts_finding', 'findings_age'))
		+ (SELECT count(*) FROM pg_proc WHERE proname IN ('mhai_hospital_on_hold', 'emr_hold_check', 'mhai_legal_hold_lock', 'emr_purge_ids', 'emr_purge_lock',
			'emr_purge_patient_data', 'emr_purge_hold_alerts', 'emr_maint_hospitals', 'emr_release_holds', 'emr_pending_purges',
			'emr_purge_patient', 'emr_held_overdue', 'emr_retention_sweep', 'emr_remove_hospital_data', 'emr_patients_by_mrn'))
		+ (SELECT count(*) FROM pg_policy WHERE polname IN ('owner_maint', 'emr_maint_append', 'emr_admin_append',
			'provision_emr_append', 'emr_admin_read', 'admin_read'))
		+ (SELECT count(*) FROM pg_proc p WHERE p.proname = 'emr_exclude_patient' AND 'request_id' = ANY (p.proargnames))`).
		Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("after down: %d objects left", left)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// A hold and a purge never interleave: a hold placed while an exclusion's
// purge is in flight waits for it to commit; an exclusion that starts
// while a hold is being placed waits, then sees the hold and deletes
// nothing.
func TestHoldAndPurgeNeverInterleave(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	begin := func(role string, h uuid.UUID) pgx.Tx {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{`SET LOCAL ROLE ` + role, `SELECT set_config('mhai.hospital_id', '` + h.String() + `', true)`} {
			if _, err := tx.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		return tx
	}

	// The purge first: the hold waits for it.
	r := newRetention(t, pool, "IA")
	p := r.patient(t, pool, "p-ia")
	r.obs(t, pool, p, "p-ia", "1 day")
	purge := begin("mhai_gateway", r.h)
	var held bool
	if err := purge.QueryRow(ctx, `SELECT held FROM emr_exclude_patient($1, 'objection')`, p).Scan(&held); err != nil || held {
		t.Fatalf("purge: held %v %v", held, err)
	}
	placed := make(chan error, 1)
	go func() {
		_, err := pool.Exec(ctx, `INSERT INTO legal_holds (hospital_id, reference) VALUES ($1, 'LH-2026-0042')`, r.h)
		placed <- err
	}()
	select {
	case err := <-placed:
		t.Fatalf("the hold didn't wait for the purge in flight: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := purge.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-placed; err != nil {
		t.Fatal(err)
	}

	// The hold first: the exclusion waits, then defers its purge.
	r2 := newRetention(t, pool, "IB")
	p2 := r2.patient(t, pool, "p-ib")
	ob := r2.obs(t, pool, p2, "p-ib", "1 day")
	hold := begin("mhai_provision", r2.h)
	if _, err := hold.Exec(ctx, `INSERT INTO legal_holds (hospital_id, reference) VALUES ($1, 'LH-2026-0043')`, r2.h); err != nil {
		t.Fatal(err)
	}
	type res struct {
		x   exclusion
		err error
	}
	done := make(chan res, 1)
	go func() {
		x, err := excludeAs(ctx, pool, "mhai_gateway", r2.h, p2)
		done <- res{x, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("the exclusion didn't wait for the hold being placed: %+v", got)
	case <-time.After(500 * time.Millisecond):
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || !got.x.held {
		t.Fatalf("exclusion after the hold: %+v", got)
	}
	if n := countWhere(t, pool, `SELECT count(*) FROM emr.observations WHERE id = $1`, ob); n != 1 {
		t.Fatal("deleted under the hold")
	}
}
