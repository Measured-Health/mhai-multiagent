package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// MaxGuardTimeouts is how many times a period's guard may run past its
// budget before the period is stored unavailable (T17).
const MaxGuardTimeouts = 3

// Every is how often the scheduler wakes (spec §8.3).
const Every = 5 * time.Minute

// scheduleLock keeps the scheduler to one core copy (A25).
const scheduleLock = "mhai.analytics.schedule"

// Scheduler builds every finished, eligible period that has no stored
// snapshot, oldest first and week, month, quarter at the same instant
// (spec §8.3). One core copy runs it at a time, under a session advisory
// lock; every copy serves the stored rows.
type Scheduler struct {
	Pool    *pgxpool.Pool // for the advisory lock
	Builder Builder
	Every   time.Duration // 0: Every
	Log     *slog.Logger
	// Lock replaces the advisory lock (tests without a database). nil:
	// pg_try_advisory_lock on Pool.
	Lock func(ctx context.Context) (unlock func(), ok bool, err error)

	mu       sync.Mutex
	timeouts map[string]int // guard timeouts per period, on this copy
}

// Clock is the source's clock: the database's, so the scheduler's idea of
// "finished" is the freeze function's and the insert policy's.
type Clock interface {
	Now(ctx context.Context) (time.Time, error)
	StoredKeys(ctx context.Context, since time.Time) (map[string]bool, error)
}

func (s *Scheduler) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Run ticks until ctx ends.
func (s *Scheduler) Run(ctx context.Context) error {
	every := s.Every
	if every <= 0 {
		every = Every
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.log().Error("analytics: scheduler tick failed", "reason", "internal_error")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Due is every finished period that is eligible (it starts at or after the
// epoch, so all its children do) and not in have, oldest first; periods
// that end together come week, month, quarter.
func Due(epoch, now time.Time, have map[string]bool) []calendar.Period {
	var out []calendar.Period
	w := calendar.Of(calendar.Week, epoch)
	if w.From.Before(epoch) {
		w = calendar.Next(w)
	}
	for ; w.Finished(now); w = calendar.Next(w) {
		ps := []calendar.Period{w}
		m, _ := calendar.Parent(w)
		if m.To.Equal(w.To) && !m.From.Before(epoch) {
			ps = append(ps, m)
			q, _ := calendar.Parent(m)
			if q.To.Equal(m.To) && !q.From.Before(epoch) {
				ps = append(ps, q)
			}
		}
		for _, p := range ps {
			if !have[key(p)] {
				out = append(out, p)
			}
		}
	}
	return out
}

// Tick builds what is due, if this copy holds the lock, then evaluates
// What changed for every stored week not yet evaluated (spec §8.3 step 2).
// The build stops at the first period that fails other than by being
// unavailable, so a parent is never built after a child that will be
// retried. It returns the periods it stored.
func (s *Scheduler) Tick(ctx context.Context) ([]calendar.Period, error) {
	unlock, ok, err := s.lock(ctx)
	if err != nil || !ok {
		return nil, err
	}
	defer unlock()
	done, err := s.build(ctx)
	if eerr := s.evaluateDue(ctx); eerr != nil {
		// A fixed reason; never a figure.
		s.log().Error("analytics: what changed not evaluated", "reason", "internal_error")
		err = errors.Join(err, eerr)
	}
	return done, err
}

// build stores every due period (Tick, under the lock).
func (s *Scheduler) build(ctx context.Context) ([]calendar.Period, error) {
	clock, ok := s.Builder.Src.(Clock)
	if !ok {
		return nil, errUnguardable
	}
	now, err := clock.Now(ctx)
	if err != nil {
		return nil, err
	}
	epoch, err := s.Builder.Src.Epoch(ctx)
	if err != nil {
		return nil, err
	}
	have, err := clock.StoredKeys(ctx, epoch)
	if err != nil {
		return nil, err
	}
	var done []calendar.Period
	for _, p := range Due(epoch, now, have) {
		err := s.Builder.Build(ctx, p)
		if errors.Is(err, ErrGuardTimeout) {
			// T17: no row; retried next wake, until MaxGuardTimeouts.
			s.log().Error("analytics: period guard timed out", "kind", string(p.Kind), "from", p.From.Format(time.DateOnly), "reason", "timeout")
			if s.timedOut(p) < MaxGuardTimeouts {
				return done, err
			}
			if err := s.Builder.MarkUnavailable(ctx, p); err != nil {
				return done, err
			}
			s.log().Error("analytics: period stored unavailable after repeated guard timeouts", "kind", string(p.Kind),
				"from", p.From.Format(time.DateOnly), "reason", "timeout")
			done = append(done, p)
			continue
		}
		if err != nil {
			// The period and a fixed reason; never a count.
			s.log().Error("analytics: period not built", "kind", string(p.Kind), "from", p.From.Format(time.DateOnly), "reason", "internal_error")
			return done, err
		}
		done = append(done, p)
	}
	return done, nil
}

// timedOut counts one more guard timeout of p on this copy and returns the
// count.
func (s *Scheduler) timedOut(p calendar.Period) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timeouts == nil {
		s.timeouts = map[string]int{}
	}
	s.timeouts[key(p)]++
	return s.timeouts[key(p)]
}

func (s *Scheduler) lock(ctx context.Context) (func(), bool, error) {
	if s.Lock != nil {
		return s.Lock(ctx)
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, scheduleLock).Scan(&locked); err != nil || !locked {
		conn.Release()
		return nil, false, err
	}
	return func() {
		// Unlock on a context of its own: the session lock must not outlive
		// this tick on a pooled connection.
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock(hashtext($1))`, scheduleLock); err != nil {
			_ = conn.Conn().Close(uctx) // a lock that may still be held: drop the connection
		}
		conn.Release()
	}, true, nil
}
