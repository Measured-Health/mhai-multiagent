//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00015 (Plan 4 Task 11, spec §6.1): the health functions'
// bodies. They aggregate every hospital, so these tests run on a database
// of their own, where the expected figures are exact. All data synthetic.

type timingRow struct {
	step     string
	p50, p95 *int32
	samples  int64
}

func healthTimings(t *testing.T, pool *pgxpool.Pool, window string) []timingRow {
	t.Helper()
	ctx := context.Background()
	var out []timingRow
	err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT step, p50_ms, p95_ms, samples FROM mhai_health_timings($1)`, window)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r timingRow
			if err := rows.Scan(&r.step, &r.p50, &r.p95, &r.samples); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("timings %s: %v", window, err)
	}
	return out
}

func i32(v int32) *int32 { return &v }

func checkTimings(t *testing.T, window string, got []timingRow, want []timingRow) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d rows, want %d: %+v", window, len(got), len(want), got)
	}
	eq := func(a, b *int32) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
	for i := range want {
		g, w := got[i], want[i]
		if g.step != w.step || g.samples != w.samples || !eq(g.p50, w.p50) || !eq(g.p95, w.p95) {
			t.Errorf("%s row %d: got %s p50 %v p95 %v n %d, want %s p50 %v p95 %v n %d", window, i,
				g.step, deref(g.p50), deref(g.p95), g.samples, w.step, deref(w.p50), deref(w.p95), w.samples)
		}
	}
}

func deref(p *int32) any {
	if p == nil {
		return nil
	}
	return *p
}

// seedTimed adds a question of hospital h completed ago before now, that
// took ms milliseconds, with a decision carrying timings (nil: none).
func seedTimed(t *testing.T, pool *pgxpool.Pool, h uuid.UUID, ago string, ms int, timings string) {
	t.Helper()
	ctx := context.Background()
	q := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, status, created_at, completed_at)
		VALUES ($1, $2, 'synthetic question', 'routine', 'completed',
		        now() - $3::interval - $4::int * interval '1 millisecond', now() - $3::interval)`, q, h, ago, ms); err != nil {
		t.Fatalf("seed timed query: %v", err)
	}
	var tm any
	if timings != "" {
		tm = timings
	}
	if _, err := pool.Exec(ctx, `INSERT INTO clinical_decisions (id, query_id, hospital_id, timings_ms) VALUES (gen_random_uuid(), $1, $2, $3::jsonb)`,
		q, h, tm); err != nil {
		t.Fatalf("seed decision: %v", err)
	}
}

func seedFailed(t *testing.T, pool *pgxpool.Pool, h uuid.UUID, status, errCode, ago string) {
	t.Helper()
	var code any
	if errCode != "" {
		code = errCode
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, status, error, completed_at)
		VALUES (gen_random_uuid(), $1, 'synthetic question', 'routine', $2, $3, now() - $4::interval)`, h, status, code, ago); err != nil {
		t.Fatalf("seed %s query: %v", status, err)
	}
}

// TestHealthFunctionsAggregate: the timings are p50 and p95 per step over
// the window, across hospitals; the failures are counts by type and code;
// the guidelines check sees only shared chunks.
func TestHealthFunctionsAggregate(t *testing.T) {
	_, pool := testutil.MigratedPostgres(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("A"))
	b, _ := seedHospital(t, pool, uniqueCode("B"))

	// No samples yet: five rows, null percentiles, 0 samples.
	empty := []timingRow{{step: "overall"}, {step: "privacy"}, {step: "guidelines"}, {step: "answer"}, {step: "evidence"}}
	checkTimings(t, "1 hour, empty", healthTimings(t, pool, "1 hour"), empty)

	seedTimed(t, pool, a, "10 minutes", 1000, `{"privacy": 100, "guidelines": 200, "answer": 500, "evidence": 200}`)
	seedTimed(t, pool, a, "20 minutes", 3000, `{"privacy": 300, "answer": 1500}`)
	seedTimed(t, pool, b, "5 hours", 5000, `{"privacy": 500}`)
	seedTimed(t, pool, b, "30 hours", 9000, `{"privacy": 900}`) // outside both windows
	seedTimed(t, pool, b, "15 minutes", 7000, "")               // no step timings: overall only
	// Not completed: never a sample.
	seedFailed(t, pool, a, "running", "", "1 minute")

	checkTimings(t, "1 hour", healthTimings(t, pool, "1 hour"), []timingRow{
		// 1000, 3000, 7000: p50 3000; p95 = 3000 + 0.9 × 4000 = 6600.
		{step: "overall", p50: i32(3000), p95: i32(6600), samples: 3},
		{step: "privacy", p50: i32(200), p95: i32(290), samples: 2},
		{step: "guidelines", p50: i32(200), p95: i32(200), samples: 1},
		{step: "answer", p50: i32(1000), p95: i32(1450), samples: 2},
		{step: "evidence", p50: i32(200), p95: i32(200), samples: 1},
	})
	checkTimings(t, "24 hours", healthTimings(t, pool, "24 hours"), []timingRow{
		// 1000, 3000, 5000, 7000: p50 4000; p95 = 5000 + 0.85 × 2000 = 6700.
		{step: "overall", p50: i32(4000), p95: i32(6700), samples: 4},
		{step: "privacy", p50: i32(300), p95: i32(480), samples: 3},
		{step: "guidelines", p50: i32(200), p95: i32(200), samples: 1},
		{step: "answer", p50: i32(1000), p95: i32(1450), samples: 2},
		{step: "evidence", p50: i32(200), p95: i32(200), samples: 1},
	})

	// Failures: questions by completed_at, jobs by failed_at; a cancelled
	// question is not a failure.
	seedFailed(t, pool, a, "failed", "deadline_exceeded", "10 minutes")
	seedFailed(t, pool, b, "failed", "unavailable", "3 hours")
	seedFailed(t, pool, a, "failed", "unavailable", "3 days")
	seedFailed(t, pool, a, "cancelled", "", "5 minutes")
	u := seedClinician(t, pool, a)
	job := seedJob(t, pool, a, u, "failed")
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'failed', error = 'internal', failed_at = now() - interval '30 minutes' WHERE id = $1`, job); err != nil {
		t.Fatal(err)
	}
	type failRow struct {
		typ          string
		code         *string
		now, h1, h24 int64
	}
	var got []failRow
	err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT job_type, error, failed_now, failed_last_hour, failed_last_24h FROM mhai_health_failures()`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r failRow
			if err := rows.Scan(&r.typ, &r.code, &r.now, &r.h1, &r.h24); err != nil {
				return err
			}
			got = append(got, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("failures: %v", err)
	}
	type want struct {
		typ, code    string
		now, h1, h24 int64
	}
	wants := []want{
		{"approve_correction", "internal", 1, 1, 1},
		{"run_query", "deadline_exceeded", 1, 1, 1},
		{"run_query", "unavailable", 2, 0, 1},
	}
	if len(got) != len(wants) {
		t.Fatalf("failures: %+v", got)
	}
	for i, w := range wants {
		g := got[i]
		if g.typ != w.typ || g.code == nil || *g.code != w.code || g.now != w.now || g.h1 != w.h1 || g.h24 != w.h24 {
			t.Errorf("failure row %d: %+v (code %v), want %+v", i, g, deref32s(g.code), w)
		}
	}

	guidelines := func() bool {
		t.Helper()
		var ok bool
		if err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT mhai_health_guidelines()`).Scan(&ok)
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if guidelines() {
		t.Fatal("guidelines with no chunk at all")
	}
	// A hospital's own chunk is not shared reference content.
	if _, err := pool.Exec(ctx, `INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type) VALUES ('c-own', $1, 'synthetic', 'note')`, a); err != nil {
		t.Fatal(err)
	}
	if guidelines() {
		t.Fatal("a hospital's chunk counted as shared guidelines")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO kb_chunks (chunk_id, content, chunk_type) VALUES ('c-shared', 'synthetic guideline', 'guideline')`); err != nil {
		t.Fatal(err)
	}
	if !guidelines() {
		t.Fatal("a shared chunk not seen")
	}
}

func deref32s(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestMigration15DownRestoresStubs: 00015's Down puts 00014's interval
// stub and refusing bodies back, drops the guidelines check and the owner's
// read policies; Up applies again.
func TestMigration15DownRestoresStubs(t *testing.T) {
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
	asPlatform := func(q string) error {
		return db.WithPlatform(ctx, pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
	}
	policies := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_policies WHERE policyname = 'health_read'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	built := func() {
		t.Helper()
		for _, q := range []string{`SELECT * FROM mhai_health_timings('24 hours')`, `SELECT * FROM mhai_health_failures()`, `SELECT mhai_health_guidelines()`} {
			if err := asPlatform(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		if n := policies(); n != 4 {
			t.Fatalf("%d health_read policies, want 4", n)
		}
	}
	built()
	if err := db.MigrateDownTo(ctx, pg.URL, 14); err != nil {
		t.Fatalf("down to 14: %v", err)
	}
	if err := asPlatform(`SELECT * FROM mhai_health_timings(interval '1 hour')`); pgCode(err) != "0A000" {
		t.Errorf("after down, timings(interval): want 0A000, got %v", err)
	}
	if err := asPlatform(`SELECT * FROM mhai_health_timings(interval '5 minutes')`); pgCode(err) != "22023" {
		t.Errorf("after down, a bad window: want 22023, got %v", err)
	}
	if err := asPlatform(`SELECT * FROM mhai_health_failures()`); pgCode(err) != "0A000" {
		t.Errorf("after down, failures: want 0A000, got %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname = 'mhai_health_guidelines'
		OR (proname = 'mhai_health_timings' AND pg_get_function_identity_arguments(oid) = 'p_window text')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("after down, %d of 00015's functions remain", n)
	}
	if n := policies(); n != 0 {
		t.Errorf("after down, %d health_read policies remain", n)
	}
	var grantees []string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(array_agg(DISTINCT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END), '{}')
		FROM pg_proc p, aclexplode(p.proacl) a
		WHERE p.proname IN ('mhai_health_timings', 'mhai_health_failures') AND a.privilege_type = 'EXECUTE' AND a.grantee <> p.proowner`).Scan(&grantees); err != nil {
		t.Fatal(err)
	}
	if len(grantees) != 1 || grantees[0] != "mhai_platform" {
		t.Errorf("after down, EXECUTE held by %v, want mhai_platform only", grantees)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
	built()
}
