package analysis

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EngineVersion is recorded on every period frozen by this build.
const EngineVersion = "1"

// Every is how often the nightly run wakes; each hospital runs once per
// hospital day, after its local midnight plus 30 minutes.
const Every = 5 * time.Minute

// nightlyLock keeps the run to one core copy.
const nightlyLock = "mhai.analysis.nightly"

// AlertFreezeFailed is the alert a failed step raises (design §12).
const AlertFreezeFailed = "analysis_freeze_failed"

// Due is a hospital the nightly run serves now.
type Due struct {
	Hospital      uuid.UUID
	On, Demo      bool
	IngestedUntil *time.Time
}

// Store is the nightly run's database, as mhai_analysis_job.
type Store interface {
	Due(ctx context.Context, now time.Time) ([]Due, error)
	// Ingest reads the source in (after, until], builds the facts with
	// build and writes them, in one transaction; it returns the late count.
	Ingest(ctx context.Context, hospital uuid.UUID, after *time.Time, until time.Time,
		build func([]SourceRow) ([]byte, error)) (int, error)
	Sweep(ctx context.Context, hospital uuid.UUID, now time.Time) error
	Freeze(ctx context.Context, hospital uuid.UUID, now time.Time) (int, error)
	Ran(ctx context.Context, hospital uuid.UUID, now time.Time) error
}

// Clock is when the run acts. In production it is the database's now()
// (DBClock), so "finished" is the freeze function's; tests pin it.
type Clock interface {
	Now(ctx context.Context) (time.Time, error)
}

// Nightly is the analysis run (design §11 steps 1, 2 and 4): per due
// hospital, ingest (switched on and not a demo), sweep, then freeze
// (switched on). It logs the hospital id, the step and a fixed reason only.
type Nightly struct {
	Store Store
	Keys  *Keys // nil: a signed-off hospital with rows to ingest fails closed
	Clock Clock
	Log   *slog.Logger
	Every time.Duration // 0: Every
	// Lock holds the one-copy lock for a tick. NewNightly sets
	// pg_try_advisory_lock on the pool.
	Lock func(ctx context.Context) (unlock func(), ok bool, err error)

	// After, when set, runs once per tick after the hospitals' steps (the
	// clearance letters' ending and reminders, internal/analysis/letters):
	// a failure is logged and alerted as the step "letters".
	After func(ctx context.Context, now time.Time) error

	LateFacts atomic.Int64 // facts that arrived after their block froze
	Failures  atomic.Int64 // failed steps (the analysis_freeze_failed alert)
}

// NewNightly is the production run on pool (the core login's pool; every
// statement runs as mhai_analysis_job).
func NewNightly(pool *pgxpool.Pool, keys *Keys, log *slog.Logger) *Nightly {
	return &Nightly{Store: &DBStore{Pool: pool}, Keys: keys, Clock: DBClock{Pool: pool}, Log: log,
		Lock: advisoryLock(pool, nightlyLock)}
}

func (n *Nightly) log() *slog.Logger {
	if n.Log != nil {
		return n.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Run ticks until ctx ends.
func (n *Nightly) Run(ctx context.Context) error {
	every := n.Every
	if every <= 0 {
		every = Every
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := n.Tick(ctx); err != nil && ctx.Err() == nil {
			n.log().Error("analysis: nightly run incomplete", "reason", "internal_error")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

var errStep = errors.New("analysis: a nightly step failed")

// Tick runs every due hospital, if this copy holds the lock, and returns
// the hospitals whose run finished. A failed hospital is logged, alerted
// and retried at the next wake; the others still run.
func (n *Nightly) Tick(ctx context.Context) ([]uuid.UUID, error) {
	unlock, ok, err := n.Lock(ctx)
	if err != nil || !ok {
		return nil, err
	}
	defer unlock()
	now, err := n.Clock.Now(ctx)
	if err != nil {
		return nil, err
	}
	due, err := n.Store.Due(ctx, now)
	if err != nil {
		n.fail(uuid.Nil, "due")
		return nil, errStep
	}
	var done []uuid.UUID
	var failed bool
	for _, d := range due {
		if step := n.runHospital(ctx, d, now); step != "" {
			n.fail(d.Hospital, step)
			failed = true
			continue
		}
		done = append(done, d.Hospital)
	}
	if n.After != nil {
		if err := n.After(ctx, now); err != nil {
			n.fail(uuid.Nil, "letters")
			failed = true
		}
	}
	if failed {
		return done, errStep
	}
	return done, nil
}

// runHospital runs one hospital's steps in order and returns the step that
// failed, or "".
func (n *Nightly) runHospital(ctx context.Context, d Due, now time.Time) string {
	if d.On && !d.Demo {
		late, err := n.Store.Ingest(ctx, d.Hospital, d.IngestedUntil, now, func(rows []SourceRow) ([]byte, error) {
			return buildIngest(n.Keys, d.Hospital, rows)
		})
		if err != nil {
			return "ingest"
		}
		n.LateFacts.Add(int64(late))
	}
	if err := n.Store.Sweep(ctx, d.Hospital, now); err != nil {
		return "sweep"
	}
	if d.On {
		if _, err := n.Store.Freeze(ctx, d.Hospital, now); err != nil {
			return "freeze"
		}
	}
	if err := n.Store.Ran(ctx, d.Hospital, now); err != nil {
		return "ran"
	}
	return ""
}

// fail logs a step's failure with ids and fixed words only: never the
// error, which may hold a code, a day or a count.
func (n *Nightly) fail(hospital uuid.UUID, step string) {
	n.Failures.Add(1)
	n.log().Error("analysis: nightly step failed", "hospital_id", hospital.String(), "step", step,
		"reason", "internal_error", "alert", AlertFreezeFailed)
}
