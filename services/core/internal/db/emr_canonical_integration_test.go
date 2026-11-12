//go:build integration

package db_test

import (
	"context"
	"crypto/sha256"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00023 (EMR plan Task 16; design §8, §10, §13.2): raw records,
// the canonical tables, the per-hospital patient index and core's patient
// functions.

const (
	fkViolation = "23503"
	noDataFound = "P0002"
)

var emrSHA = sha256.Sum256([]byte("synthetic"))

// seedRaw inserts a connector raw record as the superuser.
func seedRaw(t *testing.T, pool *pgxpool.Pool, hospital, conn uuid.UUID, kind, sourceID, version string, payload string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `INSERT INTO emr.raw_records (hospital_id, source_kind, connection_id,
		resource_kind, source_id, source_version, payload, payload_sha256, connector_type, connector_version)
		VALUES ($1, 'connector', $2, $3, $4, $5, $6::jsonb, $7, 'fhir_r4', 1) RETURNING id`,
		hospital, conn, kind, sourceID, version, payload, emrSHA[:]).Scan(&id); err != nil {
		t.Fatalf("seed raw: %v", err)
	}
	return id
}

// seedPatient inserts a patient with a link to (conn, sourceID).
func seedPatient(t *testing.T, pool *pgxpool.Pool, hospital, conn uuid.UUID, sourceID, mrn string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO emr.patients (hospital_id, display_name, mrn, birth_date, sex)
		VALUES ($1, 'Synthetic Patient', $2, '1970-01-01', 'female') RETURNING id`, hospital, mrn).Scan(&id); err != nil {
		t.Fatalf("seed patient: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO emr.patient_links (hospital_id, connection_id, source_patient_id, patient_id)
		VALUES ($1, $2, $3, $4)`, hospital, conn, sourceID, id); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	return id
}

// seedObservation inserts an observation of patient from a new raw record.
func seedObservation(t *testing.T, pool *pgxpool.Pool, hospital, conn, patient uuid.UUID, sourcePatient string) uuid.UUID {
	t.Helper()
	src := "obs-" + uuid.NewString()[:8]
	raw := seedRaw(t, pool, hospital, conn, "observation", src, "1", `{"patient": "`+sourcePatient+`", "value_quantity": "5.1"}`)
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `INSERT INTO emr.observations (hospital_id, patient_id, category, loinc,
		value_num, unit_ucum, value_si, status, effective_at, standardised, raw_id, source_kind, connection_id, connector_type,
		connector_version, source_id, source_version, received_at, mapping_version, normaliser_version)
		VALUES ($1, $2, 'laboratory', '99901-1', 5.1, 'mmol/L', 5.1, 'final', now(), true, $3, 'connector', $4, 'fhir_r4', 1,
		        $5, '1', now(), 'synthetic-map', 'n1') RETURNING id`, hospital, patient, raw, conn, src).Scan(&id); err != nil {
		t.Fatalf("seed observation: %v", err)
	}
	return id
}

func seedMatch(t *testing.T, pool *pgxpool.Pool, hospital, a, b uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `INSERT INTO emr.possible_matches (hospital_id, patient_a, patient_b, reason)
		VALUES ($1, $2, $3, 'mrn_birth_sex') RETURNING id`, hospital, a, b).Scan(&id); err != nil {
		t.Fatalf("seed match: %v", err)
	}
	return id
}

// asRoleIn runs fn in a transaction as role with the hospital set.
func asRoleIn(ctx context.Context, pool *pgxpool.Pool, role string, hospital uuid.UUID, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+role); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, hospital.String()); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var emrCanonicalTables = []string{"emr.raw_records", "emr.patients", "emr.patient_links", "emr.possible_matches",
	"emr.merges", "emr.units", "emr.encounters", "emr.observations", "emr.medications", "emr.unmapped_codes"}

// A26: a document raw record needs its document and its review, and has no
// connection; a connector record has a connection and no document.
func TestDocumentRawRecordNeedsReview(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("DR"))
	conn := seedConn(t, pool, h)
	insert := func(kind string, conn, doc, review any) error {
		_, err := pool.Exec(ctx, `INSERT INTO emr.raw_records (hospital_id, source_kind, connection_id, document_id, review_id,
			resource_kind, source_id, payload_sha256, connector_type, connector_version)
			VALUES ($1, $2, $3, $4, $5, 'observation', $6, $7, 'document', 1)`, h, kind, conn, doc, review, uuid.NewString(), emrSHA[:])
		return err
	}
	d, r := uuid.New(), uuid.New()
	for name, args := range map[string][]any{
		"document without review":      {"document", nil, d, nil},
		"document without document":    {"document", nil, nil, r},
		"document with a connection":   {"document", conn, d, r},
		"connector with a document":    {"connector", conn, d, nil},
		"connector without connection": {"connector", nil, nil, nil},
	} {
		if err := insert(args[0].(string), args[1], args[2], args[3]); pgCode(err) != checkViolation {
			t.Errorf("%s: %v, want a check violation", name, err)
		}
	}
	if err := insert("document", nil, d, r); err != nil {
		t.Fatalf("reviewed document record: %v", err)
	}
}

// ADR 0009 rule 5: a link ties a connection and a patient of one hospital.
func TestPatientLinkSameHospitalOnly(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("LA"))
	b, _ := seedHospital(t, pool, uniqueCode("LB"))
	ca := seedConn(t, pool, a)
	pb := seedPatient(t, pool, b, seedConn(t, pool, b), "p-b", "MRN-B")
	for _, h := range []uuid.UUID{a, b} {
		_, err := pool.Exec(ctx, `INSERT INTO emr.patient_links (hospital_id, connection_id, source_patient_id, patient_id)
			VALUES ($1, $2, 'p-x', $3)`, h, ca, pb)
		if pgCode(err) != fkViolation {
			t.Errorf("A's connection linked to B's patient (hospital %v): %v, want a key violation", h == a, err)
		}
	}
	// Canonical rows and matches name patients of their own hospital only.
	pa := seedPatient(t, pool, a, ca, "p-a", "MRN-A")
	if _, err := pool.Exec(ctx, `INSERT INTO emr.possible_matches (hospital_id, patient_a, patient_b, reason)
		VALUES ($1, $2, $3, 'mrn_birth_sex')`, a, pa, pb); pgCode(err) != fkViolation {
		t.Errorf("a match across hospitals: %v", err)
	}
}

// Design §8.1: the same source record version is stored once.
func TestRawRecordReplayIsNoOp(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("RP"))
	conn := seedConn(t, pool, h)
	seedRaw(t, pool, h, conn, "observation", "o-1", "1", `{}`)
	for i := 0; i < 3; i++ {
		tag, err := pool.Exec(ctx, `INSERT INTO emr.raw_records (hospital_id, source_kind, connection_id, resource_kind,
			source_id, source_version, payload_sha256, connector_type, connector_version)
			VALUES ($1, 'connector', $2, 'observation', 'o-1', '1', $3, 'fhir_r4', 1) ON CONFLICT DO NOTHING`, h, conn, emrSHA[:])
		if err != nil || tag.RowsAffected() != 0 {
			t.Fatalf("replay %d: %d rows, %v", i, tag.RowsAffected(), err)
		}
	}
	seedRaw(t, pool, h, conn, "observation", "o-1", "2", `{}`) // a new version is a new row
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM emr.raw_records WHERE connection_id = $1`, conn).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows %d %v, want 2", n, err)
	}
}

// A13: as mhai_emr with hospital A set, no row of B, and an insert naming
// B fails WITH CHECK; the gateway reads the alert columns of A only, and
// nothing of the raw records, links or matches.
func TestEMRCanonicalRLS(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("CA"))
	b, _ := seedHospital(t, pool, uniqueCode("CB"))
	ca, cb := seedConn(t, pool, a), seedConn(t, pool, b)
	for _, s := range []struct{ h, c uuid.UUID }{{a, ca}, {b, cb}} {
		p := seedPatient(t, pool, s.h, s.c, "p-1", "MRN-1")
		p2 := seedPatient(t, pool, s.h, s.c, "p-2", "MRN-2")
		seedObservation(t, pool, s.h, s.c, p, "p-1")
		seedMatch(t, pool, s.h, p, p2)
		if _, err := pool.Exec(ctx, `INSERT INTO emr.unmapped_codes (hospital_id, connection_id, kind, local_system, local_code)
			VALUES ($1, $2, 'observation', 'urn:synthetic', 'X1')`, s.h, s.c); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"emr.raw_records", "emr.patients", "emr.patient_links", "emr.possible_matches",
		"emr.observations", "emr.unmapped_codes"} {
		var hs []uuid.UUID
		if err := asRoleIn(ctx, pool, "mhai_emr", a, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT DISTINCT hospital_id FROM `+table+` WHERE hospital_id IN ($1, $2)`, a, b)
			if err != nil {
				return err
			}
			hs, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			return err
		}); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if len(hs) != 1 || hs[0] != a {
			t.Errorf("%s as A: %v, want A only", table, hs)
		}
		if n, err := countAs(t, pool, "mhai_emr", `SELECT count(*) FROM `+table); err != nil || n != 0 {
			t.Errorf("%s with no hospital: %d %v", table, n, err)
		}
	}
	for what, q := range map[string]string{
		"patient": `INSERT INTO emr.patients (hospital_id, sex) VALUES ($1, 'unknown')`,
		"raw": `INSERT INTO emr.raw_records (hospital_id, source_kind, connection_id, resource_kind, source_id, payload_sha256,
			connector_type, connector_version) VALUES ($1, 'connector', '` + cb.String() + `', 'patient', 'x', '\x` +
			strings.Repeat("00", 32) + `', 'fhir_r4', 1)`,
		"unmapped": `INSERT INTO emr.unmapped_codes (hospital_id, connection_id, kind, local_system, local_code)
			VALUES ($1, '` + cb.String() + `', 'unit', 's', 'c')`,
	} {
		if _, err := asEMR(ctx, pool, &a, q, b); pgCode(err) != rlsViolation {
			t.Errorf("%s for B as A: %v, want an RLS refusal", what, err)
		}
	}
	// The gateway: the alert columns, its hospital only.
	var n int
	if err := asRoleIn(ctx, pool, "mhai_gateway", a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM emr.patients p JOIN emr.observations o
			ON o.patient_id = p.id AND o.hospital_id = p.hospital_id WHERE p.hospital_id IN ($1, $2)`, a, b).Scan(&n)
	}); err != nil || n != 1 {
		t.Errorf("gateway as A: %d rows %v, want 1", n, err)
	}
	for _, q := range []string{`SELECT count(*) FROM emr.raw_records`, `SELECT count(*) FROM emr.patient_links`,
		`SELECT count(*) FROM emr.possible_matches`, `SELECT count(*) FROM emr.merges`, `SELECT count(*) FROM emr.unmapped_codes`,
		`SELECT count(raw_id) FROM emr.observations`, `SELECT count(*) FROM emr.patients WHERE origin = 'connector'`} {
		err := asRoleIn(ctx, pool, "mhai_gateway", a, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
		if pgCode(err) != permissionDenied {
			t.Errorf("gateway %q: %v, want permission denied", q, err)
		}
	}
	for _, role := range []string{"mhai_worker", "mhai_platform", "mhai_jobs", "mhai_auth", "mhai_emr_sched", "mhai_emr_core"} {
		for _, table := range emrCanonicalTables {
			var held bool
			if err := pool.QueryRow(ctx, `SELECT has_table_privilege($1, $2, 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE')
				OR has_any_column_privilege($1, $2, 'SELECT,INSERT,UPDATE')`, role, table).Scan(&held); err != nil {
				t.Fatal(err)
			}
			if held {
				t.Errorf("%s holds a privilege on %s", role, table)
			}
		}
	}
}

// Design §10.2, §10.6: the emr service can't merge, exclude or delete:
// no EXECUTE on core's functions, no DELETE, and no column that marks a
// patient merged or excluded or decides a match.
func TestEMRCannotMergeOrDelete(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for _, fn := range []string{"emr_merge_patients(uuid, text)", "emr_exclude_patient(uuid, text)", "emr_patient_in_hospital(uuid)"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT has_function_privilege('mhai_emr', $1, 'EXECUTE')`, fn).Scan(&ok); err != nil || ok {
			t.Errorf("mhai_emr may execute %s (%v)", fn, err)
		}
	}
	for _, table := range emrCanonicalTables {
		var del bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege('mhai_emr', $1, 'DELETE') OR has_table_privilege('mhai_emr', $1, 'TRUNCATE')`, table).Scan(&del); err != nil || del {
			t.Errorf("mhai_emr may delete from %s (%v)", table, err)
		}
	}
	for table, cols := range map[string][]string{
		"emr.patients":         {"status", "merged_into", "excluded_at", "exclude_reason", "hospital_id", "id", "origin"},
		"emr.possible_matches": {"decision", "decided_at", "patient_a", "patient_b"},
		"emr.patient_links":    {"patient_id", "merge_id"},
		"emr.observations":     {"patient_id", "merge_id", "value_num", "loinc", "hospital_id"},
		"emr.raw_records":      {"hospital_id", "source_id", "payload_sha256"},
	} {
		for _, c := range cols {
			var ok bool
			if err := pool.QueryRow(ctx, `SELECT has_column_privilege('mhai_emr', $1, $2, 'UPDATE')`, table, c).Scan(&ok); err != nil || ok {
				t.Errorf("mhai_emr may update %s.%s (%v)", table, c, err)
			}
		}
	}
	for _, c := range []string{"status", "merged_into", "excluded_at", "origin"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT has_column_privilege('mhai_emr', 'emr.patients', $1, 'INSERT')`, c).Scan(&ok); err != nil || ok {
			t.Errorf("mhai_emr may insert emr.patients.%s (%v)", c, err)
		}
	}
	h, _ := seedHospital(t, pool, uniqueCode("ND"))
	conn := seedConn(t, pool, h)
	p := seedPatient(t, pool, h, conn, "p-1", "MRN-1")
	if _, err := asEMR(ctx, pool, &h, `DELETE FROM emr.patients WHERE id = $1`, p); pgCode(err) != permissionDenied {
		t.Errorf("mhai_emr deleting a patient: %v", err)
	}
	if _, err := asEMR(ctx, pool, &h, `SELECT emr_exclude_patient($1, 'other')`, p); pgCode(err) != permissionDenied {
		t.Errorf("mhai_emr excluding: %v", err)
	}
}

// Design §10.2, §17.4: two merges racing on one pair: one wins, the other
// errors; a merged patient can't be merged again; a match of another
// hospital is not found.
func TestMergeRechecksAndLocks(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("MG"))
	other, _ := seedHospital(t, pool, uniqueCode("MO"))
	c1, c2 := seedConn(t, pool, h), seedConn(t, pool, h)
	pa := seedPatient(t, pool, h, c1, "p-a", "MRN-7")
	pb := seedPatient(t, pool, h, c2, "p-b", "MRN-7")
	obs := seedObservation(t, pool, h, c2, pb, "p-b")
	m := seedMatch(t, pool, h, pa, pb)

	merge := func(hospital uuid.UUID, match uuid.UUID, keep string) (uuid.UUID, error) {
		var id uuid.UUID
		err := asRoleIn(ctx, pool, "mhai_gateway", hospital, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT emr_merge_patients($1, $2)`, match, keep).Scan(&id)
		})
		return id, err
	}
	if _, err := merge(other, m, "a"); pgCode(err) != noDataFound {
		t.Fatalf("merge as another hospital: %v, want not found", err)
	}
	if _, err := merge(h, m, "c"); pgCode(err) != noDataFound {
		t.Fatalf("merge keeping neither: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	ids := make([]uuid.UUID, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keep := "a"
			if i%2 == 1 {
				keep = "b"
			}
			ids[i], errs[i] = merge(h, m, keep)
		}(i)
	}
	wg.Wait()
	won := 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case pgCode(err) != noDataFound:
			t.Errorf("merge %d: %v", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("%d merges won, want 1", won)
	}
	var statusA, statusB string
	var into *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT (SELECT status FROM emr.patients WHERE id = $1), (SELECT status FROM emr.patients WHERE id = $2)`, pa, pb).Scan(&statusA, &statusB); err != nil {
		t.Fatal(err)
	}
	kept, gone := pa, pb
	if statusA == "merged_into" {
		kept, gone = pb, pa
	}
	if err := pool.QueryRow(ctx, `SELECT merged_into FROM emr.patients WHERE id = $1`, gone).Scan(&into); err != nil || into == nil || *into != kept {
		t.Fatalf("merged_into %v %v", into, err)
	}
	var owner uuid.UUID
	var links int
	if err := pool.QueryRow(ctx, `SELECT patient_id FROM emr.observations WHERE id = $1`, obs).Scan(&owner); err != nil || owner != kept {
		t.Errorf("observation patient %v %v, want the kept one", owner, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM emr.patient_links WHERE patient_id = $1 AND merge_id IS NOT NULL`, kept).Scan(&links); err != nil || links != 1 {
		t.Errorf("moved links %d %v", links, err)
	}
	// A merged patient can't be merged again, under a new match either.
	pc := seedPatient(t, pool, h, c1, "p-c", "MRN-7")
	m2 := seedMatch(t, pool, h, gone, pc)
	if _, err := merge(h, m2, "b"); pgCode(err) != noDataFound {
		t.Errorf("merging a merged patient: %v", err)
	}
}

// SKILLS §1: the reserved ids can never be patient ids (they are not uuids,
// and the CHECK says so anyway).
func TestReservedIDsRefused(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("RI"))
	for _, id := range []string{"reference", "unassigned", "unknown", "ref"} {
		if _, err := pool.Exec(ctx, `INSERT INTO emr.patients (id, hospital_id) VALUES ($1, $2)`, id, h); err == nil {
			t.Errorf("patient id %q accepted", id)
		}
	}
	var def string
	if err := pool.QueryRow(ctx, `SELECT pg_get_expr(adbin, adrelid) FROM pg_attrdef
		WHERE adrelid = 'emr.patients'::regclass AND adnum = 1`).Scan(&def); err != nil || def != "gen_random_uuid()" {
		t.Errorf("patients.id default %q %v, want gen_random_uuid()", def, err)
	}
}

// Design §10.6 (product owner, 11 Nov): excluding a patient purges its
// data at once: canonical rows, raw records and possible matches go; the
// patient row stays, cleared, with its links, so later records are dropped.
// Another patient's data and another hospital's are untouched.
func TestExclusionPurgesAtOnce(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("EX"))
	other, _ := seedHospital(t, pool, uniqueCode("EO"))
	c := seedConn(t, pool, h)
	p := seedPatient(t, pool, h, c, "p-x", "MRN-X")
	keep := seedPatient(t, pool, h, c, "p-k", "MRN-K")
	seedRaw(t, pool, h, c, "patient", "p-x", "1", `{"given": "Synthetic"}`)
	seedObservation(t, pool, h, c, p, "p-x")
	seedObservation(t, pool, h, c, p, "p-x")
	kept := seedObservation(t, pool, h, c, keep, "p-k")
	seedMatch(t, pool, h, p, keep)
	rejected := seedRaw(t, pool, h, c, "observation", "o-rej", "1", `{"patient": "p-x", "value_quantity": "5,5"}`)

	exclude := func(hospital, pid uuid.UUID, reason string) ([5]int64, error) {
		var n [5]int64
		err := asRoleIn(ctx, pool, "mhai_gateway", hospital, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT * FROM emr_exclude_patient($1, $2)`, pid, reason).Scan(&n[0], &n[1], &n[2], &n[3], &n[4])
		})
		return n, err
	}
	if _, err := exclude(other, p, "objection"); pgCode(err) != noDataFound {
		t.Fatalf("exclude from another hospital: %v", err)
	}
	if _, err := exclude(h, p, "because"); err == nil {
		t.Fatal("exclude with a free-text reason accepted")
	}
	n, err := exclude(h, p, "objection")
	if err != nil {
		t.Fatal(err)
	}
	if n != [5]int64{2, 0, 0, 4, 1} {
		t.Errorf("counts %v, want 2 observations, 4 raw records (2 behind them, the patient's own, the rejected one), 1 match", n)
	}
	var status string
	var name, mrn *string
	if err := pool.QueryRow(ctx, `SELECT status, display_name, mrn FROM emr.patients WHERE id = $1`, p).Scan(&status, &name, &mrn); err != nil ||
		status != "excluded" || name != nil || mrn != nil {
		t.Errorf("excluded patient: %s %v %v %v", status, name, mrn, err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM emr.observations WHERE patient_id = $1)
		+ (SELECT count(*) FROM emr.raw_records WHERE id = $2 OR source_id = 'p-x')
		+ (SELECT count(*) FROM emr.possible_matches WHERE patient_a = $1 OR patient_b = $1)`, p, rejected).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d rows of the excluded patient remain (%v)", left, err)
	}
	var links, others int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM emr.patient_links WHERE patient_id = $1`, p).Scan(&links); err != nil || links != 1 {
		t.Errorf("links %d %v, want the link kept", links, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM emr.observations WHERE id = $1`, kept).Scan(&others); err != nil || others != 1 {
		t.Errorf("another patient's observation: %d %v", others, err)
	}
	// Excluding again is a no-op; the patient-in-hospital check says no.
	if n, err := exclude(h, p, "other"); err != nil || n != [5]int64{} {
		t.Errorf("exclude again: %v %v", n, err)
	}
	var in bool
	if err := asRoleIn(ctx, pool, "mhai_gateway", h, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT emr_patient_in_hospital($1)`, p).Scan(&in)
	}); err != nil || in {
		t.Errorf("excluded patient in hospital: %v %v", in, err)
	}
	for hid, want := range map[uuid.UUID]bool{h: true, other: false} {
		if err := asRoleIn(ctx, pool, "mhai_gateway", hid, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT emr_patient_in_hospital($1)`, keep).Scan(&in)
		}); err != nil || in != want {
			t.Errorf("patient in hospital (own %v): %v %v", want, in, err)
		}
	}
}

// K2 and the definer rules for 00023: owner mhai_migrate, the pinned
// search_path, EXECUTE for mhai_gateway and mhai_emr_admin only; a temp
// table can't shadow what they read. Every new table: RLS on and forced,
// owner mhai_migrate.
func TestEMRCanonicalDefiners(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for _, name := range []string{"emr_patient_in_hospital", "emr_merge_patients", "emr_exclude_patient"} {
		var owner string
		var definer bool
		var config, grantees []string
		if err := pool.QueryRow(ctx, `
			SELECT pg_get_userbyid(p.proowner), p.prosecdef, COALESCE(p.proconfig, '{}'),
			       COALESCE((SELECT array_agg(CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END ORDER BY 1)
			                 FROM aclexplode(p.proacl) a
			                 WHERE a.privilege_type = 'EXECUTE' AND a.grantee <> p.proowner), '{}')
			FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'public' AND p.proname = $1`, name).Scan(&owner, &definer, &config, &grantees); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if owner != "mhai_migrate" || !definer || len(config) != 1 || config[0] != "search_path=pg_catalog, pg_temp" {
			t.Errorf("%s: owner %s definer %v config %v", name, owner, definer, config)
		}
		slices.Sort(grantees)
		if strings.Join(grantees, ",") != "mhai_emr_admin,mhai_gateway" {
			t.Errorf("%s: EXECUTE held by %v", name, grantees)
		}
	}
	for _, table := range emrCanonicalTables {
		var rls, force bool
		var owner string
		if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity, pg_get_userbyid(relowner) FROM pg_class WHERE oid = $1::regclass`, table).
			Scan(&rls, &force, &owner); err != nil {
			t.Fatal(err)
		}
		if !rls || !force || owner != "mhai_migrate" {
			t.Errorf("%s: rls %v force %v owner %s", table, rls, force, owner)
		}
	}
	h, _ := seedHospital(t, pool, uniqueCode("TT"))
	p := seedPatient(t, pool, h, seedConn(t, pool, h), "p-1", "MRN-1")
	if err := asRoleIn(ctx, pool, "mhai_gateway", h, func(tx pgx.Tx) error {
		for _, q := range []string{
			`CREATE TEMP TABLE patients (id uuid, hospital_id uuid, status text) ON COMMIT DROP`,
			`INSERT INTO pg_temp.patients VALUES ('` + p.String() + `', '` + h.String() + `', 'excluded')`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		var in bool
		if err := tx.QueryRow(ctx, `SELECT emr_patient_in_hospital($1)`, p).Scan(&in); err != nil {
			return err
		}
		if !in {
			t.Error("a temp table changed emr_patient_in_hospital")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// 00023 goes down to 00022 and up again: its tables, functions and the
// gateway's schema usage are gone; 00020's objects stay.
func TestMigration00023DownUp(t *testing.T) {
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
	if err := db.MigrateDownTo(ctx, pg.URL, 22); err != nil {
		t.Fatalf("down to 22: %v", err)
	}
	var left, kept int
	var usage bool
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'emr' AND c.relname IN ('raw_records', 'patients', 'patient_links', 'possible_matches', 'merges',
			'units', 'encounters', 'observations', 'medications', 'unmapped_codes'))
		+ (SELECT count(*) FROM pg_proc WHERE proname IN ('emr_patient_in_hospital', 'emr_merge_patients', 'emr_exclude_patient'))`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE oid IN ('emr.credentials'::regclass, 'emr_connections'::regclass)`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT has_schema_privilege('mhai_gateway', 'emr', 'USAGE')`).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if left != 0 || kept != 2 || usage {
		t.Errorf("after down: %d of 00023's objects left, %d of 00020's kept, gateway usage %v", left, kept, usage)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// Race review 1: the exclusion takes the emr service's link lock (the same
// advisory key) before it purges, so a normaliser transaction holding it
// finishes first and what it wrote is purged too.
func TestExclusionWaitsForLinkLock(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("XL"))
	c := seedConn(t, pool, h)
	p := seedPatient(t, pool, h, c, "p-lock", "MRN-L")
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "emr-link:"+c.String()+":p-lock"); err != nil {
		t.Fatal(err)
	}
	// The holder stands for a normaliser transaction on this source id
	// that hasn't reached the patient row yet.
	raw := seedRaw(t, pool, h, c, "observation", "o-lock", "1", `{"patient": "p-lock"}`)
	done := make(chan error, 1)
	go func() {
		done <- asRoleIn(ctx, pool, "mhai_gateway", h, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT * FROM emr_exclude_patient($1, 'objection')`, p)
			return err
		})
	}()
	select {
	case err := <-done:
		t.Fatalf("the exclusion didn't wait for the link lock: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM emr.observations WHERE patient_id = $1)
		+ (SELECT count(*) FROM emr.raw_records WHERE id = $2)`, p, raw).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d rows left after the exclusion (%v)", left, err)
	}
}
