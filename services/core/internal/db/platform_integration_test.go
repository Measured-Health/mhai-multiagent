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
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00014: the platform's reads. mhai_platform reaches clinical data
// only through four SECURITY DEFINER functions; it writes audit_verify_runs
// and appends spec §5.5's eight actions to the chain, nothing more.

const uniqueViolation = "23505"

// platformFunctions are the platform's functions with their final
// signatures (spec §6.1-§6.3): identity arguments and declared result, as
// Postgres prints them. Tasks 11-13 fill the bodies. 00015 (Task 11)
// replaced the health window's interval with text (an interval can't tell
// '60 minutes' from '1 hour') and added mhai_health_guidelines. 00016
// (Task 12) added p_not_errors to mhai_platform_jobs (the "other" kind is
// every code that isn't another kind's).
var platformFunctions = map[string][2]string{
	"mhai_health_timings": {"p_window text",
		"TABLE(step text, p50_ms integer, p95_ms integer, samples bigint)"},
	"mhai_health_guidelines": {"", "boolean"},
	"mhai_health_failures": {"",
		"TABLE(job_type text, error text, failed_now bigint, failed_last_hour bigint, failed_last_24h bigint)"},
	"mhai_platform_jobs": {"p_status text, p_type text, p_hospital uuid, p_errors text[], p_not_errors text[], p_after_failed_at timestamp with time zone, p_after_id uuid, p_limit integer",
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

	// C17, any path: the exact set of relations mhai_platform holds any
	// privilege on (directly, through PUBLIC or a column grant), each with
	// its reason. A new grant to the platform fails here until it is
	// justified in this list.
	wantRelations := map[string]string{
		"hospitals":         "00003: names and status for the platform screens",
		"users":             "00003: column SELECT without password_hash (the audit's actor, people search)",
		"platform_users":    "00003: security_invoker view over users' granted columns",
		"user_roles":        "00003: roles of the audit's actors",
		"sessions":          "00003: session metadata (no tokens: those are refresh_tokens)",
		"audit_log":         "00003: read the chain; append its eight actions (00014 platform_append)",
		"audit_checkpoints": "00003: the chain's checkpoints, for verification",
		"idempotency_keys":  "00003: Idempotency-Key replies of platform writes, null-hospital rows only",
		"audit_verify_runs": "00014: the chain checks",
	}
	got := map[string]bool{}
	rows, err := pool.Query(ctx, `
		SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'
		  AND c.relkind IN ('r', 'v', 'm', 'p', 'f')
		  AND (has_table_privilege('mhai_platform', c.oid, 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
		       OR has_any_column_privilege('mhai_platform', c.oid, 'SELECT,INSERT,UPDATE,REFERENCES'))`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		got[name] = true
		if _, ok := wantRelations[name]; !ok {
			t.Errorf("mhai_platform holds a privilege on %s, which isn't in the justified list", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for name := range wantRelations {
		if !got[name] {
			t.Errorf("mhai_platform has no privilege on %s: update the justified list", name)
		}
	}
	// Sequences: the chain's id only (00003, for its appends).
	var seqs []string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(array_agg(c.relname ORDER BY c.relname), '{}') FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
		  AND CASE WHEN c.relkind = 'S' THEN has_sequence_privilege('mhai_platform', c.oid, 'USAGE,SELECT,UPDATE') END`).Scan(&seqs); err != nil {
		t.Fatal(err)
	}
	if len(seqs) != 1 || seqs[0] != "audit_log_id_seq" {
		t.Errorf("mhai_platform may use sequences %v, want audit_log_id_seq only", seqs)
	}

	// The exact set of SECURITY DEFINER functions it may run, PUBLIC's
	// EXECUTE included, each with its full arguments and result: the four
	// of 00014 (one replaced by 00015), 00015's guidelines check, the
	// buffer count (00013) and the chain head (00003).
	wantFuncs := map[string][2]string{
		"audit_chain_head":             {"OUT id bigint, OUT hash text", "record"},
		"mhai_feedback_buffer_waiting": {"", "bigint"},
	}
	for name, sig := range platformFunctions {
		wantFuncs[name] = sig
	}
	gotFuncs := map[string]bool{}
	rows, err = pool.Query(ctx, `
		SELECT p.proname, pg_get_function_arguments(p.oid), pg_get_function_result(p.oid)
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND p.prosecdef AND has_function_privilege('mhai_platform', p.oid, 'EXECUTE')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, args, result string
		if err := rows.Scan(&name, &args, &result); err != nil {
			t.Fatal(err)
		}
		gotFuncs[name] = true
		sig, ok := wantFuncs[name]
		if !ok {
			t.Errorf("mhai_platform may run SECURITY DEFINER %s(%s), which isn't in the list", name, args)
			continue
		}
		if args != sig[0] || result != sig[1] {
			t.Errorf("%s(%s) RETURNS %s\nwant (%s) RETURNS %s", name, args, result, sig[0], sig[1])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for name := range wantFuncs {
		if !gotFuncs[name] {
			t.Errorf("mhai_platform can't run %s", name)
		}
	}

	// And it is a member of no role, inheriting or not, so no grant reaches
	// it through another role.
	var member []string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(array_agg(r.rolname), '{}') FROM pg_roles r
		WHERE r.rolname <> 'mhai_platform' AND pg_has_role('mhai_platform', r.oid, 'MEMBER')`).Scan(&member); err != nil {
		t.Fatal(err)
	}
	if len(member) != 0 {
		t.Errorf("mhai_platform is a member of %v", member)
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
		// The health functions are owned by their own column-limited reader
		// (00015); the rest by mhai_migrate.
		wantOwner := "mhai_migrate"
		switch {
		case strings.HasPrefix(name, "mhai_health_"):
			wantOwner = "mhai_health_reader"
		case name == "mhai_platform_jobs":
			// 00016 (Task 12): the failed-jobs list's own reader.
			wantOwner = "mhai_jobs_reader"
		case name == "mhai_guardrail_counts":
			// 00017 (Task 13): the guardrail statistics' own reader.
			wantOwner = "mhai_guardrail_reader"
		}
		if owner != wantOwner || !definer {
			t.Errorf("%s: owner %s (want %s), security definer %v", name, owner, wantOwner, definer)
		}
		if len(config) != 1 || config[0] != "search_path=pg_catalog, pg_temp" {
			t.Errorf("%s: config %v, want the fixed search_path", name, config)
		}
		sort.Strings(grantees)
		if len(grantees) != 1 || grantees[0] != "mhai_platform" {
			t.Errorf("%s: EXECUTE held by %v, want mhai_platform only", name, grantees)
		}
	}

	// The platform may call them: the health bodies are built (00015), the
	// jobs list (00016) and the guardrail counts (00017); nobody else may
	// call any.
	call := map[string]string{
		"mhai_health_timings":    `SELECT * FROM mhai_health_timings('1 hour')`,
		"mhai_health_failures":   `SELECT * FROM mhai_health_failures()`,
		"mhai_health_guidelines": `SELECT mhai_health_guidelines()`,
		"mhai_platform_jobs":     `SELECT * FROM mhai_platform_jobs('failed', NULL, NULL, NULL, NULL, NULL, NULL, 50)`,
		"mhai_guardrail_counts":  `SELECT * FROM mhai_guardrail_counts(now() - interval '7 days', now())`,
	}
	built := map[string]bool{"mhai_health_timings": true, "mhai_health_failures": true, "mhai_health_guidelines": true,
		"mhai_platform_jobs": true, "mhai_guardrail_counts": true}
	for name, q := range call {
		err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
		switch {
		case built[name] && err != nil:
			t.Errorf("platform calling %s: %v", name, err)
		case !built[name] && pgCode(err) != "0A000":
			t.Errorf("platform calling %s: want the not-built-yet refusal (0A000), got %v", name, err)
		}
		for _, role := range []string{"mhai_gateway", "mhai_auth", "mhai_worker", "mhai_jobs", "mhai_audit_drain", "mhai_provision"} {
			requireDenied(t, asRole(ctx, pool, role, q), role+" calling "+name)
		}
	}
	// The window is exactly '1 hour' or '24 hours', compared as text: an
	// equal interval written another way is refused too.
	for _, w := range []string{"'1 hour'", "'24 hours'"} {
		err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT * FROM mhai_health_timings(`+w+`)`)
			return err
		})
		if err != nil {
			t.Errorf("window %s: %v", w, err)
		}
	}
	for _, w := range []string{"'5 minutes'", "'60 minutes'", "'3600 seconds'", "'1 day'", "'01:00:00'", "'1 hours'",
		"'1 Hour'", "' 1 hour'", "'24 hour'", "''", "NULL"} {
		err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT * FROM mhai_health_timings(`+w+`)`)
			return err
		})
		if pgCode(err) != "22023" {
			t.Errorf("window %s: want 22023, got %v", w, err)
		}
	}
	// The interval form 00014 had is gone.
	err = db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT * FROM mhai_health_timings(interval '1 hour')`)
		return err
	})
	if err == nil {
		t.Error("mhai_health_timings(interval) still exists")
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

	// A real append, as a platform route makes it, extends the chain: the
	// row it wrote (found by its own id, through the sequence it used)
	// links to the row before it.
	var written int64
	err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		if err := audit.Write(ctx, tx, audit.Event{ActorID: uuid.NewString(), Action: "audit.searched",
			Entity: "audit_search", Payload: map[string]any{"searched_kind": "request"}}); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT currval('audit_log_id_seq')`).Scan(&written)
	})
	if err != nil {
		t.Fatalf("audit.Write as the platform: %v", err)
	}
	var prev, before, action string
	var hospital *uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT a.prev_hash, a.action, a.hospital_id,
		       (SELECT b.hash FROM audit_log b WHERE b.id < a.id ORDER BY b.id DESC LIMIT 1)
		FROM audit_log a WHERE a.id = $1`, written).Scan(&prev, &action, &hospital, &before); err != nil {
		t.Fatal(err)
	}
	if prev != before || action != "audit.searched" || hospital != nil {
		t.Errorf("platform row %d: prev %q (the row before has %q), action %s, hospital %v", written, prev, before, action, hospital)
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
	// intact needs every proof field (C24's database half), entries_checked
	// included: a NULL there must not slip through the CHECK.
	for name, q := range map[string]string{
		"no proof at all":      `UPDATE audit_verify_runs SET state = 'intact', finished_at = now() WHERE id = $1`,
		"no entries_checked":   `UPDATE audit_verify_runs SET state = 'intact', finished_at = now(), from_first = true, to_seq = 3, to_at = now() WHERE id = $1`,
		"no entries (0)":       `UPDATE audit_verify_runs SET state = 'intact', finished_at = now(), entries_checked = 0, from_first = true, to_seq = 3, to_at = now() WHERE id = $1`,
		"no to_seq":            `UPDATE audit_verify_runs SET state = 'intact', finished_at = now(), entries_checked = 3, from_first = true, to_at = now() WHERE id = $1`,
		"no to_at":             `UPDATE audit_verify_runs SET state = 'intact', finished_at = now(), entries_checked = 3, from_first = true, to_seq = 3 WHERE id = $1`,
		"not from the first":   `UPDATE audit_verify_runs SET state = 'intact', finished_at = now(), entries_checked = 3, to_seq = 3, to_at = now() WHERE id = $1`,
		"finished but running": `UPDATE audit_verify_runs SET finished_at = now() WHERE id = $1`,
	} {
		if err := asPlatform(q, first); pgCode(err) != checkViolation {
			t.Errorf("intact, %s: want 23514, got %v", name, err)
		}
	}
	if err := asPlatform(`UPDATE audit_verify_runs SET state = 'intact', finished_at = now(), entries_checked = 3,
		from_first = true, to_seq = 3, to_at = now(), version = version + 1 WHERE id = $1`, first); err != nil {
		t.Fatalf("finishing the run: %v", err)
	}
	var second uuid.UUID
	err = db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO audit_verify_runs DEFAULT VALUES RETURNING id`).Scan(&second)
	})
	if err != nil {
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

	// M1: a finished run is fixed but for its outcome and version, even
	// where the CHECKs would allow the change, and whoever writes it.
	for name, q := range map[string]string{
		"rewrite entries_checked": `UPDATE audit_verify_runs SET entries_checked = 4 WHERE id = $1`,
		"rewrite to_seq":          `UPDATE audit_verify_runs SET to_seq = 2 WHERE id = $1`,
		"reopen":                  `UPDATE audit_verify_runs SET state = 'running', finished_at = NULL WHERE id = $1`,
		"intact to error":         `UPDATE audit_verify_runs SET state = 'error', error_code = 'timed_out', entries_checked = NULL WHERE id = $1`,
		"move its finish":         `UPDATE audit_verify_runs SET finished_at = finished_at - interval '1 hour' WHERE id = $1`,
	} {
		if err := asPlatform(q, first); pgCode(err) != checkViolation {
			t.Errorf("finished run, %s: want 23514, got %v", name, err)
		}
		if _, err := pool.Exec(ctx, q, first); pgCode(err) != checkViolation {
			t.Errorf("finished run as the owner, %s: want 23514, got %v", name, err)
		}
	}
	if err := asPlatform(`UPDATE audit_verify_runs SET version = version + 1 WHERE id = $1`, first); err != nil {
		t.Errorf("a version bump on a finished run: %v", err)
	}

	// A broken run takes an outcome: note, author and time, the note at
	// most 1000 characters; then nothing but the outcome moves.
	if err := asPlatform(`UPDATE audit_verify_runs SET state = 'broken', finished_at = now(), entries_checked = 2,
		first_broken_seq = 2, first_broken_id = '2', first_broken_at = now() WHERE id = $1`, second); err != nil {
		t.Fatalf("finishing a run broken: %v", err)
	}
	auditor := seedUser(t, pool, nil)
	outcome := `UPDATE audit_verify_runs SET outcome_note = $2, outcome_by = $3, outcome_at = now(), version = version + 1 WHERE id = $1`
	long := strings.Repeat("n", 1001)
	if err := asPlatform(outcome, second, long, auditor); pgCode(err) != checkViolation {
		t.Errorf("1001-character outcome note: want 23514, got %v", err)
	}
	if err := asPlatform(`UPDATE audit_verify_runs SET outcome_note = 'synthetic', outcome_at = now() WHERE id = $1`, second); pgCode(err) != checkViolation {
		t.Errorf("an outcome note with no author: want 23514, got %v", err)
	}
	if err := asPlatform(outcome, second, long[:1000], auditor); err != nil {
		t.Errorf("1000-character outcome note on a broken run: %v", err)
	}
	if err := asPlatform(`UPDATE audit_verify_runs SET first_broken_seq = 3, first_broken_id = '3' WHERE id = $1`, second); pgCode(err) != checkViolation {
		t.Errorf("moving a broken run's first broken entry: want 23514, got %v", err)
	}

	// The platform starts runs only as running, with no outcome.
	for name, q := range map[string]string{
		"an intact run": `INSERT INTO audit_verify_runs (state, finished_at, entries_checked, from_first, to_seq, to_at)
			VALUES ('intact', now(), 5, true, 5, now())`,
		"an error run": `INSERT INTO audit_verify_runs (state, finished_at, error_code) VALUES ('error', now(), 'timed_out')`,
		// The proof fields are written only when a run finishes (Task 8
		// review): a running row born with them could be finished intact
		// without a walk.
		"running with entries_checked": `INSERT INTO audit_verify_runs (entries_checked) VALUES (5)`,
		"running from the first":       `INSERT INTO audit_verify_runs (from_first) VALUES (true)`,
		"running with to_seq":          `INSERT INTO audit_verify_runs (to_seq) VALUES (5)`,
		"running with to_at":           `INSERT INTO audit_verify_runs (to_at) VALUES (now())`,
		"running with every proof": `INSERT INTO audit_verify_runs (entries_checked, from_first, to_seq, to_at)
			VALUES (5, true, 5, now())`,
		"running at version 2": `INSERT INTO audit_verify_runs (version) VALUES (2)`,
	} {
		requireDenied(t, asPlatform(q), "platform inserting "+name)
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

// TestMigration14DownRestoresPlatformAppend: 00014's Down puts 00003's
// unrestricted platform append back and drops both audit_log indexes; Up
// applies again.
func TestMigration14DownRestoresPlatformAppend(t *testing.T) {
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
	state := func() (check string, indexes int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT coalesce((SELECT with_check FROM pg_policies WHERE tablename = 'audit_log' AND policyname = 'platform_append'), ''),
			(SELECT count(*) FROM pg_indexes WHERE tablename = 'audit_log' AND indexname IN ('audit_log_actor_idx', 'audit_log_request_idx'))`).
			Scan(&check, &indexes); err != nil {
			t.Fatal(err)
		}
		return check, indexes
	}
	if check, n := state(); check == "true" || n != 2 {
		t.Fatalf("after up: with check %q, %d indexes", check, n)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 13); err != nil {
		t.Fatalf("down to 13: %v", err)
	}
	if check, n := state(); check != "true" || n != 0 {
		t.Fatalf("after down: with check %q (want true), %d indexes (want 0)", check, n)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if check, n := state(); check == "true" || n != 2 {
		t.Fatalf("after up again: with check %q, %d indexes", check, n)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
}
