// Package cloudreap settles stale cloud budget reservations (cloud model
// providers plan, failure case 4; migration 00047). A crashed worker, a
// reserve that timed out after core committed, or a settle that failed
// leaves a cloud_reservations row that held its tokens as "reserved" until
// the month ended. Every Every (5 minutes), one core copy (the holder of a
// session advisory lock) lists the hospitals with a reservation older than
// Age, and for each one, as mhai_cloud_reaper with the hospital set, calls
// mhai_cloud_reap: the rows are settled as fully used and each is audited
// cloud.reservation_reaped (its id, month, tokens and age; no person, no
// text) in the same transaction.
//
// The race rule is the database's: a reservation row is consumed once, by
// whichever of the reap and the worker's settle commits first, and the
// other changes nothing. The reap charges the whole reservation, so it
// never frees budget and a late reply that used less is not refunded: it
// fails toward sending less. Logs carry ids, counts and SQLSTATEs only.
package cloudreap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

const (
	// Every is how often the leader runs.
	Every = 5 * time.Minute
	// DefaultAge is how old a reservation is before it is reaped.
	DefaultAge = time.Hour
	// MinAge is the youngest age the reaper takes (the database refuses
	// less too): a younger reservation may still be in flight.
	MinAge = 30 * time.Minute
	// MaxAge bounds the setting (a week): an older age is a mistake.
	MaxAge = 7 * 24 * time.Hour
	// Batch is how many rows one transaction reaps.
	Batch = 100
	// maxPasses bounds one hospital's visit per run.
	maxPasses = 50
	// role is the database role the loop works as.
	role = "mhai_cloud_reaper"
	// lockName keeps the loop to one core copy.
	lockName = "mhai.cloud.reaper"
	// Action is the audit action of one reaped reservation.
	Action = "cloud.reservation_reaped"
)

// ValidAge reports whether d is an age the reaper accepts.
func ValidAge(d time.Duration) bool { return d >= MinAge && d <= MaxAge }

// Metrics are the loop's in-process counts.
type Metrics struct {
	Reaped   atomic.Int64 // reservations settled as fully used
	Tokens   atomic.Int64 // their tokens
	Failures atomic.Int64 // failed steps (alert when it rises)
	Empty    atomic.Int64 // listed hospitals whose reap found nothing (alert when it keeps rising)
}

// Loop is the reaper. Run runs it as the leader.
type Loop struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	// Age is how old a reservation must be (DefaultAge when 0). Run and
	// RunOnce refuse an age outside MinAge..MaxAge.
	Age time.Duration
	// Every is the period (Every when 0).
	Every   time.Duration
	Metrics *Metrics
	// Lock replaces the advisory lock (tests). nil: pg_try_advisory_lock.
	Lock func(ctx context.Context) (unlock func(), ok bool, err error)
}

var (
	errNoPool = errors.New("cloudreap: no pool")
	errAge    = errors.New("cloudreap: the age is outside 30 minutes to 7 days")
)

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

func (l *Loop) age() time.Duration {
	if l.Age == 0 {
		return DefaultAge
	}
	return l.Age
}

// interval is the age as a Postgres interval.
func (l *Loop) interval() pgtype.Interval {
	return pgtype.Interval{Microseconds: l.age().Microseconds(), Valid: true}
}

func (l *Loop) check() error {
	if l.Pool == nil {
		return errNoPool
	}
	if !ValidAge(l.age()) {
		return errAge
	}
	return nil
}

// Run ticks until ctx ends. A pool or age it can't use is refused before
// the first tick.
func (l *Loop) Run(ctx context.Context) error {
	if err := l.check(); err != nil {
		return err
	}
	every := l.Every
	if every <= 0 {
		every = Every
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := l.Tick(ctx); err != nil && ctx.Err() == nil {
			l.fail("run", uuid.Nil, err)
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
// in one hospital is counted and logged, and the others still run; the
// first failure is returned after all of them.
func (l *Loop) RunOnce(ctx context.Context) error {
	if err := l.check(); err != nil {
		return err
	}
	hospitals, err := l.hospitals(ctx)
	if err != nil {
		return err
	}
	var first error
	for _, h := range hospitals {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := l.reapHospital(ctx, h); err != nil {
			l.fail("reap", h, err)
			if first == nil {
				first = err
			}
		}
	}
	return first
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
	l.log().Error("cloudreap: step failed", attrs...)
}

// inHospital runs fn as the reaper's role with hospital h set (h nil: none).
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
		rows, err := tx.Query(ctx, `SELECT h FROM mhai_cloud_stale_hospitals($1) h`, l.interval())
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	if err != nil {
		l.fail("hospitals", uuid.Nil, err)
	}
	return out, err
}

// reaped is one row mhai_cloud_reap returned.
type reaped struct {
	id         uuid.UUID
	month      time.Time
	tokens     int64
	ageSeconds int64
}

// reapHospital reaps h's stale rows, a batch per transaction, until a
// batch comes back short.
//
// A listed hospital whose first batch reaps nothing is logged: a settle
// holding every stale row does that rarely and briefly, but a reap that
// keeps finding nothing is a fault the failure count would not show.
func (l *Loop) reapHospital(ctx context.Context, h uuid.UUID) error {
	for pass := range maxPasses {
		n, err := l.reapBatch(ctx, h)
		if err == nil && n == 0 && pass == 0 {
			l.metrics().Empty.Add(1)
			l.log().Warn("cloudreap: a hospital with stale reservations had none to reap", "hospital", h.String())
		}
		if err != nil || n < Batch {
			return err
		}
	}
	return nil
}

// reapBatch reaps one batch of h's and audits each row, in one
// transaction: a failed audit rolls the reap back. Metrics count after the
// commit only.
func (l *Loop) reapBatch(ctx context.Context, h uuid.UUID) (int, error) {
	var got []reaped
	err := l.inHospital(ctx, h, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT reservation, month, tokens, age_seconds FROM mhai_cloud_reap($1, $2)`, l.interval(), Batch)
		if err != nil {
			return err
		}
		got, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (reaped, error) {
			var r reaped
			err := row.Scan(&r.id, &r.month, &r.tokens, &r.ageSeconds)
			return r, err
		})
		if err != nil {
			return err
		}
		for _, r := range got {
			if err := audit.Write(ctx, tx, audit.Event{HospitalID: h.String(), Action: Action,
				Entity: "cloud_reservation", EntityID: r.id.String(),
				Payload: map[string]any{
					"reservation_id":  r.id.String(),
					"month":           r.month.UTC().Format("2006-01"),
					"reserved_tokens": r.tokens,
					"age_minutes":     r.ageSeconds / 60,
				}}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if len(got) == 0 {
		return 0, nil
	}
	var tokens int64
	ids := make([]string, len(got))
	for i, r := range got {
		tokens += r.tokens
		ids[i] = r.id.String()
	}
	m := l.metrics()
	m.Reaped.Add(int64(len(got)))
	m.Tokens.Add(tokens)
	l.log().Warn("cloudreap: stale reservations settled as fully used", "hospital", h.String(),
		"reservations", ids, "count", len(got), "tokens", tokens)
	return len(got), nil
}
