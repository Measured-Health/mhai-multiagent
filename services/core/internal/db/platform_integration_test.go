//go:build integration

package db_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
)

// Migration 00014: the platform's reads. mhai_platform reaches clinical data
// only through four SECURITY DEFINER functions; it writes audit_verify_runs
// and appends spec §5.5's eight actions to the chain, nothing more.

const uniqueViolation = "23505"

// platformFunctions are the four functions with their final signatures
// (spec §6.1-§6.3): identity arguments and declared result, as Postgres
// prints them. Tasks 11-13 fill the bodies; the columns don't change.
var platformFunctions = map[string][2]string{
	"mhai_health_timings": {"time_window interval",
		"TABLE(step text, p50_ms integer, p95_ms integer, samples bigint)"},
	"mhai_health_failures": {"",
		"TABLE(job_type text, error text, failed_now bigint, failed_last_hour bigint, failed_last_24h bigint)"},
	"mhai_platform_jobs": {"p_status text, p_type text, p_hospital uuid, p_errors text[], p_after_failed_at timestamp with time zone, p_after_id uuid, p_limit integer",
		"TABLE(id uuid, type text, hospital_id uuid, created_at timestamp with time zone, failed_at timestamp with time zone, attempts integer, error text, status text, retry_count integer, cancelled_by uuid, cancelled_at timestamp with time zone)"},
	"mhai_guardrail_counts": {"p_from timestamp with time zone, p_to timestamp with time zone",
		"TABLE(hospital_id uuid, answers bigint, pass bigint, regenerate bigint, escalate bigint, degraded bigint)"},
}

// TestPlatformRoleHasNoClinicalRows (C17): mhai_platform holds no grant at
// all on a clinical table, so every read fails, with a row there to see;
// the four functions are its only way in, with their declared columns.
func TestPlatformRoleHasNoClinicalRows(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, q := seedHospital(t, pool, uniqueCode("C"))
	u := seedClinician(t, pool, a)
	seedCorrection(t, pool, a, q, u)
	seedJob(t, pool, a, u, "failed")
	if _, err := pool.Exec(ctx, bufferInsertSQL, a, seedApproved(t, pool, a)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO clinical_decisions (id, query_id, hospital_id) VALUES (gen_random_uuid(), $1, $2)`, q, a); err != nil {
		t.Fatalf("seed decision: %v", err)
	}

	clinical := []string{"clinical_queries", "clinical_decisions", "hga_evaluations", "feedback_events", "feedback_buffer", "jobs", "intelligence_audit_outbox"}
	for _, table := range clinical {
		err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
			var n int
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
		})
		requireDenied(t, err, "platform reading "+table)
		var any bool
		if err := pool.QueryRow(ctx,
			`SELECT has_table_privilege('mhai_platform', $1, 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
			     OR has_any_column_privilege('mhai_platform', $1, 'SELECT,INSERT,UPDATE,REFERENCES')`, table).Scan(&any); err != nil {
			t.Fatal(err)
		}
		if any {
			t.Errorf("mhai_platform holds a privilege on %s", table)
		}
	}

	for name, sig := range platformFunctions {
		var args, result, owner string
		var definer bool
		var config []string
		var grantees []string
		err := pool.QueryRow(ctx, `
			SELECT pg_get_function_identity_arguments(p.oid), pg_get_function_result(p.oid),
			       pg_get_userbyid(p.proowner), p.prosecdef, COALESCE(p.proconfig, '{}'),
			       COALESCE((SELECT array_agg(CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END)
			                 FROM aclexplode(p.proacl) a
			                 WHERE a.privilege_type = 'EXECUTE' AND a.grantee <> p.proowner), '{}')
			FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'public' AND p.proname = $1`, name).Scan(&args, &result, &owner, &definer, &config, &grantees)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if args != sig[0] || result != sig[1] {
			t.Errorf("%s(%s) RETURNS %s\nwant (%s) RETURNS %s", name, args, result, sig[0], sig[1])
		}
		if owner != "mhai_migrate" || !definer {
			t.Errorf("%s: owner %s, security definer %v", name, owner, definer)
		}
		if len(config) != 1 || config[0] != "search_path=pg_catalog, pg_temp" {
			t.Errorf("%s: config %v, want the fixed search_path", name, config)
		}
		sort.Strings(grantees)
		if len(grantees) != 1 || grantees[0] != "mhai_platform" {
			t.Errorf("%s: EXECUTE held by %v, want mhai_platform only", name, grantees)
		}
	}

	// The platform may call them (the bodies come with Tasks 11-13), and
	// the timing window is checked first; nobody else may call them.
	call := map[string]string{
		"mhai_health_timings":   `SELECT * FROM mhai_health_timings(interval '1 hour')`,
		"mhai_health_failures":  `SELECT * FROM mhai_health_failures()`,
		"mhai_platform_jobs":    `SELECT * FROM mhai_platform_jobs('failed', NULL, NULL, NULL, NULL, NULL, 50)`,
		"mhai_guardrail_counts": `SELECT * FROM mhai_guardrail_counts(now() - interval '7 days', now())`,
	}
	for name, q := range call {
		err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
		if pgCode(err) != "0A000" {
			t.Errorf("platform calling %s: want the not-built-yet refusal (0A000), got %v", name, err)
		}
		for _, role := range []string{"mhai_gateway", "mhai_auth", "mhai_worker", "mhai_jobs", "mhai_audit_drain", "mhai_provision"} {
			requireDenied(t, asRole(ctx, pool, role, q), role+" calling "+name)
		}
	}
	err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT * FROM mhai_health_timings(interval '5 minutes')`)
		return err
	})
	if pgCode(err) != "22023" {
		t.Errorf("a window other than 1 hour or 24 hours: want 22023, got %v", err)
	}
}

// platformActions are spec §5.5's eight.
var platformActions = []string{"audit.viewed", "audit.searched", "audit.verify_started", "audit.verify_finished",
	"audit.check_outcome", "audit.undisplayable_reported", "job.retried", "job.cancelled"}

func platformInsert(ctx context.Context, tx pgx.Tx, hospital *uuid.UUID, action string) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_log (hospital_id, action, payload, prev_hash, hash)
		VALUES ($1, $2, '{}', 'p', $3)`, hospital, action, uuid.NewString())
	return err
}

// TestPlatformAppendOnlyItsActions: mhai_platform appends the eight
// actions, with no hospital, and nothing else; and its append through
// audit.Write (the sequence, audit_chain_head) links to the chain's head.
func TestPlatformAppendOnlyItsActions(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("P"))
	for _, action := range platformActions {
		if err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error { return platformInsert(ctx, tx, nil, action) }); err != nil {
			t.Errorf("platform appending %s: %v", action, err)
		}
		err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error { return platformInsert(ctx, tx, &h, action) })
		requireDenied(t, err, "platform appending "+action+" for a hospital")
	}
	for _, action := range []string{"auth.logout", "auth.login.succeeded", "query.submitted", "query.completed",
		"correction.approved", "correction.applied", "feedback.validated", "hospital.created", "user.created",
		"intelligence.granted", "job.dead_lettered", "audit.viewed.extra", "AUDIT.VIEWED", "test.platform"} {
		err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error { return platformInsert(ctx, tx, nil, action) })
		requireDenied(t, err, "platform appending "+action)
	}

	// A real append, as a platform route makes it, extends the chain.
	var headBefore string
	err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT hash FROM audit_chain_head()`).Scan(&headBefore); err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Event{ActorID: uuid.NewString(), Action: "audit.searched",
			Entity: "audit_search", Payload: map[string]any{"searched_kind": "request"}})
	})
	if err != nil {
		t.Fatalf("audit.Write as the platform: %v", err)
	}
	var prev, action string
	var hospital *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT prev_hash, action, hospital_id FROM audit_log ORDER BY id DESC LIMIT 1`).
		Scan(&prev, &action, &hospital); err != nil {
		t.Fatal(err)
	}
	if prev != headBefore || action != "audit.searched" || hospital != nil {
		t.Errorf("platform row: prev %q (head was %q), action %s, hospital %v", prev, headBefore, action, hospital)
	}
	// Still append-only.
	for _, q := range []string{`UPDATE audit_log SET action = 'audit.viewed'`, `DELETE FROM audit_log`} {
		err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
		requireDenied(t, err, "platform "+q)
	}
}

// TestOneRunningVerifyRun: one running run at a time (spec §5.4, C25's
// database half); the platform writes runs but never deletes one or
// rewrites who started it; the row's own checks hold whoever writes.
func TestOneRunningVerifyRun(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM audit_verify_runs`) })
	if _, err := pool.Exec(ctx, `DELETE FROM audit_verify_runs`); err != nil {
		t.Fatal(err)
	}
	asPlatform := func(q string, args ...any) error {
		return db.WithPlatform(ctx, pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q, args...); return err })
	}
	var first uuid.UUID
	err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO audit_verify_runs DEFAULT VALUES RETURNING id`).Scan(&first)
	})
	if err != nil {
		t.Fatalf("platform starting a run: %v", err)
	}
	if err := asPlatform(`INSERT INTO audit_verify_runs DEFAULT VALUES`); pgCode(err) != uniqueViolation ||
		pgConstraint(err) != "audit_verify_one_running" {
		t.Fatalf("second running run: want audit_verify_one_running, got %v", err)
	}
	// intact needs every proof field (C24's database half).
	if err := asPlatform(`UPDATE audit_verify_runs SET state = 'intact', finished_at = now() WHERE id = $1`, first); pgCode(err) != checkViolation {
		t.Errorf("intact without proof: want 23514, got %v", err)
	}
	if err := asPlatform(`UPDATE audit_verify_runs SET state = 'intact', finished_at = now(), entries_checked = 3,
		from_first = true, to_seq = 3, to_at = now(), version = version + 1 WHERE id = $1`, first); err != nil {
		t.Fatalf("finishing the run: %v", err)
	}
	if err := asPlatform(`INSERT INTO audit_verify_runs DEFAULT VALUES`); err != nil {
		t.Fatalf("a run after the first finished: %v", err)
	}

	for name, q := range map[string]string{
		"unknown state":          `UPDATE audit_verify_runs SET state = 'fine' WHERE id = $1`,
		"note on an intact run":  `UPDATE audit_verify_runs SET outcome_note = 'synthetic', outcome_by = by, outcome_at = now() WHERE id = $1`,
		"error code when intact": `UPDATE audit_verify_runs SET error_code = 'timed_out' WHERE id = $1`,
		"broken seq when intact": `UPDATE audit_verify_runs SET first_broken_seq = 2, first_broken_id = '2' WHERE id = $1`,
	} {
		if err := asPlatform(q, first); pgCode(err) != checkViolation {
			t.Errorf("%s: want 23514, got %v", name, err)
		}
	}
	auditor := seedUser(t, pool, nil)
	broken := `INSERT INTO audit_verify_runs (state, finished_at, first_broken_seq, first_broken_id, first_broken_at,
		entries_checked, outcome_note, outcome_by, outcome_at) VALUES ('broken', now(), 2, '2', now(), 2, $1, $2, now())`
	long := strings.Repeat("n", 1001)
	if err := asPlatform(broken, long, auditor); pgCode(err) != checkViolation {
		t.Errorf("1001-character outcome note: want 23514, got %v", err)
	}
	if err := asPlatform(broken, long[:1000], auditor); err != nil {
		t.Errorf("1000-character outcome note on a broken run: %v", err)
	}
	if err := asPlatform(`INSERT INTO audit_verify_runs (state, finished_at, first_broken_seq, first_broken_id, first_broken_at,
		entries_checked, outcome_note, outcome_at) VALUES ('broken', now(), 2, '2', now(), 2, 'synthetic', now())`); pgCode(err) != checkViolation {
		t.Errorf("an outcome note with no author: want 23514, got %v", err)
	}

	for _, q := range []string{
		`DELETE FROM audit_verify_runs`,
		`UPDATE audit_verify_runs SET started_at = now() - interval '1 day'`,
		`UPDATE audit_verify_runs SET by = NULL`,
		`UPDATE audit_verify_runs SET id = gen_random_uuid()`,
	} {
		requireDenied(t, asPlatform(q), "platform "+q)
	}
	for _, role := range []string{"mhai_gateway", "mhai_auth", "mhai_worker", "mhai_jobs", "mhai_audit_drain", "mhai_provision"} {
		requireDenied(t, asRole(ctx, pool, role, `SELECT count(*) FROM audit_verify_runs`), role+" reading audit_verify_runs")
	}
}
