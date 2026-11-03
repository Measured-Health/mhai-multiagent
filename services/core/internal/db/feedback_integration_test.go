//go:build integration

package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00011: the two-person rule in the database (spec §4.3, §4.9).
// Every id and string here is synthetic.

const checkViolation = "23514"

func seedClinician(t *testing.T, pool *pgxpool.Pool, hospital uuid.UUID) uuid.UUID {
	t.Helper()
	u := seedUser(t, pool, &hospital)
	if _, err := pool.Exec(context.Background(), `INSERT INTO user_roles (user_id, role) VALUES ($1, 'clinician')`, u); err != nil {
		t.Fatalf("seed clinician role: %v", err)
	}
	return u
}

func seedQueryIn(t *testing.T, pool *pgxpool.Pool, hospital uuid.UUID) uuid.UUID {
	t.Helper()
	q := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency) VALUES ($1, $2, 'synthetic question', 'routine')`, q, hospital); err != nil {
		t.Fatalf("seed query: %v", err)
	}
	return q
}

// seedCorrection inserts a waiting correction as the owner connection.
func seedCorrection(t *testing.T, pool *pgxpool.Pool, hospital, query, submitter uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO feedback_events (id, hospital_id, query_id, submitted_by, action, status, reason, corrected_diagnosis)
		 VALUES ($1, $2, $3, $4, 'correct', 'pending_review', 'synthetic reason', 'synthetic diagnosis')`,
		id, hospital, query, submitter); err != nil {
		t.Fatalf("seed correction: %v", err)
	}
	return id
}

// asUser runs q as the gateway for hospital h with mhai.user_id = u.
func asUser(ctx context.Context, pool *pgxpool.Pool, h, u uuid.UUID, q string, args ...any) error {
	return db.WithHospitalUser(ctx, pool, h, u, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, q, args...)
		return err
	})
}

const approveSQL = `UPDATE feedback_events SET status = 'approved', decided_by = $2, decided_at = now() WHERE id = $1`

// C2: the submitter cannot decide their own correction, by the gateway or
// by the owner connection.
func TestFeedbackSelfDecisionFailsCheck(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("F"))
	sub := seedClinician(t, pool, h)
	id := seedCorrection(t, pool, h, seedQueryIn(t, pool, h), sub)

	// The setting names the submitter, so the transition trigger (decider =
	// session user) passes and only the CHECK can refuse.
	err := asUser(ctx, pool, h, sub, `UPDATE feedback_events SET status = 'approved', decided_by = submitted_by, decided_at = now() WHERE id = $1`, id)
	if c := pgConstraint(err); pgCode(err) != checkViolation || c != "feedback_not_self_decided" {
		t.Fatalf("gateway self-approval: want 23514 feedback_not_self_decided, got %q %v", c, err)
	}
	// The owner connection (no RLS), with mhai.user_id set to the submitter
	// too: again only the CHECK stands in the way.
	for _, st := range []string{"approved", "rejected"} {
		_, err = inTx(ctx, pool, []string{`SELECT set_config('mhai.user_id', '` + sub.String() + `', true)`}, nil,
			`UPDATE feedback_events SET status = $2, decided_by = submitted_by, decided_at = now(),
			rejection_code = CASE WHEN $2 = 'rejected' THEN 'unsafe' END WHERE id = $1`, id, st)
		if c := pgConstraint(err); pgCode(err) != checkViolation || c != "feedback_not_self_decided" {
			t.Fatalf("owner self-%s: want 23514 feedback_not_self_decided, got %q %v", st, c, err)
		}
	}
	// As mhai_migrate itself: refused (FORCE RLS shows it no row, so at worst nothing changes).
	if _, err := inTx(ctx, pool, []string{`SET LOCAL ROLE mhai_migrate`}, nil,
		`UPDATE feedback_events SET status = 'approved', decided_by = submitted_by, decided_at = now() WHERE id = $1`, id); err != nil && pgCode(err) != checkViolation {
		t.Fatalf("mhai_migrate self-approval: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM feedback_events WHERE id = $1`, id).Scan(&status); err != nil || status != "pending_review" {
		t.Fatalf("status = %q, %v; want pending_review", status, err)
	}
}

// C6, database part: the decider is an active clinician of the same hospital.
func TestFeedbackDeciderMustBeActiveClinicianSameHospital(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("F"))
	hb, _ := seedHospital(t, pool, uniqueCode("G"))
	sub := seedClinician(t, pool, h)

	admin := seedUser(t, pool, &h)
	if _, err := pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'hospital_admin')`, admin); err != nil {
		t.Fatal(err)
	}
	inactive := seedClinician(t, pool, h)
	if _, err := pool.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1`, inactive); err != nil {
		t.Fatal(err)
	}
	otherHospital := seedClinician(t, pool, hb)
	platform := seedUser(t, pool, nil)
	if _, err := pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'platform_admin')`, platform); err != nil {
		t.Fatal(err)
	}
	good := seedClinician(t, pool, h)

	for name, decider := range map[string]uuid.UUID{
		"hospital admin without clinician": admin,
		"deactivated clinician":            inactive,
		"clinician of another hospital":    otherHospital,
		"platform account":                 platform,
	} {
		id := seedCorrection(t, pool, h, seedQueryIn(t, pool, h), sub)
		// The setting names the decider, so only the decider check can refuse.
		if err := asUser(ctx, pool, h, decider, approveSQL, id, decider); err == nil {
			t.Errorf("%s decided a correction", name)
		} else if pgCode(err) != checkViolation {
			t.Errorf("%s: want 23514, got %v", name, err)
		}
	}
	// The same check on insert: a row born approved.
	if _, err := pool.Exec(ctx,
		`INSERT INTO feedback_events (id, hospital_id, query_id, submitted_by, action, status, reason, corrected_diagnosis, decided_by, decided_at)
		 VALUES (gen_random_uuid(), $1, $2, $3, 'correct', 'approved', 'synthetic reason', 'synthetic diagnosis', $4, now())`,
		h, seedQueryIn(t, pool, h), sub, admin); err == nil {
		t.Error("a row inserted approved by a non-clinician")
	}

	id := seedCorrection(t, pool, h, seedQueryIn(t, pool, h), sub)
	if err := asUser(ctx, pool, h, good, approveSQL, id, good); err != nil {
		t.Fatalf("an active clinician of the hospital approving: %v", err)
	}
}

// Status moves only pending_review -> approved | rejected | withdrawn, and a
// recorded or decided row never changes again.
func TestFeedbackTransitionsAreOneWay(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("F"))
	sub := seedClinician(t, pool, h)
	dec := seedClinician(t, pool, h)

	approved := seedCorrection(t, pool, h, seedQueryIn(t, pool, h), sub)
	if err := asUser(ctx, pool, h, dec, approveSQL, approved, dec); err != nil {
		t.Fatal(err)
	}
	rejected := seedCorrection(t, pool, h, seedQueryIn(t, pool, h), sub)
	if err := asUser(ctx, pool, h, dec, `UPDATE feedback_events SET status = 'rejected', decided_by = $2, decided_at = now(), rejection_code = 'unsafe', rejection_note = 'synthetic note' WHERE id = $1`, rejected, dec); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		what, sql string
		id        uuid.UUID
		by        uuid.UUID
	}{
		{"approved -> rejected", `UPDATE feedback_events SET status = 'rejected', rejection_code = 'unsafe' WHERE id = $1`, approved, dec},
		{"approved -> pending_review", `UPDATE feedback_events SET status = 'pending_review' WHERE id = $1`, approved, dec},
		{"approved -> withdrawn", `UPDATE feedback_events SET status = 'withdrawn', decided_by = NULL WHERE id = $1`, approved, sub},
		{"rejected -> pending_review", `UPDATE feedback_events SET status = 'pending_review', decided_by = NULL, decided_at = NULL, rejection_code = NULL, rejection_note = NULL WHERE id = $1`, rejected, dec},
		{"rejected -> approved", `UPDATE feedback_events SET status = 'approved', rejection_code = NULL, rejection_note = NULL WHERE id = $1`, rejected, dec},
		{"decided row, note rewritten", `UPDATE feedback_events SET rejection_note = 'other' WHERE id = $1`, rejected, dec},
	} {
		if err := asUser(ctx, pool, h, c.by, c.sql, c.id); err == nil {
			t.Errorf("%s was allowed", c.what)
		} else if pgCode(err) != checkViolation {
			t.Errorf("%s: want 23514, got %v", c.what, err)
		}
	}

	// recorded never changes.
	rec := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO feedback_events (id, hospital_id, query_id, submitted_by, action, status) VALUES ($1, $2, $3, $4, 'validate', 'recorded')`,
		rec, h, seedQueryIn(t, pool, h), sub); err != nil {
		t.Fatalf("validate row: %v", err)
	}
	for _, st := range []string{"approved", "rejected", "withdrawn", "pending_review"} {
		if _, err := pool.Exec(ctx, `UPDATE feedback_events SET status = $2 WHERE id = $1`, rec, st); pgCode(err) != checkViolation {
			t.Errorf("recorded -> %s: want 23514, got %v", st, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE feedback_events SET decided_at = now() WHERE id = $1`, rec); pgCode(err) != checkViolation {
		t.Errorf("recorded row given a decided_at: want 23514, got %v", err)
	}

	// A waiting correction: nothing but the decision columns changes.
	w := seedCorrection(t, pool, h, seedQueryIn(t, pool, h), sub)
	for _, set := range []string{
		`reason = 'a different reason'`, `corrected_diagnosis = 'other'`, `action = 'validate'`,
		`submitted_by = '` + dec.String() + `'`, `query_id = '` + seedQueryIn(t, pool, h).String() + `'`,
	} {
		if _, err := pool.Exec(ctx, `UPDATE feedback_events SET `+set+` WHERE id = $1`, w); pgCode(err) != checkViolation {
			t.Errorf("changing %q after submission: want 23514, got %v", set, err)
		}
	}
	// And a no-op status stay is not a transition.
	if err := asUser(ctx, pool, h, dec, `UPDATE feedback_events SET status = 'pending_review' WHERE id = $1`, w); pgCode(err) != checkViolation {
		t.Errorf("pending_review -> pending_review: want 23514, got %v", err)
	}
	// The decider is checked at the moment of the move: the setting can't
	// stand in for a deactivated account.
	if _, err := pool.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1`, dec); err != nil {
		t.Fatal(err)
	}
	if err := asUser(ctx, pool, h, dec, approveSQL, w, dec); pgCode(err) != checkViolation {
		t.Errorf("a deactivated clinician approving: want 23514, got %v", err)
	}
	// A move to approved by the owner connection with no user set is refused.
	other := seedClinician(t, pool, h)
	if _, err := pool.Exec(ctx, `UPDATE feedback_events SET status = 'approved', decided_by = $2, decided_at = now() WHERE id = $1`, w, other); pgCode(err) != checkViolation {
		t.Errorf("approval with no mhai.user_id: want 23514, got %v", err)
	}
	// The gateway's column grant leaves the text alone.
	err := asUser(ctx, pool, h, sub, `UPDATE feedback_events SET reason = 'x y z' WHERE id = $1`, w)
	requireDenied(t, err, "gateway updating reason")
}

func TestFeedbackWithdrawOnlyBySubmitter(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("F"))
	sub := seedClinician(t, pool, h)
	other := seedClinician(t, pool, h)
	id := seedCorrection(t, pool, h, seedQueryIn(t, pool, h), sub)
	withdraw := `UPDATE feedback_events SET status = 'withdrawn', decided_at = now() WHERE id = $1`

	// mhai.user_id unset.
	err := db.WithHospital(ctx, pool, h, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, withdraw, id)
		return err
	})
	if pgCode(err) != checkViolation {
		t.Fatalf("withdraw with no user: want 23514, got %v", err)
	}
	if err := asUser(ctx, pool, h, other, withdraw, id); pgCode(err) != checkViolation {
		t.Fatalf("withdraw by another user: want 23514, got %v", err)
	}
	// The submitter can't use withdraw to name a decider.
	if err := asUser(ctx, pool, h, sub, `UPDATE feedback_events SET status = 'withdrawn', decided_by = $2, decided_at = now() WHERE id = $1`, id, other); pgCode(err) != checkViolation {
		t.Fatalf("withdraw naming a decider: want 23514, got %v", err)
	}
	if err := asUser(ctx, pool, h, sub, withdraw, id); err != nil {
		t.Fatalf("withdraw by the submitter: %v", err)
	}
}

// C11, database part.
func TestFeedbackOneLivePerQuery(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("F"))
	sub := seedClinician(t, pool, h)
	q := seedQueryIn(t, pool, h)
	first := seedCorrection(t, pool, h, q, sub)

	for _, act := range []string{"correct", "validate"} {
		var err error
		if act == "correct" {
			_, err = pool.Exec(ctx, `INSERT INTO feedback_events (id, hospital_id, query_id, submitted_by, action, status, reason, corrected_diagnosis)
				VALUES (gen_random_uuid(), $1, $2, $3, 'correct', 'pending_review', 'synthetic reason', 'synthetic diagnosis')`, h, q, sub)
		} else {
			_, err = pool.Exec(ctx, `INSERT INTO feedback_events (id, hospital_id, query_id, submitted_by, action, status)
				VALUES (gen_random_uuid(), $1, $2, $3, 'validate', 'recorded')`, h, q, sub)
		}
		if pgCode(err) != "23505" {
			t.Fatalf("second live %s: want 23505, got %v", act, err)
		}
	}
	if err := asUser(ctx, pool, h, sub, `UPDATE feedback_events SET status = 'withdrawn', decided_at = now() WHERE id = $1`, first); err != nil {
		t.Fatal(err)
	}
	seedCorrection(t, pool, h, q, sub) // allowed after a withdrawal
}

func TestFeedbackShapeChecks(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("F"))
	sub := seedClinician(t, pool, h)
	long := func(n int) string { return repeat("a", n) }
	ins := `INSERT INTO feedback_events (id, hospital_id, query_id, submitted_by, action, status, reason, corrected_diagnosis) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7)`
	for _, c := range []struct {
		what              string
		action, status    string
		reason, diagnosis string
	}{
		{"unknown action", "approve", "pending_review", "synthetic reason", "d"},
		{"validate with a correction status", "validate", "pending_review", "", ""},
		{"correct recorded", "correct", "recorded", "synthetic reason", "d"},
		{"reason too short", "correct", "pending_review", "ab", "d"},
		{"reason too long", "correct", "pending_review", long(1001), "d"},
		{"empty diagnosis", "correct", "pending_review", "synthetic reason", ""},
		{"diagnosis too long", "correct", "pending_review", "synthetic reason", long(201)},
		{"born approved with no decider", "correct", "approved", "synthetic reason", "d"},
	} {
		var r, d any = c.reason, c.diagnosis
		if c.reason == "" && c.diagnosis == "" {
			r, d = nil, nil
		} else if c.diagnosis == "" {
			d = ""
		}
		_, err := pool.Exec(ctx, ins, h, seedQueryIn(t, pool, h), sub, c.action, c.status, r, d)
		if pgCode(err) != checkViolation {
			t.Errorf("%s: want 23514, got %v", c.what, err)
		}
	}
	// Boundary values pass.
	for _, n := range [][2]int{{3, 1}, {1000, 200}} {
		if _, err := pool.Exec(ctx, ins, h, seedQueryIn(t, pool, h), sub, "correct", "pending_review", repeat("a", n[0]), repeat("b", n[1])); err != nil {
			t.Errorf("lengths %v: %v", n, err)
		}
	}
	// Rejection code set and note length.
	dec := seedClinician(t, pool, h)
	for _, c := range []struct {
		code, note string
		ok         bool
	}{{"unsafe", "", true}, {"not_supported", repeat("n", 500), true}, {"out_of_scope", "", true}, {"needs_rewording", "", true},
		{"bad", "", false}, {"unsafe", repeat("n", 501), false}} {
		id := seedCorrection(t, pool, h, seedQueryIn(t, pool, h), sub)
		err := asUser(context.Background(), pool, h, dec, `UPDATE feedback_events SET status = 'rejected', decided_by = $2, decided_at = now(), rejection_code = $3, rejection_note = nullif($4, '') WHERE id = $1`, id, dec, c.code, c.note)
		if (err == nil) != c.ok {
			t.Errorf("rejection %q/%d chars: ok=%v err=%v", c.code, len(c.note), c.ok, err)
		}
	}
}

func pgConstraint(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.ConstraintName
	}
	return ""
}

// R4: the gateway inserts feedback only as the signed-in user, and never
// with an assignee: a forged submitter, no user at all, or an assigned_to
// is refused by the restrictive insert policy.
func TestFeedbackInsertOnlyAsSessionUser(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("F"))
	me := seedClinician(t, pool, h)
	other := seedClinician(t, pool, h)
	ins := `INSERT INTO feedback_events (id, hospital_id, query_id, submitted_by, action, status, reason, corrected_diagnosis, assigned_to)
		VALUES (gen_random_uuid(), $1, $2, $3, 'correct', 'pending_review', 'synthetic reason', 'synthetic diagnosis', $4)`

	if err := asUser(ctx, pool, h, me, ins, h, seedQueryIn(t, pool, h), other, nil); pgCode(err) != permissionDenied {
		t.Errorf("forged submitter: want 42501, got %v", err)
	}
	if err := asUser(ctx, pool, h, me, ins, h, seedQueryIn(t, pool, h), me, other); pgCode(err) != permissionDenied {
		t.Errorf("born assigned: want 42501, got %v", err)
	}
	err := db.WithHospital(ctx, pool, h, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, ins, h, seedQueryIn(t, pool, h), me, nil)
		return err
	})
	if pgCode(err) != permissionDenied {
		t.Errorf("no session user: want 42501, got %v", err)
	}
	if err := asUser(ctx, pool, h, me, ins, h, seedQueryIn(t, pool, h), me, nil); err != nil {
		t.Errorf("the session user submitting: %v", err)
	}
}

func repeat(s string, n int) string {
	b := make([]byte, 0, n*len(s))
	for i := 0; i < n; i++ {
		b = append(b, s...)
	}
	return string(b)
}

// The pipeline reads ids and statuses of feedback, never the free text.
func TestWorkerCannotReadFeedbackText(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("F"))
	sub := seedClinician(t, pool, h)
	id := seedCorrection(t, pool, h, seedQueryIn(t, pool, h), sub)

	for _, col := range []string{"reason", "corrected_diagnosis", "rejection_note", "rejection_code", "request_id", "*"} {
		_, err := asWorker(ctx, pool, h, `SELECT `+col+` FROM feedback_events`)
		requireDenied(t, err, "worker selecting "+col)
	}
	n, err := asWorker(ctx, pool, h, `SELECT id, hospital_id, query_id, action, status, submitted_by, decided_by FROM feedback_events WHERE id = $1`, id)
	if err != nil || n != 1 {
		t.Fatalf("worker selecting the granted columns: %d rows, %v", n, err)
	}
	for _, q := range []string{
		`INSERT INTO feedback_events (id, hospital_id, query_id, submitted_by, action, status) VALUES (gen_random_uuid(), $1, $2, $3, 'validate', 'recorded')`,
	} {
		_, err := asWorker(ctx, pool, h, q, h, uuid.New(), sub)
		requireDenied(t, err, "worker inserting feedback")
	}
	_, err = asWorker(ctx, pool, h, `UPDATE feedback_events SET status = 'withdrawn'`)
	requireDenied(t, err, "worker updating feedback")
	_, err = asWorker(ctx, pool, h, `DELETE FROM feedback_events`)
	requireDenied(t, err, "worker deleting feedback")
	// The gateway can't delete either.
	err = asUser(ctx, pool, h, sub, `DELETE FROM feedback_events WHERE id = $1`, id)
	requireDenied(t, err, "gateway deleting feedback")
}

func TestTimingsKeysClosed(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("T"))
	ins := `INSERT INTO clinical_decisions (id, hospital_id, query_id, timings_ms) VALUES (gen_random_uuid(), $1, $2, $3::jsonb)`
	for _, bad := range []string{`{"privacy": 3, "x": 1}`, `{"privacy": -1}`, `{"answer": 1.5}`, `{"answer": "3"}`, `[1]`, `3`} {
		if _, err := pool.Exec(ctx, ins, h, seedQueryIn(t, pool, h), bad); pgCode(err) != checkViolation {
			t.Errorf("timings %s: want 23514, got %v", bad, err)
		}
	}
	for _, good := range []string{`{}`, `{"privacy": 3, "guidelines": 0, "answer": 12, "evidence": 7}`, `{"answer": 1}`} {
		if _, err := pool.Exec(ctx, ins, h, seedQueryIn(t, pool, h), good); err != nil {
			t.Errorf("timings %s: %v", good, err)
		}
	}
	// NULL is fine (pre-Plan-4 decisions); masked columns have bounds.
	q := seedQueryIn(t, pool, h)
	if _, err := pool.Exec(ctx, `INSERT INTO clinical_decisions (id, hospital_id, query_id, masked_question, masked_patient_json) VALUES (gen_random_uuid(), $1, $2, 'masked', '{"age": "[AGE]"}')`, h, q); err != nil {
		t.Errorf("masked columns: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO clinical_decisions (id, hospital_id, query_id, masked_question) VALUES (gen_random_uuid(), $1, $2, $3)`, h, seedQueryIn(t, pool, h), repeat("q", 8001)); pgCode(err) != checkViolation {
		t.Errorf("masked_question over 8000: want 23514, got %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO clinical_decisions (id, hospital_id, query_id, masked_patient_json) VALUES (gen_random_uuid(), $1, $2, '[1]')`, h, seedQueryIn(t, pool, h)); pgCode(err) != checkViolation {
		t.Errorf("masked_patient_json array: want 23514, got %v", err)
	}
	big := `{"k": "` + repeat("p", 33*1024) + `"}`
	if _, err := pool.Exec(ctx, `INSERT INTO clinical_decisions (id, hospital_id, query_id, masked_patient_json) VALUES (gen_random_uuid(), $1, $2, $3::jsonb)`, h, seedQueryIn(t, pool, h), big); pgCode(err) != checkViolation {
		t.Errorf("masked_patient_json over 32 KB: want 23514, got %v", err)
	}
	// The worker may write the new columns on its INSERT.
	q2 := seedQueryIn(t, pool, h)
	if n, err := asWorker(ctx, pool, h, `INSERT INTO clinical_decisions (id, hospital_id, query_id, masked_question, masked_patient_json, timings_ms) VALUES (gen_random_uuid(), $1, $2, 'm', '{}', '{"answer": 2}')`, h, q2); err != nil || n != 1 {
		t.Errorf("worker inserting the new columns: %d, %v", n, err)
	}
}

// Down puts back 00002's shape and 00003's grants; Up applies again.
func TestMigration00011RoundTrip(t *testing.T) {
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
	state := func() (cols string, fns int, gatewayUpdatesReason, workerReadsReason bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT
			(SELECT string_agg(column_name, ',' ORDER BY column_name) FROM information_schema.columns
			  WHERE table_name = 'feedback_events' AND column_name IN ('clinician_id', 'submitted_by', 'decision_id', 'processed', 'hga_weight_suggestions_json', 'processed_at', 'query_id', 'status')),
			(SELECT count(*) FROM pg_proc WHERE proname IN ('mhai_current_user', 'mhai_feedback_decider_check', 'mhai_feedback_transition_check')),
			has_column_privilege('mhai_gateway', 'feedback_events', 'reason', 'UPDATE'),
			has_column_privilege('mhai_worker', 'feedback_events', 'reason', 'SELECT')`).
			Scan(&cols, &fns, &gatewayUpdatesReason, &workerReadsReason); err != nil {
			t.Fatal(err)
		}
		return
	}
	if c, f, g, w := state(); c != "query_id,status,submitted_by" || f != 3 || g || w {
		t.Fatalf("after up: columns %q functions %d gateway updates reason %v worker reads reason %v", c, f, g, w)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 10); err != nil {
		t.Fatalf("down to 10: %v", err)
	}
	if c, f, g, w := state(); c != "clinician_id,decision_id,hga_weight_suggestions_json,processed,processed_at" || f != 0 || !g || w {
		t.Fatalf("after down: columns %q functions %d gateway updates reason %v (00003's grant back) worker reads reason %v", c, f, g, w)
	}
	var masked int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'clinical_decisions' AND column_name IN ('masked_question', 'masked_patient_json', 'timings_ms')`).Scan(&masked); err != nil || masked != 0 {
		t.Fatalf("clinical_decisions columns left after down: %d %v", masked, err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if c, f, g, w := state(); c != "query_id,status,submitted_by" || f != 3 || g || w {
		t.Fatalf("after up again: columns %q functions %d gateway updates reason %v worker reads reason %v", c, f, g, w)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
}
