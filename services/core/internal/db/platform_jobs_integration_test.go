//go:build integration

package db_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00016 (Plan 4 Task 12): the failed jobs list's function and
// reader, query_cancellations, retry_count and failed_at on questions,
// queued_at on jobs, and the narrowed mhai_jobs. Every id and string is
// synthetic.

// platformJobs runs mhai_platform_jobs as mhai_platform: (id, shown
// status) of each row, for hospital h.
func platformJobs(t *testing.T, h uuid.UUID, status string, in, notIn []string) map[uuid.UUID]string {
	t.Helper()
	out := map[uuid.UUID]string{}
	err := db.WithPlatform(context.Background(), sharedPool, func(tx pgx.Tx) error {
		r, err := tx.Query(context.Background(), `SELECT id, status FROM mhai_platform_jobs($1, NULL, $2, $3, $4, NULL, NULL, 100)`,
			status, h, in, notIn)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var id uuid.UUID
			var s string
			if err := r.Scan(&id, &s); err != nil {
				return err
			}
			out[id] = s
		}
		return r.Err()
	})
	if err != nil {
		t.Fatalf("mhai_platform_jobs(%s): %v", status, err)
	}
	return out
}

func seedQuestionAs(t *testing.T, h uuid.UUID, status, errCode string, retries int, done string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var e any
	if errCode != "" {
		e = errCode
	}
	if _, err := sharedPool.Exec(context.Background(), `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, status, error, retry_count, completed_at)
		VALUES ($1, $2, 'synthetic question', 'routine', $3, $4, $5, now() - $6::interval)`, id, h, status, e, retries, done); err != nil {
		t.Fatal(err)
	}
	return id
}

// The list function: the contract's status from status and retry_count,
// the 30-day window, the kind codes in and out, and its argument checks.
func TestPlatformJobsFunction(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("L"))
	failed := seedQuestionAs(t, h, "failed", "internal", 0, "1 minute")
	expired := seedQuestionAs(t, h, "failed", "deadline_exceeded", 0, "40 days") // failed: listed however old
	retrying := seedQuestionAs(t, h, "queued", "", 1, "1 minute")
	retried := seedQuestionAs(t, h, "completed", "", 1, "1 minute")
	oldRetried := seedQuestionAs(t, h, "completed", "", 1, "40 days")
	cancelled := seedQuestionAs(t, h, "cancelled", "", 0, "1 minute")
	seedQuestionAs(t, h, "completed", "", 0, "1 minute") // never listed
	seedQuestionAs(t, h, "queued", "", 0, "1 minute")    // never listed
	u := seedClinician(t, pool, h)
	job := seedJob(t, pool, h, u, "failed")
	if _, err := pool.Exec(ctx, `UPDATE jobs SET error = 'unavailable', failed_at = now() WHERE id = $1`, job); err != nil {
		t.Fatal(err)
	}
	all := platformJobs(t, h, "all", nil, nil)
	want := map[uuid.UUID]string{failed: "failed", expired: "failed", retrying: "retrying", retried: "retried",
		cancelled: "cancelled", job: "failed"}
	if len(all) != len(want) {
		t.Errorf("all: %v, want %v", all, want)
	}
	for id, st := range want {
		if all[id] != st {
			t.Errorf("%s: %q, want %q", id, all[id], st)
		}
	}
	if _, ok := all[oldRetried]; ok {
		t.Error("a retried job older than 30 days is listed")
	}
	for status, ids := range map[string][]uuid.UUID{
		"failed": {failed, expired, job}, "retrying": {retrying}, "retried": {retried}, "cancelled": {cancelled},
	} {
		got := platformJobs(t, h, status, nil, nil)
		if len(got) != len(ids) {
			t.Errorf("%s: %v", status, got)
		}
		for _, id := range ids {
			if got[id] != status {
				t.Errorf("%s: %s missing", status, id)
			}
		}
	}
	// Codes in (a kind), and codes out (other: an empty code included).
	if got := platformJobs(t, h, "all", []string{"deadline_exceeded", "overloaded"}, nil); len(got) != 1 || got[expired] == "" {
		t.Errorf("timeout codes: %v", got)
	}
	got := platformJobs(t, h, "all", nil, []string{"deadline_exceeded", "overloaded", "unavailable", "unauthorized", "forbidden", "invalid_request"})
	for _, id := range []uuid.UUID{failed, retrying, retried, cancelled} {
		if got[id] == "" {
			t.Errorf("other: %s missing (%v)", id, got)
		}
	}
	if got[expired] != "" || got[job] != "" {
		t.Errorf("other: %v", got)
	}
	for _, q := range []string{
		`SELECT * FROM mhai_platform_jobs('running', NULL, NULL, NULL, NULL, NULL, NULL, 10)`,
		`SELECT * FROM mhai_platform_jobs(NULL, NULL, NULL, NULL, NULL, NULL, NULL, 10)`,
		`SELECT * FROM mhai_platform_jobs('all', NULL, NULL, NULL, NULL, now(), NULL, 10)`,
	} {
		err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
		if pgCode(err) != "22023" {
			t.Errorf("%s: want 22023, got %v", q, err)
		}
	}
	// Keyset: after the newest row, the rest.
	var first uuid.UUID
	var at time.Time
	if err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id, failed_at FROM mhai_platform_jobs('all', NULL, $1, NULL, NULL, NULL, NULL, 1)`, h).Scan(&first, &at)
	}); err != nil {
		t.Fatal(err)
	}
	var rest int
	if err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM mhai_platform_jobs('all', NULL, $1, NULL, NULL, $2, $3, 100) WHERE id <> $3`, h, at, first).Scan(&rest)
	}); err != nil || rest != len(want)-1 {
		t.Errorf("after the first: %d %v", rest, err)
	}
}

// mhai_jobs_reader: a role like 00015's mhai_health_reader, owning only
// the list function, reading only the listed columns of listable rows.
func TestJobsReaderScoped(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, plain := seedHospital(t, pool, uniqueCode("R"))
	failed := seedQuestionAs(t, h, "failed", "internal", 0, "1 minute")
	var login, inherit, bypass bool
	var members, memberOf int
	if err := pool.QueryRow(ctx, `
		SELECT r.rolcanlogin, r.rolinherit, r.rolbypassrls,
		       (SELECT count(*) FROM pg_auth_members m WHERE m.roleid = r.oid),
		       (SELECT count(*) FROM pg_auth_members m WHERE m.member = r.oid)
		FROM pg_roles r WHERE r.rolname = 'mhai_jobs_reader'`).Scan(&login, &inherit, &bypass, &members, &memberOf); err != nil {
		t.Fatal(err)
	}
	if login || inherit || bypass || members != 0 || memberOf != 0 {
		t.Errorf("mhai_jobs_reader: login %v inherit %v bypassrls %v, %d members, member of %d", login, inherit, bypass, members, memberOf)
	}
	var owned []string
	var rels int
	if err := pool.QueryRow(ctx, `SELECT COALESCE(array_agg(p.proname::text), '{}') FROM pg_proc p WHERE p.proowner = 'mhai_jobs_reader'::regrole`).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relowner = 'mhai_jobs_reader'::regrole`).Scan(&rels); err != nil {
		t.Fatal(err)
	}
	if strings.Join(owned, ",") != "mhai_platform_jobs" || rels != 0 {
		t.Errorf("mhai_jobs_reader owns %v and %d relations", owned, rels)
	}
	if n, err := countAs(t, pool, "mhai_jobs_reader", `SELECT count(*) FROM clinical_queries WHERE id = ANY($1)`, []uuid.UUID{failed, plain}); err != nil || n != 1 {
		t.Errorf("reader sees %d of a failed and a plain question (%v)", n, err)
	}
	for _, q := range []string{`SELECT count(clinical_question) FROM clinical_queries`, `SELECT count(patient_data_json) FROM clinical_queries`,
		`SELECT count(created_by) FROM clinical_queries`, `SELECT count(subject_id) FROM jobs`, `SELECT count(created_by) FROM jobs`,
		`SELECT count(cancel_note) FROM jobs`, `SELECT count(cancel_note) FROM query_cancellations`, `SELECT count(*) FROM feedback_events`,
		`SELECT count(*) FROM clinical_decisions`} {
		_, err := countAs(t, pool, "mhai_jobs_reader", q)
		requireDenied(t, err, "mhai_jobs_reader: "+q)
	}
	// Nobody but mhai_platform runs the function.
	for _, role := range []string{"mhai_gateway", "mhai_auth", "mhai_worker", "mhai_jobs", "mhai_audit_drain"} {
		requireDenied(t, asRole(ctx, pool, role, `SELECT * FROM mhai_platform_jobs('failed', NULL, NULL, NULL, NULL, NULL, NULL, 1)`), role)
	}
}

// query_cancellations: mhai_jobs records only a question it has cancelled;
// no other role reads it (the note and the platform admin's id never reach
// the worker or the gateway).
func TestQueryCancellationsScoped(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	_, app := loginPool(t, "IN ROLE mhai_app")
	h, queued := seedHospital(t, pool, uniqueCode("C"))
	admin := seedUser(t, pool, nil)
	cancelled := seedQuestionAs(t, h, "cancelled", "", 0, "1 minute")
	ins := `INSERT INTO query_cancellations (query_id, hospital_id, cancelled_by, cancel_note) VALUES ($1, $2, $3, 'synthetic')`
	if err := db.WithJobs(ctx, app, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, ins, cancelled, h, admin); return err }); err != nil {
		t.Errorf("jobs role recording a cancelled question: %v", err)
	}
	err := db.WithJobs(ctx, app, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, ins, queued, h, admin); return err })
	requireDenied(t, err, "jobs role recording a queued question")
	for _, role := range []string{"mhai_worker", "mhai_gateway", "mhai_platform", "mhai_jobs", "mhai_audit_drain"} {
		_, err := inTx(ctx, pool, []string{`SET LOCAL ROLE ` + role}, &h, `SELECT cancel_note FROM query_cancellations`)
		requireDenied(t, err, role+" reading query_cancellations")
	}
	// The worker and gateway read clinical_queries whole (00008): the new
	// columns there hold no text.
	for _, col := range []string{"retry_count", "failed_at"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'clinical_queries' AND column_name = $1
			AND data_type IN ('integer', 'timestamp with time zone')`, col).Scan(&n); err != nil || n != 1 {
			t.Errorf("clinical_queries.%s: %d %v", col, n, err)
		}
	}
}

// failed_at is stamped whoever fails a question, and kept through a retry;
// a job's queued_at is its created_at, and the gateway can't set another.
func TestFailedAtAndQueuedAt(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	_, app := loginPool(t, "IN ROLE mhai_app")
	h, q := seedHospital(t, pool, uniqueCode("F"))
	if _, err := pool.Exec(ctx, `UPDATE clinical_queries SET deadline_at = now() + interval '5 minutes' WHERE id = $1`, q); err != nil {
		t.Fatal(err)
	}
	err := db.WithJobs(ctx, app, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE clinical_queries SET status = 'failed', error = 'internal' WHERE id = $1`, q)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var failedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT failed_at FROM clinical_queries WHERE id = $1`, q).Scan(&failedAt); err != nil || failedAt == nil {
		t.Fatalf("failed_at: %v %v", failedAt, err)
	}
	err = db.WithJobs(ctx, app, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE clinical_queries SET status = 'queued', error = NULL, retry_count = retry_count + 1 WHERE id = $1`, q)
		return err
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	var kept *time.Time
	if err := pool.QueryRow(ctx, `SELECT failed_at FROM clinical_queries WHERE id = $1`, q).Scan(&kept); err != nil || kept == nil || !kept.Equal(*failedAt) {
		t.Errorf("failed_at after a retry: %v (was %v) %v", kept, failedAt, err)
	}

	u := seedClinician(t, pool, h)
	j := seedJob(t, pool, h, u, "queued")
	var same bool
	if err := pool.QueryRow(ctx, `SELECT queued_at = created_at FROM jobs WHERE id = $1`, j).Scan(&same); err != nil || !same {
		t.Errorf("queued_at = created_at: %v %v", same, err)
	}
	corr := seedCorrection(t, pool, h, seedQueryIn(t, pool, h), u)
	asUserSetup := []string{`SET LOCAL ROLE mhai_gateway`, `SELECT set_config('mhai.user_id', '` + u.String() + `', true)`}
	_, err = inTx(ctx, pool, asUserSetup, &h, `INSERT INTO jobs (id, hospital_id, type, subject_id, created_by, queued_at)
		VALUES (gen_random_uuid(), $1, 'approve_correction', $2, $3, now() - interval '1 day')`, h, corr, u)
	requireDenied(t, err, "gateway insert ahead of the queue")
	if n, err := inTx(ctx, pool, asUserSetup, &h, `INSERT INTO jobs (id, hospital_id, type, subject_id, created_by)
		VALUES (gen_random_uuid(), $1, 'approve_correction', $2, $3)`, h, corr, u); err != nil || n != 1 {
		t.Errorf("gateway insert: %d %v", n, err)
	}
	// mhai_jobs reads and moves queued_at; nobody else may read it but the owner.
	if err := db.WithJobs(ctx, app, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT queued_at FROM jobs LIMIT 1`)
		return err
	}); err != nil {
		t.Errorf("jobs role reading queued_at: %v", err)
	}
}

// 00016 goes down to 00015 and up again; the stub of 00014 is back while
// it is down.
func TestMigration00016RoundTrip(t *testing.T) {
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
	h, _ := seedHospital(t, pool, uniqueCode("M"))
	admin := seedUser(t, pool, nil)
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, status) VALUES ($1, $2, 'synthetic', 'routine', 'cancelled')`, id, h); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO query_cancellations (query_id, hospital_id, cancelled_by, cancel_note) VALUES ($1, $2, $3, 'synthetic')`, id, h, admin); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 15); err != nil {
		t.Fatalf("down to 15: %v", err)
	}
	var table, newFn, oldFn bool
	var cols []string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.query_cancellations') IS NOT NULL,
		to_regprocedure('mhai_platform_jobs(text, text, uuid, text[], text[], timestamptz, uuid, integer)') IS NOT NULL,
		to_regprocedure('mhai_platform_jobs(text, text, uuid, text[], timestamptz, uuid, integer)') IS NOT NULL,
		ARRAY(SELECT table_name||'.'||column_name FROM information_schema.columns
		      WHERE (table_name = 'clinical_queries' AND column_name IN ('retry_count', 'failed_at'))
		         OR (table_name = 'jobs' AND column_name = 'queued_at'))`).Scan(&table, &newFn, &oldFn, &cols); err != nil {
		t.Fatal(err)
	}
	if table || newFn || !oldFn || len(cols) != 0 {
		t.Fatalf("after down: table %v new %v old %v columns %v", table, newFn, oldFn, cols)
	}
	var owner string
	if err := pool.QueryRow(ctx, `SELECT pg_get_userbyid(proowner) FROM pg_proc WHERE proname = 'mhai_platform_jobs'`).Scan(&owner); err != nil || owner != "mhai_migrate" {
		t.Errorf("the stub's owner: %s %v", owner, err)
	}
	var policies []string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(array_agg(policyname::text), '{}') FROM pg_policies WHERE policyname IN ('jobs_list_read', 'jobs_facts_read')`).Scan(&policies); err != nil || len(policies) != 0 {
		t.Errorf("policies left: %v %v", policies, err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
}
