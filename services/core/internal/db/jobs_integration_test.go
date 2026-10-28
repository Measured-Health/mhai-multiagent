//go:build integration

package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
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
			_, err := tx.Exec(ctx, `INSERT INTO audit_log (hospital_id, action, prev_hash, hash) VALUES ($1, $2, 'p', $3)`,
				hid, action, "h"+uuid.NewString())
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
