// Package emrmaint runs the EMR purges and retention from core (EMR design
// §10.6, §14.2; plan departure 9, Task 40). Every 5 minutes one core copy
// (the holder of a session advisory lock) visits each hospital with EMR
// data in transactions of its own, as mhai_emr_maint with the hospital set:
//
//  1. held purge requests go back to pending once the hospital's legal
//     hold is lifted (emr_release_holds), audited
//     emr.exclusion_purge_released;
//  2. every pending request is purged (emr_purge_patient), audited
//     emr.exclusion_purged with counts, or held again if a hold was
//     placed meanwhile (emr.exclusion_purge_held). The loop reads durable
//     rows, never a job message, so a lost message can't drop a purge;
//  3. requests held for more than 90 days are counted and logged (ids
//     only) so they aren't forgotten;
//  4. once a day from 03:00 UTC, when the retention sweep is switched on,
//     emr_retention_sweep(10000) until it reports no more, each pass
//     audited emr.retention_swept with its counts.
//
// The database decides every hold: nothing is deleted for a hospital on
// hold, whatever this loop asks. Logs carry ids, codes and counts only;
// database errors by SQLSTATE.
package emrmaint

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

const (
	// Every is how often the leader runs.
	Every = 5 * time.Minute
	// SweepHourUTC is the hour from which the day's retention sweep runs.
	SweepHourUTC = 3
	// SweepCap is the sweep's cap per step and pass.
	SweepCap = 10000
	// maxSweepPasses bounds one day's sweep of one hospital.
	maxSweepPasses = 100
	// purgeBatch is how many pending requests one hospital's visit takes.
	purgeBatch = 100
	// maxReleasedIDs is the most request ids one audit row names.
	maxReleasedIDs = 500
	// role is the database role the loop works as.
	role = "mhai_emr_maint"
	// lockName keeps the loop to one core copy.
	lockName = "mhai.emr.maint"
)

// Metrics are the loop's in-process counts.
type Metrics struct {
	Purged      atomic.Int64 // requests purged
	Held        atomic.Int64 // requests found under a hold when due
	Released    atomic.Int64 // held requests released after a lift
	SweepPasses atomic.Int64 // retention passes run
	HeldOverdue atomic.Int64 // requests held over 90 days, at the last run (a gauge)
	Failures    atomic.Int64 // failed steps (page when it rises)
}

// Loop is the maintenance loop. Run runs it as the leader.
type Loop struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	// Every is the period (Every when 0).
	Every time.Duration
	// Retention switches the nightly retention sweep on
	// (CORE_EMR_RETENTION_SWEEP; off by default until the privacy lead
	// confirms the values, H5). Purges run either way.
	Retention bool
	Metrics   *Metrics
	// Now is the clock (time.Now when nil).
	Now func() time.Time
	// Lock replaces the advisory lock (tests). nil: pg_try_advisory_lock.
	Lock func(ctx context.Context) (unlock func(), ok bool, err error)

	sweptOn string // the UTC day this copy last finished the sweep
}

func (l *Loop) log() *slog.Logger {
	if l.Log != nil {
		return l.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (l *Loop) metrics() *Metrics {
	if l.Metrics == nil {
		l.Metrics = &Metrics{}
	}
	return l.Metrics
}

func (l *Loop) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

var errNoPool = errors.New("emrmaint: no pool")

// Run ticks until ctx ends.
func (l *Loop) Run(ctx context.Context) error {
	if l.Pool == nil {
		return errNoPool
	}
	every := l.Every
	if every <= 0 {
		every = Every
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := l.Tick(ctx); err != nil && ctx.Err() == nil {
			l.metrics().Failures.Add(1)
			l.log().Error("emrmaint: run failed", "reason", "internal_error")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick runs once if this copy takes the lock; false when another leads.
func (l *Loop) Tick(ctx context.Context) (bool, error) {
	unlock, ok, err := l.lock(ctx)
	if err != nil || !ok {
		return false, err
	}
	defer unlock()
	return true, l.RunOnce(ctx)
}

func (l *Loop) lock(ctx context.Context) (func(), bool, error) {
	if l.Lock != nil {
		return l.Lock(ctx)
	}
	conn, err := l.Pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, lockName).Scan(&locked); err != nil || !locked {
		conn.Release()
		return nil, false, err
	}
	return func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock(hashtext($1))`, lockName); err != nil {
			_ = conn.Conn().Close(uctx) // a lock that may still be held: drop the connection
		}
		conn.Release()
	}, true, nil
}

// RunOnce does one run without the leader lock (Tick holds it). A failure
// in one hospital is counted and logged, and the others still run.
func (l *Loop) RunOnce(ctx context.Context) error {
	if l.Pool == nil {
		return errNoPool
	}
	hospitals, err := l.hospitals(ctx)
	if err != nil {
		l.fail("hospitals", uuid.Nil, err)
		return err
	}
	now := l.now()
	day := now.Format(time.DateOnly)
	sweep := l.Retention && now.Hour() >= SweepHourUTC && l.sweptOn != day
	var overdue int64
	var failed bool
	for _, h := range hospitals {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := l.release(ctx, h); err != nil {
			l.fail("release", h, err)
			failed = true
		}
		if err := l.purgeDue(ctx, h); err != nil {
			l.fail("purge", h, err)
			failed = true
		}
		n, err := l.overdue(ctx, h)
		if err != nil {
			l.fail("overdue", h, err)
			failed = true
		}
		overdue += n
		if sweep {
			if err := l.sweep(ctx, h); err != nil {
				l.fail("sweep", h, err)
				failed = true
			}
		}
	}
	l.metrics().HeldOverdue.Store(overdue)
	if sweep && !failed {
		l.sweptOn = day
	}
	return nil
}

// fail counts and logs a failed step: the step, the hospital id and the
// SQLSTATE, never the error's text.
func (l *Loop) fail(step string, h uuid.UUID, err error) {
	l.metrics().Failures.Add(1)
	attrs := []any{"step", step, "reason", "internal_error"}
	if h != uuid.Nil {
		attrs = append(attrs, "hospital", h.String())
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		attrs = append(attrs, "sqlstate", pe.Code)
	}
	l.log().Error("emrmaint: step failed", attrs...)
}

// inHospital runs fn as the loop's role with hospital h set (h nil: none).
func (l *Loop) inHospital(ctx context.Context, h uuid.UUID, fn func(pgx.Tx) error) error {
	tx, err := l.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
		return err
	}
	if h != uuid.Nil {
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, h.String()); err != nil {
			return err
		}
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (l *Loop) hospitals(ctx context.Context) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := l.inHospital(ctx, uuid.Nil, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT h FROM emr_maint_hospitals() h ORDER BY 1`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	return out, err
}

func (l *Loop) release(ctx context.Context, h uuid.UUID) error {
	return l.inHospital(ctx, h, func(tx pgx.Tx) error {
		var ids []uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT emr_release_holds()`).Scan(&ids); err != nil {
			return err
		}
		for i := 0; i < len(ids); i += maxReleasedIDs {
			part := ids[i:min(i+maxReleasedIDs, len(ids))]
			list := make([]any, len(part))
			for j, id := range part {
				list[j] = id.String()
			}
			if err := audit.Write(ctx, tx, audit.Event{HospitalID: h.String(), Action: "emr.exclusion_purge_released",
				Entity: "hospital", EntityID: h.String(),
				Payload: map[string]any{"request_ids": list, "count": len(part)}}); err != nil {
				return err
			}
		}
		l.metrics().Released.Add(int64(len(ids)))
		return nil
	})
}

func (l *Loop) purgeDue(ctx context.Context, h uuid.UUID) error {
	var ids []uuid.UUID
	if err := l.inHospital(ctx, h, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT r FROM emr_pending_purges($1) r`, purgeBatch)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	}); err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := l.purge(ctx, h, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// purgeCounts are the counts a purge returns, audited as they are.
var purgeCounts = []string{"observations", "medications", "encounters", "raw_records", "matches", "findings", "alerts"}

// purge runs one request and audits what happened, in one transaction.
func (l *Loop) purge(ctx context.Context, h, id uuid.UUID) error {
	return l.inHospital(ctx, h, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT emr_purge_patient($1)`, id).Scan(&raw); err != nil {
			return err
		}
		var res map[string]any
		if err := json.Unmarshal(raw, &res); err != nil {
			return err
		}
		pid, _ := res["patient_id"].(string)
		if _, err := uuid.Parse(pid); err != nil {
			return errors.New("emrmaint: purge result without a patient")
		}
		switch res["state"] {
		case "done":
			payload := map[string]any{"request_id": id.String(), "patient_id": pid, "via": "loop"}
			for _, k := range purgeCounts {
				n, ok := res[k].(float64)
				if !ok {
					return errors.New("emrmaint: purge result without its counts")
				}
				payload[k] = int64(n)
			}
			if err := audit.Write(ctx, tx, audit.Event{HospitalID: h.String(), Action: "emr.exclusion_purged",
				Entity: "purge_request", EntityID: id.String(), Payload: payload}); err != nil {
				return err
			}
			l.metrics().Purged.Add(1)
		case "held":
			if err := audit.Write(ctx, tx, audit.Event{HospitalID: h.String(), Action: "emr.exclusion_purge_held",
				Entity: "purge_request", EntityID: id.String(),
				Payload: map[string]any{"request_id": id.String(), "patient_id": pid, "via": "loop"}}); err != nil {
				return err
			}
			l.metrics().Held.Add(1)
		}
		return nil
	})
}

// overdue counts the hospital's requests held over 90 days and logs their
// ids (design §10.6: listed to the operators).
func (l *Loop) overdue(ctx context.Context, h uuid.UUID) (int64, error) {
	var ids []uuid.UUID
	err := l.inHospital(ctx, h, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT request_id FROM emr_held_overdue()`)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	list := make([]string, len(ids))
	for i, id := range ids {
		list[i] = id.String()
	}
	l.log().Warn("emrmaint: purge requests held over 90 days", "hospital", h.String(), "requests", list, "count", len(ids))
	return int64(len(ids)), nil
}

// sweepCounts are the counts a sweep pass returns, audited as they are.
var sweepCounts = []string{"raw_payloads", "findings", "observations", "medications", "encounters", "raw_records",
	"alert_patients", "alerts", "consumed", "patients"}

// sweep runs the hospital's retention passes until one reports no more.
func (l *Loop) sweep(ctx context.Context, h uuid.UUID) error {
	for i := 0; i < maxSweepPasses; i++ {
		more := false
		err := l.inHospital(ctx, h, func(tx pgx.Tx) error {
			var raw []byte
			if err := tx.QueryRow(ctx, `SELECT emr_retention_sweep($1)`, SweepCap).Scan(&raw); err != nil {
				return err
			}
			var res map[string]any
			if err := json.Unmarshal(raw, &res); err != nil {
				return err
			}
			if res["held"] == true {
				return nil
			}
			more = res["more"] == true
			payload := map[string]any{}
			var total int64
			for _, k := range sweepCounts {
				n, ok := res[k].(float64)
				if !ok {
					return errors.New("emrmaint: sweep result without its counts")
				}
				payload[k] = int64(n)
				total += int64(n)
			}
			l.metrics().SweepPasses.Add(1)
			if total == 0 {
				return nil
			}
			return audit.Write(ctx, tx, audit.Event{HospitalID: h.String(), Action: "emr.retention_swept",
				Entity: "hospital", EntityID: h.String(), Payload: payload})
		})
		if err != nil || !more {
			return err
		}
	}
	return nil
}
