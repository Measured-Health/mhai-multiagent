//go:build integration

package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration 00013: the learning buffer takes approved two-person corrections
// only (spec §4.8, §4.9). Every id and string here is synthetic.

const bufferInsertSQL = `INSERT INTO feedback_buffer (hospital_id, correction_id, nli_score, system_decision)
	VALUES ($1, $2, 0.5, 'PASS')`

// seedApproved is a correction approved by a second clinician.
func seedApproved(t *testing.T, pool *pgxpool.Pool, h uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	sub, dec := seedClinician(t, pool, h), seedClinician(t, pool, h)
	id := seedCorrection(t, pool, h, seedQueryIn(t, pool, h), sub)
	if err := asUser(ctx, pool, h, dec, approveSQL, id, dec); err != nil {
		t.Fatalf("approve: %v", err)
	}
	return id
}

func bufferRows(t *testing.T, pool *pgxpool.Pool, correction uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM feedback_buffer WHERE correction_id = $1`, correction).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// C8: pending, rejected, withdrawn, and a self-approved row all stay out.
func TestBufferRefusesUnapproved(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("A"))
	sub, dec := seedClinician(t, pool, h), seedClinician(t, pool, h)

	pending := seedCorrection(t, pool, h, seedQueryIn(t, pool, h), sub)
	rejected := seedCorrection(t, pool, h, seedQueryIn(t, pool, h), sub)
	if err := asUser(ctx, pool, h, dec, `UPDATE feedback_events SET status = 'rejected', decided_by = $2, decided_at = now(), rejection_code = 'unsafe' WHERE id = $1`, rejected, dec); err != nil {
		t.Fatal(err)
	}
	withdrawn := seedCorrection(t, pool, h, seedQueryIn(t, pool, h), sub)
	if err := asUser(ctx, pool, h, sub, `UPDATE feedback_events SET status = 'withdrawn' WHERE id = $1`, withdrawn); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]uuid.UUID{"pending_review": pending, "rejected": rejected, "withdrawn": withdrawn} {
		_, err := asWorker(ctx, pool, h, bufferInsertSQL, h, id)
		if pgCode(err) != rlsViolation || !strings.Contains(err.Error(), "buffer_needs_approval") {
			t.Errorf("%s correction in the buffer: want buffer_needs_approval refusal, got %v", name, err)
		}
		if bufferRows(t, pool, id) != 0 {
			t.Errorf("%s correction has a buffer row", name)
		}
	}

	// Forced self-approved. Inside one superuser transaction the CHECK and
	// the feedback triggers are lifted, the row is made self-approved, and
	// the insert is tried as mhai_worker. Only the policy is left to refuse.
	// The transaction always rolls back, so nothing stays dropped or off.
	forced := seedCorrection(t, pool, h, seedQueryIn(t, pool, h), sub)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, s := range []string{
		`ALTER TABLE feedback_events DROP CONSTRAINT feedback_not_self_decided`,
		`ALTER TABLE feedback_events DROP CONSTRAINT feedback_decided_shape`,
		`ALTER TABLE feedback_events DISABLE TRIGGER feedback_decider_check`,
		`ALTER TABLE feedback_events DISABLE TRIGGER feedback_transition_check`,
	} {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE feedback_events SET status = 'approved', decided_by = submitted_by, decided_at = now() WHERE id = $1`, forced); err != nil {
		t.Fatalf("force self-approval: %v", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_worker`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, h.String()); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, bufferInsertSQL, h, forced)
	if pgCode(err) != rlsViolation || !strings.Contains(err.Error(), "buffer_needs_approval") {
		t.Fatalf("self-approved correction in the buffer: want buffer_needs_approval refusal, got %v", err)
	}
	_ = tx.Rollback(ctx)

	// Nothing was left dropped or disabled.
	var n int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_constraint WHERE conname IN ('feedback_not_self_decided', 'feedback_decided_shape'))
		+ (SELECT count(*) FROM pg_trigger WHERE tgrelid = 'feedback_events'::regclass AND tgenabled <> 'O'
		   AND tgname IN ('feedback_decider_check', 'feedback_transition_check'))`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("feedback_events guards after rollback: %d present (want 2 constraints, no disabled triggers), %v", n, err)
	}
}

func TestBufferAcceptsApprovedOnce(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("A"))
	id := seedApproved(t, pool, h)

	// Commit for real: the second attempt must meet the first.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_worker`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, h.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, bufferInsertSQL, h, id); err != nil {
		t.Fatalf("approved correction refused: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if bufferRows(t, pool, id) != 1 {
		t.Fatal("no buffer row")
	}
	if _, err := asWorker(ctx, pool, h, bufferInsertSQL, h, id); pgCode(err) != "23505" {
		t.Fatalf("second insert of one correction: want unique violation, got %v", err)
	}
	// Bad scores and decisions are refused.
	id2 := seedApproved(t, pool, h)
	if _, err := asWorker(ctx, pool, h, `INSERT INTO feedback_buffer (hospital_id, correction_id, nli_score, system_decision) VALUES ($1, $2, 1.5, 'PASS')`, h, id2); pgCode(err) != checkViolation {
		t.Fatalf("score out of range: want 23514, got %v", err)
	}
	if _, err := asWorker(ctx, pool, h, `INSERT INTO feedback_buffer (hospital_id, correction_id, system_decision) VALUES ($1, $2, 'MAYBE')`, h, id2); pgCode(err) != checkViolation {
		t.Fatalf("bad decision: want 23514, got %v", err)
	}
	// The worker can't set the clearing columns.
	if _, err := asWorker(ctx, pool, h, `INSERT INTO feedback_buffer (hospital_id, correction_id, system_decision, cleared_at) VALUES ($1, $2, 'PASS', now())`, h, id2); pgCode(err) != permissionDenied {
		t.Fatalf("worker setting cleared_at: want 42501, got %v", err)
	}
}

func TestBufferOtherHospitalInvisible(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("A"))
	b, _ := seedHospital(t, pool, uniqueCode("B"))
	inA := seedApproved(t, pool, a)

	// B's worker can't buffer A's correction, under either hospital id.
	for _, hid := range []uuid.UUID{a, b} {
		if _, err := asWorker(ctx, pool, b, bufferInsertSQL, hid, inA); pgCode(err) != rlsViolation {
			t.Errorf("hospital B worker, row for %v: want RLS refusal, got %v", hid == a, err)
		}
	}
	// A's own worker can't write a row for B's correction either.
	inB := seedApproved(t, pool, b)
	if _, err := asWorker(ctx, pool, a, bufferInsertSQL, a, inB); pgCode(err) != rlsViolation {
		t.Errorf("hospital A worker, B's correction: want RLS refusal, got %v", err)
	}
	// The FK ties the pair even for the owner.
	if _, err := pool.Exec(ctx, bufferInsertSQL, b, inA); pgCode(err) != "23503" {
		t.Errorf("owner row naming A's correction under B: want FK violation, got %v", err)
	}
	// Nobody reads the table but the owner's waiting count: no role is granted SELECT.
	for _, role := range []string{"mhai_worker", "mhai_gateway", "mhai_platform", "mhai_auth", "mhai_jobs", "mhai_audit_drain", "mhai_provision"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT has_any_column_privilege($1, 'feedback_buffer', 'SELECT, UPDATE, REFERENCES')
			OR has_table_privilege($1, 'feedback_buffer', 'DELETE, TRUNCATE')`, role).Scan(&ok); err != nil || ok {
			t.Errorf("%s holds a read or write privilege on feedback_buffer (%v)", role, err)
		}
	}
}

// C10: no Plan 4 role writes the weights or the knowledge base. The gateway's
// 00003 grant on kb_chunks (a hospital's own knowledge base, under
// hospital_rows) predates Plan 4 and is left as it is; the brief's "none"
// for it contradicts the schema, so only the weights are asserted for it.
func TestNoPlan4RoleWritesWeightsOrKB(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for _, role := range []string{"mhai_worker", "mhai_gateway"} {
		for _, table := range []string{"guardrail_weights", "kb_chunks"} {
			if role == "mhai_gateway" && table == "kb_chunks" {
				continue
			}
			for _, priv := range []string{"INSERT", "UPDATE", "DELETE"} {
				var ok bool
				if err := pool.QueryRow(ctx, `SELECT has_table_privilege($1, $2, $3)`, role, table, priv).Scan(&ok); err != nil {
					t.Fatal(err)
				}
				if ok {
					t.Errorf("%s has %s on %s", role, priv, table)
				}
			}
		}
	}
}

func TestBufferWaitingOnlyPlatform(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("A"))
	count := func() int64 {
		var n int64
		if err := pool.QueryRow(ctx, `SELECT mhai_feedback_buffer_waiting()`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	id := seedApproved(t, pool, h)
	if _, err := pool.Exec(ctx, bufferInsertSQL, h, id); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != before+1 {
		t.Fatalf("waiting = %d, want %d", got, before+1)
	}

	// As the platform: the number, and nothing else.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_platform`); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := tx.QueryRow(ctx, `SELECT mhai_feedback_buffer_waiting()`).Scan(&n); err != nil || n != before+1 {
		t.Fatalf("platform count = %d, %v; want %d", n, err, before+1)
	}
	_, err = tx.Exec(ctx, `SELECT count(*) FROM feedback_buffer`)
	_ = tx.Rollback(ctx)
	requireDenied(t, err, "platform reading the table")

	for _, role := range []string{"mhai_gateway", "mhai_worker", "mhai_auth", "mhai_jobs", "mhai_audit_drain", "mhai_provision"} {
		_, err := inTx(ctx, pool, []string{`SET LOCAL ROLE ` + role}, nil, `SELECT mhai_feedback_buffer_waiting()`)
		requireDenied(t, err, role+" calling the count")
	}

	// A cleared row stops counting.
	if _, err := pool.Exec(ctx, `UPDATE feedback_buffer SET cleared_at = now(), reset_id = gen_random_uuid() WHERE correction_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != before {
		t.Fatalf("waiting after clearing = %d, want %d", got, before)
	}
}
