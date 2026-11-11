// Package schedtest is an in-memory scheduler.Source for tests: the freeze
// function's rules (finished, eligible, within reach, children frozen,
// once per period) over counts a test sets. It holds synthetic data only.
package schedtest

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/scheduler"
)

// Fake is the source. Live holds each week's counts as the clinical tables
// would give them now, per hospital (a hospital with a row there is listed
// in the week); a test may change them after a week froze (a late row).
type Fake struct {
	mu        sync.Mutex
	Clock     time.Time
	EpochAt   time.Time
	Hosp      []scheduler.Hospital
	StaffN    map[uuid.UUID]int64
	Live      map[string]map[uuid.UUID]scheduler.FrozenRow // week key → hospital → counts
	frozen    map[string]map[uuid.UUID]scheduler.FrozenRow
	stored    map[string]scheduler.Stored
	Freezes   []string // the order periods were frozen in
	Inserts   []string // the order periods were stored in
	FailOnce  map[string]error
	FailFrozn error
}

// New is an empty source at clock with the epoch.
func New(clock, epoch time.Time) *Fake {
	return &Fake{Clock: clock, EpochAt: epoch, StaffN: map[uuid.UUID]int64{}, Live: map[string]map[uuid.UUID]scheduler.FrozenRow{},
		frozen: map[string]map[uuid.UUID]scheduler.FrozenRow{}, stored: map[string]scheduler.Stored{}, FailOnce: map[string]error{}}
}

// Key is a period's key, as build uses it.
func Key(p calendar.Period) string { return string(p.Kind) + " " + p.From.Format(time.DateOnly) }

// Set puts hospital h's counts for week w.
func (f *Fake) Set(w calendar.Period, h uuid.UUID, r scheduler.FrozenRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Live[Key(w)] == nil {
		f.Live[Key(w)] = map[uuid.UUID]scheduler.FrozenRow{}
	}
	r.Kind, r.From, r.Hospital = calendar.Week, w.From, h
	f.Live[Key(w)][h] = r
}

func (f *Fake) Freeze(_ context.Context, p calendar.Period) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.frozen[Key(p)]; ok {
		return nil
	}
	if !p.Finished(f.Clock) || p.From.Before(f.EpochAt) || p.From.Before(f.Clock.Add(-scheduler.Reach)) {
		return scheduler.ErrNotFreezable
	}
	out := map[uuid.UUID]scheduler.FrozenRow{}
	if p.Kind == calendar.Week {
		for h, r := range f.Live[Key(p)] {
			out[h] = r
		}
	} else {
		for _, c := range calendar.Children(p) {
			rows, ok := f.frozen[Key(c)]
			if !ok {
				return scheduler.ErrNotFreezable
			}
			for h, r := range rows {
				s := out[h]
				for i := range s.QG {
					s.QG[i] += r.QG[i]
				}
				s.R[0] += r.R[0]
				s.R[1] += r.R[1]
				s.Clinicians = max(s.Clinicians, r.Clinicians)
				s.Decided += r.Decided
				if s.Median == nil {
					s.Median = r.Median
				}
				s.Kind, s.From, s.Hospital = p.Kind, p.From, h
				out[h] = s
			}
		}
	}
	f.frozen[Key(p)] = out
	f.Freezes = append(f.Freezes, Key(p))
	return nil
}

func (f *Fake) Frozen(_ context.Context, q calendar.Period) ([]scheduler.FrozenRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailFrozn != nil {
		return nil, f.FailFrozn
	}
	var out []scheduler.FrozenRow
	for _, rows := range f.frozen {
		for _, r := range rows {
			if q.Contains(calendar.Of(r.Kind, r.From)) {
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hospital.String() < out[j].Hospital.String() })
	return out, nil
}

func (f *Fake) Hospitals(context.Context) ([]scheduler.Hospital, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]scheduler.Hospital{}, f.Hosp...), nil
}

func (f *Fake) Staff(context.Context) (map[uuid.UUID]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[uuid.UUID]int64{}
	for k, v := range f.StaffN {
		out[k] = v
	}
	return out, nil
}

func (f *Fake) Stored(_ context.Context, q calendar.Period) ([]scheduler.Stored, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []scheduler.Stored
	for _, s := range f.stored {
		if q.Contains(s.Period) {
			out = append(out, s)
		}
	}
	return out, nil
}

// ErrInsert is a failure a test asks for.
var ErrInsert = errors.New("schedtest: insert failed")

func (f *Fake) Insert(_ context.Context, p calendar.Period, state string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.FailOnce[Key(p)]; ok {
		delete(f.FailOnce, Key(p))
		return err
	}
	if _, ok := f.stored[Key(p)]; ok {
		return nil
	}
	if !p.Finished(f.Clock) || p.From.Before(f.EpochAt) {
		return errors.New("schedtest: the insert policy refuses the period")
	}
	f.stored[Key(p)] = scheduler.Stored{Period: p, State: state, Body: append([]byte(nil), body...)}
	f.Inserts = append(f.Inserts, Key(p))
	return nil
}

func (f *Fake) Epoch(context.Context) (time.Time, error) { return f.EpochAt, nil }

func (f *Fake) Now(context.Context) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Clock, nil
}

func (f *Fake) StoredKeys(context.Context, time.Time) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for k := range f.stored {
		out[k] = true
	}
	return out, nil
}

// Snapshot is a stored snapshot, if any.
func (f *Fake) Snapshot(p calendar.Period) (scheduler.Stored, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.stored[Key(p)]
	return s, ok
}

// All is every stored snapshot.
func (f *Fake) All() []scheduler.Stored {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []scheduler.Stored
	for _, s := range f.stored {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return Key(out[i].Period) < Key(out[j].Period) })
	return out
}

// NoLock is a Scheduler.Lock that always succeeds.
func NoLock(context.Context) (func(), bool, error) { return func() {}, true, nil }
