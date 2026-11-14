//go:build integration

package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00046 (analysis backend plan Tasks 27-28, PR 10): consultations
// that name a patient, EMR findings and alerts as coded facts, the
// dictionaries from the EMR configuration, and the patient-index changes
// (exclusions and merges) that never touch a frozen period. Every patient,
// name, MRN, value and note is synthetic and distinctive (Vantreek,
// SYN-AN10-, 7.4419), so a search for it can't hit anything else.

const (
	an10Name  = "Synthetic Orsolya Vantreek"
	an10MRN   = "SYN-AN10-"
	an10Value = "7.4419"
	an10Note  = "Synthetic note Vantreek 7.4419"
)

// sourceRow is one row of mhai_analysis_ingest_source.
type sourceRow struct {
	Ref, Kind, Sex                                   string
	Day                                              time.Time
	Condition, Location, Source                      *uuid.UUID
	AgeBand, Setting, Specialty, Urgency, Confidence *string
	Rating, Correction, Flag, Severity, Dismiss      *string
	AckMinutes                                       *int
}

// emrWorld is a hospital switched on (real references), with one
// connection and one ward.
type emrWorld struct {
	h, conn, ward uuid.UUID
	zone          string
}

func newEMRWorld(t *testing.T, pool *pgxpool.Pool, zone string) emrWorld {
	t.Helper()
	h := onHospital(t, pool, zone)
	w := emrWorld{h: h, conn: seedConn(t, pool, h), zone: zone}
	w.ward = w.unit(t, pool, "ward-kestrel", "Synthetic Ward Kestrel", "ward")
	return w
}

func (w emrWorld) unit(t *testing.T, pool *pgxpool.Pool, src, label, kind string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `INSERT INTO emr.units (hospital_id, connection_id, source_location_id, label, kind)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`, w.h, w.conn, src, label, kind).Scan(&id); err != nil {
		t.Fatalf("seed unit: %v", err)
	}
	return id
}

// patient is an active patient with the distinctive name and an MRN.
func (w emrWorld) patient(t *testing.T, pool *pgxpool.Pool, birth, sex string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `INSERT INTO emr.patients (hospital_id, display_name, mrn, birth_date, sex)
		VALUES ($1, $2, $3, $4::date, $5) RETURNING id`, w.h, an10Name, an10MRN+uuid.NewString()[:8], birth, sex).Scan(&id); err != nil {
		t.Fatalf("seed patient: %v", err)
	}
	return id
}

// consultation is a question naming patient (or none) with a decision at
// at.
func (w emrWorld) consultation(t *testing.T, pool *pgxpool.Pool, patient *uuid.UUID, at time.Time, hga string, trust float64,
	degraded bool, icd string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	q := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, patient_id, created_at)
		VALUES ($1, $2, 'synthetic question about Vantreek', 'urgent', $3, $4)`, q, w.h, patient, at); err != nil {
		t.Fatalf("seed question: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO clinical_decisions (id, hospital_id, query_id, hga_decision, trust_score, degraded,
		icd10_code, diagnosis, response_text, created_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, NULLIF($6, ''), 'Synthetic diagnosis Vantreek', 'synthetic answer', $7)`,
		w.h, q, hga, trust, degraded, icd, at); err != nil {
		t.Fatalf("seed decision: %v", err)
	}
	return q
}

// encounter is an inpatient encounter of p in unit (or none).
func (w emrWorld) encounter(t *testing.T, pool *pgxpool.Pool, p uuid.UUID, class string, unit *uuid.UUID) uuid.UUID {
	t.Helper()
	s := "enc-" + uuid.NewString()[:8]
	raw := seedRaw(t, pool, w.h, w.conn, "encounter", s, "1", `{"patient": "synthetic"}`)
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `INSERT INTO emr.encounters (hospital_id, patient_id, class, status, period_start,
		unit_id, bed_label, raw_id, source_kind, connection_id, connector_type, connector_version, source_id, source_version,
		received_at, mapping_version, normaliser_version)
		VALUES ($1, $2, $3, 'in-progress', now() - interval '3 days', $4, 'Bed Vantreek', $5, 'connector', $6, 'fhir_r4', 1, $7,
		        '1', now(), 'synthetic-map', 'n1') RETURNING id`, w.h, p, class, unit, raw, w.conn, s).Scan(&id); err != nil {
		t.Fatalf("seed encounter: %v", err)
	}
	return id
}

// result is a laboratory observation of p, effective at at.
func (w emrWorld) result(t *testing.T, pool *pgxpool.Pool, p uuid.UUID, enc *uuid.UUID, flag *string, value, low, high float64,
	at time.Time) uuid.UUID {
	t.Helper()
	s := "obs-" + uuid.NewString()[:8]
	raw := seedRaw(t, pool, w.h, w.conn, "observation", s, "1", `{"patient": "synthetic", "value_quantity": "`+an10Value+`"}`)
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `INSERT INTO emr.observations (hospital_id, patient_id, encounter_id, category,
		loinc, local_display, value_num, unit_ucum, value_si, ref_low, ref_high, ref_text, source_flag, status, effective_at,
		standardised, raw_id, source_kind, connection_id, connector_type, connector_version, source_id, source_version, received_at,
		mapping_version, normaliser_version)
		VALUES ($1, $2, $3, 'laboratory', '99901-1', 'Synthetic test Vantreek', $4, 'mmol/L', $4, $5, $6, 'Synthetic range Vantreek',
		        $7, 'final', $8, true, $9, 'connector', $10, 'fhir_r4', 1, $11, '1', now(), 'synthetic-map', 'n1') RETURNING id`,
		w.h, p, enc, value, low, high, flag, at, raw, w.conn, s).Scan(&id); err != nil {
		t.Fatalf("seed observation: %v", err)
	}
	return id
}

// finding is a finding of p on obs: status, shadow, and the finding it
// amends (or none).
func (w emrWorld) finding(t *testing.T, pool *pgxpool.Pool, p, obs uuid.UUID, status string, shadow bool, supersedes *uuid.UUID,
	at time.Time) uuid.UUID {
	t.Helper()
	var reason *string
	if status == "withdrawn" {
		r := "source_retracted"
		reason = &r
	}
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `INSERT INTO emr.findings (hospital_id, patient_id, connection_id, rule_set_version,
		rule_set_sha256, rule_id, kind, category, severity, primary_observation_id, causes, effective_at, received_at, status,
		withdrawn_reason, shadow, preliminary, arrived_late, standardised, supersedes, dedup_key)
		VALUES ($1, $2, $3, '1', $4, 'potassium-critical-high', 'threshold', 'blood_chemistry', 'critical', $5,
		        '[{"text": "Synthetic cause Vantreek"}]', $6, now(), $7, $8, $9, false, false, true, $10, $11) RETURNING id`,
		w.h, p, w.conn, emrSHA[:], obs, at, status, reason, shadow, supersedes, "an10:"+uuid.NewString()).Scan(&id); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	return id
}

// alert is an alert of p for f, raised at raised; status "handled" or
// "dismissed" settles it by clinician by after minutes; "withdrawn" is
// withdrawn by the source; "new" leaves it open.
func (w emrWorld) alert(t *testing.T, pool *pgxpool.Pool, p, f uuid.UUID, rule, severity, status string, by uuid.UUID,
	raised time.Time, minutes int) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO alerts (hospital_id, finding_id, patient_id, connection_id, rule_id, loinc, category,
		severity, routed_queue, created_at) VALUES ($1, $2, $3, $4, $5, '99901-1', 'blood_chemistry', $6, true, $7) RETURNING id`,
		w.h, f, p, w.conn, rule, severity, raised).Scan(&id); err != nil {
		t.Fatalf("seed alert: %v", err)
	}
	settled := raised.Add(time.Duration(minutes) * time.Minute)
	var q string
	var args []any
	switch status {
	case "handled":
		q, args = `UPDATE alerts SET status = 'handled', handled_by = $2, handled_at = $3 WHERE id = $1`, []any{id, by, settled}
	case "dismissed":
		q, args = `UPDATE alerts SET status = 'dismissed', handled_by = $2, handled_at = $3, dismiss_reason = 'already_known',
			dismiss_note = $4 WHERE id = $1`, []any{id, by, settled, an10Note}
	case "withdrawn":
		q, args = `UPDATE alerts SET status = 'dismissed', handled_at = $2, dismiss_reason = 'other', withdrawn = 'source_amended'
			WHERE id = $1`, []any{id, settled}
	default:
		return id
	}
	if _, err := pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("settle alert: %v", err)
	}
	return id
}

// dbNow is the database's clock: the job's functions refuse a later time.
func dbNow(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(context.Background(), `SELECT now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

// source reads mhai_analysis_ingest_source as the job, committing (it
// writes the dictionaries).
func source(t *testing.T, pool *pgxpool.Pool, h uuid.UUID, after *time.Time, until time.Time) []sourceRow {
	t.Helper()
	ctx := context.Background()
	var out []sourceRow
	if err := asAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT ref, day, kind, condition_id, age_band, sex, setting, location_id, specialty, source_id,
			urgency, confidence, rating, correction, flag, severity, ack_minutes, dismiss_reason
			FROM mhai_analysis_ingest_source($1, $2, $3)`, h, after, until)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r sourceRow
			if err := rows.Scan(&r.Ref, &r.Day, &r.Kind, &r.Condition, &r.AgeBand, &r.Sex, &r.Setting, &r.Location, &r.Specialty,
				&r.Source, &r.Urgency, &r.Confidence, &r.Rating, &r.Correction, &r.Flag, &r.Severity, &r.AckMinutes,
				&r.Dismiss); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("ingest source: %v", err)
	}
	return out
}

func ofKind(rows []sourceRow, kind string) []sourceRow {
	var out []sourceRow
	for _, r := range rows {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// testHMAC stands in for core's reference HMAC (the key never reaches
// SQL): the same reference always gives the same 32 bytes.
func testHMAC(ref string) string {
	sum := sha256.Sum256([]byte("an10-synthetic-hmac:" + ref))
	return hex.EncodeToString(sum[:])
}

func testEnc(ref string) string {
	sum := sha256.Sum256([]byte("an10-synthetic-enc:" + ref))
	return base64.StdEncoding.EncodeToString(append(sum[:], sum[:12]...))
}

// ingestRows writes source rows as facts, as Go would (with the test
// HMAC), and moves the watermark to until.
func ingestRows(t *testing.T, pool *pgxpool.Pool, h uuid.UUID, until time.Time, rows []sourceRow) {
	t.Helper()
	by := map[string][]map[string]any{}
	var order []string
	for _, r := range rows {
		if _, ok := by[r.Ref]; !ok {
			order = append(order, r.Ref)
		}
		f := map[string]any{"day": r.Day.Format(time.DateOnly), "kind": r.Kind, "sex": r.Sex}
		for k, v := range map[string]any{"condition": r.Condition, "location": r.Location, "source": r.Source} {
			if p := v.(*uuid.UUID); p != nil {
				f[k] = p.String()
			}
		}
		for k, v := range map[string]*string{"age_band": r.AgeBand, "setting": r.Setting, "specialty": r.Specialty,
			"urgency": r.Urgency, "confidence": r.Confidence, "rating": r.Rating, "correction": r.Correction, "flag": r.Flag,
			"severity": r.Severity, "dismiss_reason": r.Dismiss} {
			if v != nil {
				f[k] = *v
			}
		}
		if r.AckMinutes != nil {
			f["ack_minutes"] = *r.AckMinutes
		}
		by[r.Ref] = append(by[r.Ref], f)
	}
	subjects := []map[string]any{}
	for _, ref := range order {
		subjects = append(subjects, map[string]any{"ref_hmac": testHMAC(ref), "ref_enc": testEnc(ref), "facts": by[ref]})
	}
	b, err := json.Marshal(map[string]any{"subjects": subjects})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest(context.Background(), pool, h, until, string(b)); err != nil {
		t.Fatalf("ingest: %v", err)
	}
}

// putFacts writes facts for the subject of ref directly (days chosen by
// the test), as ingest would.
func putFacts(t *testing.T, pool *pgxpool.Pool, h uuid.UUID, until time.Time, ref string, facts ...fact) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"subjects": []map[string]any{{"ref_hmac": testHMAC(ref), "ref_enc": testEnc(ref),
		"facts": facts}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest(context.Background(), pool, h, until, string(b)); err != nil {
		t.Fatalf("put facts: %v", err)
	}
}

type change struct{ Kind, Ref, Survivor string }

func patientChanges(t *testing.T, pool *pgxpool.Pool, h uuid.UUID, until time.Time) []change {
	t.Helper()
	ctx := context.Background()
	var out []change
	if err := asAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT kind, ref, COALESCE(survivor, '') FROM mhai_analysis_patient_changes($1, $2)`, h, until)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c change
			if err := rows.Scan(&c.Kind, &c.Ref, &c.Survivor); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("patient changes: %v", err)
	}
	return out
}

func applyChanges(ctx context.Context, pool *pgxpool.Pool, h uuid.UUID, until time.Time, changes ...map[string]any) (int, error) {
	if changes == nil {
		changes = []map[string]any{}
	}
	b, err := json.Marshal(map[string]any{"changes": changes})
	if err != nil {
		return 0, err
	}
	var n int
	err = asAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mhai_analysis_apply_changes($1, $2, $3::jsonb)`, h, until, string(b)).Scan(&n)
	})
	return n, err
}

func forget(ref string) map[string]any {
	return map[string]any{"kind": "forget", "ref_hmac": testHMAC(ref)}
}

func merge(from, to string) map[string]any {
	return map[string]any{"kind": "merge", "ref_hmac": testHMAC(from), "to_hmac": testHMAC(to), "to_enc": testEnc(to)}
}

// subjectOf is the subject of ref in h, or uuid.Nil.
func subjectOf(t *testing.T, pool *pgxpool.Pool, h uuid.UUID, ref string) uuid.UUID {
	t.Helper()
	var s uuid.UUID
	err := pool.QueryRow(context.Background(), `SELECT subject_id FROM analysis_subjects WHERE hospital_id = $1 AND ref_hmac = $2`,
		h, mustHex(testHMAC(ref))).Scan(&s)
	if err == pgx.ErrNoRows {
		return uuid.Nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// frozenState is every fact and first day of h in a frozen block, as text:
// what any body of a frozen period is computed from.
func frozenState(t *testing.T, pool *pgxpool.Pool, h uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT
		COALESCE((SELECT string_agg(concat_ws('|', f.id, f.subject_id, f.day, f.kind, f.condition_id, f.age_band, f.sex, f.setting,
			f.location_id, f.source_id, f.flag, f.severity, f.ack_minutes, f.period_late, f.ingested_at), ';' ORDER BY f.id)
		 FROM analysis_facts f WHERE f.hospital_id = $1 AND mhai_analysis_block_frozen(f.hospital_id, f.day)), '')
		|| '#' ||
		COALESCE((SELECT string_agg(concat_ws('|', fs.subject_id, fs.condition_id, fs.first_day, fs.first_late), ';'
			ORDER BY fs.subject_id, fs.condition_id)
		 FROM analysis_first_seen fs WHERE fs.hospital_id = $1 AND mhai_analysis_block_frozen(fs.hospital_id, fs.first_day)
		   AND EXISTS (SELECT 1 FROM analysis_facts f WHERE f.hospital_id = fs.hospital_id AND f.subject_id = fs.subject_id
		               AND f.condition_id = fs.condition_id AND mhai_analysis_block_frozen(f.hospital_id, f.day))), '')
		|| '#' ||
		COALESCE((SELECT string_agg(concat_ws('|', p.level, p.start, p.frozen_at), ';' ORDER BY p.level, p.start)
		 FROM analysis_periods p WHERE p.hospital_id = $1), '')`, h).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Task 27: a consultation counts only when it names an active patient of
// its hospital; one with no patient, or one whose patient was excluded or
// merged away, gives no row.
func TestConsultationWithoutReferenceNoFact(t *testing.T) {
	_, pool := shared(t)
	w := newEMRWorld(t, pool, "Africa/Lagos")
	now := dbNow(t, pool)
	at := now.Add(-2 * time.Hour)
	w.consultation(t, pool, nil, at, "PASS", 0.9, false, "E11.9")
	p := w.patient(t, pool, "1983-06-09", "male")
	w.consultation(t, pool, &p, at, "PASS", 0.9, false, "E11.9")
	gone := w.patient(t, pool, "1990-01-02", "female")
	w.consultation(t, pool, &gone, at, "PASS", 0.9, false, "E11.9")
	if _, err := pool.Exec(context.Background(), `UPDATE emr.patients SET status = 'excluded', excluded_at = now(),
		exclude_reason = 'objection' WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	// Another hospital's consultation with a patient is never read.
	o := newEMRWorld(t, pool, "Africa/Lagos")
	op := o.patient(t, pool, "1983-06-09", "male")
	o.consultation(t, pool, &op, at, "PASS", 0.9, false, "E11.9")

	rows := ofKind(source(t, pool, w.h, nil, dbNow(t, pool)), "consultation")
	if len(rows) != 1 || rows[0].Ref != p.String() {
		t.Fatalf("consultation rows %+v, want one for the active patient", rows)
	}
}

// Task 27: codes come from the patient index (age band at the day, sex)
// and the decision's ICD-10 code, entered with the code as its name; an
// invalid code gives no condition.
func TestConsultationFactFromPatientIndex(t *testing.T) {
	_, pool := shared(t)
	w := newEMRWorld(t, pool, "Pacific/Kiritimati")
	now := dbNow(t, pool)
	at := now.Add(-3 * time.Hour)
	p := w.patient(t, pool, "1983-06-09", "male")
	unknown := w.patient(t, pool, "1950-02-03", "unknown")
	w.consultation(t, pool, &p, at, "PASS", 0.95, false, "e11.9")
	w.consultation(t, pool, &unknown, at, "PASS", 0.95, false, "not-a-code")
	rows := ofKind(source(t, pool, w.h, nil, dbNow(t, pool)), "consultation")
	if len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
	loc, _ := time.LoadLocation(w.zone)
	day := at.In(loc).Format(time.DateOnly)
	age := at.In(loc).Year() - 1983
	if at.In(loc).YearDay() < time.Date(at.In(loc).Year(), 6, 9, 0, 0, 0, 0, loc).YearDay() {
		age--
	}
	band := map[bool]string{true: "40-64", false: "18-39"}[age >= 40]
	for _, r := range rows {
		if r.Day.Format(time.DateOnly) != day || str(r.Urgency) != "urgent" || r.Location != nil || r.Source != nil ||
			r.Setting != nil || r.Specialty != nil || r.Rating != nil || r.Correction != nil {
			t.Errorf("row %+v: day %s want %s", r, r.Day.Format(time.DateOnly), day)
		}
		switch r.Ref {
		case p.String():
			if str(r.AgeBand) != band || r.Sex != "male" || r.Condition == nil {
				t.Errorf("patient row %+v (band want %s)", r, band)
			}
			var icd, name string
			if err := pool.QueryRow(context.Background(), `SELECT icd10, name FROM analysis_conditions WHERE hospital_id = $1 AND id = $2`,
				w.h, *r.Condition).Scan(&icd, &name); err != nil || icd != "E11.9" || name != "E11.9" {
				t.Errorf("condition %s %s (%v)", icd, name, err)
			}
		case unknown.String():
			if str(r.AgeBand) != "65-79" || r.Sex != "not_recorded" || r.Condition != nil {
				t.Errorf("unknown row %+v", r)
			}
		default:
			t.Errorf("unexpected ref %s", r.Ref)
		}
	}
}

// Task 27 (H2's to confirm): the confidence mapping, pinned.
func TestConfidenceMappingPinned(t *testing.T) {
	_, pool := shared(t)
	w := newEMRWorld(t, pool, "Africa/Lagos")
	at := dbNow(t, pool).Add(-time.Hour)
	want := map[uuid.UUID]string{}
	for _, c := range []struct {
		hga      string
		trust    float64
		degraded bool
		want     string
	}{
		{"PASS", 0.8, false, "high"}, {"PASS", 0.79, false, "caution"}, {"REGENERATE", 0.9, false, "review"},
		{"ESCALATE", 0.9, false, "review"}, {"PASS", 0.95, true, "failed"},
	} {
		p := w.patient(t, pool, "1971-03-04", "female")
		w.consultation(t, pool, &p, at, c.hga, c.trust, c.degraded, "J45")
		want[p] = c.want
	}
	rows := ofKind(source(t, pool, w.h, nil, dbNow(t, pool)), "consultation")
	if len(rows) != len(want) {
		t.Fatalf("rows %d", len(rows))
	}
	for _, r := range rows {
		if got := str(r.Confidence); got != want[uuid.MustParse(r.Ref)] {
			t.Errorf("confidence %s, want %s", got, want[uuid.MustParse(r.Ref)])
		}
	}
}

// Task 27: rows older than the 25 months put_facts takes are left out, so
// a first run after a long time off doesn't fail.
func TestSourceKeepsTwentyFiveMonths(t *testing.T) {
	_, pool := shared(t)
	w := newEMRWorld(t, pool, "Africa/Lagos")
	now := dbNow(t, pool)
	old := w.patient(t, pool, "1971-03-04", "female")
	kept := w.patient(t, pool, "1971-03-04", "female")
	w.consultation(t, pool, &old, now.AddDate(0, -26, 0), "PASS", 0.9, false, "J45")
	w.consultation(t, pool, &kept, now.AddDate(0, -24, 0), "PASS", 0.9, false, "J45")
	until := dbNow(t, pool)
	rows := source(t, pool, w.h, nil, until)
	if len(rows) != 1 || rows[0].Ref != kept.String() {
		t.Fatalf("rows %+v", rows)
	}
	ingestRows(t, pool, w.h, until, rows) // put_facts takes them
}

// Task 28: findings and alerts as facts: each kept and left-out case.
func TestFindingAndAlertFacts(t *testing.T) {
	_, pool := shared(t)
	w := newEMRWorld(t, pool, "Africa/Lagos")
	now := dbNow(t, pool)
	at := now.Add(-26 * time.Hour)
	clin := seedClinician(t, pool, w.h)
	p := w.patient(t, pool, "1983-06-09", "male")
	enc := w.encounter(t, pool, p, "inpatient", &w.ward)
	hh, a := "HH", "A"
	high := w.result(t, pool, p, &enc, &hh, 7.4419, 3.5, 5.1, at)
	low := w.result(t, pool, p, &enc, nil, 2.1, 3.5, 5.1, at)
	odd := w.result(t, pool, p, nil, &a, 7.4419, 3.5, 5.1, at)
	inRange := w.result(t, pool, p, nil, nil, 4.2, 3.5, 5.1, at)
	fHigh := w.finding(t, pool, p, high, "open", false, nil, at)
	w.finding(t, pool, p, low, "historical", false, nil, at)
	w.finding(t, pool, p, odd, "open", true, nil, at)       // shadow: none
	w.finding(t, pool, p, odd, "withdrawn", false, nil, at) // withdrawn: none
	w.finding(t, pool, p, high, "open", false, &fHigh, at)  // an amendment: none
	w.finding(t, pool, p, inRange, "open", false, nil, at)
	w.alert(t, pool, p, fHigh, "r-handled", "critical", "handled", clin, at, 42)
	w.alert(t, pool, p, fHigh, "r-dismissed", "urgent", "dismissed", clin, at, 17)
	w.alert(t, pool, p, fHigh, "r-open", "info", "new", clin, at, 0)
	w.alert(t, pool, p, fHigh, "r-withdrawn", "info", "withdrawn", clin, at, 5)

	rows := source(t, pool, w.h, nil, dbNow(t, pool))
	day := at.In(mustZone(t, w.zone)).Format(time.DateOnly)
	findings := ofKind(rows, "finding")
	flags := map[string]sourceRow{}
	for _, r := range findings {
		flags[str(r.Flag)] = r
		if r.Ref != p.String() || r.Day.Format(time.DateOnly) != day || r.Condition != nil || r.Source == nil || *r.Source != w.conn ||
			str(r.AgeBand) == "" || r.Sex != "male" || r.Severity != nil {
			t.Errorf("finding row %+v", r)
		}
	}
	if len(findings) != 3 || len(flags) != 3 {
		t.Fatalf("findings %+v, want high, low and abnormal", findings)
	}
	if r := flags["high"]; str(r.Setting) != "admitted" || r.Location == nil || *r.Location != w.ward {
		t.Errorf("high finding's setting and location: %+v", r)
	}
	if r := flags["low"]; str(r.Setting) != "admitted" {
		t.Errorf("low finding: %+v", r)
	}
	if r := flags["abnormal"]; r.Setting != nil || r.Location != nil {
		t.Errorf("a finding with no encounter: %+v", r)
	}
	alerts := ofKind(rows, "alert")
	if len(alerts) != 2 {
		t.Fatalf("alerts %+v, want the handled and the dismissed", alerts)
	}
	for _, r := range alerts {
		if r.Day.Format(time.DateOnly) != day || str(r.Setting) != "admitted" || r.Location == nil || r.Flag != nil {
			t.Errorf("alert row %+v", r)
		}
		switch str(r.Severity) {
		case "critical":
			if r.AckMinutes == nil || *r.AckMinutes != 42 || r.Dismiss != nil {
				t.Errorf("handled alert %+v", r)
			}
		case "urgent":
			if r.AckMinutes == nil || *r.AckMinutes != 17 || str(r.Dismiss) != "already_known" {
				t.Errorf("dismissed alert %+v", r)
			}
		default:
			t.Errorf("alert severity %s", str(r.Severity))
		}
	}
	ingestRows(t, pool, w.h, dbNow(t, pool), rows) // put_facts takes every row
}

func mustZone(t *testing.T, zone string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// Task 28: the dictionaries from the EMR configuration, for the hospital
// only; a label the dictionary's check refuses is left out, and a fact in
// that unit carries no location.
func TestDictionariesFromEMR(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newEMRWorld(t, pool, "Africa/Lagos")
	clinic := w.unit(t, pool, "clinic-1", "Synthetic Clinic Heron", "clinic")
	er := w.unit(t, pool, "er-1", "Synthetic Emergency Ibis", "emergency")
	other := w.unit(t, pool, "other-1", "Synthetic Annex Plover", "other")
	bad := w.unit(t, pool, "bad-1", "Ward P-ABCD2345", "ward")
	o := newEMRWorld(t, pool, "Africa/Lagos")
	p := w.patient(t, pool, "1983-06-09", "male")
	enc := w.encounter(t, pool, p, "outpatient", &bad)
	obs := w.result(t, pool, p, &enc, nil, 2.1, 3.5, 5.1, dbNow(t, pool).Add(-time.Hour))
	w.finding(t, pool, p, obs, "open", false, nil, dbNow(t, pool).Add(-time.Hour))

	rows := ofKind(source(t, pool, w.h, nil, dbNow(t, pool)), "finding")
	if len(rows) != 1 || rows[0].Location != nil || str(rows[0].Setting) != "outpatient" {
		t.Fatalf("finding in a refused unit: %+v", rows)
	}
	for id, want := range map[uuid.UUID]string{w.ward: "admitted", clinic: "outpatient", er: "emergency", other: ""} {
		var setting *string
		if err := pool.QueryRow(ctx, `SELECT setting FROM analysis_locations WHERE hospital_id = $1 AND id = $2`, w.h, id).
			Scan(&setting); err != nil || str(setting) != want {
			t.Errorf("location %s: %q (%v), want %q", id, str(setting), err, want)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_locations WHERE id = $1`, bad); n != 0 {
		t.Error("a refused label was entered")
	}
	var label string
	if err := pool.QueryRow(ctx, `SELECT label FROM analysis_sources WHERE hospital_id = $1 AND id = $2`, w.h, w.conn).Scan(&label); err != nil ||
		!strings.HasPrefix(label, "Record ") {
		t.Errorf("source %q (%v)", label, err)
	}
	if n := countRows(t, pool, `SELECT (SELECT count(*) FROM analysis_sources WHERE id = $1 OR hospital_id = $2)
		+ (SELECT count(*) FROM analysis_locations WHERE id = $3 OR hospital_id = $2)`, o.conn, o.h, o.ward); n != 0 {
		t.Errorf("another hospital's configuration entered: %d", n)
	}
}

// Task 28: the writer can't read a name, an MRN, a note or a free-text
// value, and after a run nothing in any analysis table holds the synthetic
// name, MRN, value or note.
func TestNoNameOrMRNInAnyFact(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for tbl, cols := range map[string][]string{
		"emr.patients":     {"display_name", "mrn"},
		"emr.observations": {"local_display", "value_text", "ref_text", "value_num"},
		"emr.findings":     {"causes", "rule_id"},
		"emr.encounters":   {"bed_label"},
		"emr.raw_records":  {"payload"},
		"alerts":           {"dismiss_note", "amended", "handled_by"},
		"clinical_queries": {"clinical_question", "patient_data_json"},
	} {
		for _, col := range cols {
			var ok bool
			if err := pool.QueryRow(ctx, `SELECT has_column_privilege('mhai_analysis_writer', $1, $2, 'SELECT')`, tbl, col).Scan(&ok); err != nil || ok {
				t.Errorf("the writer may read %s.%s (%v)", tbl, col, err)
			}
		}
	}
	w := newEMRWorld(t, pool, "Africa/Lagos")
	at := dbNow(t, pool).Add(-26 * time.Hour)
	clin := seedClinician(t, pool, w.h)
	p := w.patient(t, pool, "1983-06-09", "male")
	enc := w.encounter(t, pool, p, "emergency", &w.ward)
	obs := w.result(t, pool, p, &enc, nil, 7.4419, 3.5, 5.1, at)
	f := w.finding(t, pool, p, obs, "open", false, nil, at)
	w.alert(t, pool, p, f, "r-note", "urgent", "dismissed", clin, at, 9)
	w.consultation(t, pool, &p, at, "PASS", 0.9, false, "I10")
	until := dbNow(t, pool)
	rows := source(t, pool, w.h, nil, until)
	if len(rows) != 3 {
		t.Fatalf("rows %d", len(rows))
	}
	b, _ := json.Marshal(rows)
	ingestRows(t, pool, w.h, until, rows)
	var dump string
	if err := pool.QueryRow(ctx, `SELECT concat(
		(SELECT string_agg(f::text, ';') FROM analysis_facts f WHERE hospital_id = $1),
		(SELECT string_agg(s::text, ';') FROM analysis_subjects s WHERE hospital_id = $1),
		(SELECT string_agg(c::text, ';') FROM analysis_conditions c WHERE hospital_id = $1),
		(SELECT string_agg(l::text, ';') FROM analysis_locations l WHERE hospital_id = $1),
		(SELECT string_agg(s::text, ';') FROM analysis_sources s WHERE hospital_id = $1),
		(SELECT string_agg(a::text, ';') FROM analysis_subject_attrs a WHERE hospital_id = $1))`, w.h).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"Vantreek", an10MRN, an10Value, "1983-06-09", p.String()} {
		if strings.Contains(dump, needle) {
			t.Errorf("an analysis table holds %q", needle)
		}
		if needle != p.String() && strings.Contains(string(b), needle) {
			t.Errorf("a source row holds %q", needle)
		}
	}
}

// Task 28 (design §10.6, the plan's conflict, departure 87): once the EMR
// purge is done, the excluded patient's subject loses its unfrozen facts,
// its unfrozen first days and its reference; nothing frozen changes, and
// another subject is untouched.
func TestExclusionForgetsUnfrozenOnly(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newEMRWorld(t, pool, "Africa/Lagos")
	cond := condition(t, pool, w.h, "J45")
	cond2 := condition(t, pool, w.h, "I10")
	now := dbNow(t, pool)
	loc := mustZone(t, w.zone)
	frozenDay := now.In(loc).AddDate(0, 0, -40).Format(time.DateOnly)
	openDay := now.In(loc).Format(time.DateOnly)
	p := w.patient(t, pool, "1983-06-09", "male")
	other := w.patient(t, pool, "1983-06-09", "female")
	putFacts(t, pool, w.h, now, p.String(),
		fact{"day": frozenDay, "kind": "diagnosis", "condition": cond.String(), "sex": "male"},
		fact{"day": openDay, "kind": "diagnosis", "condition": cond.String(), "sex": "male"},
		fact{"day": openDay, "kind": "diagnosis", "condition": cond2.String(), "sex": "male"})
	putFacts(t, pool, w.h, now, other.String(), fact{"day": openDay, "kind": "diagnosis", "condition": cond.String(), "sex": "female"})
	if _, err := freezeAt(ctx, pool, w.h, now); err != nil {
		t.Fatal(err)
	}
	before := frozenState(t, pool, w.h)
	if !strings.Contains(before, frozenDay) {
		t.Fatal("the frozen fact isn't in a frozen block")
	}
	s := subjectOf(t, pool, w.h, p.String())

	// No purge yet: nothing listed.
	if c := patientChanges(t, pool, w.h, dbNow(t, pool)); len(c) != 0 {
		t.Fatalf("changes before any exclusion: %+v", c)
	}
	if _, err := excludeAs(ctx, pool, "mhai_gateway", w.h, p); err != nil {
		t.Fatal(err)
	}
	until := dbNow(t, pool)
	c := patientChanges(t, pool, w.h, until)
	if len(c) != 1 || c[0] != (change{"excluded", p.String(), ""}) {
		t.Fatalf("changes %+v", c)
	}
	if n, err := applyChanges(ctx, pool, w.h, until, forget(c[0].Ref)); err != nil || n != 1 {
		t.Fatalf("apply: %d %v", n, err)
	}
	if after := frozenState(t, pool, w.h); after != before {
		t.Errorf("frozen state changed:\n%s\n%s", before, after)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_facts WHERE subject_id = $1 AND NOT mhai_analysis_block_frozen(hospital_id, day)`, s); n != 0 {
		t.Errorf("%d unfrozen facts left", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_first_seen WHERE subject_id = $1 AND condition_id = $2`, s, cond2); n != 0 {
		t.Error("the unfrozen first day is left")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_subjects WHERE subject_id = $1 AND ref_enc IS NULL AND ref_hmac <> $2`,
		s, mustHex(testHMAC(p.String()))); n != 1 {
		t.Error("the subject still holds its reference or its HMAC")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_facts WHERE subject_id = $1`, subjectOf(t, pool, w.h, other.String())); n != 1 {
		t.Error("another subject was touched")
	}
	// Changes are read again for a day after the watermark (a purge that
	// committed late is never missed: security review H1); applied again,
	// a forget changes nothing.
	until = dbNow(t, pool)
	again := patientChanges(t, pool, w.h, until)
	if len(again) != 1 || again[0] != c[0] {
		t.Fatalf("listed again within the overlap: %+v", again)
	}
	if n, err := applyChanges(ctx, pool, w.h, until, forget(again[0].Ref)); err != nil || n != 0 {
		t.Errorf("a second forget: %d %v", n, err)
	}
}

// Security review H1: a purge whose transaction began before the
// watermark but committed after it (done_at is the transaction's start) is
// still listed on the next run.
func TestLateCommittedPurgeIsListed(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newEMRWorld(t, pool, "Africa/Lagos")
	now := dbNow(t, pool)
	p := w.patient(t, pool, "1983-06-09", "male")
	putFacts(t, pool, w.h, now, p.String(), fact{"day": now.In(mustZone(t, w.zone)).Format(time.DateOnly), "kind": "consultation",
		"sex": "male"})
	if _, err := applyChanges(ctx, pool, w.h, dbNow(t, pool)); err != nil {
		t.Fatal(err)
	}
	// Committed now, stamped before the watermark.
	if _, err := pool.Exec(ctx, `UPDATE emr.patients SET status = 'excluded', excluded_at = now() - interval '2 hours',
		exclude_reason = 'objection' WHERE id = $1`, p); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO emr.purge_requests (hospital_id, patient_id, requested_at, due_at, state, done_at)
		VALUES ($1, $2, now() - interval '2 hours', now() - interval '2 hours', 'done', now() - interval '2 hours')`, w.h, p); err != nil {
		t.Fatal(err)
	}
	c := patientChanges(t, pool, w.h, dbNow(t, pool))
	if len(c) != 1 || c[0] != (change{"excluded", p.String(), ""}) {
		t.Fatalf("a late-committed purge: %+v", c)
	}
}

// Security review H1 (the overlap's other side): a merge read again after
// its survivor was excluded never makes a sealed subject for the excluded
// survivor; one whose survivor was merged on names the final survivor.
func TestMergeSurvivorIsTheActiveOne(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newEMRWorld(t, pool, "Africa/Lagos")
	now := dbNow(t, pool)
	day := now.In(mustZone(t, w.zone)).Format(time.DateOnly)
	a, b, c, x, y := w.patient(t, pool, "1983-06-09", "male"), w.patient(t, pool, "1983-06-09", "male"),
		w.patient(t, pool, "1983-06-09", "male"), w.patient(t, pool, "1983-06-09", "male"), w.patient(t, pool, "1983-06-09", "male")
	putFacts(t, pool, w.h, now, a.String(), fact{"day": day, "kind": "consultation", "sex": "male"})
	mergeRow := func(merged, kept uuid.UUID) {
		t.Helper()
		match := seedMatch(t, pool, w.h, merged, kept)
		if _, err := pool.Exec(ctx, `INSERT INTO emr.merges (hospital_id, match_id, kept_patient, merged_patient) VALUES ($1, $2, $3, $4)`,
			w.h, match, kept, merged); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE emr.patients SET status = 'merged_into', merged_into = $2 WHERE id = $1`, merged, kept); err != nil {
			t.Fatal(err)
		}
	}
	mergeRow(a, b) // a -> b -> c: the survivor is c
	mergeRow(b, c)
	mergeRow(x, y) // x -> y, then y excluded: no merge listed
	if _, err := pool.Exec(ctx, `UPDATE emr.patients SET status = 'excluded', excluded_at = now(), exclude_reason = 'objection'
		WHERE id = $1`, y); err != nil {
		t.Fatal(err)
	}
	got := map[change]bool{}
	for _, ch := range patientChanges(t, pool, w.h, dbNow(t, pool)) {
		got[ch] = true
	}
	if !got[change{"merged", a.String(), c.String()}] || !got[change{"merged", b.String(), c.String()}] {
		t.Errorf("survivors: %v", got)
	}
	for ch := range got {
		if ch.Survivor == y.String() || ch.Survivor == b.String() || ch.Ref == x.String() {
			t.Errorf("listed %+v", ch)
		}
	}
}

// Task 28: under a legal hold the purge waits, and so does the forget;
// once the hold is lifted and the purge done, it is listed.
func TestExclusionWaitsForLegalHold(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newEMRWorld(t, pool, "Africa/Lagos")
	now := dbNow(t, pool)
	p := w.patient(t, pool, "1983-06-09", "male")
	putFacts(t, pool, w.h, now, p.String(), fact{"day": now.In(mustZone(t, w.zone)).Format(time.DateOnly), "kind": "consultation",
		"sex": "male"})
	hold := placeHold(t, pool, w.h)
	x, err := excludeAs(ctx, pool, "mhai_gateway", w.h, p)
	if err != nil || !x.held {
		t.Fatalf("held exclusion: %+v %v", x, err)
	}
	if c := patientChanges(t, pool, w.h, dbNow(t, pool)); len(c) != 0 {
		t.Fatalf("listed under the hold: %+v", c)
	}
	liftHold(t, pool, hold)
	var released []uuid.UUID
	if err := maint(ctx, pool, w.h, `SELECT emr_release_holds()`, &released); err != nil {
		t.Fatal(err)
	}
	var res json.RawMessage
	if err := maint(ctx, pool, w.h, `SELECT emr_purge_patient($1)`, &res, x.request); err != nil || !jsonHas(res, "state", "done") {
		t.Fatalf("purge: %s %v", res, err)
	}
	until := dbNow(t, pool)
	c := patientChanges(t, pool, w.h, until)
	if len(c) != 1 || c[0].Kind != "excluded" || c[0].Ref != p.String() {
		t.Fatalf("after the purge: %+v", c)
	}
	// Forgotten with no fact left, the subject keeps no attributes
	// (security review L2).
	s := subjectOf(t, pool, w.h, p.String())
	if _, err := applyChanges(ctx, pool, w.h, until, forget(p.String())); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_subject_attrs WHERE subject_id = $1`, s); n != 0 {
		t.Error("a forgotten subject with no facts keeps its attributes")
	}
}

// Task 28 (departure 88): a merge moves the merged subject's unfrozen
// facts to the survivor and gives it the merged subject's first days,
// late where frozen; nothing frozen changes. A patient merged into an
// excluded one is forgotten with it.
func TestMergeMovesUnfrozenOnly(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newEMRWorld(t, pool, "Africa/Lagos")
	cond := condition(t, pool, w.h, "J45")
	cond2 := condition(t, pool, w.h, "E11")
	now := dbNow(t, pool)
	loc := mustZone(t, w.zone)
	frozenDay := now.In(loc).AddDate(0, 0, -40).Format(time.DateOnly)
	d1 := now.In(loc).AddDate(0, 0, -1).Format(time.DateOnly)
	d2 := now.In(loc).Format(time.DateOnly)
	merged := w.patient(t, pool, "1983-06-09", "male")
	kept := w.patient(t, pool, "1983-06-09", "male")
	putFacts(t, pool, w.h, now, merged.String(),
		fact{"day": frozenDay, "kind": "diagnosis", "condition": cond.String(), "sex": "male"},
		fact{"day": d1, "kind": "diagnosis", "condition": cond.String(), "sex": "male"},
		fact{"day": d1, "kind": "diagnosis", "condition": cond2.String(), "sex": "male"})
	putFacts(t, pool, w.h, now, kept.String(), fact{"day": d2, "kind": "diagnosis", "condition": cond2.String(), "sex": "male"})
	if _, err := freezeAt(ctx, pool, w.h, now); err != nil {
		t.Fatal(err)
	}
	before := frozenState(t, pool, w.h)
	m, k := subjectOf(t, pool, w.h, merged.String()), subjectOf(t, pool, w.h, kept.String())

	match := seedMatch(t, pool, w.h, merged, kept)
	if _, err := pool.Exec(ctx, `INSERT INTO emr.merges (hospital_id, match_id, kept_patient, merged_patient) VALUES ($1, $2, $3, $4)`,
		w.h, match, kept, merged); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE emr.patients SET status = 'merged_into', merged_into = $2 WHERE id = $1`, merged, kept); err != nil {
		t.Fatal(err)
	}
	until := dbNow(t, pool)
	c := patientChanges(t, pool, w.h, until)
	if len(c) != 1 || c[0] != (change{"merged", merged.String(), kept.String()}) {
		t.Fatalf("changes %+v", c)
	}
	if n, err := applyChanges(ctx, pool, w.h, until, merge(merged.String(), kept.String())); err != nil || n != 1 {
		t.Fatalf("apply: %d %v", n, err)
	}
	if after := frozenState(t, pool, w.h); after != before {
		t.Errorf("frozen state changed:\n%s\n%s", before, after)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_facts WHERE subject_id = $1`, m); n != 1 {
		t.Errorf("the merged subject keeps %d facts, want its frozen one", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_facts WHERE subject_id = $1 AND day = $2::date`, k, d1); n != 2 {
		t.Errorf("the survivor has %d of the moved facts", n)
	}
	var day string
	var late bool
	if err := pool.QueryRow(ctx, `SELECT first_day::text, first_late FROM analysis_first_seen WHERE subject_id = $1 AND condition_id = $2`,
		k, cond).Scan(&day, &late); err != nil || day != frozenDay || !late {
		t.Errorf("survivor's first day for a case the merged record had before: %s late %v (%v)", day, late, err)
	}
	if err := pool.QueryRow(ctx, `SELECT first_day::text, first_late FROM analysis_first_seen WHERE subject_id = $1 AND condition_id = $2`,
		k, cond2).Scan(&day, &late); err != nil || day != d1 || late {
		t.Errorf("survivor's first day for cond2: %s late %v (%v)", day, late, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_first_seen WHERE subject_id = $1 AND condition_id = $2`, m, cond2); n != 0 {
		t.Error("the merged subject keeps an unfrozen first day")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_subjects WHERE subject_id = $1 AND ref_enc IS NOT NULL`, m); n != 1 {
		t.Error("the merged subject lost its reference")
	}

	// A survivor with no subject yet is made, sealed.
	lone := w.patient(t, pool, "1983-06-09", "male")
	putFacts(t, pool, w.h, until, lone.String(), fact{"day": d2, "kind": "consultation", "sex": "male"})
	fresh := w.patient(t, pool, "1983-06-09", "male")
	if _, err := applyChanges(ctx, pool, w.h, dbNow(t, pool), merge(lone.String(), fresh.String())); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_facts f JOIN analysis_subjects s USING (subject_id)
		WHERE s.ref_hmac = $1 AND s.ref_enc IS NOT NULL AND s.hospital_id = $2`, mustHex(testHMAC(fresh.String())), w.h); n != 1 {
		t.Errorf("the new survivor's subject holds %d facts", n)
	}

	// Excluding the survivor lists it and the patient merged into it.
	if _, err := excludeAs(ctx, pool, "mhai_gateway", w.h, kept); err != nil {
		t.Fatal(err)
	}
	got := map[change]bool{}
	for _, ch := range patientChanges(t, pool, w.h, dbNow(t, pool)) {
		got[ch] = true
	}
	if !got[change{"excluded", kept.String(), ""}] || !got[change{"excluded", merged.String(), ""}] {
		t.Errorf("after excluding the survivor: %v", got)
	}
}

// Task 28: the apply function refuses what isn't its shape, a watermark
// moving back and the future; only the job may run either function; a
// change acts only in its own hospital.
func TestApplyChangesRefusals(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newEMRWorld(t, pool, "Africa/Lagos")
	o := newEMRWorld(t, pool, "Africa/Lagos")
	now := dbNow(t, pool)
	today := now.In(mustZone(t, w.zone)).Format(time.DateOnly)
	putFacts(t, pool, o.h, now, "shared-ref", fact{"day": today, "kind": "consultation", "sex": "female"})
	for name, body := range map[string]string{
		"not an object":   `[]`,
		"unknown key":     `{"changes": [], "extra": 1}`,
		"unknown kind":    `{"changes": [{"kind": "rename", "ref_hmac": "` + testHMAC("x") + `"}]}`,
		"bad hmac":        `{"changes": [{"kind": "forget", "ref_hmac": "zz"}]}`,
		"extra field":     `{"changes": [{"kind": "forget", "ref_hmac": "` + testHMAC("x") + `", "name": "Vantreek"}]}`,
		"merge, no enc":   `{"changes": [{"kind": "merge", "ref_hmac": "` + testHMAC("x") + `", "to_hmac": "` + testHMAC("y") + `"}]}`,
		"merge, bad enc":  `{"changes": [{"kind": "merge", "ref_hmac": "` + testHMAC("x") + `", "to_hmac": "` + testHMAC("y") + `", "to_enc": "!!"}]}`,
		"merge into self": `{"changes": [{"kind": "merge", "ref_hmac": "` + testHMAC("x") + `", "to_hmac": "` + testHMAC("x") + `", "to_enc": "` + testEnc("x") + `"}]}`,
	} {
		err := asAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT mhai_analysis_apply_changes($1, $2, $3::jsonb)`, w.h, now, body)
			return err
		})
		if pgCode(err) != "22023" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := applyChanges(ctx, pool, w.h, now.Add(time.Hour)); pgCode(err) != "22023" {
		t.Errorf("the future: %v", err)
	}
	later := dbNow(t, pool)
	if _, err := applyChanges(ctx, pool, w.h, later); err != nil {
		t.Fatal(err)
	}
	if _, err := applyChanges(ctx, pool, w.h, later.Add(-time.Minute)); pgCode(err) != "22023" {
		t.Errorf("the watermark moving back: %v", err)
	}
	// A forget of the other hospital's reference, applied to w: o's
	// subject is untouched.
	if n, err := applyChanges(ctx, pool, w.h, dbNow(t, pool), forget("shared-ref")); err != nil || n != 0 {
		t.Errorf("forget in another hospital: %d %v", n, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_subjects WHERE hospital_id = $1 AND ref_enc IS NOT NULL`, o.h); n != 1 {
		t.Error("another hospital's subject was forgotten")
	}
	for _, role := range []string{"mhai_gateway", "mhai_app", "mhai_platform", "mhai_provision", "mhai_jobs", "mhai_emr"} {
		for _, q := range []string{
			`SELECT count(*) FROM mhai_analysis_patient_changes('` + w.h.String() + `', now())`,
			`SELECT mhai_analysis_apply_changes('` + w.h.String() + `', now(), '{"changes": []}')`,
		} {
			err := asAnalysisRole(ctx, pool, role, &w.h, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, q)
				return err
			})
			requireDenied(t, err, role+" "+q)
		}
	}
}

// Task 28 (departure 89): a hospital holding a sealed reference is due
// (its exclusions are applied after its facts aged out); one holding only
// the demo's unsealed subjects, with no facts, isn't.
func TestDueListsSealedHospitals(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	sealed, _ := seedHospital(t, pool, uniqueCode("SL"))
	bare, _ := seedHospital(t, pool, uniqueCode("BR"))
	for h, enc := range map[uuid.UUID]any{sealed: mustB64(testEnc("sealed")), bare: nil} {
		if _, err := pool.Exec(ctx, `INSERT INTO analysis_subjects (hospital_id, ref_hmac, ref_enc, created_day)
			VALUES ($1, $2, $3, '2026-01-05')`, h, mustHex(testHMAC(h.String())), enc); err != nil {
			t.Fatal(err)
		}
	}
	// 02:00 in Lagos on a day after both were made.
	at := time.Date(2026, 3, 10, 1, 0, 0, 0, time.UTC)
	due := map[uuid.UUID]bool{}
	if err := asAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT hospital_id FROM mhai_analysis_due($1)`, at)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		for _, id := range ids {
			due[id] = true
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !due[sealed] || due[bare] {
		t.Errorf("due: sealed %v, bare %v", due[sealed], due[bare])
	}
}

func mustB64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// Down to 00044 restores 00021's source and due functions and takes every
// grant, column and function 00046 added; up again works.
func TestMigration00046DownUp(t *testing.T) {
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
	var src, due string
	if err := pool.QueryRow(ctx, `SELECT pg_get_functiondef('mhai_analysis_ingest_source(uuid, timestamptz, timestamptz)'::regprocedure),
		pg_get_functiondef('mhai_analysis_due(timestamptz)'::regprocedure)`).Scan(&src, &due); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 44); err != nil {
		t.Fatalf("down to 44: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_proc WHERE proname IN ('mhai_analysis_patient_changes', 'mhai_analysis_apply_changes'))
		+ (SELECT count(*) FROM information_schema.columns WHERE table_name = 'analysis_watermarks' AND column_name = 'changes_until')
		+ (SELECT count(*) FROM pg_policies WHERE policyname = 'analysis_writer_emr')
		+ (SELECT count(*) FROM unnest(ARRAY['emr.patients', 'emr.findings', 'emr.observations', 'emr.encounters', 'emr.units',
		   'emr.merges', 'emr.purge_requests', 'emr_connections', 'alerts']) t
		   WHERE has_any_column_privilege('mhai_analysis_writer', t, 'SELECT'))
		+ (SELECT count(*) FROM pg_namespace WHERE nspname = 'emr' AND has_schema_privilege('mhai_analysis_writer', oid, 'USAGE'))
		+ (CASE WHEN has_column_privilege('mhai_analysis_writer', 'clinical_queries', 'patient_id', 'SELECT') THEN 1 ELSE 0 END)
		+ (CASE WHEN has_column_privilege('mhai_analysis_writer', 'analysis_subjects', 'ref_enc', 'UPDATE') THEN 1 ELSE 0 END)`).
		Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("after down, %d objects or grants of 00046 remain", left)
	}
	var oldSrc, oldDue string
	if err := pool.QueryRow(ctx, `SELECT pg_get_functiondef('mhai_analysis_ingest_source(uuid, timestamptz, timestamptz)'::regprocedure),
		pg_get_functiondef('mhai_analysis_due(timestamptz)'::regprocedure)`).Scan(&oldSrc, &oldDue); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(oldSrc, "AND false") || strings.Contains(oldSrc, "emr.") || oldSrc == src ||
		strings.Contains(oldDue, "ref_enc") || oldDue == due {
		t.Errorf("00021's functions not restored:\n%s\n%s", oldSrc, oldDue)
	}
	var owner string
	if err := pool.QueryRow(ctx, `SELECT pg_get_userbyid(proowner) FROM pg_proc WHERE proname = 'mhai_analysis_ingest_source'`).Scan(&owner); err != nil ||
		owner != "mhai_analysis_writer" {
		t.Errorf("source owner %s (%v)", owner, err)
	}
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT has_function_privilege('mhai_analysis_job', 'mhai_analysis_ingest_source(uuid, timestamptz, timestamptz)', 'EXECUTE')
		AND NOT has_function_privilege('mhai_gateway', 'mhai_analysis_ingest_source(uuid, timestamptz, timestamptz)', 'EXECUTE')`).Scan(&ok); err != nil || !ok {
		t.Errorf("source grants after down (%v)", err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
