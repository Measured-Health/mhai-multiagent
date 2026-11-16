//go:build integration

package cloudreap_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/cloudreap"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// The reaper loop end to end (migration 00047), on core's kind of login (a
// plain member of mhai_app). Every name and value is synthetic.

type env struct {
	super *pgxpool.Pool // the superuser: seeds and reads
	app   *pgxpool.Pool // core's kind of login: the loop's pool
}

func setup(t *testing.T) env {
	t.Helper()
	pg, super := testutil.MigratedPostgres(t)
	app, err := testutil.AppLogin(context.Background(), pg, super)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return env{super: super, app: app}
}

func exec(t *testing.T, pool *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// hospital is an active synthetic hospital with an openai policy.
func hospital(t *testing.T, e env, budget int64) uuid.UUID {
	t.Helper()
	h := uuid.New()
	code := "CR" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	exec(t, e.super, `INSERT INTO hospitals (id, name, short_code, status, time_zone) VALUES ($1, 'Synthetic Hospital', $2, 'active', 'Africa/Lagos')`, h, code)
	exec(t, e.super, `INSERT INTO hospital_model_policy (hospital_id, cloud_provider, agreement_ref, monthly_token_budget)
		VALUES ($1, 'openai', 'AGR-2026-0001', $2)`, h, budget)
	return h
}

// as runs fn as role with hospital h set, committing.
func as(ctx context.Context, pool *pgxpool.Pool, role string, h uuid.UUID, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{`SET LOCAL ROLE ` + role, `SELECT set_config('mhai.hospital_id', '` + h.String() + `', true)`} {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func reserve(t *testing.T, e env, h uuid.UUID, n int64) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := as(context.Background(), e.super, "mhai_worker", h, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT reservation FROM mhai_cloud_reserve('openai', $1)`, n).Scan(&id)
	}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	return id
}

func settle(ctx context.Context, e env, h, id uuid.UUID, used int64) (bool, error) {
	var done bool
	err := as(ctx, e.super, "mhai_worker", h, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mhai_cloud_settle($1, $2)`, id, used).Scan(&done)
	})
	return done, err
}

func backdate(t *testing.T, e env, by string, ids ...uuid.UUID) {
	t.Helper()
	exec(t, e.super, `UPDATE cloud_reservations SET created_at = now() - $2::interval WHERE id = ANY($1)`, ids, by)
}

func ledger(t *testing.T, e env, h uuid.UUID) (used, reserved int64) {
	t.Helper()
	if err := e.super.QueryRow(context.Background(), `SELECT COALESCE(sum(used), 0), COALESCE(sum(reserved), 0) FROM cloud_usage WHERE hospital_id = $1`, h).
		Scan(&used, &reserved); err != nil {
		t.Fatal(err)
	}
	return used, reserved
}

type auditRow struct {
	noActor bool
	entity  string
	payload map[string]any
}

func auditRows(t *testing.T, e env, h uuid.UUID) []auditRow {
	t.Helper()
	rows, err := e.super.Query(context.Background(), `SELECT actor_id IS NULL, entity_id, payload FROM audit_log
		WHERE hospital_id = $1 AND action = $2 ORDER BY id`, h, cloudreap.Action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		var raw []byte
		if err := rows.Scan(&r.noActor, &r.entity, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &r.payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// One run settles every stale reservation of every hospital as fully used,
// audits each (ids and counts only, no actor), counts them, and leaves a
// fresh one; the next run does nothing. The chain still verifies.
func TestRunReapsStaleAuditsAndCounts(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a, b := hospital(t, e, 1_000_000), hospital(t, e, 1_000_000)
	var staleA []uuid.UUID
	for range cloudreap.Batch + 5 { // more than one batch
		staleA = append(staleA, reserve(t, e, a, 10))
	}
	freshA := reserve(t, e, a, 999)
	staleB := reserve(t, e, b, 77)
	backdate(t, e, "61 minutes", append(staleA, staleB)...)

	var logs bytes.Buffer
	m := &cloudreap.Metrics{}
	l := &cloudreap.Loop{Pool: e.app, Log: slog.New(slog.NewJSONHandler(&logs, nil)), Metrics: m}
	ran, err := l.Tick(ctx)
	if err != nil || !ran {
		t.Fatalf("tick: ran %v err %v", ran, err)
	}
	wantRows := int64(len(staleA) + 1)
	if m.Reaped.Load() != wantRows || m.Tokens.Load() != int64(len(staleA))*10+77 || m.Failures.Load() != 0 {
		t.Errorf("metrics: reaped %d tokens %d failures %d", m.Reaped.Load(), m.Tokens.Load(), m.Failures.Load())
	}
	if used, res := ledger(t, e, a); used != int64(len(staleA))*10 || res != 999 {
		t.Errorf("a: used %d reserved %d", used, res)
	}
	if used, res := ledger(t, e, b); used != 77 || res != 0 {
		t.Errorf("b: used %d reserved %d", used, res)
	}
	rowsA := auditRows(t, e, a)
	if len(rowsA) != len(staleA) {
		t.Fatalf("a: %d audit rows, want %d", len(rowsA), len(staleA))
	}
	seen := map[string]bool{}
	for _, r := range rowsA {
		id, _ := r.payload["reservation_id"].(string)
		seen[id] = true
		if !r.noActor || r.entity != id || r.payload["reserved_tokens"] != float64(10) || r.payload["age_minutes"].(float64) < 60 ||
			len(r.payload) != 4 {
			t.Errorf("audit row %+v", r)
		}
		if month, _ := r.payload["month"].(string); month != time.Now().UTC().Format("2006-01") {
			// The reservation's month is the database's, in UTC, when it was
			// reserved: today's unless the test ran across a month turn.
			t.Logf("audit month %s", month)
		}
	}
	for _, id := range staleA {
		if !seen[id.String()] {
			t.Errorf("reservation %s not audited", id)
		}
	}
	if rows := auditRows(t, e, b); len(rows) != 1 || rows[0].payload["reservation_id"] != staleB.String() {
		t.Errorf("b's audit rows %+v", rows)
	}
	out := logs.String()
	if !strings.Contains(out, a.String()) || !strings.Contains(out, staleB.String()) || strings.Contains(out, "Synthetic Hospital") {
		t.Errorf("logs: %s", out)
	}
	// The fresh one is still the worker's.
	if done, err := settle(ctx, e, a, freshA, 100); err != nil || !done {
		t.Errorf("fresh settle: %v %v", done, err)
	}
	// A late settle of a reaped one changes nothing.
	if done, err := settle(ctx, e, a, staleA[0], 1); err != nil || done {
		t.Errorf("late settle: %v %v", done, err)
	}
	if used, res := ledger(t, e, a); used != int64(len(staleA))*10+100 || res != 0 {
		t.Errorf("a after settles: used %d reserved %d", used, res)
	}
	// Nothing more to do.
	if err := l.RunOnce(ctx); err != nil || m.Reaped.Load() != wantRows {
		t.Errorf("second run: %v, reaped %d", err, m.Reaped.Load())
	}
	// The audit chain still verifies with the reaper's rows in it.
	if ok, first, err := audit.Verify(ctx, e.super); err != nil || !ok {
		t.Errorf("audit chain: ok %v first broken %d err %v", ok, first, err)
	}
}

// The age is the loop's: a two-hour reaper leaves a 61-minute reservation.
func TestRunHonoursTheAge(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := hospital(t, e, 1_000_000)
	id := reserve(t, e, a, 10)
	backdate(t, e, "61 minutes", id)
	m := &cloudreap.Metrics{}
	l := &cloudreap.Loop{Pool: e.app, Age: 2 * time.Hour, Metrics: m}
	if err := l.RunOnce(ctx); err != nil || m.Reaped.Load() != 0 {
		t.Fatalf("2h run: %v, reaped %d", err, m.Reaped.Load())
	}
	l.Age = time.Hour
	if err := l.RunOnce(ctx); err != nil || m.Reaped.Load() != 1 {
		t.Fatalf("1h run: %v, reaped %d", err, m.Reaped.Load())
	}
}

// Only one core copy runs at a time: while one holds the lock, another's
// tick does nothing.
func TestOneLeader(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := hospital(t, e, 1_000_000)
	id := reserve(t, e, a, 10)
	backdate(t, e, "2 hours", id)
	conn, err := e.super.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('mhai.cloud.reaper'))`); err != nil {
		t.Fatal(err)
	}
	m := &cloudreap.Metrics{}
	l := &cloudreap.Loop{Pool: e.app, Metrics: m}
	if ran, err := l.Tick(ctx); ran || err != nil || m.Reaped.Load() != 0 {
		t.Errorf("tick beside a leader: ran %v err %v reaped %d", ran, err, m.Reaped.Load())
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext('mhai.cloud.reaper'))`); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	if ran, err := l.Tick(ctx); !ran || err != nil || m.Reaped.Load() != 1 {
		t.Errorf("tick as leader: ran %v err %v reaped %d", ran, err, m.Reaped.Load())
	}
}

// A failed audit write rolls the reap back: nothing is charged without its
// audit row, and the failure is counted and logged by hospital and code.
func TestFailedAuditRollsBackTheReap(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := hospital(t, e, 1_000_000)
	id := reserve(t, e, a, 10)
	backdate(t, e, "2 hours", id)
	exec(t, e.super, `ALTER POLICY cloud_reaper_append ON audit_log WITH CHECK (false)`)
	var logs bytes.Buffer
	m := &cloudreap.Metrics{}
	l := &cloudreap.Loop{Pool: e.app, Log: slog.New(slog.NewJSONHandler(&logs, nil)), Metrics: m}
	if err := l.RunOnce(ctx); err == nil {
		t.Fatal("the run hid the failure")
	}
	if m.Failures.Load() != 1 || m.Reaped.Load() != 0 {
		t.Errorf("failures %d reaped %d", m.Failures.Load(), m.Reaped.Load())
	}
	if used, res := ledger(t, e, a); used != 0 || res != 10 {
		t.Errorf("ledger moved without its audit: used %d reserved %d", used, res)
	}
	if out := logs.String(); !strings.Contains(out, a.String()) || !strings.Contains(out, `"sqlstate":"42501"`) {
		t.Errorf("logs: %s", out)
	}
	exec(t, e.super, `ALTER POLICY cloud_reaper_append ON audit_log WITH CHECK (hospital_id = mhai_current_hospital() AND actor_id IS NULL AND action = 'cloud.reservation_reaped')`)
	if err := l.RunOnce(ctx); err != nil || m.Reaped.Load() != 1 {
		t.Errorf("after the fix: %v reaped %d", err, m.Reaped.Load())
	}
}

// Concurrent settles and loop runs over the same stale reservations: each
// is consumed once; reserved ends at 0; used is what the winners charged.
func TestLoopRacesSettles(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := hospital(t, e, 1_000_000)
	const rows, n, reported = 30, 100, 9
	ids := make([]uuid.UUID, rows)
	for i := range ids {
		ids[i] = reserve(t, e, a, n)
	}
	backdate(t, e, "2 hours", ids...)
	m := &cloudreap.Metrics{}
	l := &cloudreap.Loop{Pool: e.app, Metrics: m}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		settled int64
		errs    []error
		start   = make(chan struct{})
	)
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			done, err := settle(ctx, e, a, id, reported)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
			if done {
				settled++
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for range 3 {
			if err := l.RunOnce(ctx); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}
	}()
	close(start)
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if err := l.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	reaped := m.Reaped.Load()
	if settled+reaped != rows {
		t.Errorf("settled %d + reaped %d, want %d", settled, reaped, rows)
	}
	if used, res := ledger(t, e, a); res != 0 || used != settled*reported+reaped*n {
		t.Errorf("ledger: used %d reserved %d, want %d and 0", used, res, settled*reported+reaped*n)
	}
	if got := int64(len(auditRows(t, e, a))); got != reaped {
		t.Errorf("%d audit rows, want one per reaped reservation (%d)", got, reaped)
	}
	t.Logf("%d settled, %d reaped", settled, reaped)
}

// A listed hospital whose stale rows a settle holds reaps none: the run
// waits for nothing, counts it as empty and logs it; once the settle
// commits there is nothing left to reap.
func TestRunCountsAHospitalWithNothingToReap(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := hospital(t, e, 1_000_000)
	id := reserve(t, e, a, 10)
	backdate(t, e, "2 hours", id)
	tx, err := e.super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{`SET LOCAL ROLE mhai_worker`, `SELECT set_config('mhai.hospital_id', '` + a.String() + `', true)`} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var done bool
	if err := tx.QueryRow(ctx, `SELECT mhai_cloud_settle($1, 4)`, id).Scan(&done); err != nil || !done {
		t.Fatalf("settle in progress: %v %v", done, err)
	}
	var logs bytes.Buffer
	m := &cloudreap.Metrics{}
	l := &cloudreap.Loop{Pool: e.app, Log: slog.New(slog.NewJSONHandler(&logs, nil)), Metrics: m}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = l.RunOnce(rctx)
	cancel()
	if err != nil || m.Reaped.Load() != 0 || m.Empty.Load() != 1 {
		t.Fatalf("run beside a settle: %v reaped %d empty %d", err, m.Reaped.Load(), m.Empty.Load())
	}
	if !strings.Contains(logs.String(), a.String()) {
		t.Errorf("logs: %s", logs.String())
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.RunOnce(ctx); err != nil || m.Reaped.Load() != 0 || m.Empty.Load() != 1 {
		t.Errorf("after the settle: %v reaped %d empty %d", err, m.Reaped.Load(), m.Empty.Load())
	}
	if used, res := ledger(t, e, a); used != 4 || res != 0 {
		t.Errorf("ledger: used %d reserved %d, want 4 and 0", used, res)
	}
}
