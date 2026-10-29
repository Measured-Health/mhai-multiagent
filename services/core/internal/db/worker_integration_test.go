//go:build integration

package db_test

import (
	"context"
	"fmt"
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

// Migration 00008: the pipeline's own login (plan departure 2), the
// narrowed mhai_worker grants (spec §8.2), the audit outbox and the
// mhai_audit_drain role (spec §10.2, plan departure 3). Every id, password
// and row here is synthetic.

const rlsViolation = "42501" // "new row violates row-level security policy" shares 42501

// intelligenceLogin provisions a pipeline login the way core migrate up
// does (EnsureLogin, member of mhai_worker_app only) and connects as it.
func intelligenceLogin(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	pg, super := shared(t)
	name := "intel_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	pw := "synthetic-intel-password-" + uuid.NewString()
	if err := db.EnsureLogin(context.Background(), super, name, pw, "mhai_worker_app"); err != nil {
		t.Fatalf("ensure intelligence login: %v", err)
	}
	p := connectAs(t, pg, name, pw)
	t.Cleanup(p.Close)
	return name, p
}

// inTx runs q in one transaction on pool after the setup statements, and
// returns the rows affected. The transaction always rolls back.
func inTx(ctx context.Context, pool *pgxpool.Pool, setup []string, hospital *uuid.UUID, q string, args ...any) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, s := range setup {
		if _, err := tx.Exec(ctx, s); err != nil {
			return 0, err
		}
	}
	if hospital != nil {
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, hospital.String()); err != nil {
			return 0, err
		}
	}
	tag, err := tx.Exec(ctx, q, args...)
	return tag.RowsAffected(), err
}

func asWorker(ctx context.Context, pool *pgxpool.Pool, hospital uuid.UUID, q string, args ...any) (int64, error) {
	return inTx(ctx, pool, []string{`SET LOCAL ROLE mhai_worker`}, &hospital, q, args...)
}

func asDrain(ctx context.Context, pool *pgxpool.Pool, q string, args ...any) (int64, error) {
	return inTx(ctx, pool, []string{`SET LOCAL ROLE mhai_audit_drain`}, nil, q, args...)
}

// A22, core side: the provisioned intelligence login holds nothing until
// SET LOCAL ROLE mhai_worker, and can reach no other role.
func TestIntelligenceLoginHoldsNothing(t *testing.T) {
	_, super := shared(t)
	ctx := context.Background()
	name, intel := intelligenceLogin(t)
	hid, qid := seedHospital(t, super, uniqueCode("I"))

	for _, q := range []string{
		`SELECT 1 FROM clinical_queries`,
		`SELECT 1 FROM clinical_decisions`,
		`SELECT 1 FROM kb_chunks`,
		`SELECT 1 FROM guardrail_weights`,
		`SELECT 1 FROM intelligence_audit_outbox`,
		`SELECT 1 FROM users`,
		`SELECT 1 FROM audit_log`,
		`SELECT * FROM audit_chain_head()`,
	} {
		_, err := inTx(ctx, intel, nil, nil, q)
		requireDenied(t, err, "intelligence login outside SET ROLE: "+q)
	}
	for _, role := range []string{"mhai_gateway", "mhai_auth", "mhai_platform", "mhai_jobs", "mhai_audit_drain",
		"mhai_migrate", "mhai_app", "mhai_provision"} {
		_, err := inTx(ctx, intel, nil, nil, `SET LOCAL ROLE `+role)
		requireDenied(t, err, "intelligence login SET ROLE "+role)
	}
	// The one way in.
	var n int
	err := func() error {
		tx, err := intel.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_worker`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, hid.String()); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM clinical_queries WHERE id = $1`, qid).Scan(&n)
	}()
	if err != nil || n != 1 {
		t.Fatalf("SET LOCAL ROLE mhai_worker: n=%d err=%v", n, err)
	}

	// The shape behind it: the login inherits nothing that holds a grant.
	var usesWorker, usesApp, appInherits bool
	if err := super.QueryRow(ctx, `SELECT pg_has_role($1, 'mhai_worker', 'USAGE'), pg_has_role($1, 'mhai_worker_app', 'MEMBER'),
		(SELECT rolinherit FROM pg_roles WHERE rolname = 'mhai_worker_app')`, name).
		Scan(&usesWorker, &usesApp, &appInherits); err != nil {
		t.Fatal(err)
	}
	if usesWorker || !usesApp || appInherits {
		t.Fatalf("login uses mhai_worker %v, member of mhai_worker_app %v, mhai_worker_app INHERIT %v", usesWorker, usesApp, appInherits)
	}
	var inherit, set bool
	if err := super.QueryRow(ctx, `SELECT m.inherit_option, m.set_option FROM pg_auth_members m
		JOIN pg_roles r ON r.oid = m.roleid JOIN pg_roles u ON u.oid = m.member
		WHERE r.rolname = 'mhai_worker' AND u.rolname = 'mhai_worker_app'`).Scan(&inherit, &set); err != nil {
		t.Fatal(err)
	}
	if inherit || !set {
		t.Fatalf("mhai_worker_app -> mhai_worker: inherit %v set %v, want false, true", inherit, set)
	}
	// mhai_worker_app itself holds no privilege on any table.
	var direct int
	if err := super.QueryRow(ctx, `SELECT count(*) FROM information_schema.role_table_grants WHERE grantee = 'mhai_worker_app'`).
		Scan(&direct); err != nil || direct != 0 {
		t.Fatalf("mhai_worker_app holds %d table grants (%v)", direct, err)
	}
	// Core's own login check refuses it: it is not core's login.
	if err := db.CheckLoginRole(ctx, intel); err == nil {
		t.Fatal("core's login check accepted the intelligence login")
	}
}

// Spec §8.2: mhai_worker keeps only what the pipeline uses.
func TestWorkerGrantsNarrowed(t *testing.T) {
	_, super := shared(t)
	ctx := context.Background()
	a, qa := seedHospital(t, super, uniqueCode("W"))
	_, qb := seedHospital(t, super, uniqueCode("X"))

	deny := func(what, q string, args ...any) {
		t.Helper()
		_, err := asWorker(ctx, super, a, q, args...)
		requireDenied(t, err, "worker "+what)
	}
	allow := func(what string, want int64, q string, args ...any) {
		t.Helper()
		n, err := asWorker(ctx, super, a, q, args...)
		if err != nil || n != want {
			t.Fatalf("worker %s: rows %d (want %d), err %v", what, n, want, err)
		}
	}

	// clinical_queries: SELECT, and UPDATE of the status columns only.
	allow("select queries", 1, `SELECT * FROM clinical_queries WHERE id = $1`, qa)
	allow("update status columns", 1, `UPDATE clinical_queries SET status = 'running', progress = 0.5, current_stage = 'retrieval',
		started_at = now(), attempts = attempts + 1, lease_until = now() + interval '1 minute' WHERE id = $1`, qa)
	allow("finish", 1, `UPDATE clinical_queries SET status = 'failed', error = 'internal', completed_at = now() WHERE id = $1`, qa)
	allow("update another hospital's row", 0, `UPDATE clinical_queries SET status = 'running' WHERE id = $1`, qb)
	for _, col := range []string{"clinical_question = 'x'", "patient_data_json = '{}'", "urgency = 'urgent'", "category = 'x'",
		"created_by = NULL", "request_id = 'r'", "published_at = now()", "deadline_at = now()", "cancel_reason = 'x'",
		"hospital_id = hospital_id", "created_at = now()", "id = id"} {
		deny("update "+col, `UPDATE clinical_queries SET `+col+` WHERE id = $1`, qa)
	}
	deny("insert query", `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency) VALUES ($1, $2, 'x', 'routine')`, uuid.New(), a)
	deny("delete query", `DELETE FROM clinical_queries WHERE id = $1`, qa)

	// The status moves (worker_status, RESTRICTIVE): rows in flight only,
	// to running, completed (with a decision) or failed.
	freshQuery := func(set string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := super.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency) VALUES ($1, $2, 'synthetic question', 'routine')`, id, a); err != nil {
			t.Fatal(err)
		}
		if set != "" {
			if _, err := super.Exec(ctx, `UPDATE clinical_queries SET `+set+` WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	for _, final := range []string{"cancelled", "failed", "completed"} {
		q := freshQuery(`status = '` + final + `'`)
		for _, to := range []string{"running", "failed", "completed", "queued"} {
			allow("move a "+final+" row to "+to, 0, `UPDATE clinical_queries SET status = '`+to+`' WHERE id = $1`, q)
		}
		allow("touch a "+final+" row's progress", 0, `UPDATE clinical_queries SET progress = 0.9 WHERE id = $1`, q)
	}
	refusedByPolicy := func(what, q string, args ...any) {
		t.Helper()
		_, err := asWorker(ctx, super, a, q, args...)
		if pgCode(err) != rlsViolation || !strings.Contains(err.Error(), "row-level security") {
			t.Fatalf("worker %s: want an RLS WITH CHECK refusal, got %v", what, err)
		}
	}
	queued := freshQuery("")
	refusedByPolicy("complete without a decision", `UPDATE clinical_queries SET status = 'completed' WHERE id = $1`, queued)
	refusedByPolicy("cancel", `UPDATE clinical_queries SET status = 'cancelled' WHERE id = $1`, queued)
	refusedByPolicy("re-queue a running row", `UPDATE clinical_queries SET status = 'queued' WHERE id = $1`,
		freshQuery(`status = 'running'`))
	tried := freshQuery(`attempts = 3`)
	if _, err := asWorker(ctx, super, a, `UPDATE clinical_queries SET attempts = 1 WHERE id = $1`, tried); pgCode(err) != "23514" {
		t.Fatalf("worker decreasing attempts: want check violation, got %v", err)
	}
	// Raising attempts is part of a claim, which moves the row to running;
	// a row can't be touched and left queued.
	allow("claim, raising attempts", 1, `UPDATE clinical_queries SET status = 'running', attempts = attempts + 1 WHERE id = $1`, tried)
	refusedByPolicy("leave a row queued", `UPDATE clinical_queries SET attempts = attempts + 1 WHERE id = $1`, tried)
	// The allowed paths: queued -> running -> completed (decision first),
	// and running -> failed, each in the worker's own transaction.
	steps := func(what string, q uuid.UUID, stmts ...string) {
		t.Helper()
		err := func() error {
			tx, err := super.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_worker`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, a.String()); err != nil {
				return err
			}
			for _, st := range stmts {
				tag, err := tx.Exec(ctx, st, q)
				if err != nil {
					return fmt.Errorf("%s: %w", st, err)
				}
				if tag.RowsAffected() != 1 {
					return fmt.Errorf("%s: %d rows", st, tag.RowsAffected())
				}
			}
			return tx.Commit(ctx)
		}()
		if err != nil {
			t.Fatalf("worker %s: %v", what, err)
		}
	}
	done := freshQuery("")
	steps("queued -> running -> completed", done,
		`UPDATE clinical_queries SET status = 'running', attempts = attempts + 1, started_at = now(), lease_until = now() + interval '1 minute' WHERE id = $1`,
		`UPDATE clinical_queries SET progress = 0.5, current_stage = 'retrieval' WHERE id = $1`,
		`INSERT INTO clinical_decisions (id, hospital_id, query_id) SELECT gen_random_uuid(), hospital_id, id FROM clinical_queries WHERE id = $1`,
		`UPDATE clinical_queries SET status = 'completed', progress = 1, completed_at = now() WHERE id = $1`)
	steps("running -> failed", freshQuery(`status = 'running', attempts = 1`),
		`UPDATE clinical_queries SET status = 'failed', error = 'internal', completed_at = now() WHERE id = $1`)
	// A running row whose lease ran out is re-claimed while running.
	steps("re-claim a running row", freshQuery(`status = 'running', attempts = 1, lease_until = now() - interval '1 minute'`),
		`UPDATE clinical_queries SET attempts = attempts + 1, lease_until = now() + interval '1 minute' WHERE id = $1`)

	// clinical_decisions and hga_evaluations: SELECT, INSERT.
	dec := uuid.New()
	allow("insert decision", 1, `INSERT INTO clinical_decisions (id, hospital_id, query_id) VALUES ($1, $2, $3)`, dec, a, qa)
	allow("select decisions", 0, `SELECT * FROM clinical_decisions WHERE id = $1`, dec)
	deny("update decision", `UPDATE clinical_decisions SET diagnosis = 'x'`)
	deny("delete decision", `DELETE FROM clinical_decisions`)
	allow("insert evaluation", 1, `INSERT INTO hga_evaluations (hospital_id, query_id, decision) VALUES ($1, $2, 'PASS')`, a, qa)
	allow("select evaluations", 0, `SELECT * FROM hga_evaluations WHERE hospital_id <> $1`, a)
	deny("update evaluation", `UPDATE hga_evaluations SET decision = 'x'`)
	deny("delete evaluation", `DELETE FROM hga_evaluations`)

	// kb_chunks: SELECT only (no enrichment in this plan).
	allow("select kb", 0, `SELECT * FROM kb_chunks WHERE chunk_id = 'none'`)
	deny("insert kb", `INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type) VALUES ($1, $2, 'x', 'guideline')`, uniqueCode("K"), a)
	deny("update kb", `UPDATE kb_chunks SET content = 'x'`)
	deny("delete kb", `DELETE FROM kb_chunks`)
	deny("kb sequence", `SELECT nextval('kb_chunks_id_seq')`)

	// guardrail_weights: SELECT only.
	allow("select weights", 0, `SELECT * FROM guardrail_weights WHERE false`)
	deny("insert weights", `INSERT INTO guardrail_weights (alpha) VALUES (1)`)
	deny("update weights", `UPDATE guardrail_weights SET alpha = 1`)
	deny("delete weights", `DELETE FROM guardrail_weights`)

	// Nothing else.
	for _, tbl := range []string{"users", "sessions", "audit_log", "hospitals", "user_roles", "idempotency_keys",
		"feedback_events", "refresh_tokens", "audit_checkpoints"} {
		deny("select "+tbl, `SELECT 1 FROM `+tbl)
	}
	deny("chain head", `SELECT * FROM audit_chain_head()`)
	deny("expire function", `SELECT * FROM mhai_fail_expired_queries(1)`)
}

// The outbox: the worker appends its own hospital's facts and reads none;
// the owner (the SECURITY DEFINER triggers of 00009, F4) can append too.
func TestOutboxRLS(t *testing.T) {
	_, super := shared(t)
	ctx := context.Background()
	a, qa := seedHospital(t, super, uniqueCode("O"))
	b, qb := seedHospital(t, super, uniqueCode("P"))
	ins := `INSERT INTO intelligence_audit_outbox (hospital_id, query_id, action, entity, entity_id, payload)
		VALUES ($1, $2::uuid, $3, 'clinical_query', $2::uuid::text, '{"attempts": 1}')`

	if n, err := asWorker(ctx, super, a, ins, a, qa, "query.completed"); err != nil || n != 1 {
		t.Fatalf("worker appends its own hospital's fact: %d %v", n, err)
	}
	_, err := asWorker(ctx, super, a, ins, b, qb, "query.completed")
	if pgCode(err) != rlsViolation || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("worker at A appending B's fact: want an RLS refusal, got %v", err)
	}
	// Its own hospital with another's query: the composite foreign key.
	if _, err := asWorker(ctx, super, a, ins, a, qb, "query.completed"); pgCode(err) != "23503" {
		t.Fatalf("A's hospital with B's query: want foreign key violation, got %v", err)
	}
	// Only the three job actions, and a payload object.
	if _, err := asWorker(ctx, super, a, ins, a, qa, "auth.login.succeeded"); pgCode(err) != "23514" {
		t.Fatalf("foreign action: want check violation, got %v", err)
	}
	if _, err := asWorker(ctx, super, a, `INSERT INTO intelligence_audit_outbox (hospital_id, query_id, action, payload)
		VALUES ($1, $2, 'query.failed', '[1]')`, a, qa); pgCode(err) != "23514" {
		t.Fatalf("array payload: want check violation, got %v", err)
	}
	for _, q := range []string{
		`SELECT * FROM intelligence_audit_outbox`,
		`UPDATE intelligence_audit_outbox SET action = 'query.failed'`,
		`DELETE FROM intelligence_audit_outbox`,
		`TRUNCATE intelligence_audit_outbox`,
	} {
		_, err := asWorker(ctx, super, a, q)
		requireDenied(t, err, "worker "+q)
	}
	// Insert without RETURNING needs no SELECT; RETURNING is refused.
	_, err = asWorker(ctx, super, a, ins+` RETURNING id`, a, qa, "query.completed")
	requireDenied(t, err, "worker insert returning")

	// F4: mhai_migrate, the owner, is held by FORCE RLS; it has an append
	// policy so 00009's SECURITY DEFINER triggers can write.
	if n, err := inTx(ctx, super, []string{`SET LOCAL ROLE mhai_migrate`}, nil, ins, b, qb, "query.cancelled"); err != nil || n != 1 {
		t.Fatalf("owner append: %d %v", n, err)
	}

	var owner string
	var rls, force bool
	if err := super.QueryRow(ctx, `SELECT pg_get_userbyid(relowner), relrowsecurity, relforcerowsecurity
		FROM pg_class WHERE relname = 'intelligence_audit_outbox'`).Scan(&owner, &rls, &force); err != nil {
		t.Fatal(err)
	}
	if owner != "mhai_migrate" || !rls || !force {
		t.Fatalf("outbox owner %s rls %v force %v", owner, rls, force)
	}
	// No other app role touches it.
	for _, role := range []string{"mhai_gateway", "mhai_platform", "mhai_auth", "mhai_jobs", "mhai_provision"} {
		_, err := inTx(ctx, super, []string{`SET LOCAL ROLE ` + role}, &a, ins, a, qa, "query.completed")
		requireDenied(t, err, role+" outbox insert")
		_, err = inTx(ctx, super, []string{`SET LOCAL ROLE ` + role}, &a, `SELECT 1 FROM intelligence_audit_outbox`)
		requireDenied(t, err, role+" outbox read")
	}
}

// Plan departure 3: the drain role reads and deletes outbox rows of every
// hospital, reads four id columns of clinical_queries, and appends only the
// three job actions to the chain. Nothing else.
func TestAuditDrainRoleNarrow(t *testing.T) {
	_, super := shared(t)
	ctx := context.Background()
	_, app := loginPool(t, "IN ROLE mhai_app")
	a, qa := seedHospital(t, super, uniqueCode("D"))
	b, qb := seedHospital(t, super, uniqueCode("E"))

	deny := func(what, q string, args ...any) {
		t.Helper()
		_, err := asDrain(ctx, app, q, args...)
		requireDenied(t, err, "drain "+what)
	}
	deny("question", `SELECT clinical_question FROM clinical_queries`)
	deny("patient data", `SELECT patient_data_json FROM clinical_queries`)
	deny("every column", `SELECT * FROM clinical_queries`)
	deny("status", `SELECT status FROM clinical_queries`)
	deny("update query", `UPDATE clinical_queries SET status = 'failed' WHERE id = $1`, qa)
	deny("audit read", `SELECT id FROM audit_log`)
	deny("audit update", `UPDATE audit_log SET action = 'x'`)
	deny("audit delete", `DELETE FROM audit_log`)
	deny("outbox insert", `INSERT INTO intelligence_audit_outbox (hospital_id, query_id, action) VALUES ($1, $2, 'query.completed')`, a, qa)
	deny("outbox update", `UPDATE intelligence_audit_outbox SET action = 'query.failed'`)
	for _, tbl := range []string{"users", "sessions", "hospitals", "clinical_decisions", "hga_evaluations", "kb_chunks",
		"guardrail_weights", "idempotency_keys", "feedback_events"} {
		deny("select "+tbl, `SELECT 1 FROM `+tbl)
	}
	deny("expire function", `SELECT * FROM mhai_fail_expired_queries(1)`)

	_, err := asDrain(ctx, app, `INSERT INTO audit_log (hospital_id, action, prev_hash, hash) VALUES ($1, 'auth.login.succeeded', 'p', $2)`,
		a, "h"+uuid.NewString())
	if pgCode(err) != rlsViolation {
		t.Fatalf("drain appending auth.login.succeeded: want RLS refusal, got %v", err)
	}
	if _, err := asDrain(ctx, app, `INSERT INTO audit_log (hospital_id, action, prev_hash, hash) VALUES (NULL, 'query.completed', 'p', $1)`,
		"h"+uuid.NewString()); pgCode(err) != rlsViolation {
		t.Fatalf("drain appending a fact with no hospital: want RLS refusal, got %v", err)
	}

	// What it is for, across hospitals.
	for _, h := range []struct{ hid, qid uuid.UUID }{{a, qa}, {b, qb}} {
		if _, err := super.Exec(ctx, `INSERT INTO intelligence_audit_outbox (hospital_id, query_id, action) VALUES ($1, $2, 'query.completed')`,
			h.hid, h.qid); err != nil {
			t.Fatal(err)
		}
	}
	err = func() error {
		tx, err := app.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_audit_drain`); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM clinical_queries q WHERE q.id = ANY($1)
			AND q.hospital_id IS NOT NULL AND q.request_id IS NULL AND q.created_by IS NULL`, []uuid.UUID{qa, qb}).Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			t.Errorf("drain sees %d of 2 hospitals' query ids", n)
		}
		tag, err := tx.Exec(ctx, `DELETE FROM intelligence_audit_outbox WHERE query_id = ANY($1)`, []uuid.UUID{qa, qb})
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 2 {
			t.Errorf("drain deleted %d of 2 facts", tag.RowsAffected())
		}
		if _, err := tx.Exec(ctx, `SELECT id, hash FROM audit_chain_head()`); err != nil {
			return err
		}
		for _, action := range []string{"query.completed", "query.failed", "query.cancelled"} {
			if _, err := tx.Exec(ctx, `INSERT INTO audit_log (hospital_id, action, prev_hash, hash) VALUES ($1, $2, 'p', $3)`,
				a, action, "h"+uuid.NewString()); err != nil {
				return err
			}
		}
		return nil
	}()
	if err != nil {
		t.Fatalf("drain work: %v", err)
	}

	// The core login reaches it only by SET ROLE: mhai_app holds it WITH
	// INHERIT FALSE, and core's login check still accepts the core login.
	var inherit, set bool
	if err := super.QueryRow(ctx, `SELECT m.inherit_option, m.set_option FROM pg_auth_members m
		JOIN pg_roles r ON r.oid = m.roleid JOIN pg_roles u ON u.oid = m.member
		WHERE r.rolname = 'mhai_audit_drain' AND u.rolname = 'mhai_app'`).Scan(&inherit, &set); err != nil {
		t.Fatal(err)
	}
	if inherit || !set {
		t.Fatalf("mhai_app -> mhai_audit_drain: inherit %v set %v", inherit, set)
	}
	if err := db.CheckLoginRole(ctx, app); err != nil {
		t.Fatalf("core login after 00008: %v", err)
	}
	// The intelligence login can't reach it.
	_, intel := intelligenceLogin(t)
	_, err = asDrain(ctx, intel, `SELECT 1`)
	requireDenied(t, err, "intelligence login SET ROLE mhai_audit_drain")
}

// F2: the outbox's shape lets two drainers run at once and each fact go to
// exactly one: DELETE ... RETURNING over the lowest ids, in id order. The
// second drainer waits on the first's row locks and then finds the rows
// gone, so it never returns them.
func TestOutboxTwoDrainersTakeEachFactOnce(t *testing.T) {
	_, super := shared(t)
	ctx := context.Background()
	_, app := loginPool(t, "IN ROLE mhai_app")
	a, qa := seedHospital(t, super, uniqueCode("T"))
	// A clean outbox, so LIMIT picks only this test's rows.
	if _, err := super.Exec(ctx, `DELETE FROM intelligence_audit_outbox`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := super.Exec(ctx, `INSERT INTO intelligence_audit_outbox (hospital_id, query_id, action) VALUES ($1, $2, 'query.completed')`,
			a, qa); err != nil {
			t.Fatal(err)
		}
	}
	const drain = `DELETE FROM intelligence_audit_outbox WHERE id IN
		(SELECT id FROM intelligence_audit_outbox ORDER BY id LIMIT 4) RETURNING id`
	take := func(tx pgx.Tx) ([]int64, error) {
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_audit_drain`); err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, drain)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowTo[int64])
	}
	tx1, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	first, err := take(tx1)
	if err != nil || len(first) != 4 {
		t.Fatalf("first drainer: %v %v", first, err)
	}
	var (
		wg     sync.WaitGroup
		second []int64
		err2   error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		tx2, err := app.Begin(ctx)
		if err != nil {
			err2 = err
			return
		}
		defer func() { _ = tx2.Rollback(ctx) }()
		if second, err2 = take(tx2); err2 == nil {
			err2 = tx2.Commit(ctx)
		}
	}()
	time.Sleep(300 * time.Millisecond) // let the second block on the row locks
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if err2 != nil {
		t.Fatalf("second drainer: %v", err2)
	}
	seen := map[int64]bool{}
	for _, id := range first {
		seen[id] = true
	}
	for _, id := range second {
		if seen[id] {
			t.Fatalf("fact %d taken by both drainers (first %v, second %v)", id, first, second)
		}
	}
	var left int
	if err := super.QueryRow(ctx, `SELECT count(*) FROM intelligence_audit_outbox`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if len(first)+len(second)+left != 6 {
		t.Fatalf("first %d + second %d + left %d != 6", len(first), len(second), left)
	}
}

// F20: core's login check refuses a login that inherits any of the roles
// added since Plan 2, by name.
func TestCheckLoginRoleKnowsBoundaryRoles(t *testing.T) {
	_, super := shared(t)
	ctx := context.Background()
	for _, strong := range []string{"mhai_jobs", "mhai_audit_drain", "mhai_worker_app"} {
		name, login := loginPool(t, "IN ROLE mhai_app")
		if _, err := super.Exec(ctx, `GRANT `+strong+` TO `+name); err != nil {
			t.Fatal(err)
		}
		err := db.CheckLoginRole(ctx, login)
		if err == nil || !strings.Contains(err.Error(), "inherits the privileges of "+strong) {
			t.Errorf("login inheriting %s: want refusal naming it, got %v", strong, err)
		}
	}
}

// EnsureLogin makes the pipeline's login a member of mhai_worker_app only,
// and takes any other membership away on a re-run.
func TestEnsureLoginWorkerAppMembership(t *testing.T) {
	_, super := shared(t)
	ctx := context.Background()
	name, _ := intelligenceLogin(t)
	if _, err := super.Exec(ctx, `GRANT mhai_gateway TO `+name+` WITH INHERIT FALSE`); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureLogin(ctx, super, name, "synthetic-intel-password-rerun", "mhai_worker_app"); err != nil {
		t.Fatal(err)
	}
	rows, err := super.Query(ctx, `SELECT g.rolname FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid
		JOIN pg_roles u ON u.oid = m.member WHERE u.rolname = $1`, name)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(got) != 1 || got[0] != "mhai_worker_app" {
		t.Fatalf("memberships %v (%v), want [mhai_worker_app]", got, err)
	}
	// Still not a door to mhai_worker itself, nor to the other wrappers.
	for _, role := range []string{"mhai_worker", "mhai_migrate", "mhai_gateway"} {
		if err := db.EnsureLogin(ctx, super, name, "synthetic-intel-password-rerun", role); err == nil {
			t.Errorf("EnsureLogin as a direct member of %s accepted", role)
		}
	}
}

// Rule 10: 00008 goes down and up again, and its Down leaves 00007's
// function and policies as they were.
func TestMigration00008RoundTrip(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up: %v", err)
	}
	pool, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	state := func() (outbox bool, fn bool, policies int, workerUpdatesQuestion bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT to_regclass('public.intelligence_audit_outbox') IS NOT NULL,
			to_regprocedure('mhai_fail_expired_queries(integer)') IS NOT NULL,
			(SELECT count(*) FROM pg_policies WHERE policyname IN ('jobs_read', 'jobs_update', 'expire_read', 'expire_update', 'jobs_append')),
			has_column_privilege('mhai_worker', 'clinical_queries', 'clinical_question', 'UPDATE')`).
			Scan(&outbox, &fn, &policies, &workerUpdatesQuestion); err != nil {
			t.Fatal(err)
		}
		return
	}
	if o, f, p, u := state(); !o || !f || p != 5 || u {
		t.Fatalf("after up: outbox %v fn %v 00007 policies %d worker updates question %v", o, f, p, u)
	}
	if g := gatewayWrites(t, pool); g != "f f f t" {
		t.Fatalf("after up: gateway writes %q", g)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 7); err != nil {
		t.Fatalf("down to 7: %v", err)
	}
	if o, f, p, u := state(); o || !f || p != 5 || !u {
		t.Fatalf("after down: outbox %v fn %v 00007 policies %d worker updates question %v (00003's grant back)", o, f, p, u)
	}
	if g := gatewayWrites(t, pool); g != "t t t t" {
		t.Fatalf("after down: gateway writes %q (00003's grants back)", g)
	}
	var drainPolicies int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_policies WHERE 'mhai_audit_drain' = ANY(roles::text[])`).Scan(&drainPolicies); err != nil || drainPolicies != 0 {
		t.Fatalf("drain policies left after down: %d %v", drainPolicies, err)
	}
	// Final review M-6: 00007's Down takes mhai_app's membership of
	// mhai_jobs back, as 00008's does for mhai_audit_drain.
	if err := db.MigrateDownTo(ctx, pg.URL, 6); err != nil {
		t.Fatalf("down to 6: %v", err)
	}
	var jobsMember bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.roleid
		JOIN pg_roles u ON u.oid = m.member WHERE r.rolname = 'mhai_jobs' AND u.rolname = 'mhai_app')`).Scan(&jobsMember); err != nil || jobsMember {
		t.Fatalf("mhai_app still a member of mhai_jobs after down to 6: %v %v", jobsMember, err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if o, f, p, u := state(); !o || !f || p != 5 || u {
		t.Fatalf("after up again: outbox %v fn %v 00007 policies %d worker updates question %v", o, f, p, u)
	}
	if g := gatewayWrites(t, pool); g != "f f f t" {
		t.Fatalf("after up again: gateway writes %q", g)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
}

// gatewayWrites is mhai_gateway's UPDATE of clinical_queries.status,
// INSERT on clinical_decisions and hga_evaluations, and UPDATE of
// clinical_queries.published_at (final review I-4: only the last, after 00008).
func gatewayWrites(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT concat_ws(' ',
		has_column_privilege('mhai_gateway', 'clinical_queries', 'status', 'UPDATE'),
		has_table_privilege('mhai_gateway', 'clinical_decisions', 'INSERT'),
		has_table_privilege('mhai_gateway', 'hga_evaluations', 'INSERT'),
		has_column_privilege('mhai_gateway', 'clinical_queries', 'published_at', 'UPDATE'))`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// A login with a session default (ALTER ROLE ... SET role, or a hospital)
// starts every connection outside db.With*: core's check refuses it.
func TestCheckLoginRoleRefusesRoleDefaults(t *testing.T) {
	pg, super := shared(t)
	ctx := context.Background()
	name, _ := loginPool(t, "IN ROLE mhai_app")
	if _, err := super.Exec(ctx, `ALTER ROLE `+name+` SET role = 'mhai_gateway'`); err != nil {
		t.Fatal(err)
	}
	p := connectAs(t, pg, name, "test-only-login-password")
	err := db.CheckLoginRole(ctx, p)
	p.Close()
	if err == nil || !strings.Contains(err.Error(), "starts as mhai_gateway") {
		t.Fatalf("login with a default role: %v", err)
	}
	if _, err := super.Exec(ctx, `ALTER ROLE `+name+` RESET role`); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `ALTER ROLE `+name+` SET mhai.hospital_id = '`+uuid.NewString()+`'`); err != nil {
		t.Fatal(err)
	}
	p = connectAs(t, pg, name, "test-only-login-password")
	err = db.CheckLoginRole(ctx, p)
	p.Close()
	if err == nil || !strings.Contains(err.Error(), "starts with a hospital set") {
		t.Fatalf("login with a default hospital: %v", err)
	}
}

// EnsureLogin clears a login's session defaults, role-wide and for this
// database, so a re-run of core migrate up cleans a tampered login.
func TestEnsureLoginResetsRoleDefaults(t *testing.T) {
	pg, super := shared(t)
	ctx := context.Background()
	for _, role := range []string{"mhai_app", "mhai_worker_app"} {
		name := "dflt_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
		pw := "synthetic-login-password-" + uuid.NewString()
		if err := db.EnsureLogin(ctx, super, name, pw, role); err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{
			`ALTER ROLE ` + name + ` SET role = 'mhai_worker'`,
			`ALTER ROLE ` + name + ` IN DATABASE mhai SET mhai.hospital_id = '` + uuid.NewString() + `'`,
		} {
			if _, err := super.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.EnsureLogin(ctx, super, name, pw, role); err != nil {
			t.Fatalf("%s: re-run: %v", role, err)
		}
		var left int
		if err := super.QueryRow(ctx, `SELECT count(*) FROM pg_db_role_setting s JOIN pg_roles r ON r.oid = s.setrole
			WHERE r.rolname = $1`, name).Scan(&left); err != nil || left != 0 {
			t.Fatalf("%s: %d session defaults left (%v)", role, left, err)
		}
		p := connectAs(t, pg, name, pw)
		var who, hid string
		err := p.QueryRow(ctx, `SELECT current_user, COALESCE(current_setting('mhai.hospital_id', true), '')`).Scan(&who, &hid)
		if err == nil && role == "mhai_app" {
			err = db.CheckLoginRole(ctx, p)
		}
		p.Close()
		if err != nil || who != name || hid != "" {
			t.Fatalf("%s: connects as %q hospital %q (%v)", role, who, hid, err)
		}
	}
}

// Final review I-4: mhai_gateway on the pipeline tables is what core's
// request path writes: clinical_queries INSERT (the submit) and UPDATE of
// published_at only (the publish); clinical_decisions and hga_evaluations
// read only. The worker writes results, never core's request role.
func TestGatewayPipelineGrantsNarrowed(t *testing.T) {
	_, super := shared(t)
	ctx := context.Background()
	a, qa := seedHospital(t, super, uniqueCode("G"))
	asGateway := func(q string, args ...any) (int64, error) {
		return inTx(ctx, super, []string{`SET LOCAL ROLE mhai_gateway`}, &a, q, args...)
	}
	allow := func(what string, want int64, q string, args ...any) {
		t.Helper()
		n, err := asGateway(q, args...)
		if err != nil || n != want {
			t.Fatalf("gateway %s: rows %d (want %d), err %v", what, n, want, err)
		}
	}
	deny := func(what, q string, args ...any) {
		t.Helper()
		_, err := asGateway(q, args...)
		requireDenied(t, err, "gateway "+what)
	}
	allow("select queries", 1, `SELECT * FROM clinical_queries WHERE id = $1`, qa)
	allow("submit", 1, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, status, created_by, request_id, deadline_at)
		VALUES ($1, $2, 'synthetic question', 'routine', 'queued', NULL, 'rid', now())`, uuid.New(), a)
	allow("publish", 1, `UPDATE clinical_queries SET published_at = now() WHERE id = $1`, qa)
	for _, col := range []string{"status = 'completed'", "error = 'internal'", "attempts = 9", "lease_until = now()",
		"progress = 1", "current_stage = 'x'", "completed_at = now()", "cancel_reason = 'x'", "clinical_question = 'x'",
		"patient_data_json = '{}'", "deadline_at = now()", "request_id = 'r'", "retried_kid = 'k'", "hospital_id = hospital_id"} {
		deny("update "+col, `UPDATE clinical_queries SET `+col+` WHERE id = $1`, qa)
	}
	deny("delete query", `DELETE FROM clinical_queries WHERE id = $1`, qa)
	for _, tbl := range []string{"clinical_decisions", "hga_evaluations"} {
		allow("select "+tbl, 0, `SELECT 1 FROM `+tbl+` WHERE query_id = $1`, qa)
		deny("update "+tbl, `UPDATE `+tbl+` SET query_id = query_id WHERE query_id = $1`, qa)
		deny("delete "+tbl, `DELETE FROM `+tbl+` WHERE query_id = $1`, qa)
	}
	deny("insert decision", `INSERT INTO clinical_decisions (id, hospital_id, query_id, response_text) VALUES ($1, $2, $3, 'synthetic')`,
		uuid.New(), a, qa)
	deny("insert evaluation", `INSERT INTO hga_evaluations (hospital_id, query_id, target_agent) VALUES ($1, $2, 'CCA')`, a, qa)
}
