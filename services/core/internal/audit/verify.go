package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/telemetry"
)

// Chain verification runs (Plan 4 spec §5.4; contract §14.9 "Integrity").
//
// A run is a row of audit_verify_runs. It is born running (StartRunTx: one
// at a time, the audit_verify_one_running index), with audit.verify_started
// on the chain; the walk (VerifyRange) then reads the chain as
// mhai_platform under a budget, and Finish writes the result with
// audit.verify_finished. A run is intact only when the whole chain was
// walked: Complete, at least one entry, the last entry's time and from the
// first entry (C24). Anything short of that is error, with a code; a
// timeout is error timed_out, never intact. The proof fields are written
// only by Finish, never on insert (00014's platform_runs_start).

// VerifyBudget is the walk's time budget (spec §5.4).
const VerifyBudget = 10 * time.Minute

// staleAfter is how long a run may stay running before the next start
// closes it as timed out: its walk's budget, plus a margin for the finish.
// A run left running by a core that died would otherwise block every
// later run.
const staleAfter = VerifyBudget + 5*time.Minute

// ErrBudget is a walk stopped by its budget.
var ErrBudget = errors.New("audit: verify budget spent")

// Result is how far a walk got. EntriesChecked counts the entries whose
// link and hash held; ToSeq and ToAt are the last of them. FirstBroken,
// FirstBrokenID (the entry's opaque id, its decimal seq) and FirstBrokenAt
// name the first entry that didn't hold, if any. Complete is true only when
// the walk reached the end of the chain with every entry holding.
type Result struct {
	EntriesChecked int64
	ToSeq          int64
	ToAt           *time.Time
	FirstBroken    *int64
	FirstBrokenID  string
	FirstBrokenAt  *time.Time
	Complete       bool
}

// walkPage is how many entries one statement of the walk reads (a var
// only so a test can make the pages small).
var walkPage = 5000

// VerifyRange walks the whole chain in id order from the first entry, as
// mhai_platform, within budget (0: no budget). It reads keyset pages of
// walkPage entries inside one REPEATABLE READ, READ ONLY transaction, so
// every page sees the same chain: entries appended during the walk wait for
// the next run. It stops at the first entry whose prev_hash doesn't match
// the entry before it, or whose hash doesn't match its contents.
//
// A break found is the result, whatever happens after it: an error while
// the rest of its page drains, or at the end of the transaction, never
// turns a broken chain into timed_out. Without a break, a spent budget
// returns the partial Result and ErrBudget.
func VerifyRange(ctx context.Context, pool *pgxpool.Pool, budget time.Duration) (Result, error) {
	wctx := ctx
	if budget > 0 {
		var cancel context.CancelFunc
		wctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	var res Result
	err := db.WithPlatformSnapshot(wctx, pool, func(tx pgx.Tx) error {
		prev := genesis
		last := int64(0)
		for {
			n, err := walkOnePage(wctx, tx, &res, &prev, &last)
			if res.FirstBroken != nil {
				return errStopWalk
			}
			if err != nil {
				return err
			}
			if n < walkPage {
				break
			}
		}
		// The budget may run out between the last page and here.
		if wctx.Err() != nil {
			return wctx.Err()
		}
		res.Complete = true
		return nil
	})
	if res.FirstBroken != nil {
		res.Complete = false
		return res, nil
	}
	if err != nil {
		res.Complete = false
		if budget > 0 && wctx.Err() != nil && ctx.Err() == nil {
			return res, ErrBudget
		}
		return res, fmt.Errorf("audit: verify: %w", err)
	}
	return res, nil
}

// errStopWalk ends the walk's transaction (rolled back: it only read) once
// a break is found.
var errStopWalk = errors.New("audit: walk stopped at a break")

// walkOnePage checks the entries after *last, at most walkPage of them, and
// returns how many it read. It returns as soon as it records a break in res.
func walkOnePage(ctx context.Context, tx pgx.Tx, res *Result, prev *string, last *int64) (int, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, ts, actor_id::text, hospital_id::text, request_id, action, entity, entity_id,
		       payload, prev_hash, hash
		FROM audit_log WHERE id > $1 ORDER BY id LIMIT $2`, *last, walkPage)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		var (
			id                                           int64
			r                                            record
			actor, hospital, requestID, entity, entityID *string
			prevHash, hash                               string
		)
		if err := rows.Scan(&id, &r.ts, &actor, &hospital, &requestID, &r.action, &entity, &entityID,
			&r.payload, &prevHash, &hash); err != nil {
			return n, err
		}
		r.actorID, r.hospitalID, r.requestID = deref(actor), deref(hospital), deref(requestID)
		r.entity, r.entityID = deref(entity), deref(entityID)
		want, err := r.hash(prevHash)
		if err != nil || prevHash != *prev || want != hash {
			at := r.ts.UTC()
			res.FirstBroken, res.FirstBrokenID, res.FirstBrokenAt = &id, strconv.FormatInt(id, 10), &at
			return n, nil
		}
		at := r.ts.UTC()
		res.EntriesChecked, res.ToSeq, res.ToAt = res.EntriesChecked+1, id, &at
		*prev, *last = hash, id
	}
	return n, rows.Err()
}

// Verify walks the whole chain with no budget. ok is true, and firstBroken
// 0, for an intact (or empty) chain.
func Verify(ctx context.Context, pool *pgxpool.Pool) (ok bool, firstBroken int64, err error) {
	res, err := VerifyRange(ctx, pool, 0)
	if err != nil {
		return false, 0, err
	}
	if res.FirstBroken != nil {
		return false, *res.FirstBroken, nil
	}
	return res.Complete, 0, nil
}

// runEvent is a run's audit entry: entity audit_run, the run id.
func runEvent(action string, runID uuid.UUID, actor *uuid.UUID, requestID string, payload map[string]any) Event {
	e := Event{Action: action, Entity: "audit_run", EntityID: runID.String(), RequestID: requestID, Payload: payload}
	if actor != nil {
		e.ActorID = actor.String()
	}
	return e
}

// StartRun starts a run as mhai_platform: the schedule's (by nil), or a
// person's. It returns the running run's id, and started false when one
// was already running (spec §5.4: POST /verify returns the running one).
func StartRun(ctx context.Context, pool *pgxpool.Pool, by *uuid.UUID) (runID uuid.UUID, started bool, err error) {
	err = db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		runID, started, err = StartRunTx(ctx, tx, by, "")
		return err
	})
	return runID, started, err
}

// StartRunTx is StartRun inside the caller's mhai_platform transaction
// (authz.Tx for a platform principal). First it closes a run left running
// past staleAfter (its core died) as error timed_out. Then it inserts a
// running run; on a conflict with the one running run (the unique index,
// so two parallel starts make one run) it returns that run's id. A new run
// writes audit.verify_started {run_id, by} in the same transaction; call
// it last in tx (the chain lock is held until commit).
func StartRunTx(ctx context.Context, tx pgx.Tx, by *uuid.UUID, requestID string) (runID uuid.UUID, started bool, err error) {
	if err := closeStale(ctx, tx, requestID); err != nil {
		return uuid.Nil, false, err
	}
	// A run that finished between the conflict and the read leaves none
	// running: try again, a few times.
	for range 3 {
		var who any
		if by != nil {
			who = *by
		}
		err := tx.QueryRow(ctx, `INSERT INTO audit_verify_runs ("by") VALUES ($1) ON CONFLICT DO NOTHING RETURNING id`, who).Scan(&runID)
		if err == nil {
			kind := "schedule"
			if by != nil {
				kind = "person"
			}
			if err := Write(ctx, tx, runEvent("audit.verify_started", runID, by, requestID,
				map[string]any{"run_id": runID.String(), "by": kind})); err != nil {
				return uuid.Nil, false, err
			}
			return runID, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, fmt.Errorf("audit: start run: %w", err)
		}
		err = tx.QueryRow(ctx, `SELECT id FROM audit_verify_runs WHERE state = 'running'`).Scan(&runID)
		if err == nil {
			return runID, false, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, fmt.Errorf("audit: running run: %w", err)
		}
	}
	return uuid.Nil, false, errors.New("audit: start run: no run started or running")
}

// closeStale closes every run left running past staleAfter (its core
// died mid-walk) as error timed_out, with its audit.verify_finished, in tx.
// Its finished_at is at least staleAfter after its start, which is how the
// schedule tells such a run from a walk that spent its budget.
func closeStale(ctx context.Context, tx pgx.Tx, requestID string) error {
	rows, err := tx.Query(ctx, `
		UPDATE audit_verify_runs SET state = 'error', error_code = 'timed_out', finished_at = now(), version = version + 1
		WHERE state = 'running' AND started_at < now() - make_interval(secs => $1)
		RETURNING id`, staleAfter.Seconds())
	if err != nil {
		return fmt.Errorf("audit: stale runs: %w", err)
	}
	stale, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("audit: stale runs: %w", err)
	}
	for _, id := range stale {
		if err := Write(ctx, tx, runEvent("audit.verify_finished", id, nil, requestID,
			map[string]any{"run_id": id.String(), "state": "error", "error_code": "timed_out"})); err != nil {
			return err
		}
	}
	return nil
}

// outcome is how a walk ends: a state, and for error its code.
func outcome(res Result, walkErr error) (state, code string) {
	switch {
	case errors.Is(walkErr, ErrBudget):
		return "error", "timed_out"
	case walkErr != nil:
		var pg *pgconn.PgError
		if errors.As(walkErr, &pg) {
			return "error", "unexpected_reply"
		}
		return "error", "unreachable"
	case res.FirstBroken != nil:
		return "broken", ""
	case res.Complete && res.EntriesChecked > 0 && res.ToAt != nil && res.ToSeq > 0:
		// from_first: the walk always starts at the first entry (no
		// signed checkpoints yet).
		return "intact", ""
	default:
		// An empty chain, or a walk that stopped short with no error:
		// never intact (C24).
		return "error", "unexpected_reply"
	}
}

// Finish ends a running run with res (or walkErr) and writes
// audit.verify_finished, in one transaction. A run that is no longer
// running (closed as stale meanwhile) is left as it is. It returns the
// state written, or "" when the run had already finished. The
// audit_chain_broken gauge is refreshed after.
func Finish(ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID, res Result, walkErr error) (string, error) {
	state, code := outcome(res, walkErr)
	var nullable = func(v int64) any {
		if v == 0 {
			return nil
		}
		return v
	}
	written := ""
	err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		var tag pgconn.CommandTag
		var err error
		payload := map[string]any{"run_id": runID.String(), "state": state, "entries_checked": res.EntriesChecked}
		switch state {
		case "intact":
			tag, err = tx.Exec(ctx, `
				UPDATE audit_verify_runs SET state = 'intact', finished_at = now(), entries_checked = $2,
				       from_first = true, to_seq = $3, to_at = $4, version = version + 1
				WHERE id = $1 AND state = 'running'`, runID, res.EntriesChecked, res.ToSeq, res.ToAt)
		case "broken":
			tag, err = tx.Exec(ctx, `
				UPDATE audit_verify_runs SET state = 'broken', finished_at = now(), entries_checked = $2,
				       from_first = true, to_seq = $3, to_at = $4,
				       first_broken_seq = $5, first_broken_id = $6, first_broken_at = $7, version = version + 1
				WHERE id = $1 AND state = 'running'`, runID, res.EntriesChecked, nullable(res.ToSeq), res.ToAt,
				*res.FirstBroken, res.FirstBrokenID, res.FirstBrokenAt)
			payload["first_broken_seq"] = *res.FirstBroken
		default:
			tag, err = tx.Exec(ctx, `
				UPDATE audit_verify_runs SET state = 'error', finished_at = now(), entries_checked = $2,
				       error_code = $3, version = version + 1
				WHERE id = $1 AND state = 'running'`, runID, res.EntriesChecked, code)
			payload["error_code"] = code
		}
		if err != nil {
			return fmt.Errorf("audit: finish run: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return nil
		}
		written = state
		return Write(ctx, tx, runEvent("audit.verify_finished", runID, nil, "", payload))
	})
	if err != nil {
		return "", err
	}
	if rerr := RefreshBrokenGauge(ctx, pool); rerr != nil && state == "broken" {
		// The database said broken a moment ago: alert even if the
		// re-read failed.
		telemetry.SetAuditChainBroken(true)
	}
	return written, nil
}

// Execute walks the chain for runID within budget and finishes the run.
// The finish has its own short deadline, so a spent budget is still
// recorded.
func Execute(ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID, budget time.Duration) (string, error) {
	res, walkErr := VerifyRange(ctx, pool, budget)
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return Finish(fctx, pool, runID, res, walkErr)
}

// RefreshBrokenGauge sets audit_chain_broken from the database: 1 while
// a failure is open (OpenFailure), else 0.
func RefreshBrokenGauge(ctx context.Context, pool *pgxpool.Pool) error {
	var open bool
	err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		id, err := OpenFailure(ctx, tx)
		open = id != uuid.Nil
		return err
	})
	if err != nil {
		return err
	}
	telemetry.SetAuditChainBroken(open)
	return nil
}

// --- the daily schedule ------------------------------------------------------

// ScheduleAt is the daily check's time, UTC (spec §5.4).
const ScheduleAt = "06:00"

// scheduleLock is the advisory lock that keeps the schedule to one core
// copy (C25).
const scheduleLock = "mhai.audit.verify"

// Scheduler starts the daily run at At (UTC, "HH:MM", by the database's
// clock). Each Tick at or after today's At, under pg_try_advisory_lock(hashtext('mhai.audit.verify')),
// starts the run unless a scheduled run already started since then, and
// walks it. A core that missed At (it was down) runs the check when it
// comes back.
type Scheduler struct {
	Pool   *pgxpool.Pool
	At     string
	Budget time.Duration    // 0: VerifyBudget
	Now    func() time.Time // nil: the database's clock (tests fix it)
	Every  time.Duration    // 0: a minute
	Log    *slog.Logger
}

// RunScheduler runs the daily check at at ("06:00", UTC) until ctx ends.
func RunScheduler(ctx context.Context, pool *pgxpool.Pool, at string, log *slog.Logger) error {
	return Scheduler{Pool: pool, At: at, Log: log}.Run(ctx)
}

func parseAt(at string) (h, m int, err error) {
	t, err := time.Parse("15:04", at)
	if err != nil || t.Format("15:04") != at {
		return 0, 0, fmt.Errorf("audit: schedule time %q is not HH:MM", at)
	}
	return t.Hour(), t.Minute(), nil
}

func (s Scheduler) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Run ticks every s.Every until ctx ends. It first sets the
// audit_chain_broken gauge from the database, so a restart doesn't clear
// an open failure's alert.
func (s Scheduler) Run(ctx context.Context) error {
	if _, _, err := parseAt(s.At); err != nil {
		return err
	}
	if err := RefreshBrokenGauge(ctx, s.Pool); err != nil && ctx.Err() == nil {
		s.log().Error("audit: chain gauge not read", "reason", "internal_error")
	}
	every := s.Every
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, _, err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.log().Error("audit: scheduled check failed", "reason", "internal_error")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick runs on every tick, on every core copy: it first refreshes the
// audit_chain_broken gauge from the database (so every copy converges on
// the open failure, whichever copy saw the break; a failed read keeps the
// last value, so a 1 holds) and closes runs left running by a core that
// died. Then, if today's run is due and no copy has started it, it starts
// and walks it. Due is computed by the database's clock, not this
// process's: today's At in UTC. state is the finished run's state, "" when
// nothing ran.
func (s Scheduler) Tick(ctx context.Context) (runID uuid.UUID, state string, err error) {
	if _, _, err := parseAt(s.At); err != nil {
		return uuid.Nil, "", err
	}
	if err := RefreshBrokenGauge(ctx, s.Pool); err != nil && ctx.Err() == nil {
		s.log().Error("audit: chain gauge not read", "reason", "internal_error")
	}
	if err := db.WithPlatform(ctx, s.Pool, func(tx pgx.Tx) error { return closeStale(ctx, tx, "") }); err != nil {
		return uuid.Nil, "", err
	}
	// now is the database's, unless a test fixes it.
	var now any
	if s.Now != nil {
		now = s.Now().UTC()
	}
	var isDue bool
	var due time.Time
	if err := db.WithPlatform(ctx, s.Pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			WITH c AS (SELECT coalesce($2::timestamptz, now()) AS t),
			     d AS (SELECT t, (date_trunc('day', t AT TIME ZONE 'UTC') + $1::time) AT TIME ZONE 'UTC' AS due FROM c)
			SELECT t >= due, due FROM d`, s.At, now).Scan(&isDue, &due)
	}); err != nil {
		return uuid.Nil, "", fmt.Errorf("audit: schedule: %w", err)
	}
	if !isDue {
		return uuid.Nil, "", nil
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("audit: schedule: %w", err)
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, scheduleLock).Scan(&locked); err != nil {
		return uuid.Nil, "", fmt.Errorf("audit: schedule lock: %w", err)
	}
	if !locked {
		return uuid.Nil, "", nil
	}
	var started bool
	err = func() error {
		// Unlock on a context of its own: the session lock must not
		// outlive this tick on a pooled connection.
		defer func() {
			uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock(hashtext($1))`, scheduleLock); err != nil {
				// A lock that may still be held: drop the connection.
				_ = conn.Conn().Close(uctx)
			}
		}()
		return db.WithPlatform(ctx, s.Pool, func(tx pgx.Tx) error {
			var done bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM audit_verify_runs WHERE "by" IS NULL AND started_at >= $1
				               AND NOT (state = 'error' AND error_code = 'timed_out'
				                        AND finished_at >= started_at + make_interval(secs => $2)))`,
				due, staleAfter.Seconds()).Scan(&done); err != nil {
				return fmt.Errorf("audit: schedule: %w", err)
			}
			if done {
				return nil
			}
			var err error
			runID, started, err = StartRunTx(ctx, tx, nil, "")
			return err
		})
	}()
	if err != nil || !started {
		return uuid.Nil, "", err
	}
	budget := s.Budget
	if budget <= 0 {
		budget = VerifyBudget
	}
	state, err = Execute(ctx, s.Pool, runID, budget)
	return runID, state, err
}
