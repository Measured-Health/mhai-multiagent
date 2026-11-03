//go:build integration

package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/telemetry"
)

// Plan 4 Task 10: chain verification runs (spec §5.4; C24, C25). The chain
// and the runs are global, so these reset both.

func resetRuns(t *testing.T) {
	t.Helper()
	reset := func() {
		if _, err := super.Exec(context.Background(), `DELETE FROM audit_verify_runs`); err != nil {
			t.Fatal(err)
		}
	}
	reset()
	t.Cleanup(reset)
}

// writeMany appends n entries in one transaction as the auth role.
func writeMany(t *testing.T, n int) {
	t.Helper()
	ctx := context.Background()
	if err := db.WithAuth(ctx, app, func(tx pgx.Tx) error {
		for i := 0; i < n; i++ {
			if err := audit.Write(ctx, tx, audit.Event{Action: "test.chain.auth", Payload: map[string]any{"n": i}}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type runRow struct {
	state                     string
	finished                  *time.Time
	entries, toSeq, brokenSeq *int64
	fromFirst                 bool
	toAt, brokenAt            *time.Time
	brokenID, code            *string
	version                   int
	by                        *uuid.UUID
}

func readRun(t *testing.T, id uuid.UUID) runRow {
	t.Helper()
	var r runRow
	if err := super.QueryRow(context.Background(), `
		SELECT state, finished_at, entries_checked, to_seq, first_broken_seq, from_first, to_at, first_broken_at,
		       first_broken_id, error_code, version, "by"
		FROM audit_verify_runs WHERE id = $1`, id).Scan(&r.state, &r.finished, &r.entries, &r.toSeq, &r.brokenSeq,
		&r.fromFirst, &r.toAt, &r.brokenAt, &r.brokenID, &r.code, &r.version, &r.by); err != nil {
		t.Fatal(err)
	}
	return r
}

// runEntries are the chain's entries about run id, in order.
func runEntries(t *testing.T, id uuid.UUID) []map[string]any {
	t.Helper()
	rows, err := super.Query(context.Background(), `SELECT action, payload FROM audit_log
		WHERE entity = 'audit_run' AND entity_id = $1 ORDER BY id`, id.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var action string
		var raw []byte
		if err := rows.Scan(&action, &raw); err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		p["_action"] = action
		out = append(out, p)
	}
	return out
}

func startAndRun(t *testing.T, budget time.Duration) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	id, started, err := audit.StartRun(ctx, app, nil)
	if err != nil || !started {
		t.Fatalf("start: %v, started %v", err, started)
	}
	state, err := audit.Execute(ctx, app, id, budget)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return id, state
}

// C24: a walk stopped by its budget ends error timed_out, never intact,
// with no proof fields.
func TestVerifyTimeoutIsErrorNotIntact(t *testing.T) {
	resetChain(t)
	resetRuns(t)
	writeMany(t, 10000)
	res, err := audit.VerifyRange(context.Background(), app, time.Millisecond)
	if !errors.Is(err, audit.ErrBudget) || res.Complete {
		t.Fatalf("1 ms walk: err %v, complete %v", err, res.Complete)
	}
	id, state := startAndRun(t, time.Millisecond)
	if state != "error" {
		t.Fatalf("state %q, want error", state)
	}
	r := readRun(t, id)
	if r.state != "error" || r.code == nil || *r.code != "timed_out" || r.finished == nil ||
		r.toSeq != nil || r.toAt != nil || r.fromFirst || r.brokenSeq != nil {
		t.Fatalf("timed-out run: %+v", r)
	}
	es := runEntries(t, id)
	if len(es) != 2 || es[1]["_action"] != "audit.verify_finished" || es[1]["state"] != "error" || es[1]["error_code"] != "timed_out" {
		t.Fatalf("entries: %v", es)
	}
	var intact int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM audit_verify_runs WHERE state = 'intact'`).Scan(&intact); err != nil || intact != 0 {
		t.Fatalf("intact runs: %d %v", intact, err)
	}
	// The same chain with the real budget is intact.
	if _, state := startAndRun(t, audit.VerifyBudget); state != "intact" {
		t.Fatalf("full budget: %q", state)
	}
}

// C24: an intact run carries every proof field: the whole chain walked
// from the first entry, to the last entry's seq and time.
func TestIntactHasProofFields(t *testing.T) {
	resetChain(t)
	resetRuns(t)
	telemetry.SetAuditChainBroken(true)
	writeMany(t, 5)
	id, state := startAndRun(t, audit.VerifyBudget)
	if state != "intact" {
		t.Fatalf("state %q", state)
	}
	var head int64
	var headAt time.Time
	var count int64
	ctx := context.Background()
	// The run's own verify_started is the last entry it walked.
	if err := super.QueryRow(ctx, `SELECT id, ts FROM audit_log WHERE action = 'audit.verify_started' AND entity_id = $1`,
		id.String()).Scan(&head, &headAt); err != nil {
		t.Fatal(err)
	}
	if err := super.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE id <= $1`, head).Scan(&count); err != nil {
		t.Fatal(err)
	}
	r := readRun(t, id)
	if r.state != "intact" || r.finished == nil || r.entries == nil || *r.entries != count || count != 6 || !r.fromFirst ||
		r.toSeq == nil || *r.toSeq != head || r.toAt == nil || !r.toAt.Equal(headAt) || r.code != nil || r.brokenSeq != nil ||
		r.version != 2 || r.by != nil {
		t.Fatalf("intact run: %+v (head %d at %v, count %d)", r, head, headAt, count)
	}
	es := runEntries(t, id)
	if len(es) != 2 || es[0]["by"] != "schedule" || es[1]["state"] != "intact" || es[1]["entries_checked"] != float64(6) {
		t.Fatalf("entries: %v", es)
	}
	if telemetry.AuditChainBroken() {
		t.Error("an intact run with no open failure left audit_chain_broken at 1")
	}
}

// A tampered entry: the run is broken at it, with its seq, id and time;
// the gauge goes to 1 and the failure is open.
func TestBrokenRunFindsFirstBroken(t *testing.T) {
	resetChain(t)
	resetRuns(t)
	t.Cleanup(func() { telemetry.SetAuditChainBroken(false) })
	writeMany(t, 4)
	ctx := context.Background()
	var target int64
	var targetAt time.Time
	if err := super.QueryRow(ctx, `SELECT id, ts FROM audit_log ORDER BY id OFFSET 1 LIMIT 1`).Scan(&target, &targetAt); err != nil {
		t.Fatal(err)
	}
	// The log is append-only for app roles: tampering needs the superuser.
	if _, err := super.Exec(ctx, `UPDATE audit_log SET hash = repeat('f', 64) WHERE id = $1`, target); err != nil {
		t.Fatal(err)
	}
	id, state := startAndRun(t, audit.VerifyBudget)
	if state != "broken" {
		t.Fatalf("state %q", state)
	}
	r := readRun(t, id)
	if r.brokenSeq == nil || *r.brokenSeq != target || r.brokenID == nil || *r.brokenID != strconv.FormatInt(target, 10) ||
		r.brokenAt == nil || !r.brokenAt.Equal(targetAt) || r.entries == nil || *r.entries != 1 || r.finished == nil {
		t.Fatalf("broken run: %+v (target %d)", r, target)
	}
	es := runEntries(t, id)
	if len(es) != 2 || es[1]["state"] != "broken" || es[1]["first_broken_seq"] != float64(target) {
		t.Fatalf("entries: %v", es)
	}
	if !telemetry.AuditChainBroken() {
		t.Error("a broken run left audit_chain_broken at 0")
	}
	var open uuid.UUID
	if err := db.WithPlatform(ctx, app, func(tx pgx.Tx) error {
		var err error
		open, err = audit.OpenFailure(ctx, tx)
		return err
	}); err != nil || open != id {
		t.Fatalf("open failure %v, %v; want %v", open, err, id)
	}
	// Verify (the wrapper) agrees.
	if ok, broken, err := audit.Verify(ctx, app); err != nil || ok || broken != target {
		t.Fatalf("Verify: %v %d %v", ok, broken, err)
	}
}

// C25: parallel starts make one run; every caller gets its id, and only
// one verify_started is written.
func TestTwoStartsOneRun(t *testing.T) {
	resetChain(t)
	resetRuns(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	ids := make([]uuid.UUID, 12)
	started := make([]bool, 12)
	errs := make([]error, 12)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], started[i], errs[i] = audit.StartRun(ctx, app, nil)
		}(i)
	}
	wg.Wait()
	n := 0
	for i := range ids {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("start %d: %v %v (first %v)", i, ids[i], errs[i], ids[0])
		}
		if started[i] {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d starts reported started", n)
	}
	var runs, startedRows int
	if err := super.QueryRow(ctx, `SELECT count(*) FROM audit_verify_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := super.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'audit.verify_started'`).Scan(&startedRows); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || startedRows != 1 {
		t.Fatalf("runs %d, verify_started %d", runs, startedRows)
	}
}

// A run left running by a core that died is closed as timed out by the
// next start, so it can't block every later run.
func TestStaleRunClosed(t *testing.T) {
	resetChain(t)
	resetRuns(t)
	ctx := context.Background()
	var stale uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO audit_verify_runs (started_at) VALUES (now() - interval '1 hour') RETURNING id`).
		Scan(&stale); err != nil {
		t.Fatal(err)
	}
	id, started, err := audit.StartRun(ctx, app, nil)
	if err != nil || !started || id == stale {
		t.Fatalf("start past a stale run: %v %v %v", id, started, err)
	}
	r := readRun(t, stale)
	if r.state != "error" || r.code == nil || *r.code != "timed_out" || r.finished == nil {
		t.Fatalf("stale run: %+v", r)
	}
	if es := runEntries(t, stale); len(es) != 1 || es[0]["state"] != "error" || es[0]["error_code"] != "timed_out" {
		t.Fatalf("stale run's entries: %v", es)
	}
	// A recent running run is not stale: it is returned.
	again, started, err := audit.StartRun(ctx, app, nil)
	if err != nil || started || again != id {
		t.Fatalf("second start: %v %v %v", again, started, err)
	}
}

// C25: two schedulers with the clock at 06:00 start one run between them,
// however often they tick; before 06:00 neither starts one.
func TestSchedulerOneCopy(t *testing.T) {
	resetChain(t)
	resetRuns(t)
	ctx := context.Background()
	n := time.Now().UTC()
	at := time.Date(n.Year(), n.Month(), n.Day(), 6, 0, 0, 0, time.UTC)
	if n.Before(at) {
		at = at.AddDate(0, 0, -1)
	}
	early := func() time.Time { return at.Add(-time.Minute) }
	for _, now := range []func() time.Time{early} {
		if id, _, err := (audit.Scheduler{Pool: app, At: "06:00", Now: now}).Tick(ctx); err != nil || id != uuid.Nil {
			t.Fatalf("05:59 tick: %v %v", id, err)
		}
	}
	clock := func() time.Time { return at }
	a := audit.Scheduler{Pool: app, At: "06:00", Now: clock}
	b := audit.Scheduler{Pool: app, At: "06:00", Now: clock}
	for round := 0; round < 3; round++ {
		var wg sync.WaitGroup
		for _, s := range []audit.Scheduler{a, b, a, b} {
			wg.Add(1)
			go func(s audit.Scheduler) {
				defer wg.Done()
				if _, _, err := s.Tick(ctx); err != nil {
					t.Errorf("tick: %v", err)
				}
			}(s)
		}
		wg.Wait()
	}
	var runs, started int
	if err := super.QueryRow(ctx, `SELECT count(*) FROM audit_verify_runs WHERE "by" IS NULL`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := super.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'audit.verify_started' AND payload->>'by' = 'schedule'`).
		Scan(&started); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || started != 1 {
		t.Fatalf("scheduled runs %d, verify_started %d", runs, started)
	}
	var state string
	if err := super.QueryRow(ctx, `SELECT state FROM audit_verify_runs`).Scan(&state); err != nil || state != "intact" {
		t.Fatalf("the scheduled run: %q %v", state, err)
	}
	// The lock is released: a session lock left on a pooled connection
	// would block the next day's tick.
	var held int
	if err := super.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'`).
		Scan(&held); err != nil || held != 0 {
		t.Fatalf("advisory locks held: %d %v", held, err)
	}
	if _, _, err := (audit.Scheduler{Pool: app, At: "6:00"}).Tick(ctx); err == nil || !strings.Contains(err.Error(), "HH:MM") {
		t.Fatalf("a bad schedule time: %v", err)
	}
}
