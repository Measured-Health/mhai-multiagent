//go:build integration

package db_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// The job columns of migration 00007 (plan departures 1 and 8).
func TestJobColumns(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	want := map[string][]string{
		"clinical_queries":   {"request_id", "attempts", "lease_until", "published_at", "deadline_at", "cancel_reason"},
		"clinical_decisions": {"verified", "degraded", "failed_agents", "model_versions", "trust_band"},
	}
	for table, cols := range want {
		for _, c := range cols {
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`, table, c).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("%s.%s missing", table, c)
			}
		}
	}
	hid, qid := seedHospital(t, pool, uniqueCode("J"))
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT attempts FROM clinical_queries WHERE id = $1`, qid).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("attempts default: %d %v", attempts, err)
	}
	// The status set and the error code shape are checked by the table.
	for _, bad := range []string{
		`UPDATE clinical_queries SET status = 'processing' WHERE id = $1`,
		`UPDATE clinical_queries SET error = 'Model crashed: stack trace' WHERE id = $1`,
		`UPDATE clinical_queries SET error = '' WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, bad, qid); pgCode(err) != "23514" {
			t.Errorf("%s: want check violation, got %v", bad, err)
		}
	}
	for _, st := range []string{"queued", "running", "completed", "failed", "cancelled"} {
		if _, err := pool.Exec(ctx, `UPDATE clinical_queries SET status = $2, error = 'deadline_exceeded' WHERE id = $1`, qid, st); err != nil {
			t.Errorf("status %s: %v", st, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO clinical_decisions (id, hospital_id, query_id, trust_band) VALUES ($1, $2, $3, 'certain')`,
		uuid.New(), hid, qid); pgCode(err) != "23514" {
		t.Errorf("trust_band outside low/mid/high: %v", err)
	}
	var idx int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'clinical_queries'
		AND indexdef LIKE '%(status, published_at)%'`).Scan(&idx); err != nil || idx != 1 {
		t.Errorf("sweeper index: %d %v", idx, err)
	}
}

// mhai_jobs sees no clinical text and no other table, writes only the job
// columns, and audits only its own actions.
func TestJobsRoleIsNarrow(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	_, app := loginPool(t, "IN ROLE mhai_app")
	hid, qid := seedHospital(t, pool, uniqueCode("N"))

	deny := func(what, q string, args ...any) {
		t.Helper()
		err := db.WithJobs(ctx, app, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, args...)
			return err
		})
		requireDenied(t, err, what)
	}
	deny("patient data", `SELECT patient_data_json FROM clinical_queries`)
	deny("question", `SELECT clinical_question FROM clinical_queries`)
	deny("every column", `SELECT * FROM clinical_queries`)
	deny("decisions", `SELECT * FROM clinical_decisions`)
	deny("users", `SELECT id FROM users`)
	deny("sessions", `SELECT id FROM sessions`)
	deny("audit read", `SELECT id FROM audit_log`)
	deny("kb", `SELECT id FROM kb_chunks`)
	deny("update question", `UPDATE clinical_queries SET clinical_question = 'x' WHERE id = $1`, qid)
	deny("update attempts", `UPDATE clinical_queries SET attempts = 9 WHERE id = $1`, qid)
	deny("update hospital", `UPDATE clinical_queries SET hospital_id = $2 WHERE id = $1`, qid, uuid.New())
	deny("insert query", `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency) VALUES ($1, $2, 'x', 'routine')`, uuid.New(), hid)
	deny("delete query", `DELETE FROM clinical_queries WHERE id = $1`, qid)
	deny("foreign audit action", `INSERT INTO audit_log (actor_id, hospital_id, action, prev_hash, hash) VALUES (NULL, $1, 'auth.login.succeeded', 'p', $2)`,
		hid, "h"+uuid.NewString())
	// A job can be failed, cancelled or re-queued, never marked completed
	// or running: only the worker does that.
	deny("mark completed", `UPDATE clinical_queries SET status = 'completed' WHERE id = $1`, qid)
	deny("mark running", `UPDATE clinical_queries SET status = 'running' WHERE id = $1`, qid)

	// A completed question is final: the jobs role can't re-queue it.
	if _, err := pool.Exec(ctx, `UPDATE clinical_queries SET status = 'completed' WHERE id = $1`, qid); err != nil {
		t.Fatal(err)
	}
	err := db.WithJobs(ctx, app, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE clinical_queries SET status = 'queued', published_at = NULL WHERE id = $1`, qid)
		if err == nil && tag.RowsAffected() != 0 {
			t.Errorf("jobs role re-queued a completed question")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE clinical_queries SET status = 'queued' WHERE id = $1`, qid); err != nil {
		t.Fatal(err)
	}

	// What it is for: the job columns across hospitals, and its own audit.
	other, otherQ := seedHospital(t, pool, uniqueCode("O"))
	err = db.WithJobs(ctx, app, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM clinical_queries WHERE id = ANY($1)`, []uuid.UUID{qid, otherQ}).Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			t.Errorf("jobs role sees %d of 2 hospitals' rows", n)
		}
		var id, h, by uuid.UUID
		var rid *string
		var status string
		var attempts int
		if err := tx.QueryRow(ctx, `SELECT id, hospital_id, COALESCE(created_by, $2), request_id, status, attempts, published_at, deadline_at, created_at
			FROM clinical_queries WHERE id = $1`, otherQ, uuid.Nil).Scan(&id, &h, &by, &rid, &status, &attempts, nil, nil, nil); err != nil {
			return err
		}
		if h != other {
			t.Errorf("hospital %s, want %s", h, other)
		}
		if _, err := tx.Exec(ctx, `UPDATE clinical_queries SET published_at = now() WHERE id = $1`, qid); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE clinical_queries SET status = 'cancelled', cancel_reason = 'account_inactive',
			completed_at = now() WHERE id = $1`, otherQ); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE clinical_queries SET status = 'failed', error = 'deadline_exceeded' WHERE id = $1`, qid); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT id, hash FROM audit_chain_head()`)
		return err
	})
	if err != nil {
		t.Fatalf("jobs role work: %v", err)
	}
	for _, action := range []string{"query.failed", "query.cancelled", "job.dead_lettered", "job.retried", "intelligence.refused"} {
		err := db.WithJobs(ctx, app, func(tx pgx.Tx) error {
			// 00009: about a job of the row's own hospital, with no actor.
			_, err := tx.Exec(ctx, `INSERT INTO audit_log (hospital_id, action, entity, entity_id, prev_hash, hash)
				VALUES ($1, $2, 'query', $3, 'p', $4)`, hid, action, qid.String(), "h"+uuid.NewString())
			return err
		})
		if err != nil {
			t.Errorf("audit %s: %v", action, err)
		}
	}
}

// F20: core's start-up login check knows mhai_jobs, so a login that
// inherits it is refused by name.
func TestCheckLoginRoleKnowsJobs(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	name, login := loginPool(t, "IN ROLE mhai_app")
	if err := db.CheckLoginRole(ctx, login); err != nil {
		t.Fatalf("plain mhai_app member: %v", err)
	}
	if _, err := pool.Exec(ctx, `GRANT mhai_jobs TO `+name); err != nil {
		t.Fatal(err)
	}
	err := db.CheckLoginRole(ctx, login)
	if err == nil || !strings.Contains(err.Error(), "inherits the privileges of mhai_jobs") {
		t.Fatalf("login inheriting mhai_jobs: %v", err)
	}
	// The real grant: mhai_app holds mhai_jobs without inheriting it, so
	// the core login can only SET ROLE to it inside db.WithJobs.
	var inherit, set bool
	if err := pool.QueryRow(ctx, `SELECT m.inherit_option, m.set_option FROM pg_auth_members m
		JOIN pg_roles r ON r.oid = m.roleid JOIN pg_roles u ON u.oid = m.member
		WHERE r.rolname = 'mhai_jobs' AND u.rolname = 'mhai_app'`).Scan(&inherit, &set); err != nil {
		t.Fatal(err)
	}
	if inherit || !set {
		t.Fatalf("mhai_app -> mhai_jobs: inherit %v set %v, want false, true", inherit, set)
	}
	var super, bypass, canLogin bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls, rolcanlogin FROM pg_roles WHERE rolname = 'mhai_jobs'`).
		Scan(&super, &bypass, &canLogin); err != nil || super || bypass || canLogin {
		t.Fatalf("mhai_jobs: super %v bypassrls %v login %v (%v)", super, bypass, canLogin, err)
	}
}

// The jobs role's transitions: it may move rows no worker holds between
// queued, failed and cancelled; it can't touch a running or completed row
// directly (0 rows), and can't set running or completed (refused). The one
// way it ends a running row is mhai_fail_expired_queries(), only past the
// deadline, only to failed / deadline_exceeded.
func TestJobsRoleTransitions(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	_, app := loginPool(t, "IN ROLE mhai_app")
	hid, _ := seedHospital(t, pool, uniqueCode("T"))
	row := func(status, extra string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, status, created_at, deadline_at)
			VALUES ($1, $2, 'synthetic', 'routine', $3, now() - interval '20 minutes', `+extra+`)`, id, hid, status); err != nil {
			t.Fatal(err)
		}
		return id
	}
	update := func(id uuid.UUID, set string) (int64, error) {
		var n int64
		err := db.WithJobs(ctx, app, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE clinical_queries SET `+set+` WHERE id = $1`, id)
			n = tag.RowsAffected()
			return err
		})
		return n, err
	}
	statusOf := func(id uuid.UUID) string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT status FROM clinical_queries WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	future := "now() + interval '5 minutes'"
	for _, c := range []struct{ from, set, to string }{
		{"queued", "status = 'failed', error = 'deadline_exceeded'", "failed"},
		{"queued", "status = 'cancelled', cancel_reason = 'account_unusable'", "cancelled"},
		{"queued", "published_at = now()", "queued"},
		{"failed", "status = 'queued', error = NULL", "queued"},
		{"cancelled", "status = 'queued'", "queued"},
	} {
		id := row(c.from, future)
		if n, err := update(id, c.set); err != nil || n != 1 || statusOf(id) != c.to {
			t.Errorf("%s -> %s: %d rows, %v, now %s", c.from, c.to, n, err, statusOf(id))
		}
	}
	// No touching a row a worker holds, or a finished one.
	for _, c := range []struct{ from, set string }{
		{"running", "status = 'queued'"},
		{"running", "status = 'failed', error = 'deadline_exceeded'"},
		{"running", "status = 'cancelled'"},
		{"completed", "status = 'queued'"},
		{"completed", "status = 'failed'"},
	} {
		id := row(c.from, future)
		if n, err := update(id, c.set); err != nil || n != 0 || statusOf(id) != c.from {
			t.Errorf("%s: %s: %d rows, %v", c.from, c.set, n, err)
		}
	}
	for _, set := range []string{"status = 'running'", "status = 'completed'"} {
		_, err := update(row("queued", future), set)
		requireDenied(t, err, "queued: "+set)
	}

	// The expiry function: expired queued and running rows (published or
	// not, with or without a deadline) become failed / deadline_exceeded;
	// live, finished and other rows stay.
	expiredRunning := row("running", "now() - interval '1 minute'")
	expiredQueued := row("queued", "now() - interval '1 minute'")
	noDeadline := row("queued", "NULL") // created 20 minutes ago: past created_at + 10 minutes
	liveRunning := row("running", future)
	doneOld := row("completed", "now() - interval '1 minute'")
	var got []uuid.UUID
	err := db.WithJobs(ctx, app, func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `SELECT id FROM mhai_fail_expired_queries(1000)`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var id uuid.UUID
			if err := r.Scan(&id); err != nil {
				return err
			}
			got = append(got, id)
		}
		return r.Err()
	})
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	for _, id := range []uuid.UUID{expiredRunning, expiredQueued, noDeadline} {
		var st, e string
		if err := pool.QueryRow(ctx, `SELECT status, error FROM clinical_queries WHERE id = $1`, id).Scan(&st, &e); err != nil ||
			st != "failed" || e != "deadline_exceeded" || !slices.Contains(got, id) {
			t.Errorf("%s: %s %s (returned %v) %v", id, st, e, slices.Contains(got, id), err)
		}
	}
	for id, want := range map[uuid.UUID]string{liveRunning: "running", doneOld: "completed"} {
		if st := statusOf(id); st != want || slices.Contains(got, id) {
			t.Errorf("%s: %s, want %s", id, st, want)
		}
	}
	// Only mhai_jobs may run it.
	err = db.WithHospital(ctx, app, hid, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT * FROM mhai_fail_expired_queries(1)`)
		return err
	})
	requireDenied(t, err, "gateway running the expiry function")
}

// Migration 00012: the generic jobs table (Plan 4 spec §6.5). Every id and
// string is synthetic.

// seedJob inserts an approve_correction job as the owner connection.
func seedJob(t *testing.T, pool *pgxpool.Pool, hospital, createdBy uuid.UUID, status string) uuid.UUID {
	t.Helper()
	return seedJobFor(t, pool, hospital, createdBy, status, seedCorrection(t, pool, hospital, seedQueryIn(t, pool, hospital), createdBy))
}

// seedJobFor is seedJob for a given subject (a correction of the hospital).
func seedJobFor(t *testing.T, pool *pgxpool.Pool, hospital, createdBy uuid.UUID, status string, subject uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO jobs (id, hospital_id, type, subject_id, created_by, request_id, status, deadline_at)
		 VALUES ($1, $2, 'approve_correction', $3, $4, 'req-synthetic', $5, now() + interval '5 minutes')`,
		id, hospital, subject, createdBy, status); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	return id
}

func jobStatus(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM jobs WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJobsTableShape(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	var owner string
	var rls, force bool
	if err := pool.QueryRow(ctx, `SELECT pg_get_userbyid(relowner), relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = 'public.jobs'::regclass`).
		Scan(&owner, &rls, &force); err != nil || owner != "mhai_migrate" || !rls || !force {
		t.Fatalf("jobs owner %q rls %v force %v: %v", owner, rls, force, err)
	}
	a, _ := seedHospital(t, pool, uniqueCode("S"))
	u := seedClinician(t, pool, a)
	id := seedJob(t, pool, a, u, "queued")
	for _, bad := range []string{
		`UPDATE jobs SET type = 'run_query' WHERE id = $1`,
		`UPDATE jobs SET error = 'Free text: boom' WHERE id = $1`,
		`UPDATE jobs SET cancel_reason = 'Has Spaces' WHERE id = $1`,
		`UPDATE jobs SET cancel_note = repeat('x', 501) WHERE id = $1`,
		`UPDATE jobs SET status = 'processing' WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, bad, id); pgCode(err) != "23514" {
			t.Errorf("%s: want check violation, got %v", bad, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET cancel_note = repeat('x', 500) WHERE id = $1`, id); err != nil {
		t.Errorf("500-char note: %v", err)
	}
}

// The worker moves only queued or running rows, only to running, completed
// or failed, and never lowers attempts.
func TestJobsWorkerStatusMoves(t *testing.T) {
	_, super := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, super, uniqueCode("W"))
	u := seedClinician(t, super, a)
	move := func(id uuid.UUID, set string) (int64, error) {
		return asWorker(ctx, super, a, `UPDATE jobs SET `+set+` WHERE id = $1`, id)
	}
	q := seedJob(t, super, a, u, "queued")
	r := seedJob(t, super, a, u, "running")
	for _, c := range []struct {
		id  uuid.UUID
		set string
	}{
		{q, "status = 'running', attempts = attempts + 1, lease_until = now()"},
		{r, "status = 'completed', completed_at = now()"},
		{r, "status = 'failed', error = 'internal', failed_at = now()"},
		{r, "lease_until = now()"},
	} {
		if n, err := move(c.id, c.set); err != nil || n != 1 {
			t.Errorf("allowed move %q: %d rows, %v", c.set, n, err)
		}
	}
	// Not allowed: the row stays as it was (0 rows) or the write is refused.
	for _, c := range []struct{ from, set string }{
		{"queued", "status = 'cancelled'"},
		{"running", "status = 'cancelled'"},
		{"running", "status = 'queued'"},
		{"completed", "status = 'running'"},
		{"completed", "status = 'failed', error = 'internal'"},
		{"failed", "status = 'running'"},
		{"cancelled", "status = 'running'"},
	} {
		id := seedJob(t, super, a, u, c.from)
		n, err := move(id, c.set)
		if err == nil && n != 0 {
			t.Errorf("%s -> %s: worker moved the row", c.from, c.set)
		}
	}
	// attempts never goes down.
	id := seedJob(t, super, a, u, "running")
	if _, err := super.Exec(ctx, `UPDATE jobs SET attempts = 3 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := move(id, "attempts = 2"); pgCode(err) != "23514" {
		t.Errorf("attempts lowered: %v", err)
	}
	// Columns the worker may not write.
	for _, col := range []string{"type = type", "subject_id = gen_random_uuid()", "cancel_reason = 'x'", "retry_count = 9", "published_at = now()", "hospital_id = hospital_id"} {
		_, err := move(id, col)
		requireDenied(t, err, "worker "+col)
	}
	// Another hospital's worker sees none of it.
	b, _ := seedHospital(t, super, uniqueCode("X"))
	if n, err := asWorker(ctx, super, b, `UPDATE jobs SET lease_until = now() WHERE id = $1`, id); err != nil || n != 0 {
		t.Errorf("other hospital's worker: %d %v", n, err)
	}
}

func TestJobsWorkerOnlyActiveHospital(t *testing.T) {
	_, super := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, super, uniqueCode("V"))
	u := seedClinician(t, super, a)
	if n, err := asWorker(ctx, super, a, `UPDATE jobs SET lease_until = now() WHERE id = $1`, seedJob(t, super, a, u, "running")); err != nil || n != 1 {
		t.Fatalf("active hospital: %d %v", n, err)
	}
	if _, err := super.Exec(ctx, `UPDATE hospitals SET status = 'deactivated' WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	// Rows the deactivation trigger never saw: inserted after it ran.
	queued, running := seedJob(t, super, a, u, "queued"), seedJob(t, super, a, u, "running")
	for _, id := range []uuid.UUID{queued, running} {
		for _, set := range []string{"status = 'running'", "lease_until = now()", "status = 'failed', error = 'internal'"} {
			if n, err := asWorker(ctx, super, a, `UPDATE jobs SET `+set+` WHERE id = $1`, id); err != nil || n != 0 {
				t.Errorf("worker %q in a deactivated hospital: %d %v", set, n, err)
			}
		}
	}
}

// mhai_jobs can't touch a running or completed job (C29), and can move
// queued, failed and cancelled ones among themselves.
func TestJobsJobsRoleCantTouchRunning(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	_, app := loginPool(t, "IN ROLE mhai_app")
	a, _ := seedHospital(t, pool, uniqueCode("R"))
	u := seedClinician(t, pool, a)
	update := func(id uuid.UUID, set string) (int64, error) {
		var n int64
		err := db.WithJobs(ctx, app, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE jobs SET `+set+` WHERE id = $1`, id)
			n = tag.RowsAffected()
			return err
		})
		return n, err
	}
	for _, from := range []string{"running", "completed"} {
		for _, set := range []string{"status = 'queued'", "status = 'cancelled', cancel_reason = 'platform_cancelled'",
			"status = 'failed', error = 'internal'", "retry_count = retry_count + 1"} {
			id := seedJob(t, pool, a, u, from)
			if n, err := update(id, set); err != nil || n != 0 || jobStatus(t, pool, id) != from {
				t.Errorf("%s: %s: %d rows, %v", from, set, n, err)
			}
		}
	}
	for _, c := range []struct{ from, set, to string }{
		{"failed", "status = 'queued', error = NULL, retry_count = retry_count + 1", "queued"},
		{"queued", "status = 'cancelled', cancel_reason = 'platform_cancelled', cancelled_at = now(), cancel_note = 'synthetic'", "cancelled"},
		{"queued", "published_at = now()", "queued"},
		{"queued", "status = 'failed', error = 'deadline_exceeded', failed_at = now()", "failed"},
	} {
		id := seedJob(t, pool, a, u, c.from)
		if n, err := update(id, c.set); err != nil || n != 1 || jobStatus(t, pool, id) != c.to {
			t.Errorf("%s -> %s: %d rows, %v", c.from, c.to, n, err)
		}
	}
	// It can't mark a job running or completed.
	for _, set := range []string{"status = 'running'", "status = 'completed'"} {
		_, err := update(seedJob(t, pool, a, u, "queued"), set)
		requireDenied(t, err, "queued: "+set)
	}
	// Columns outside its list.
	_, err := update(seedJob(t, pool, a, u, "queued"), "attempts = 5")
	requireDenied(t, err, "attempts")
	// job.dead_lettered is one of its audit actions (job.cancelled and job.retried
	// are per-call platform rows, written by mhai_platform).
	for _, action := range []string{"job.dead_lettered"} {
		id := seedJob(t, pool, a, u, "failed")
		err := db.WithJobs(ctx, app, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO audit_log (hospital_id, action, entity, entity_id, payload, prev_hash, hash)
				VALUES ($1, $2, 'job', $3, '{}', 'p', $4)`, a, action, id.String(), uuid.NewString())
			return err
		})
		if err != nil {
			t.Errorf("append %s for a job: %v", action, err)
		}
	}
	// ... and only for a job of the row's own hospital.
	b, _ := seedHospital(t, pool, uniqueCode("Q"))
	err = db.WithJobs(ctx, app, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO audit_log (hospital_id, action, entity, entity_id, payload, prev_hash, hash)
			VALUES ($1, 'job.dead_lettered', 'job', $2, '{}', 'p', $3)`, b, seedJob(t, pool, a, u, "failed").String(), uuid.NewString())
		return err
	})
	requireDenied(t, err, "job.dead_lettered for another hospital's job")
}

func TestJobsOneLivePerSubject(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("L"))
	u := seedClinician(t, pool, a)
	subject := seedCorrection(t, pool, a, seedQueryIn(t, pool, a), u)
	ins := func(status string) error {
		_, err := pool.Exec(ctx, `INSERT INTO jobs (id, hospital_id, type, subject_id, created_by, status)
			VALUES ($1, $2, 'approve_correction', $3, $4, $5)`, uuid.New(), a, subject, u, status)
		return err
	}
	if err := ins("queued"); err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{"queued", "running"} {
		if err := ins(st); pgCode(err) != "23505" {
			t.Errorf("second live job (%s): %v", st, err)
		}
	}
	for _, st := range []string{"failed", "cancelled", "completed"} {
		if err := ins(st); err != nil {
			t.Errorf("finished job (%s) beside a live one: %v", st, err)
		}
	}
}

func TestJobsExpiredFailed(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	_, app := loginPool(t, "IN ROLE mhai_app")
	a, _ := seedHospital(t, pool, uniqueCode("E"))
	u := seedClinician(t, pool, a)
	row := func(status, deadline string) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO jobs (id, hospital_id, type, subject_id, created_by, status, deadline_at, created_at)
			VALUES ($1, $2, 'approve_correction', $3, $4, $5, `+deadline+`, now() - interval '20 minutes')`,
			id, a, seedCorrection(t, pool, a, seedQueryIn(t, pool, a), u), u, status); err != nil {
			t.Fatal(err)
		}
		return id
	}
	expiredRunning := row("running", "now() - interval '1 minute'")
	expiredQueued := row("queued", "now() - interval '1 minute'")
	noDeadline := row("queued", "NULL")
	live := row("running", "now() + interval '5 minutes'")
	done := row("completed", "now() - interval '1 minute'")
	var got []uuid.UUID
	err := db.WithJobs(ctx, app, func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `SELECT id FROM mhai_fail_expired_jobs(1000)`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var id uuid.UUID
			if err := r.Scan(&id); err != nil {
				return err
			}
			got = append(got, id)
		}
		return r.Err()
	})
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	for _, id := range []uuid.UUID{expiredRunning, expiredQueued, noDeadline} {
		var st, e string
		if err := pool.QueryRow(ctx, `SELECT status, error FROM jobs WHERE id = $1`, id).Scan(&st, &e); err != nil ||
			st != "failed" || e != "deadline_exceeded" || !slices.Contains(got, id) {
			t.Errorf("%s: %s %s %v", id, st, e, err)
		}
	}
	for id, want := range map[uuid.UUID]string{live: "running", done: "completed"} {
		if jobStatus(t, pool, id) != want || slices.Contains(got, id) {
			t.Errorf("%s changed", id)
		}
	}
	err = db.WithHospital(ctx, app, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT * FROM mhai_fail_expired_jobs(1)`)
		return err
	})
	requireDenied(t, err, "gateway running the job expiry function")
}

// A21's twin: deactivating a user or a hospital cancels its queued and
// running jobs in the database; finished and other rows are untouched.
func TestDeactivationCancelsJobs(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("D"))
	leaving, staying := seedClinician(t, pool, a), seedClinician(t, pool, a)
	queued, running, done := seedJob(t, pool, a, leaving, "queued"), seedJob(t, pool, a, leaving, "running"), seedJob(t, pool, a, leaving, "completed")
	other := seedJob(t, pool, a, staying, "queued")
	if _, err := pool.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1`, leaving); err != nil {
		t.Fatalf("deactivation: %v", err)
	}
	for _, id := range []uuid.UUID{queued, running} {
		var st, reason string
		var completed *time.Time
		if err := pool.QueryRow(ctx, `SELECT status, cancel_reason, completed_at FROM jobs WHERE id = $1`, id).Scan(&st, &reason, &completed); err != nil ||
			st != "cancelled" || reason != "user_deactivated" || completed == nil {
			t.Errorf("%s: %s %s %v %v", id, st, reason, completed, err)
		}
	}
	if s := jobStatus(t, pool, done); s != "completed" {
		t.Errorf("finished job: %s", s)
	}
	if s := jobStatus(t, pool, other); s != "queued" {
		t.Errorf("another user's job: %s", s)
	}
	// The worker's next write finds no row.
	if n, err := asWorker(ctx, pool, a, `UPDATE jobs SET lease_until = now() WHERE id = $1`, running); err != nil || n != 0 {
		t.Errorf("worker after cancel: %d %v", n, err)
	}

	// A hospital.
	b, _ := seedHospital(t, pool, uniqueCode("H"))
	c, _ := seedHospital(t, pool, uniqueCode("K"))
	ub, uc := seedClinician(t, pool, b), seedClinician(t, pool, c)
	jb, jc := seedJob(t, pool, b, ub, "running"), seedJob(t, pool, c, uc, "queued")
	if _, err := pool.Exec(ctx, `UPDATE hospitals SET status = 'deactivated' WHERE id = $1`, b); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT cancel_reason FROM jobs WHERE id = $1 AND status = 'cancelled'`, jb).Scan(&reason); err != nil || reason != "hospital_deactivated" {
		t.Errorf("hospital job: %q %v", reason, err)
	}
	if s := jobStatus(t, pool, jc); s != "queued" {
		t.Errorf("another hospital's job: %s", s)
	}
}

// The owner's cancel policies on jobs are as scoped as on clinical_queries.
func TestJobsCancelPoliciesAreScoped(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("O"))
	u := seedClinician(t, pool, a)
	id := seedJob(t, pool, a, u, "queued")
	n, err := inTx(ctx, pool, []string{`SET LOCAL ROLE mhai_migrate`}, nil,
		`UPDATE jobs SET status = 'cancelled', cancel_reason = 'user_deactivated' WHERE id = $1`, id)
	if err != nil || n != 0 {
		t.Errorf("owner cancelling an active user's job: %d %v", n, err)
	}
}

// The outbox names exactly one parent, a query or a job.
func TestOutboxExactlyOneParent(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, q := seedHospital(t, pool, uniqueCode("P"))
	u := seedClinician(t, pool, a)
	j := seedJob(t, pool, a, u, "completed")
	ins := func(action string, query, job any) error {
		_, err := pool.Exec(ctx, `INSERT INTO intelligence_audit_outbox (hospital_id, query_id, job_id, action, payload)
			VALUES ($1, $2, $3, $4, '{}')`, a, query, job, action)
		return err
	}
	if err := ins("query.completed", nil, nil); pgCode(err) != "23514" {
		t.Errorf("no parent: %v", err)
	}
	if err := ins("correction.applied", q, j); pgCode(err) != "23514" {
		t.Errorf("both parents: %v", err)
	}
	if err := ins("query.completed", nil, j); pgCode(err) != "23514" {
		t.Errorf("query action with a job parent: %v", err)
	}
	if err := ins("correction.applied", q, nil); pgCode(err) != "23514" {
		t.Errorf("correction action with a query parent: %v", err)
	}
	if err := ins("query.completed", q, nil); err != nil {
		t.Errorf("query parent: %v", err)
	}
	for _, action := range []string{"correction.applied", "correction.apply_failed"} {
		if err := ins(action, nil, j); err != nil {
			t.Errorf("job parent, %s: %v", action, err)
		}
	}
	if err := ins("correction.nonsense", nil, j); pgCode(err) != "23514" {
		t.Errorf("unknown action: %v", err)
	}
	// The job must be of the fact's own hospital.
	b, _ := seedHospital(t, pool, uniqueCode("Z"))
	if _, err := pool.Exec(ctx, `INSERT INTO intelligence_audit_outbox (hospital_id, job_id, action, payload)
		VALUES ($1, $2, 'correction.applied', '{}')`, b, j); pgCode(err) != "23503" {
		t.Errorf("job of another hospital: %v", err)
	}
	// The drainer reads job_id facts and the drain role may append the two
	// correction actions.
	if n, err := asDrain(ctx, pool, `SELECT id FROM intelligence_audit_outbox WHERE job_id IS NOT NULL`); err != nil || n != 2 {
		t.Errorf("drain read: %d %v", n, err)
	}
	if n, err := asDrain(ctx, pool, `SELECT id, hospital_id, created_by, request_id, status, type FROM jobs WHERE id = $1`, j); err != nil || n != 1 {
		t.Errorf("drain reads jobs: %d %v", n, err)
	}
	_, err := asDrain(ctx, pool, `SELECT subject_id FROM jobs`)
	requireDenied(t, err, "drain reading subject_id")
	for _, action := range []string{"correction.applied", "correction.apply_failed"} {
		if _, err := asDrain(ctx, pool, `INSERT INTO audit_log (hospital_id, action, payload, prev_hash, hash) VALUES ($1, $2, '{}', 'p', $3)`, a, action, uuid.NewString()); err != nil {
			t.Errorf("drain appending %s: %v", action, err)
		}
	}
}

// 00012 goes down and up again and leaves the earlier migrations as they were.
func TestMigration00012RoundTrip(t *testing.T) {
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
	a, _ := seedHospital(t, pool, uniqueCode("M"))
	u := seedClinician(t, pool, a)
	j := seedJob(t, pool, a, u, "completed")
	if _, err := pool.Exec(ctx, `INSERT INTO intelligence_audit_outbox (hospital_id, job_id, action, payload) VALUES ($1, $2, 'correction.applied', '{}')`, a, j); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 11); err != nil {
		t.Fatalf("down to 11: %v", err)
	}
	var jobs, fn bool
	var nullable string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.jobs') IS NOT NULL, to_regprocedure('mhai_fail_expired_jobs(integer)') IS NOT NULL,
		(SELECT is_nullable FROM information_schema.columns WHERE table_name = 'intelligence_audit_outbox' AND column_name = 'query_id')`).
		Scan(&jobs, &fn, &nullable); err != nil || jobs || fn || nullable != "NO" {
		t.Fatalf("after down: jobs %v fn %v query_id nullable %s: %v", jobs, fn, nullable, err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
}

// Fix round 1: grants, hospital scoping, the new-job policy.
func TestJobsGrantsAndSubjectScoping(t *testing.T) {
	_, super := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, super, uniqueCode("F"))
	b, _ := seedHospital(t, super, uniqueCode("G"))
	ua, ub := seedClinician(t, super, a), seedClinician(t, super, b)
	ja := seedJob(t, super, a, ua, "queued")

	// Neither the worker nor the gateway reads cancel_note or cancelled_by.
	for _, role := range []string{"mhai_worker", "mhai_gateway"} {
		for _, q := range []string{`SELECT cancel_note FROM jobs`, `SELECT cancelled_by FROM jobs`, `SELECT * FROM jobs`} {
			_, err := inTx(ctx, super, []string{`SET LOCAL ROLE ` + role}, &a, q)
			requireDenied(t, err, role+" "+q)
		}
		if n, err := inTx(ctx, super, []string{`SET LOCAL ROLE ` + role}, &a, `SELECT id, status, error FROM jobs WHERE id = $1`, ja); err != nil || n != 1 {
			t.Errorf("%s reading job columns: %d %v", role, n, err)
		}
	}

	// A job naming another hospital's correction is refused, as owner and gateway.
	cb := seedCorrection(t, super, b, seedQueryIn(t, super, b), ub)
	ins := `INSERT INTO jobs (id, hospital_id, type, subject_id, created_by) VALUES (gen_random_uuid(), $1, 'approve_correction', $2, $3)`
	if _, err := super.Exec(ctx, ins, a, cb, ua); pgCode(err) != "23514" {
		t.Errorf("owner: job on another hospital's correction: %v", err)
	}
	if _, err := inTx(ctx, super, []string{`SET LOCAL ROLE mhai_gateway`}, &a, ins, a, cb, ua); pgCode(err) != "23514" {
		t.Errorf("gateway: job on another hospital's correction: %v", err)
	}
	if _, err := super.Exec(ctx, ins, a, uuid.New(), ua); pgCode(err) != "23514" {
		t.Errorf("job on no correction: %v", err)
	}
	// A correction of its own hospital is accepted by the gateway.
	own := seedCorrection(t, super, a, seedQueryIn(t, super, a), ua)
	if n, err := inTx(ctx, super, []string{`SET LOCAL ROLE mhai_gateway`}, &a, ins, a, own, ua); err != nil || n != 1 {
		t.Errorf("gateway: job on own correction: %d %v", n, err)
	}

	// The unique index is per hospital: the same subject id in two
	// hospitals doesn't collide (hospital_id is the scope, so a foreign id
	// can't block). The subject check is lifted for this one probe.
	if _, err := super.Exec(ctx, `ALTER TABLE jobs DISABLE TRIGGER jobs_subject`); err != nil {
		t.Fatal(err)
	}
	subject := uuid.New()
	_, e1 := super.Exec(ctx, ins, a, subject, ua)
	_, e2 := super.Exec(ctx, ins, b, subject, ub)
	_, e3 := super.Exec(ctx, ins, a, subject, ua)
	_, _ = super.Exec(ctx, `ALTER TABLE jobs ENABLE TRIGGER jobs_subject`)
	if e1 != nil || e2 != nil || pgCode(e3) != "23505" {
		t.Errorf("per-hospital uniqueness: %v %v %v", e1, e2, e3)
	}

	// The gateway inserts only a fresh queued job.
	for _, col := range []string{"status = 'completed'", "status = 'running'", "attempts = 1", "retry_count = 1",
		"cancelled_by = $3", "cancel_note = 'x'", "cancel_reason = 'x'", "error = 'x'"} {
		name, val, _ := strings.Cut(col, " = ")
		corr := seedCorrection(t, super, a, seedQueryIn(t, super, a), ua)
		q := `INSERT INTO jobs (id, hospital_id, type, subject_id, created_by, ` + name + `) VALUES (gen_random_uuid(), $1, 'approve_correction', $2, $3, ` + val + `)`
		_, err := inTx(ctx, super, []string{`SET LOCAL ROLE mhai_gateway`}, &a, q, a, corr, ua)
		requireDenied(t, err, "gateway insert with "+col)
	}
}
