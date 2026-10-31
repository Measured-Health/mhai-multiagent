//go:build integration

package intel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
)

// addFact puts a fact in the outbox as a worker would (superuser here: the
// worker's own path is covered by the boundary tests).
func addFact(t *testing.T, hid, qid uuid.UUID, action string, payload map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	if _, err := super.Exec(context.Background(), `INSERT INTO intelligence_audit_outbox (hospital_id, query_id, action, entity, entity_id, payload)
		VALUES ($1, $2, $3, 'query', $4, $5::jsonb)`, hid, qid, action, qid.String(), string(raw)); err != nil {
		t.Fatal(err)
	}
}

func outboxCount(t *testing.T, qid uuid.UUID) int {
	t.Helper()
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM intelligence_audit_outbox WHERE query_id = $1`, qid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func drainAll(t *testing.T) {
	t.Helper()
	for {
		n, err := DrainOnce(context.Background(), app, nil)
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if n < drainBatch {
			return
		}
	}
}

// A12: a fact that names another actor, hospital, request id or subject is
// recorded with the job's creator, hospital and request id, and those keys
// are gone from the payload. The rest of the payload stays.
func TestDrainerIgnoresFactIdentity(t *testing.T) {
	hid := seedHospital(t, "active")
	other := seedHospital(t, "active")
	creator := seedUser(t, hid, true, authz.RoleClinician)
	intruder := seedUser(t, other, true, authz.RoleClinician)
	deadline := time.Now().Add(5 * time.Minute)
	q := seedQuery(t, hid, creator, 0, &deadline, true)
	setRow(t, q, "status = 'completed'")
	addFact(t, hid, q, "query.completed", map[string]any{
		"query_id": q.String(), "decision": "PASS", "trust_band": "high", "attempts": 1,
		"actor_id": intruder.String(), "hospital_id": other.String(), "request_id": "forged", "sub": intruder.String(),
	})
	drainAll(t)
	rows := auditFor(t, "query.completed", q.String())
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	r := rows[0]
	if r.actor == nil || *r.actor != creator.String() || *r.hospital != hid.String() || *r.requestID != "rid-"+q.String() {
		t.Fatalf("identity from the fact: %+v", r)
	}
	for _, k := range identityKeys {
		if _, ok := r.payload[k]; ok {
			t.Errorf("payload kept %q: %v", k, r.payload)
		}
	}
	if r.payload["decision"] != "PASS" || r.payload["trust_band"] != "high" || r.payload["query_id"] != q.String() {
		t.Fatalf("payload %v", r.payload)
	}
	if outboxCount(t, q) != 0 {
		t.Fatal("fact left in the outbox")
	}
}

// A13 (core side): a fact committed while no core was draining (the worker
// finished, then everything stopped) reaches the chain later as
// query.completed, and the chain verifies. A fact whose payload has a
// sensitive key doesn't hold the outbox up: it is recorded without it.
func TestDrainerAppendsCompletedAfterWorkerCrash(t *testing.T) {
	hid := seedHospital(t, "active")
	creator := seedUser(t, hid, true, authz.RoleClinician)
	deadline := time.Now().Add(5 * time.Minute)
	q := seedQuery(t, hid, creator, 0, &deadline, true)
	bad := seedQuery(t, hid, creator, 0, &deadline, true)
	setRow(t, q, "status = 'completed'")
	setRow(t, bad, "status = 'failed', error = 'internal'")
	addFact(t, hid, q, "query.completed", map[string]any{"query_id": q.String(), "decision": "ESCALATE", "attempts": 2})
	addFact(t, hid, bad, "query.failed", map[string]any{"query_id": bad.String(), "token": "synthetic-not-a-secret"})
	drainAll(t)
	if rows := auditFor(t, "query.completed", q.String()); len(rows) != 1 || rows[0].payload["decision"] != "ESCALATE" {
		t.Fatalf("completed rows %+v", rows)
	}
	rows := auditFor(t, "query.failed", bad.String())
	if len(rows) != 1 || rows[0].payload["payload_refused"] != true || rows[0].payload["token"] != nil {
		t.Fatalf("sensitive fact rows %+v", rows)
	}
	ok, broken, err := audit.Verify(context.Background(), app)
	if err != nil || !ok {
		t.Fatalf("chain verify: ok %v at %d, %v", ok, broken, err)
	}
}

// F2: two drainers running at once (two core copies) append each fact
// exactly once.
func TestTwoDrainersAppendOnce(t *testing.T) {
	hid := seedHospital(t, "active")
	creator := seedUser(t, hid, true, authz.RoleClinician)
	deadline := time.Now().Add(5 * time.Minute)
	var ids []uuid.UUID
	for i := 0; i < 150; i++ {
		q := seedQuery(t, hid, creator, 0, &deadline, true)
		setRow(t, q, "status = 'completed'")
		addFact(t, hid, q, "query.completed", map[string]any{"query_id": q.String(), "decision": "PASS", "attempts": 1})
		ids = append(ids, q)
	}
	var wg sync.WaitGroup
	for c := 0; c < 2; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				if _, err := DrainOnce(context.Background(), app, nil); err != nil {
					t.Errorf("drain: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	drainAll(t)
	for _, q := range ids {
		if n := len(auditFor(t, "query.completed", q.String())); n != 1 {
			t.Fatalf("query %s appended %d times", q, n)
		}
	}
}

// The drain role can append only the three job actions, and only with a
// hospital: 00008's policy holds whatever the code does.
func TestDrainRoleActionAllowList(t *testing.T) {
	hid := seedHospital(t, "active")
	err := db.WithAuditDrain(context.Background(), app, func(tx pgx.Tx) error {
		return audit.Write(context.Background(), tx, audit.Event{HospitalID: hid.String(), Action: "user.created"})
	})
	if err == nil {
		t.Fatal("the drain role appended user.created")
	}
}

// A21 (core side): deactivating a user (through the hospital admin's own
// role) cancels that user's queued and running questions in the database
// and drains query.cancelled for each; another user's question is
// untouched, a running job's next checkpoint matches nothing, and the
// deactivation itself still succeeds. The same for a hospital.
func TestDeactivationCancelsQueuedJobs(t *testing.T) {
	ctx := context.Background()
	hid := seedHospital(t, "active")
	leaving := seedUser(t, hid, true, authz.RoleClinician)
	staying := seedUser(t, hid, true, authz.RoleClinician)
	deadline := time.Now().Add(5 * time.Minute)
	queued := seedQuery(t, hid, leaving, 0, &deadline, true)
	running := seedQuery(t, hid, leaving, 0, &deadline, true)
	setRow(t, running, "status = 'running', attempts = 1, lease_until = now() + interval '90 s'")
	done := seedQuery(t, hid, leaving, 0, &deadline, true)
	setRow(t, done, "status = 'failed', error = 'internal'")
	other := seedQuery(t, hid, staying, 0, &deadline, true)

	if err := db.WithHospital(ctx, app, hid, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1`, leaving)
		return err
	}); err != nil {
		t.Fatalf("deactivation refused: %v", err)
	}
	for name, id := range map[string]uuid.UUID{"queued": queued, "running": running} {
		s := stateOf(t, id)
		if s.status != "cancelled" || s.cancelReason == nil || *s.cancelReason != "user_deactivated" || !s.completed {
			t.Errorf("%s: %+v", name, s)
		}
	}
	if s := stateOf(t, done); s.status != "failed" {
		t.Errorf("finished row changed: %+v", s)
	}
	if s := stateOf(t, other); s.status != "queued" {
		t.Errorf("another user's row changed: %+v", s)
	}
	// The worker's next checkpoint (00008 worker_status) finds no row.
	tx, err := super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_worker`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, hid.String()); err != nil {
		t.Fatal(err)
	}
	tag, err := tx.Exec(ctx, `UPDATE clinical_queries SET progress = 0.5 WHERE id = $1 AND status = 'running' AND attempts = 1`, running)
	_ = tx.Rollback(ctx)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("checkpoint after cancel: %v rows, %v", tag.RowsAffected(), err)
	}
	drainAll(t)
	for _, id := range []uuid.UUID{queued, running} {
		rows := auditFor(t, "query.cancelled", id.String())
		if len(rows) != 1 || rows[0].payload["reason"] != "user_deactivated" || *rows[0].hospital != hid.String() ||
			rows[0].actor == nil || *rows[0].actor != leaving.String() {
			t.Errorf("cancel audit for %s: %+v", id, rows)
		}
	}
	if n := len(auditFor(t, "query.cancelled", other.String())); n != 0 {
		t.Errorf("another user's row audited cancelled")
	}

	// A hospital going from active to deactivated cancels every one of its
	// in-flight questions; another hospital's are untouched.
	h2 := seedHospital(t, "active")
	h3 := seedHospital(t, "active")
	u2 := seedUser(t, h2, true, authz.RoleClinician)
	u3 := seedUser(t, h3, true, authz.RoleClinician)
	a := seedQuery(t, h2, u2, 0, &deadline, true)
	b := seedQuery(t, h2, u2, 0, &deadline, false)
	setRow(t, b, "status = 'running', attempts = 1")
	c := seedQuery(t, h3, u3, 0, &deadline, true)
	if _, err := super.Exec(ctx, `UPDATE hospitals SET status = 'deactivated' WHERE id = $1`, h2); err != nil {
		t.Fatalf("hospital deactivation refused: %v", err)
	}
	for _, id := range []uuid.UUID{a, b} {
		if s := stateOf(t, id); s.status != "cancelled" || *s.cancelReason != "hospital_deactivated" {
			t.Errorf("hospital row %s: %+v", id, s)
		}
	}
	if s := stateOf(t, c); s.status != "queued" {
		t.Errorf("another hospital's row: %+v", s)
	}
	drainAll(t)
	if n := len(auditFor(t, "query.cancelled", a.String())); n != 1 {
		t.Errorf("hospital cancel audits: %d", n)
	}
}

// F4: the owner's cancel policies are scoped. As mhai_migrate, a question
// of an active user in an active hospital can't be seen or changed, and
// nothing can be set but cancelled with a deactivation reason.
func TestCancelPoliciesAreScoped(t *testing.T) {
	ctx := context.Background()
	hid := seedHospital(t, "active")
	u := seedUser(t, hid, true, authz.RoleClinician)
	deadline := time.Now().Add(5 * time.Minute)
	q := seedQuery(t, hid, u, 0, &deadline, true)
	tx, err := super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_migrate`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM clinical_queries WHERE id = $1`, q).Scan(&n); err != nil || n != 0 {
		t.Fatalf("owner sees an active user's question: %d %v", n, err)
	}
	tag, err := tx.Exec(ctx, `UPDATE clinical_queries SET status = 'cancelled', cancel_reason = 'user_deactivated' WHERE id = $1`, q)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("owner cancelled an active user's question: %d %v", tag.RowsAffected(), err)
	}
}

// Review fix 2: the audit row is always about the job (entity query, the
// job's id); a fact's own entity and entity id are payload detail only.
func TestDrainerEntityIsTheJob(t *testing.T) {
	hid := seedHospital(t, "active")
	creator := seedUser(t, hid, true, authz.RoleClinician)
	deadline := time.Now().Add(5 * time.Minute)
	q := seedQuery(t, hid, creator, 0, &deadline, true)
	setRow(t, q, "status = 'completed'")
	victim := uuid.NewString()
	if _, err := super.Exec(context.Background(), `INSERT INTO intelligence_audit_outbox (hospital_id, query_id, action, entity, entity_id, payload)
		VALUES ($1, $2, 'query.completed', 'user', $3, '{"decision":"PASS","attempts":1}')`, hid, q, victim); err != nil {
		t.Fatal(err)
	}
	drainAll(t)
	rows := auditFor(t, "query.completed", q.String())
	if len(rows) != 1 || *rows[0].entity != "query" || *rows[0].entityID != q.String() ||
		rows[0].payload["fact_entity"] != "user" || rows[0].payload["fact_entity_id"] != victim {
		t.Fatalf("rows %+v", rows)
	}
	if n := len(auditFor(t, "query.completed", victim)); n != 0 {
		t.Fatal("a fact chose the audit entity")
	}
}

// Final review I-1: each job action has a closed payload schema. Free text
// in any field, an unknown key, a wrong type, a list or map past its bound
// or a query_id other than the job's is recorded as {query_id,
// payload_refused} and nothing of the fact's own; a complete, well-formed
// payload is recorded as it came.
func TestDrainerClosedPayloadSchema(t *testing.T) {
	hid := seedHospital(t, "active")
	creator := seedUser(t, hid, true, authz.RoleClinician)
	deadline := time.Now().Add(5 * time.Minute)
	const freeText = "SYNTHETIC free text note"
	many := make([]any, maxChunkIDs+1)
	for i := range many {
		many[i] = fmt.Sprintf("KB#%04d", i)
	}
	tooManyModels := map[string]any{}
	for i := 0; i <= maxModelVersions; i++ {
		tooManyModels[fmt.Sprintf("m%d", i)] = "v1"
	}
	completed := func(over map[string]any) map[string]any {
		p := map[string]any{"decision": "PASS", "trust_band": "high", "chunk_ids": []any{"KB#0001"},
			"model_versions": map[string]any{"cca": "qwen2.5:7b"}, "attempts": 1}
		for k, v := range over {
			if v == nil {
				delete(p, k)
				continue
			}
			p[k] = v
		}
		return p
	}
	type tc struct {
		action, status string
		payload        map[string]any
	}
	cases := map[string]tc{
		"decision text":         {"query.completed", "completed", completed(map[string]any{"decision": freeText})},
		"trust band text":       {"query.completed", "completed", completed(map[string]any{"trust_band": freeText})},
		"chunk id text":         {"query.completed", "completed", completed(map[string]any{"chunk_ids": []any{"KB#0001", freeText}})},
		"chunk id not a string": {"query.completed", "completed", completed(map[string]any{"chunk_ids": []any{map[string]any{"x": freeText}}})},
		"too many chunk ids":    {"query.completed", "completed", completed(map[string]any{"chunk_ids": many})},
		"model version text":    {"query.completed", "completed", completed(map[string]any{"model_versions": map[string]any{"cca": freeText}})},
		"model name text":       {"query.completed", "completed", completed(map[string]any{"model_versions": map[string]any{freeText: "v1"}})},
		"too many models":       {"query.completed", "completed", completed(map[string]any{"model_versions": tooManyModels})},
		"attempts text":         {"query.completed", "completed", completed(map[string]any{"attempts": freeText})},
		"attempts fraction":     {"query.completed", "completed", completed(map[string]any{"attempts": 1.5})},
		"no decision":           {"query.completed", "completed", completed(map[string]any{"decision": nil})},
		"extra key":             {"query.completed", "completed", completed(map[string]any{"note": freeText})},
		"another query id":      {"query.completed", "completed", completed(map[string]any{"query_id": uuid.NewString()})},
		"error text":            {"query.failed", "failed", map[string]any{"error": freeText, "attempts": 1}},
		"error unknown code":    {"query.failed", "failed", map[string]any{"error": "model_crashed", "attempts": 1}},
		"failed extra key":      {"query.failed", "failed", map[string]any{"error": "internal", "attempts": 1, "detail": freeText}},
		"failed no attempts":    {"query.failed", "failed", map[string]any{"error": "internal"}},
		"reason text":           {"query.cancelled", "cancelled", map[string]any{"reason": freeText}},
		"cancelled extra key":   {"query.cancelled", "cancelled", map[string]any{"reason": "user_deactivated", "by": freeText}},
	}
	ids := map[string]uuid.UUID{}
	for name, c := range cases {
		q := seedQuery(t, hid, creator, 0, &deadline, true)
		setRow(t, q, "status = '"+c.status+"'")
		addFact(t, hid, q, c.action, c.payload)
		ids[name] = q
	}
	// A fact entity id is free text up to 64 bytes in the outbox: it must
	// be an id to reach the chain.
	qe := seedQuery(t, hid, creator, 0, &deadline, true)
	setRow(t, qe, "status = 'completed'")
	if _, err := super.Exec(context.Background(), `INSERT INTO intelligence_audit_outbox (hospital_id, query_id, action, entity, entity_id, payload)
		VALUES ($1, $2, 'query.completed', 'query', $3, '{"decision":"PASS","attempts":1}')`, hid, qe, freeText); err != nil {
		t.Fatal(err)
	}
	ids["entity id text"] = qe
	cases["entity id text"] = tc{"query.completed", "completed", nil}

	good := seedQuery(t, hid, creator, 0, &deadline, true)
	setRow(t, good, "status = 'completed'")
	addFact(t, hid, good, "query.completed", completed(map[string]any{"query_id": good.String(), "trust_band": nil}))
	goodNull := seedQuery(t, hid, creator, 0, &deadline, true)
	setRow(t, goodNull, "status = 'completed'")
	nullBand := completed(nil)
	nullBand["trust_band"] = nil // JSON null: an unverified answer has no band
	addFact(t, hid, goodNull, "query.completed", nullBand)
	drainAll(t)

	for name, q := range ids {
		rows := auditFor(t, cases[name].action, q.String())
		if len(rows) != 1 {
			t.Errorf("%s: %d rows", name, len(rows))
			continue
		}
		p := rows[0].payload
		if p["payload_refused"] != true || p["query_id"] != q.String() || len(p) != 2 {
			t.Errorf("%s: payload %v", name, p)
		}
		if raw, _ := json.Marshal(p); strings.Contains(string(raw), "SYNTHETIC") {
			t.Errorf("%s: free text on the chain: %s", name, raw)
		}
	}
	rows := auditFor(t, "query.completed", good.String())
	if len(rows) != 1 || rows[0].payload["payload_refused"] != nil || rows[0].payload["decision"] != "PASS" ||
		rows[0].payload["attempts"] != float64(1) {
		t.Fatalf("a good payload: %+v", rows)
	}
	if mv, _ := rows[0].payload["model_versions"].(map[string]any); mv["cca"] != "qwen2.5:7b" {
		t.Fatalf("model_versions: %v", rows[0].payload)
	}
	if rows := auditFor(t, "query.completed", goodNull.String()); len(rows) != 1 || rows[0].payload["payload_refused"] != nil {
		t.Fatalf("null trust band: %+v", rows)
	}
}

// Final review I-1: a fact's action must match its job's status now. A
// query.completed for a row that isn't completed (or a query.failed for a
// completed one, a query.cancelled for a running one) is a false statement:
// it is taken off the outbox and never appended.
func TestDrainerRefusesActionStatusMismatch(t *testing.T) {
	hid := seedHospital(t, "active")
	creator := seedUser(t, hid, true, authz.RoleClinician)
	deadline := time.Now().Add(5 * time.Minute)
	for _, c := range []struct {
		action, set string
		payload     map[string]any
	}{
		{"query.completed", "status = 'queued'", map[string]any{"decision": "PASS", "attempts": 1}},
		{"query.completed", "status = 'running', attempts = 1", map[string]any{"decision": "PASS", "attempts": 1}},
		{"query.completed", "status = 'cancelled', cancel_reason = 'user_deactivated'", map[string]any{"decision": "PASS", "attempts": 1}},
		{"query.failed", "status = 'completed'", map[string]any{"error": "internal", "attempts": 1}},
		{"query.cancelled", "status = 'running', attempts = 1", map[string]any{"reason": "user_deactivated"}},
		{"query.cancelled", "status = 'failed', error = 'internal'", map[string]any{"reason": "user_deactivated"}},
	} {
		q := seedQuery(t, hid, creator, 0, &deadline, true)
		setRow(t, q, c.set)
		addFact(t, hid, q, c.action, c.payload)
		drainAll(t)
		if rows := auditFor(t, c.action, q.String()); len(rows) != 0 {
			t.Errorf("%s with %s appended: %+v", c.action, c.set, rows)
		}
		if outboxCount(t, q) != 0 {
			t.Errorf("%s with %s left in the outbox", c.action, c.set)
		}
	}
	ok, broken, err := audit.Verify(context.Background(), app)
	if err != nil || !ok {
		t.Fatalf("chain verify: ok %v at %d, %v", ok, broken, err)
	}
}
