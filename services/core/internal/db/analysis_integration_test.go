//go:build integration

package db_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata" // the zones the freeze tests pin

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00021 (analysis backend plan, Task 2): the analysis roles,
// facts, nested frozen periods, the switch and the job's functions. Every
// id, reference and fact is synthetic, and every date is pinned: the
// functions take the time they act at, so no test depends on today.

// analysisTables are the tables 00021 creates.
var analysisTables = []string{"analysis_switch", "analysis_sources", "analysis_locations", "analysis_conditions",
	"analysis_subjects", "analysis_facts", "analysis_first_seen", "analysis_periods", "analysis_pseudonyms",
	"analysis_watermarks"}

// analysisDictionaries are the ones the gateway may read, for its hospital.
var analysisDictionaries = map[string]bool{"analysis_sources": true, "analysis_locations": true, "analysis_conditions": true}

// analysisJobCalls are the job's functions, each callable with a nil-safe
// argument set.
var analysisJobCalls = []string{
	`SELECT count(*) FROM mhai_analysis_ingest_source('00000000-0000-0000-0000-000000000001', NULL, '2025-01-01')`,
	`SELECT mhai_analysis_ingest('00000000-0000-0000-0000-000000000001', '2025-01-01', '{"subjects": []}')`,
	`SELECT mhai_analysis_freeze('00000000-0000-0000-0000-000000000001', '2025-01-01')`,
	`SELECT mhai_analysis_sweep('00000000-0000-0000-0000-000000000001', '2025-01-01')`,
	`SELECT count(*) FROM mhai_analysis_due('2025-01-01')`,
	`SELECT mhai_analysis_ran('00000000-0000-0000-0000-000000000001', '2025-01-01')`,
}

// at is a pinned instant: local midnight of day in zone, plus d.
func at(t *testing.T, zone, day string, d time.Duration) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	v, err := time.ParseInLocation(time.DateOnly, day, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v.Add(d)
}

// asAnalysisRole runs fn as role (with the engine version set, and the
// hospital when not nil), committing when fn returns nil.
func asAnalysisRole(ctx context.Context, pool *pgxpool.Pool, role string, hospital *uuid.UUID, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+pgx.Identifier{role}.Sanitize()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('mhai.analysis.engine', '1', true)`); err != nil {
		return err
	}
	if hospital != nil {
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, hospital.String()); err != nil {
			return err
		}
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func asAnalysisJob(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return asAnalysisRole(ctx, pool, "mhai_analysis_job", nil, fn)
}

// setSwitch replaces a switch row as the superuser (nil hospital: the
// global row). ref "" is off with no references.
func setSwitch(t *testing.T, pool *pgxpool.Pool, hospital *uuid.UUID, position, ref string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM analysis_switch WHERE hospital_id IS NOT DISTINCT FROM $1`, hospital); err != nil {
		t.Fatal(err)
	}
	var r any
	if ref != "" {
		r = ref
	}
	if _, err := pool.Exec(ctx, `INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		VALUES ($1, $2, $3, $3, $3)`, hospital, position, r); err != nil {
		t.Fatal(err)
	}
}

// demoHospital seeds a hospital marked as the synthetic demo (is_demo,
// set only when a hospital is created).
func demoHospital(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	h := uuid.New()
	if _, err := pool.Exec(context.Background(), `INSERT INTO hospitals (id, name, short_code, status, time_zone, is_demo)
		VALUES ($1, 'Synthetic Demo Hospital', $2, 'active', 'Africa/Lagos', true)`, h, uniqueCode("Y")); err != nil {
		t.Fatal(err)
	}
	return h
}

// onHospital seeds a hospital in zone with its switch and the global
// switch on, with real (SO-) references.
func onHospital(t *testing.T, pool *pgxpool.Pool, zone string) uuid.UUID {
	t.Helper()
	h, _ := seedHospital(t, pool, uniqueCode("N"))
	if _, err := pool.Exec(context.Background(), `UPDATE hospitals SET time_zone = $2 WHERE id = $1`, h, zone); err != nil {
		t.Fatal(err)
	}
	setSwitch(t, pool, nil, "on", "SO-2026-0001")
	setSwitch(t, pool, &h, "on", "SO-2026-0002")
	return h
}

// condition adds a coded condition to h's dictionary.
func condition(t *testing.T, pool *pgxpool.Pool, h uuid.UUID, icd10 string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `INSERT INTO analysis_conditions (hospital_id, id, icd10, name) VALUES ($1, $2, $3, $4)`,
		h, id, icd10, "Synthetic condition "+icd10); err != nil {
		t.Fatal(err)
	}
	return id
}

// ref is a synthetic subject reference: a random HMAC and ciphertext.
type ref struct{ hmac, enc string }

func newRef() ref {
	a, b := make([]byte, 32), make([]byte, 40)
	_, _ = rand.Read(a)
	_, _ = rand.Read(b)
	return ref{hex.EncodeToString(a), base64.StdEncoding.EncodeToString(b)}
}

type fact map[string]any

func factsJSON(t *testing.T, subjects map[ref][]fact) string {
	t.Helper()
	var out []map[string]any
	for r, fs := range subjects {
		out = append(out, map[string]any{"ref_hmac": r.hmac, "ref_enc": r.enc, "facts": fs})
	}
	if out == nil {
		out = []map[string]any{}
	}
	b, err := json.Marshal(map[string]any{"subjects": out})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func ingest(ctx context.Context, pool *pgxpool.Pool, h uuid.UUID, until time.Time, facts string) (int, error) {
	var late int
	err := asAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mhai_analysis_ingest($1, $2, $3::jsonb)`, h, until, facts).Scan(&late)
	})
	return late, err
}

func freezeAt(ctx context.Context, pool *pgxpool.Pool, h uuid.UUID, now time.Time) (int, error) {
	var n int
	err := asAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mhai_analysis_freeze($1, $2)`, h, now).Scan(&n)
	})
	return n, err
}

func frozen(t *testing.T, pool *pgxpool.Pool, h uuid.UUID, level, start string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM analysis_periods
		WHERE hospital_id = $1 AND level = $2 AND start = $3::date AND frozen_at IS NOT NULL AND engine_version = '1')`,
		h, level, start).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func countRows(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Every analysis table: owned by mhai_migrate, RLS forced, nothing for
// PUBLIC. The gateway with no hospital sees no dictionary row and is
// refused every other table; with a hospital, its own dictionaries only.
// It inserts and updates nothing.
func TestAnalysisTablesRLS(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a := onHospital(t, pool, "Africa/Lagos")
	b := onHospital(t, pool, "Africa/Lagos")
	ca, cb := condition(t, pool, a, "E11"), condition(t, pool, b, "E11")
	for _, h := range []uuid.UUID{a, b} {
		if _, err := pool.Exec(ctx, `INSERT INTO analysis_sources (hospital_id, label) VALUES ($1, 'Synthetic EMR')`, h); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO analysis_locations (hospital_id, label, setting) VALUES ($1, 'Ward 1', 'admitted')`, h); err != nil {
			t.Fatal(err)
		}
	}
	for _, tbl := range analysisTables {
		var rls, force, public bool
		var owner string
		if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity, pg_get_userbyid(relowner),
			has_table_privilege('public', oid, 'SELECT,INSERT,UPDATE,DELETE') FROM pg_class WHERE relname = $1`, tbl).
			Scan(&rls, &force, &owner, &public); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
		if !rls || !force || owner != "mhai_migrate" || public {
			t.Errorf("%s: rls %v force %v owner %s public %v", tbl, rls, force, owner, public)
		}
		var n int
		err := asAnalysisRole(ctx, pool, "mhai_gateway", nil, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n)
		})
		if analysisDictionaries[tbl] {
			if err != nil || n != 0 {
				t.Errorf("gateway without a hospital reads %s: %d rows (%v)", tbl, n, err)
			}
			err = asAnalysisRole(ctx, pool, "mhai_gateway", &a, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM `+tbl+` WHERE hospital_id <> $1`, a).Scan(&n)
			})
			if err != nil || n != 0 {
				t.Errorf("gateway at a reads %d of another hospital's %s rows (%v)", n, tbl, err)
			}
		} else {
			requireDenied(t, err, "gateway reading "+tbl)
		}
		err = asAnalysisRole(ctx, pool, "mhai_gateway", &a, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO `+tbl+` (hospital_id) VALUES ($1)`, a)
			return err
		})
		requireDenied(t, err, "gateway inserting into "+tbl)
		err = asAnalysisRole(ctx, pool, "mhai_gateway", &a, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE `+tbl+` SET hospital_id = hospital_id`)
			return err
		})
		requireDenied(t, err, "gateway updating "+tbl)
	}
	var seen []uuid.UUID
	if err := asAnalysisRole(ctx, pool, "mhai_gateway", &a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT array_agg(id) FROM analysis_conditions WHERE id IN ($1, $2)`, ca, cb).Scan(&seen)
	}); err != nil || len(seen) != 1 || seen[0] != ca {
		t.Errorf("gateway at a sees conditions %v (%v), want only its own", seen, err)
	}
}

// T22: the worker roles hold nothing on any analysis table or function.
func TestWorkerRolesSeeNoAnalysis(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	var fns []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(oid::regprocedure::text) FROM pg_proc WHERE proname LIKE 'mhai_analysis%'`).Scan(&fns); err != nil {
		t.Fatal(err)
	}
	if len(fns) < 10 {
		t.Fatalf("only %d analysis functions: %v", len(fns), fns)
	}
	for _, role := range []string{"mhai_worker", "mhai_worker_app"} {
		for _, tbl := range analysisTables {
			var any bool
			if err := pool.QueryRow(ctx, `SELECT has_table_privilege($1, $2, 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
				OR has_any_column_privilege($1, $2, 'SELECT,INSERT,UPDATE,REFERENCES')`, role, tbl).Scan(&any); err != nil || any {
				t.Errorf("%s holds a privilege on %s (%v)", role, tbl, err)
			}
			err := asAnalysisRole(ctx, pool, role, nil, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `SELECT 1 FROM `+tbl+` LIMIT 1`)
				return err
			})
			requireDenied(t, err, role+" reading "+tbl)
		}
		for _, fn := range fns {
			var ok bool
			if err := pool.QueryRow(ctx, `SELECT has_function_privilege($1, $2, 'EXECUTE')`, role, fn).Scan(&ok); err != nil || ok {
				t.Errorf("%s may execute %s (%v)", role, fn, err)
			}
		}
	}
}

// T24: the gateway writes nothing directly, and runs none of the job's or
// the seed's functions.
func TestGatewayCannotWriteAnalysis(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h := onHospital(t, pool, "Africa/Lagos")
	for _, q := range []string{
		`INSERT INTO analysis_facts (hospital_id, subject_id, day, kind, sex, period_late) VALUES ($1, gen_random_uuid(), '2025-01-01', 'diagnosis', 'female', false)`,
		`INSERT INTO analysis_subjects (hospital_id, ref_hmac, created_day) VALUES ($1, '\x00', '2025-01-01')`,
		`INSERT INTO analysis_periods (hospital_id, level, start, "end", engine_version) VALUES ($1, 'week', '2025-01-01', '2025-01-07', '1')`,
		`INSERT INTO analysis_switch (hospital_id, position) VALUES ($1, 'off')`,
		`UPDATE analysis_switch SET position = 'off' WHERE hospital_id = $1`,
		`DELETE FROM analysis_facts WHERE hospital_id = $1`,
		`DELETE FROM analysis_switch WHERE hospital_id = $1`,
		`UPDATE analysis_periods SET frozen_at = now() WHERE hospital_id = $1`,
		`SELECT count(*) FROM analysis_pseudonyms WHERE hospital_id = $1`,
	} {
		err := asAnalysisRole(ctx, pool, "mhai_gateway", &h, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, h)
			return err
		})
		requireDenied(t, err, "gateway: "+q)
	}
	for _, q := range append(append([]string(nil), analysisJobCalls...),
		`SELECT mhai_analysis_seed('00000000-0000-0000-0000-000000000001', '2025-01-01', '{}')`) {
		err := asAnalysisRole(ctx, pool, "mhai_gateway", &h, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q)
			return err
		})
		requireDenied(t, err, "gateway: "+q)
	}
}

// T25 (SQL half): a hospital that is off, or whose global is off, is
// refused by ingest and its source, and nothing is written.
func TestIngestRefusesSwitchOff(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	off, _ := seedHospital(t, pool, uniqueCode("O"))
	on := onHospital(t, pool, "Africa/Lagos")
	c := condition(t, pool, off, "E11")
	until := at(t, "Africa/Lagos", "2026-03-01", 0)
	body := factsJSON(t, map[ref][]fact{newRef(): {{"day": "2026-02-10", "kind": "diagnosis", "condition": c.String(), "sex": "female"}}})
	check := func(what string, h uuid.UUID) {
		t.Helper()
		_, err := ingest(ctx, pool, h, until, body)
		if pgCode(err) != invalidParameter {
			t.Errorf("%s: ingest: want 22023, got %v", what, err)
		}
		err = asAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
			var n int
			return tx.QueryRow(ctx, `SELECT count(*) FROM mhai_analysis_ingest_source($1, NULL, $2)`, h, until).Scan(&n)
		})
		if pgCode(err) != invalidParameter {
			t.Errorf("%s: ingest_source: want 22023, got %v", what, err)
		}
		if n := countRows(t, pool, `SELECT count(*) FROM analysis_facts WHERE hospital_id = $1`, h) +
			countRows(t, pool, `SELECT count(*) FROM analysis_subjects WHERE hospital_id = $1`, h) +
			countRows(t, pool, `SELECT count(*) FROM analysis_watermarks WHERE hospital_id = $1`, h); n != 0 {
			t.Errorf("%s: %d rows written", what, n)
		}
	}
	check("no switch rows", off)
	setSwitch(t, pool, &off, "off", "")
	check("hospital row off", off)
	setSwitch(t, pool, nil, "off", "")
	check("global off, hospital on", on)
	setSwitch(t, pool, nil, "on", "SO-2026-0001")
}

// N2: a hospital under a demo switch (its own row or the global) never
// ingests consultations; a signed-off one reads an empty source today.
func TestIngestSourceRefusesDemoSwitch(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	until := at(t, "Africa/Lagos", "2026-03-01", 0)
	source := func(h uuid.UUID) (int, error) {
		var n int
		err := asAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM mhai_analysis_ingest_source($1, NULL, $2)`, h, until).Scan(&n)
		})
		return n, err
	}
	demo := demoHospital(t, pool)
	setSwitch(t, pool, nil, "on", "SO-2026-0001")
	setSwitch(t, pool, &demo, "on", "DEMO-SYNTHETIC")
	if _, err := source(demo); pgCode(err) != invalidParameter {
		t.Errorf("demo hospital source: want 22023, got %v", err)
	}
	if _, err := ingest(ctx, pool, demo, until, factsJSON(t, nil)); pgCode(err) != invalidParameter {
		t.Errorf("demo hospital ingest: want 22023, got %v", err)
	}
	real := onHospital(t, pool, "Africa/Lagos")
	// A real consultation exists; it carries no patient reference, so none comes back.
	seedQuestion(t, pool, real, seedClinician(t, pool, real), until.Add(-24*time.Hour), "PASS", false)
	if n, err := source(real); err != nil || n != 0 {
		t.Errorf("signed-off hospital source: %d rows (%v), want 0", n, err)
	}
	setSwitch(t, pool, nil, "on", "DEMO-SYNTHETIC")
	if _, err := source(real); pgCode(err) != invalidParameter {
		t.Errorf("a demo global: want 22023 for a signed-off hospital, got %v", err)
	}
	setSwitch(t, pool, nil, "on", "SO-2026-0001")
	if _, err := source(real); err != nil {
		t.Errorf("after the global is signed off again: %v", err)
	}
	// Not a window that has passed.
	err := asAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT count(*) FROM mhai_analysis_ingest_source($1, NULL, now() + interval '1 hour')`, real)
		return err
	})
	if pgCode(err) != invalidParameter {
		t.Errorf("a future window: want 22023, got %v", err)
	}
}

// N3: the writer never sees question, answer, patient or diagnosis text.
func TestWriterSeesNoText(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for tbl, cols := range map[string][]string{
		"clinical_queries":   {"clinical_question", "patient_data_json"},
		"clinical_decisions": {"response_text", "citations_json", "diagnosis", "guardrail_report_json"},
		"feedback_events":    {"reason", "corrected_diagnosis"},
	} {
		for _, col := range cols {
			var ok bool
			if err := pool.QueryRow(ctx, `SELECT has_column_privilege('mhai_analysis_writer', $1, $2, 'SELECT')`, tbl, col).Scan(&ok); err != nil || ok {
				t.Errorf("the writer may read %s.%s (%v)", tbl, col, err)
			}
			err := asAnalysisRole(ctx, pool, "mhai_analysis_writer", nil, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `SELECT `+col+` FROM `+tbl+` LIMIT 1`)
				return err
			})
			requireDenied(t, err, "writer reading "+tbl+"."+col)
		}
	}
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT has_table_privilege('mhai_analysis_writer', 'users', 'SELECT')
		OR has_any_column_privilege('mhai_analysis_writer', 'users', 'SELECT')`).Scan(&ok); err != nil || ok {
		t.Errorf("the writer may read users (%v)", err)
	}
}

// Blocks freeze at hospital-local midnight 3 days after their end, not
// before; a month with its last block, a quarter with its last month; a
// second call freezes nothing; parallel calls freeze once.
func TestFreezeBlocksThenParents(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for _, zone := range []string{"Africa/Lagos", "Pacific/Kiritimati"} {
		h := onHospital(t, pool, zone)
		// Block 22-31 Jan 2026 ends on the 31st: it freezes at local
		// midnight starting 4 Feb.
		edge := at(t, zone, "2026-02-04", 0)
		if _, err := freezeAt(ctx, pool, h, edge.Add(-time.Second)); err != nil {
			t.Fatalf("%s: %v", zone, err)
		}
		if !frozen(t, pool, h, "week", "2026-01-15") || frozen(t, pool, h, "week", "2026-01-22") || frozen(t, pool, h, "month", "2026-01-01") {
			t.Fatalf("%s: one second early: 15 Jan %v, 22 Jan %v, January %v", zone,
				frozen(t, pool, h, "week", "2026-01-15"), frozen(t, pool, h, "week", "2026-01-22"), frozen(t, pool, h, "month", "2026-01-01"))
		}
		if !frozen(t, pool, h, "quarter", "2025-10-01") || !frozen(t, pool, h, "month", "2025-12-01") {
			t.Errorf("%s: Q4 2025 or December not frozen", zone)
		}
		var wg sync.WaitGroup
		results := make([]int, 6)
		errs := make([]error, 6)
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i], errs[i] = freezeAt(ctx, pool, h, edge)
			}(i)
		}
		wg.Wait()
		total := 0
		for i, n := range results {
			if errs[i] != nil {
				t.Fatalf("%s: parallel freeze: %v", zone, errs[i])
			}
			total += n
		}
		if total != 2 { // the 22 Jan block and January
			t.Errorf("%s: parallel calls froze %d periods, want 2 once", zone, total)
		}
		if !frozen(t, pool, h, "week", "2026-01-22") || !frozen(t, pool, h, "month", "2026-01-01") || frozen(t, pool, h, "quarter", "2026-01-01") {
			t.Errorf("%s: at the edge: the block and January frozen, Q1 not", zone)
		}
		if n, err := freezeAt(ctx, pool, h, edge); err != nil || n != 0 {
			t.Errorf("%s: second call froze %d (%v)", zone, n, err)
		}
		// Q1 2026 freezes with its last block (22-31 March), on 4 April.
		if _, err := freezeAt(ctx, pool, h, at(t, zone, "2026-04-04", 0)); err != nil {
			t.Fatal(err)
		}
		if !frozen(t, pool, h, "month", "2026-03-01") || !frozen(t, pool, h, "quarter", "2026-01-01") {
			t.Errorf("%s: March and Q1 not frozen on 4 April", zone)
		}
		if n := countRows(t, pool, `SELECT count(*) FROM analysis_periods WHERE hospital_id = $1 GROUP BY level, start ORDER BY 1 DESC LIMIT 1`, h); n != 1 {
			t.Errorf("%s: a period is stored %d times", zone, n)
		}
	}
	// Refused: the future, and a hospital that is off.
	h := onHospital(t, pool, "Africa/Lagos")
	if _, err := freezeAt(ctx, pool, h, time.Now().Add(time.Hour)); pgCode(err) != invalidParameter {
		t.Errorf("freezing the future: want 22023, got %v", err)
	}
	off, _ := seedHospital(t, pool, uniqueCode("O"))
	if _, err := freezeAt(ctx, pool, off, at(t, "Africa/Lagos", "2026-02-04", 0)); pgCode(err) != invalidParameter {
		t.Errorf("freezing an off hospital: want 22023, got %v", err)
	}
	// No engine version set.
	err := asAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.analysis.engine', '', true)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT mhai_analysis_freeze($1, $2)`, h, at(t, "Africa/Lagos", "2026-02-04", 0))
		return err
	})
	if pgCode(err) != invalidParameter {
		t.Errorf("no engine version: want 22023, got %v", err)
	}
}

// T4: a fact dated in a frozen block is stored late and counted in neither
// the block, its month nor its quarter.
func TestLateFactExcludedEverywhere(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h := onHospital(t, pool, "Africa/Lagos")
	c := condition(t, pool, h, "J45")
	now := at(t, "Africa/Lagos", "2026-04-10", 0)
	if _, err := freezeAt(ctx, pool, h, now); err != nil { // through Q1
		t.Fatal(err)
	}
	r := newRef()
	late, err := ingest(ctx, pool, h, now, factsJSON(t, map[ref][]fact{r: {
		{"day": "2026-03-10", "kind": "diagnosis", "condition": c.String(), "sex": "male", "age_band": "40-64"},
		{"day": "2026-04-06", "kind": "diagnosis", "condition": c.String(), "sex": "male", "age_band": "40-64"},
	}}))
	if err != nil || late != 1 {
		t.Fatalf("late %d (%v), want 1", late, err)
	}
	for _, p := range [][2]string{{"2026-03-08", "2026-03-14"}, {"2026-03-01", "2026-03-31"}, {"2026-01-01", "2026-03-31"}} {
		if n := countRows(t, pool, `SELECT count(*) FROM analysis_facts WHERE hospital_id = $1 AND NOT period_late
			AND day BETWEEN $2::date AND $3::date`, h, p[0], p[1]); n != 0 {
			t.Errorf("%v counts %d facts, want 0", p, n)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_facts WHERE hospital_id = $1 AND NOT period_late`, h); n != 1 {
		t.Errorf("%d counted facts, want the April one", n)
	}
}

// N4 (Review Focus 2): a late fact never changes a frozen block's "new",
// and makes a case new nowhere.
func TestLateFactNeverChangesFrozenNew(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h := onHospital(t, pool, "Africa/Lagos")
	c := condition(t, pool, h, "I10")
	newIn := func(from, to string) int {
		return countRows(t, pool, `SELECT count(*) FROM analysis_first_seen WHERE hospital_id = $1
			AND first_day BETWEEN $2::date AND $3::date AND NOT first_late`, h, from, to)
	}
	x, y, z := newRef(), newRef(), newRef()
	dx := func(day string) fact {
		return fact{"day": day, "kind": "diagnosis", "condition": c.String(), "sex": "female"}
	}
	// x is first seen on 15 Jan, before January freezes.
	if _, err := ingest(ctx, pool, h, at(t, "Africa/Lagos", "2026-01-20", 0), factsJSON(t, map[ref][]fact{x: {dx("2026-01-15")}})); err != nil {
		t.Fatal(err)
	}
	feb10 := at(t, "Africa/Lagos", "2026-02-10", 0)
	if _, err := freezeAt(ctx, pool, h, feb10); err != nil { // through January
		t.Fatal(err)
	}
	// y is first seen on 9 Feb (unfrozen).
	if _, err := ingest(ctx, pool, h, feb10, factsJSON(t, map[ref][]fact{y: {dx("2026-02-09")}})); err != nil {
		t.Fatal(err)
	}
	janBefore, febBefore := newIn("2026-01-01", "2026-01-31"), newIn("2026-02-08", "2026-02-14")
	if janBefore != 1 || febBefore != 1 {
		t.Fatalf("before: January new %d, 8-14 Feb new %d", janBefore, febBefore)
	}
	// Late facts for all three in frozen January.
	late, err := ingest(ctx, pool, h, feb10.Add(time.Hour), factsJSON(t, map[ref][]fact{x: {dx("2026-01-03")}, y: {dx("2026-01-05")}, z: {dx("2026-01-06")}}))
	if err != nil || late != 3 {
		t.Fatalf("late %d (%v)", late, err)
	}
	if got := newIn("2026-01-01", "2026-01-31"); got != janBefore {
		t.Errorf("frozen January's new moved %d -> %d", janBefore, got)
	}
	if got := newIn("2026-01-01", "2026-12-31"); got != 1 {
		t.Errorf("%d cases new anywhere, want x only (y and z are late cases)", got)
	}
	// y is existing in its unfrozen block now; x keeps its frozen first day.
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_first_seen WHERE hospital_id = $1 AND first_day = '2026-01-15' AND NOT first_late`, h); n != 1 {
		t.Errorf("x's frozen first day changed")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_first_seen WHERE hospital_id = $1 AND first_late`, h); n != 2 {
		t.Errorf("%d late first days, want y and z", n)
	}
	// A later non-late fact for z in February isn't new.
	if _, err := ingest(ctx, pool, h, feb10.Add(2*time.Hour), factsJSON(t, map[ref][]fact{z: {dx("2026-02-10")}})); err != nil {
		t.Fatal(err)
	}
	if got := newIn("2026-02-08", "2026-02-14"); got != 0 {
		t.Errorf("8-14 Feb new %d after late cases, want 0", got)
	}
}

// Ingest refuses unknown keys, free text, a day outside the last 25 months
// or in the future, and a watermark moving back; nothing is written.
func TestIngestRefusesMalformedFacts(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h := onHospital(t, pool, "Africa/Lagos")
	c := condition(t, pool, h, "E11")
	until := at(t, "Africa/Lagos", "2026-03-01", 0)
	good := fact{"day": "2026-02-10", "kind": "diagnosis", "condition": c.String(), "sex": "female"}
	with := func(k string, v any) fact {
		f := fact{}
		for a, b := range good {
			f[a] = b
		}
		f[k] = v
		return f
	}
	for name, f := range map[string]fact{
		"unknown key":  with("note", "x"),
		"free text":    with("specialty", "Chest pain since Tuesday"),
		"unknown code": with("sex", "f"),
		"too old":      with("day", "2024-01-31"),
		"future day":   with("day", "2026-03-02"),
		"number type":  with("kind", 3),
		"bad day":      with("day", "10/02/2026"),
	} {
		if _, err := ingest(ctx, pool, h, until, factsJSON(t, map[ref][]fact{newRef(): {f}})); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_subjects WHERE hospital_id = $1`, h); n != 0 {
		t.Errorf("%d subjects after refusals", n)
	}
	if _, err := ingest(ctx, pool, h, until, `{"subjects": [], "extra": 1}`); pgCode(err) != invalidParameter {
		t.Errorf("an extra top-level key: %v", err)
	}
	if _, err := ingest(ctx, pool, h, until, factsJSON(t, map[ref][]fact{newRef(): {good}})); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest(ctx, pool, h, until.Add(-time.Hour), factsJSON(t, nil)); pgCode(err) != invalidParameter {
		t.Errorf("a watermark moving back: %v", err)
	}
	r := newRef()
	r2 := ref{r.hmac, "short"}
	if _, err := ingest(ctx, pool, h, until, factsJSON(t, map[ref][]fact{r2: {good}})); pgCode(err) != invalidParameter {
		t.Errorf("a malformed ciphertext: %v", err)
	}
}

// The periods CHECK is 7c's calendar twin: an ISO week or a 30-day range
// is refused.
func TestPeriodsCheckRefusesNonPeriods(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h := onHospital(t, pool, "Africa/Lagos")
	for _, p := range [][3]string{{"week", "2026-01-05", "2026-01-11"}, {"month", "2026-01-01", "2026-01-30"}, {"quarter", "2026-02-01", "2026-04-30"}} {
		_, err := pool.Exec(ctx, `INSERT INTO analysis_periods (hospital_id, level, start, "end", engine_version) VALUES ($1, $2, $3::date, $4::date, '1')`,
			h, p[0], p[1], p[2])
		if pgCode(err) != checkViolation {
			t.Errorf("%v: want 23514, got %v", p, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO analysis_periods (hospital_id, level, start, "end", engine_version) VALUES ($1, 'week', '2026-01-22', '2026-01-31', '1')`, h); err != nil {
		t.Errorf("the last block of January: %v", err)
	}
}

// N2: who writes the switch, and the position it gives.
func TestSwitchWriters(t *testing.T) {
	pg, pool := shared(t)
	ctx := context.Background()
	prov, _, err := testutil.ProvisionLogin(ctx, pg, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer prov.Close()
	write := func(p *pgxpool.Pool, role string, h *uuid.UUID, position string, r any) error {
		return asAnalysisRole(ctx, p, role, nil, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
				VALUES ($1, $2, $3, $3, $3)
				ON CONFLICT (hospital_id) DO UPDATE SET position = EXCLUDED.position, signoff_clinical = EXCLUDED.signoff_clinical,
				    signoff_privacy = EXCLUDED.signoff_privacy, signoff_legal = EXCLUDED.signoff_legal`, h, position, r)
			return err
		})
	}
	h, _ := seedHospital(t, pool, uniqueCode("W"))
	d := demoHospital(t, pool)
	raise := func(h *uuid.UUID) error {
		return asAnalysisRole(ctx, prov, "mhai_provision", nil, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT mhai_analysis_demo_raise($1)`, h)
			return err
		})
	}
	provUpdate := func(q string, h uuid.UUID) error {
		return asAnalysisRole(ctx, prov, "mhai_provision", nil, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, h)
			return err
		})
	}
	// Provisioning writes no row directly: it raises a demo hospital's
	// switch through the function and may only lower a switch.
	for _, r := range []any{"SO-2026-0042", "DEMO-SYNTHETIC", nil} {
		if err := write(prov, "mhai_provision", &d, "on", r); pgCode(err) != permissionDenied {
			t.Errorf("provision inserting a switch row (%v): want 42501, got %v", r, err)
		}
	}
	if err := raise(&d); err != nil {
		t.Errorf("provision raising the demo hospital: %v", err)
	}
	if err := provUpdate(`UPDATE analysis_switch SET position = 'off' WHERE hospital_id = $1`, d); err != nil {
		t.Errorf("provision lowering: %v", err)
	}
	if err := provUpdate(`UPDATE analysis_switch SET position = 'on' WHERE hospital_id = $1`, d); pgCode(err) != rlsViolation {
		t.Errorf("provision raising by UPDATE: want an RLS refusal, got %v", err)
	}
	if err := provUpdate(`UPDATE analysis_switch SET signoff_clinical = NULL WHERE hospital_id = $1`, d); pgCode(err) != permissionDenied {
		t.Errorf("provision clearing a reference: want 42501, got %v", err)
	}
	if err := raise(&h); pgCode(err) != invalidParameter {
		t.Errorf("provision raising a real hospital: want 22023, got %v", err)
	}
	if err := write(pool, "mhai_platform", &d, "on", "DEMO-SYNTHETIC"); pgCode(err) != rlsViolation {
		t.Errorf("platform writing demo references: want an RLS refusal, got %v", err)
	}
	if err := write(pool, "mhai_platform", &h, "on", "SO-2026-0042"); err != nil {
		t.Errorf("platform raising with a sign-off: %v", err)
	}
	if err := write(pool, "mhai_gateway", &h, "off", nil); pgCode(err) != permissionDenied {
		t.Errorf("gateway writing the switch: %v", err)
	}
	if err := asAnalysisRole(ctx, prov, "mhai_provision", nil, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM analysis_switch WHERE hospital_id = $1`, h)
		return err
	}); pgCode(err) != permissionDenied {
		t.Errorf("provision deleting a switch row: %v", err)
	}
	// Mixed or missing references.
	if _, err := pool.Exec(ctx, `INSERT INTO analysis_switch (hospital_id, position, signoff_clinical) VALUES (NULL, 'on', 'SO-2026-0001')
		ON CONFLICT (hospital_id) DO UPDATE SET signoff_privacy = NULL, position = 'on'`); pgCode(err) != checkViolation {
		t.Errorf("on with a missing reference: want 23514, got %v", err)
	}

	position := func(role string, at *uuid.UUID, of uuid.UUID) string {
		var p string
		if err := asAnalysisRole(ctx, pool, role, at, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT mhai_analysis_position($1)`, of).Scan(&p)
		}); err != nil {
			t.Fatalf("position as %s: %v", role, err)
		}
		return p
	}
	k, _ := seedHospital(t, pool, uniqueCode("P"))
	if p := position("mhai_analysis_job", nil, k); p != "off" {
		t.Errorf("no hospital row: %s", p)
	}
	setSwitch(t, pool, &k, "on", "SO-2026-0003")
	setSwitch(t, pool, nil, "off", "")
	if p := position("mhai_analysis_job", nil, k); p != "off" {
		t.Errorf("global off: %s", p)
	}
	setSwitch(t, pool, nil, "on", "SO-2026-0001")
	if p := position("mhai_analysis_job", nil, k); p != "on" {
		t.Errorf("both on: %s", p)
	}
	setSwitch(t, pool, &k, "off", "SO-2026-0003")
	if p := position("mhai_analysis_job", nil, k); p != "off" {
		t.Errorf("hospital off: %s", p)
	}
	setSwitch(t, pool, &k, "on", "SO-2026-0003")
	if p := position("mhai_gateway", &k, k); p != "on" {
		t.Errorf("gateway, own hospital: %s", p)
	}
	if p := position("mhai_gateway", &h, k); p != "off" {
		t.Errorf("gateway, another hospital: %s", p)
	}
	if p := position("mhai_gateway", nil, k); p != "off" {
		t.Errorf("gateway with no hospital set: %s", p)
	}
	if p := position("mhai_auth", nil, k); p != "on" {
		t.Errorf("sign-in reading the user's hospital: %s", p)
	}
}

// Review item 1 (sticky sign-off), the attack exactly: a row signed off
// once is lowered with its references cleared, then raised again with
// DEMO-SYNTHETIC. ever_signed_off is set by any SO- reference, can't be
// cleared by any role, and a flagged row never takes demo references;
// provisioning can't clear references at all.
func TestStickySignOff(t *testing.T) {
	pg, pool := shared(t)
	ctx := context.Background()
	prov, _, err := testutil.ProvisionLogin(ctx, pg, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer prov.Close()
	as := func(p *pgxpool.Pool, role, q string, h *uuid.UUID) error {
		return asAnalysisRole(ctx, p, role, nil, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, h)
			return err
		})
	}
	flagged := func(h *uuid.UUID) bool {
		var f bool
		if err := pool.QueryRow(ctx, `SELECT ever_signed_off FROM analysis_switch WHERE hospital_id IS NOT DISTINCT FROM $1`, h).Scan(&f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	d := demoHospital(t, pool)
	for _, h := range []*uuid.UUID{&d, nil} {
		name := "the hospital row"
		if h == nil {
			name = "the global row"
			if _, err := pool.Exec(ctx, `DELETE FROM analysis_switch WHERE hospital_id IS NULL`); err != nil {
				t.Fatal(err)
			}
		}
		// The platform signs the row off.
		if err := as(pool, "mhai_platform", `INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
			VALUES ($1, 'on', 'SO-2026-0007', 'SO-2026-0008', 'SO-2026-0009')`, h); err != nil {
			t.Fatalf("%s: platform sign-off: %v", name, err)
		}
		if !flagged(h) {
			t.Fatalf("%s: a sign-off didn't set ever_signed_off", name)
		}
		// The attack: provisioning lowers it and clears the references ...
		if err := as(prov, "mhai_provision", `UPDATE analysis_switch SET position = 'off', signoff_clinical = NULL, signoff_privacy = NULL,
			signoff_legal = NULL WHERE hospital_id IS NOT DISTINCT FROM $1`, h); pgCode(err) != permissionDenied {
			t.Errorf("%s: provision clearing references: want 42501, got %v", name, err)
		}
		// ... and raises it again with DEMO-SYNTHETIC.
		if err := as(prov, "mhai_provision", `UPDATE analysis_switch SET position = 'on', signoff_clinical = 'DEMO-SYNTHETIC',
			signoff_privacy = 'DEMO-SYNTHETIC', signoff_legal = 'DEMO-SYNTHETIC' WHERE hospital_id IS NOT DISTINCT FROM $1`, h); pgCode(err) != permissionDenied {
			t.Errorf("%s: provision writing demo references: want 42501, got %v", name, err)
		}
		if err := as(prov, "mhai_provision", `UPDATE analysis_switch SET position = 'off' WHERE hospital_id IS NOT DISTINCT FROM $1`, h); err != nil {
			t.Errorf("%s: provision lowering: %v", name, err)
		}
		// Even once the platform clears the references, the row stays
		// flagged and refuses demo references, from any role.
		if err := as(pool, "mhai_platform", `UPDATE analysis_switch SET signoff_clinical = NULL, signoff_privacy = NULL, signoff_legal = NULL
			WHERE hospital_id IS NOT DISTINCT FROM $1`, h); err != nil {
			t.Errorf("%s: platform clearing references: %v", name, err)
		}
		if !flagged(h) {
			t.Errorf("%s: clearing the references cleared ever_signed_off", name)
		}
		if err := as(prov, "mhai_provision", `SELECT mhai_analysis_demo_raise($1)`, h); pgCode(err) != checkViolation {
			t.Errorf("%s: demo raise after a sign-off: want 23514, got %v", name, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE analysis_switch SET position = 'on', signoff_clinical = 'DEMO-SYNTHETIC', signoff_privacy = 'DEMO-SYNTHETIC',
			signoff_legal = 'DEMO-SYNTHETIC' WHERE hospital_id IS NOT DISTINCT FROM $1`, h); pgCode(err) != checkViolation {
			t.Errorf("%s: the owner writing demo references: want 23514, got %v", name, err)
		}
		if err := as(pool, "mhai_platform", `UPDATE analysis_switch SET ever_signed_off = false WHERE hospital_id IS NOT DISTINCT FROM $1`, h); pgCode(err) != permissionDenied {
			t.Errorf("%s: platform clearing the flag: want 42501, got %v", name, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE analysis_switch SET ever_signed_off = false WHERE hospital_id IS NOT DISTINCT FROM $1`, h); pgCode(err) != checkViolation {
			t.Errorf("%s: the owner clearing the flag: want 23514, got %v", name, err)
		}
		if !flagged(h) {
			t.Errorf("%s: ever_signed_off cleared", name)
		}
	}
	setSwitch(t, pool, nil, "on", "SO-2026-0001")
}

// Review item 2: is_demo is set when a hospital is created and never
// changes, for any role.
func TestDemoFlagImmutable(t *testing.T) {
	pg, pool := shared(t)
	ctx := context.Background()
	prov, _, err := testutil.ProvisionLogin(ctx, pg, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer prov.Close()
	real, _ := seedHospital(t, pool, uniqueCode("R"))
	demo := demoHospital(t, pool)
	// Each update flips the flag: a real hospital to demo, a demo one back.
	for q, h := range map[string]uuid.UUID{`UPDATE hospitals SET is_demo = true WHERE id = $1`: real,
		`UPDATE hospitals SET is_demo = false WHERE id = $1`: demo} {
		if _, err := pool.Exec(ctx, q, h); pgCode(err) != checkViolation {
			t.Errorf("owner %s: want 23514, got %v", q, err)
		}
		for _, role := range []string{"mhai_provision", "mhai_platform"} {
			p := pool
			if role == "mhai_provision" {
				p = prov
			}
			if err := asAnalysisRole(ctx, p, role, nil, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, q, h)
				return err
			}); pgCode(err) != permissionDenied {
				t.Errorf("%s %s: want 42501, got %v", role, q, err)
			}
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM hospitals WHERE (id = $1 AND is_demo) OR (id = $2 AND NOT is_demo)`, real, demo); n != 0 {
		t.Errorf("%d hospitals changed their demo flag", n)
	}
	// The provisioning login may create a demo hospital.
	if err := asAnalysisRole(ctx, prov, "mhai_provision", nil, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO hospitals (id, name, short_code, time_zone, is_demo) VALUES (gen_random_uuid(), 'Synthetic Demo', $1, 'UTC', true)`, uniqueCode("Z"))
		return err
	}); err != nil {
		t.Errorf("provision creating a demo hospital: %v", err)
	}
}

// N1: a temporary table named like an analysis table, made by the caller,
// is ignored by every definer function.
func TestTempTableCannotShadow(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	off, _ := seedHospital(t, pool, uniqueCode("T"))
	on := onHospital(t, pool, "Africa/Lagos")
	until := at(t, "Africa/Lagos", "2026-03-01", 0)
	err := asAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
		for _, q := range []string{
			`CREATE TEMP TABLE analysis_switch (hospital_id uuid, position text, signoff_clinical text, signoff_privacy text, signoff_legal text) ON COMMIT DROP`,
			`INSERT INTO analysis_switch VALUES (NULL, 'on', 'SO-2026-0001', 'SO-2026-0001', 'SO-2026-0001')`,
			`INSERT INTO analysis_switch VALUES ('` + off.String() + `', 'on', 'SO-2026-0001', 'SO-2026-0001', 'SO-2026-0001')`,
			`CREATE TEMP TABLE analysis_periods (hospital_id uuid, level text, start date, "end" date) ON COMMIT DROP`,
			`INSERT INTO analysis_periods VALUES ('` + on.String() + `', 'week', '2026-02-08', '2026-02-14')`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		var p string
		if err := tx.QueryRow(ctx, `SELECT mhai_analysis_position($1)`, off).Scan(&p); err != nil {
			return err
		}
		if p != "off" {
			t.Errorf("a temporary switch turned a hospital %s", p)
		}
		if _, err := tx.Exec(ctx, `SAVEPOINT s`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT mhai_analysis_ingest($1, $2, '{"subjects": []}')`, off, until); pgCode(err) != invalidParameter {
			t.Errorf("ingest under a temporary switch: want 22023, got %v", err)
		}
		if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT s`); err != nil {
			return err
		}
		c := condition(t, pool, on, "K21")
		var late int
		return tx.QueryRow(ctx, `SELECT mhai_analysis_ingest($1, $2, $3::jsonb)`, on, until,
			factsJSON(t, map[ref][]fact{newRef(): {{"day": "2026-02-10", "kind": "diagnosis", "condition": c.String(), "sex": "other"}}})).Scan(&late)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_facts WHERE hospital_id = $1 AND period_late`, on); n != 0 {
		t.Errorf("a temporary period made %d facts late", n)
	}
}

// N12: neither job role can run the other's functions.
func TestAnalyticsAndAnalysisGrantsDisjoint(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for role, prefix := range map[string]string{"mhai_analytics_job": "mhai_analysis%", "mhai_analysis_job": "mhai_analytics%"} {
		var bad []string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(array_agg(oid::regprocedure::text), '{}') FROM pg_proc
			WHERE proname LIKE $2 AND has_function_privilege($1, oid, 'EXECUTE')`, role, prefix).Scan(&bad); err != nil {
			t.Fatal(err)
		}
		if len(bad) != 0 {
			t.Errorf("%s may execute %v", role, bad)
		}
	}
	// The analysis job runs exactly its six functions and the position.
	var got []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(proname::text ORDER BY proname) FROM pg_proc
		WHERE proname LIKE 'mhai_analysis%' AND has_function_privilege('mhai_analysis_job', oid, 'EXECUTE')`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := "mhai_analysis_due,mhai_analysis_freeze,mhai_analysis_ingest,mhai_analysis_ingest_source,mhai_analysis_position,mhai_analysis_ran,mhai_analysis_sweep"
	if strings.Join(got, ",") != want {
		t.Errorf("mhai_analysis_job executes %v", got)
	}
	// Roles: no login, no bypass, no members but mhai_app (not inherited).
	for _, role := range []string{"mhai_analysis_writer", "mhai_analysis_reader", "mhai_analysis_job"} {
		var login, super, bypass bool
		var members int
		if err := pool.QueryRow(ctx, `SELECT rolcanlogin, rolsuper, rolbypassrls,
			(SELECT count(*) FROM pg_auth_members m WHERE m.roleid = r.oid AND m.member <> 'mhai_app'::regrole)
			FROM pg_roles r WHERE rolname = $1`, role).Scan(&login, &super, &bypass, &members); err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		if login || super || bypass || members != 0 {
			t.Errorf("%s: login %v super %v bypassrls %v, %d other members", role, login, super, bypass, members)
		}
	}
	var inherits, appWriter bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.roleid = 'mhai_analysis_job'::regrole AND m.inherit_option),
		EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.roleid IN ('mhai_analysis_writer'::regrole, 'mhai_analysis_reader'::regrole))`).
		Scan(&inherits, &appWriter); err != nil || inherits || appWriter {
		t.Errorf("job inherited %v, writer or reader has members %v (%v)", inherits, appWriter, err)
	}
	// Every definer function pins its search_path.
	var loose []string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(array_agg(proname::text), '{}') FROM pg_proc WHERE proname LIKE 'mhai_analysis%'
		AND NOT (proconfig @> ARRAY['search_path=pg_catalog, pg_temp'])`).Scan(&loose); err != nil || len(loose) != 0 {
		t.Errorf("functions without a pinned search_path: %v (%v)", loose, err)
	}
}

// N2: the seed writes only for a hospital switched on for the demo, once.
func TestSeedOnlyForDemoHospital(t *testing.T) {
	pg, pool := shared(t)
	ctx := context.Background()
	prov, _, err := testutil.ProvisionLogin(ctx, pg, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer prov.Close()
	now := at(t, "Africa/Lagos", "2026-03-01", 0)
	cond := uuid.New()
	seedBody := func() string {
		b, _ := json.Marshal(map[string]any{
			"conditions": []map[string]any{{"id": cond, "icd10": "E11", "name": "Synthetic type 2 diabetes"}},
			"subjects": []map[string]any{{"facts": []fact{{"day": "2026-02-10", "kind": "diagnosis", "condition": cond.String(),
				"sex": "female", "age_band": "40-64"}}}},
		})
		return string(b)
	}
	seed := func(h uuid.UUID) (int, error) {
		var n int
		err := asAnalysisRole(ctx, prov, "mhai_provision", nil, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT mhai_analysis_seed($1, $2, $3::jsonb)`, h, now, seedBody()).Scan(&n)
		})
		return n, err
	}
	setSwitch(t, pool, nil, "on", "SO-2026-0001")
	off, _ := seedHospital(t, pool, uniqueCode("S"))
	if _, err := seed(off); pgCode(err) != invalidParameter {
		t.Errorf("seed of an off hospital: want 22023, got %v", err)
	}
	real := onHospital(t, pool, "Africa/Lagos")
	if _, err := seed(real); pgCode(err) != invalidParameter {
		t.Errorf("seed of a signed-off hospital: want 22023, got %v", err)
	}
	// Item 2: a real hospital can't carry demo references at all, so it
	// can never be seeded; a demo hospital can.
	plain, _ := seedHospital(t, pool, uniqueCode("S"))
	if _, err := pool.Exec(ctx, `INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		VALUES ($1, 'on', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC')`, plain); pgCode(err) != checkViolation {
		t.Errorf("demo references on a real hospital: want 23514, got %v", err)
	}
	if err := asAnalysisRole(ctx, prov, "mhai_provision", nil, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT mhai_analysis_demo_raise($1)`, plain)
		return err
	}); pgCode(err) != invalidParameter {
		t.Errorf("demo raise of a real hospital: want 22023, got %v", err)
	}
	if _, err := seed(plain); pgCode(err) != invalidParameter {
		t.Errorf("seed of a real hospital: want 22023, got %v", err)
	}
	demo := demoHospital(t, pool)
	setSwitch(t, pool, &demo, "on", "DEMO-SYNTHETIC")
	if n, err := seed(demo); err != nil || n != 1 {
		t.Fatalf("seed of the demo hospital: %d (%v)", n, err)
	}
	cond = uuid.New()
	if _, err := seed(demo); pgCode(err) != "23505" {
		t.Errorf("a second seed: want 23505, got %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_subjects WHERE hospital_id = $1 AND ref_enc IS NULL`, demo); n != 1 {
		t.Errorf("%d seeded subjects with no ciphertext", n)
	}
	for _, h := range []uuid.UUID{off, real} {
		if n := countRows(t, pool, `SELECT count(*) FROM analysis_facts WHERE hospital_id = $1`, h); n != 0 {
			t.Errorf("refused seed wrote %d facts", n)
		}
	}
}

// Retention: facts and first-seen rows older than 25 months go, then the
// subjects nothing cites.
func TestSweepRetention(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h := onHospital(t, pool, "Africa/Lagos")
	c := condition(t, pool, h, "N18")
	r := newRef()
	if _, err := ingest(ctx, pool, h, at(t, "Africa/Lagos", "2024-03-01", 0),
		factsJSON(t, map[ref][]fact{r: {{"day": "2024-02-10", "kind": "diagnosis", "condition": c.String(), "sex": "female"}}})); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := asAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mhai_analysis_sweep($1, $2)`, h, at(t, "Africa/Lagos", "2026-05-01", 0)).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("swept %d (%v)", n, err)
	}
	if left := countRows(t, pool, `SELECT (SELECT count(*) FROM analysis_facts WHERE hospital_id = $1)
		+ (SELECT count(*) FROM analysis_first_seen WHERE hospital_id = $1) + (SELECT count(*) FROM analysis_subjects WHERE hospital_id = $1)`, h); left != 0 {
		t.Errorf("%d rows left after the sweep", left)
	}
}

// The nightly bookkeeping: due after local 00:30 once per hospital day.
func TestDueOncePerHospitalDay(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h := onHospital(t, pool, "Pacific/Kiritimati")
	due := func(now time.Time) bool {
		var ok bool
		if err := asAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mhai_analysis_due($1) WHERE hospital_id = $2 AND analysis_on AND NOT demo)`, now, h).Scan(&ok)
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	day := at(t, "Pacific/Kiritimati", "2026-02-10", 0)
	if due(day.Add(29 * time.Minute)) {
		t.Error("due before 00:30")
	}
	if !due(day.Add(31 * time.Minute)) {
		t.Fatal("not due after 00:30")
	}
	if err := asAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT mhai_analysis_ran($1, $2)`, h, day.Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if due(day.Add(2 * time.Hour)) {
		t.Error("due twice in one hospital day")
	}
	if !due(day.Add(24*time.Hour + 31*time.Minute)) {
		t.Error("not due the next day")
	}
}

// 00021 goes down to 00020 and up again, leaving nothing behind.
func TestMigration00021DownUp(t *testing.T) {
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
	if err := db.MigrateDownTo(ctx, pg.URL, 20); err != nil {
		t.Fatalf("down to 20: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_class WHERE relname LIKE 'analysis\_%')
		+ (SELECT count(*) FROM pg_proc WHERE proname LIKE 'mhai_analysis%')
		+ (SELECT count(*) FROM pg_policies WHERE policyname LIKE 'analysis%' OR tablename LIKE 'analysis%')
		+ (SELECT count(*) FROM unnest(ARRAY['clinical_queries', 'clinical_decisions', 'feedback_events', 'hospitals']) t
		   WHERE has_any_column_privilege('mhai_analysis_writer', t, 'SELECT'))
		+ (SELECT count(*) FROM pg_auth_members WHERE roleid = 'mhai_analysis_job'::regrole)
		+ (SELECT count(*) FROM pg_namespace n, aclexplode(n.nspacl) a WHERE n.nspname = 'public'
		   AND a.grantee IN ('mhai_analysis_job'::regrole, 'mhai_analysis_writer'::regrole, 'mhai_analysis_reader'::regrole))
		+ (SELECT count(*) FROM pg_proc WHERE proname = 'mhai_reporting_period_ok' AND has_function_privilege('mhai_analysis_writer', oid, 'EXECUTE'))`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("after down, %d objects, grants or memberships of 00021 remain", left)
	}
	var policy string
	if err := pool.QueryRow(ctx, `SELECT pg_get_expr(polwithcheck, polrelid) FROM pg_policy WHERE polname = 'provision_append'`).Scan(&policy); err != nil ||
		strings.Contains(policy, "analysis") || !strings.Contains(policy, "hospital.model_policy_changed") {
		t.Errorf("provision_append after down: %s (%v)", policy, err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
}

// A hospital created before is_demo existed (the live demo's) is marked
// once through mhai_hospital_mark_demo, as the provisioning login: never
// back, never twice, never a hospital with a real cloud agreement, and
// never once any analysis switch has been signed off. A fresh database,
// since the shared one carries sign-offs.
func TestMarkDemoOnce(t *testing.T) {
	pg, pool := testutil.MigratedPostgres(t)
	ctx := context.Background()
	prov, _, err := testutil.ProvisionLogin(ctx, pg, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer prov.Close()
	mark := func(h uuid.UUID) error {
		return asAnalysisRole(ctx, prov, "mhai_provision", nil, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT mhai_hospital_mark_demo($1)`, h)
			return err
		})
	}
	old, _ := seedHospital(t, pool, uniqueCode("O"))
	if err := mark(old); err != nil {
		t.Fatalf("marking an existing hospital: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM hospitals WHERE id = $1 AND is_demo`, old); n != 1 {
		t.Fatal("not marked")
	}
	if err := mark(old); pgCode(err) != invalidParameter {
		t.Errorf("marking twice: want 22023, got %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hospitals SET is_demo = false WHERE id = $1`, old); pgCode(err) != checkViolation {
		t.Errorf("unmarking: want 23514, got %v", err)
	}
	// Direct writes stay refused, with or without the function's setting.
	if err := asAnalysisRole(ctx, prov, "mhai_provision", nil, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.demo_mark', $1, true)`, old.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE hospitals SET is_demo = true WHERE id = $1`, old)
		return err
	}); pgCode(err) != permissionDenied {
		t.Errorf("provision updating is_demo directly: want 42501, got %v", err)
	}
	agr, _ := seedHospital(t, pool, uniqueCode("A"))
	if _, err := pool.Exec(ctx, `INSERT INTO hospital_model_policy (hospital_id, cloud_provider, agreement_ref) VALUES ($1, 'openai', 'AGR-2026-0001')`, agr); err != nil {
		t.Fatal(err)
	}
	if err := mark(agr); pgCode(err) != invalidParameter {
		t.Errorf("a hospital with a real cloud agreement: want 22023, got %v", err)
	}
	if err := mark(uuid.New()); pgCode(err) != invalidParameter {
		t.Errorf("an unknown hospital: want 22023, got %v", err)
	}
	// Once any analysis switch has been signed off, nothing more is marked.
	later, _ := seedHospital(t, pool, uniqueCode("L"))
	other, _ := seedHospital(t, pool, uniqueCode("S"))
	setSwitch(t, pool, &other, "off", "SO-2026-0001")
	if err := mark(later); pgCode(err) != invalidParameter {
		t.Errorf("after a sign-off: want 22023, got %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM hospitals WHERE is_demo`); n != 1 {
		t.Errorf("%d demo hospitals, want 1", n)
	}
	if err := asAnalysisRole(ctx, pool, "mhai_gateway", &old, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT mhai_hospital_mark_demo($1)`, old)
		return err
	}); pgCode(err) != permissionDenied {
		t.Errorf("the gateway marking: want 42501, got %v", err)
	}
}
