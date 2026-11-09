package scheduler

// What changed, the scheduler's side (Plan 7c spec §7.3, §8.3 step 2):
// after a new week snapshot is stored, the week is compared with the month
// that finished before it started, from the two stored bodies alone
// (analytics.Changes: shown cells only), and the open changes are opened,
// updated or closed to match in one transaction with the week's run row.
// A week is evaluated once, in calendar order; a wake evaluates every
// stored week newer than the newest run, so a failed evaluation is retried
// on the next wake.

import (
	"context"
	"errors"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// ChangeSource is where What changed is read and written: DB, or a fake.
type ChangeSource interface {
	// ChangesDue is every stored week newer than the newest evaluated one,
	// oldest first.
	ChangesDue(ctx context.Context) ([]calendar.Period, error)
	// ApplyChanges records week w's evaluation against month m and opens,
	// updates and closes the open changes to match flags, in one
	// transaction; nothing when w was evaluated already.
	ApplyChanges(ctx context.Context, w, m calendar.Period, flags []analytics.Flag) error
}

var _ ChangeSource = DB{}

// ErrOutOfOrder is a week older than the newest evaluated one.
var ErrOutOfOrder = errors.New("scheduler: a newer week was evaluated already")

// errNoWeek is a week to evaluate that has no stored snapshot.
var errNoWeek = errors.New("scheduler: the week has no stored snapshot")

// storedBody is p's stored body, if it is stored ok and reads back.
func (b Builder) storedBody(ctx context.Context, p calendar.Period) (analytics.Body, bool, bool, error) {
	rows, err := b.Src.Stored(ctx, calendar.BlockOf(p))
	if err != nil {
		return analytics.Body{}, false, false, err
	}
	for _, s := range rows {
		if !s.Period.Equal(p) {
			continue
		}
		if s.State != StateOK {
			return analytics.Body{}, true, false, nil
		}
		body, err := analytics.DecodeBody(s.Body, p)
		return body, true, err == nil, nil
	}
	return analytics.Body{}, false, false, nil
}

// EvaluateChanges evaluates week w (spec §7.3): the flags come from w's and
// its compared month's stored bodies only. A week stored unavailable, or a
// month not stored, unavailable or unreadable, flags nothing, so every open
// change closes: a change is never flagged on a value that isn't shown.
func (b Builder) EvaluateChanges(ctx context.Context, w calendar.Period) error {
	cs, ok := b.Src.(ChangeSource)
	if !ok {
		return errUnguardable
	}
	m := analytics.ComparedMonth(w)
	week, stored, wok, err := b.storedBody(ctx, w)
	if err != nil {
		return err
	}
	if !stored {
		return errNoWeek
	}
	month, _, mok, err := b.storedBody(ctx, m)
	if err != nil {
		return err
	}
	var flags []analytics.Flag
	if wok && mok {
		flags = analytics.Changes(w, week, m, month)
	}
	return cs.ApplyChanges(ctx, w, m, flags)
}

// evaluateDue evaluates every stored week not yet evaluated, oldest first,
// and stops at the first failure (retried on the next wake).
func (s *Scheduler) evaluateDue(ctx context.Context) error {
	cs, ok := s.Builder.Src.(ChangeSource)
	if !ok {
		return nil
	}
	weeks, err := cs.ChangesDue(ctx)
	if err != nil {
		return err
	}
	for _, w := range weeks {
		if err := s.Builder.EvaluateChanges(ctx, w); err != nil {
			return err
		}
	}
	return nil
}
