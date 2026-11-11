package scheduler_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/scheduler"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/scheduler/schedtest"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// Plan 7c Task 8: What changed, the scheduler's side, over the fake source.
// The stored bodies are written by hand (all rows only), so each week's
// flags are known. Fixed dates only.

// allOnly is a stored body of p whose all row has q questions and nothing
// else shown.
func allOnly(t *testing.T, p calendar.Period, q int64) []byte {
	t.Helper()
	from, to := analytics.Dates(p)
	h, hs := smallgroup.Guarded{Suppressed: true}, smallgroup.GuardedShare{Suppressed: true}
	r := analytics.Row{Questions: smallgroup.Count(q), Answers: h, Pass: h, Regenerate: h, Escalate: h, Degraded: h,
		PassShare: hs, RegenerateShare: hs, EscalateShare: hs, Reviews: h, AgreementRate: hs, ActiveClinicians: h,
		QuestionsPerClinician: analytics.Ratio{Number: analytics.Number{Suppressed: true}},
		MedianReviewHours:     analytics.Hours{Number: analytics.Number{Suppressed: true}}}
	raw, err := json.Marshal(analytics.Body{Kind: string(p.Kind), From: from, To: to, All: r, Hospitals: []analytics.HospitalRow{}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// february stores January 2026 (56 questions, 12.6 per 7-day week) and the
// four February weeks with the questions given.
func february(t *testing.T, q [4]int64) (*schedtest.Fake, []calendar.Period) {
	t.Helper()
	f := schedtest.New(at(2026, 6, 1), at(2026, 1, 1))
	ctx := context.Background()
	jan := calendar.Of(calendar.Month, at(2026, 1, 1))
	if err := f.Insert(ctx, jan, scheduler.StateOK, allOnly(t, jan, 56)); err != nil {
		t.Fatal(err)
	}
	weeks := calendar.Children(calendar.Of(calendar.Month, at(2026, 2, 1)))
	for i, w := range weeks {
		if err := f.Insert(ctx, w, scheduler.StateOK, allOnly(t, w, q[i])); err != nil {
			t.Fatal(err)
		}
	}
	return f, weeks
}

func openChanges(f *schedtest.Fake) []schedtest.Change {
	var out []schedtest.Change
	for _, c := range f.AllChanges() {
		if c.Closed == nil {
			out = append(out, c)
		}
	}
	return out
}

// A change keeps its id while each week flags it again; it closes when one
// doesn't; it comes back with a new id.
func TestChangeIdStable(t *testing.T) {
	f, weeks := february(t, [4]int64{60, 61, 13, 60})
	b := scheduler.Builder{Src: f}
	ctx := context.Background()
	var ids []string
	for i, w := range weeks {
		if err := b.EvaluateChanges(ctx, w); err != nil {
			t.Fatalf("%v: %v", w, err)
		}
		open := openChanges(f)
		switch i {
		case 2:
			if len(open) != 0 {
				t.Fatalf("%v: 13 against 12.6 a week is no change, but %d are open", w, len(open))
			}
			ids = append(ids, "")
			continue
		}
		if len(open) != 1 || open[0].Flag.Measure != "questions" || open[0].Flag.Direction != "rising" || open[0].Flag.Hospital != nil {
			t.Fatalf("%v: open %+v", w, open)
		}
		ids = append(ids, open[0].ID)
		if !open[0].Last.Equal(w.From) {
			t.Errorf("%v: last week %v", w, open[0].Last)
		}
	}
	if ids[0] != ids[1] {
		t.Errorf("flagged again, a new id: %v", ids)
	}
	if ids[3] == ids[0] || ids[3] == "" {
		t.Errorf("came back with the old id: %v", ids)
	}
	all := f.AllChanges()
	if len(all) != 2 || all[0].Closed == nil || !all[0].Closed.Equal(weeks[2].From) || all[0].Flag.Current.Text() != "61" {
		t.Errorf("the first change: %+v", all)
	}
	// Evaluated once: again is nothing.
	if err := b.EvaluateChanges(ctx, weeks[3]); err != nil || len(f.Evaluated) != 4 {
		t.Errorf("a second evaluation: %v, %v", err, f.Evaluated)
	}
	// An older week after a newer one is refused.
	if err := b.EvaluateChanges(ctx, weeks[1]); !errors.Is(err, scheduler.ErrOutOfOrder) {
		t.Errorf("an older week: %v", err)
	}
}

// A week stored unavailable, or a compared month not stored, flags
// nothing: the open changes close.
func TestChangeOnUnavailableClosesAll(t *testing.T) {
	f, weeks := february(t, [4]int64{60, 61, 13, 60})
	b := scheduler.Builder{Src: f}
	ctx := context.Background()
	if err := b.EvaluateChanges(ctx, weeks[0]); err != nil || len(openChanges(f)) != 1 {
		t.Fatalf("%v, %+v", err, openChanges(f))
	}
	march := calendar.Of(calendar.Week, at(2026, 3, 1))
	if err := f.Insert(ctx, march, scheduler.StateUnavailable, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.EvaluateChanges(ctx, march); err != nil || len(openChanges(f)) != 0 {
		t.Errorf("an unavailable week left changes open: %v %+v", err, openChanges(f))
	}
	// April's first week compares March, which isn't stored: nothing.
	april := calendar.Of(calendar.Week, at(2026, 4, 1))
	if err := f.Insert(ctx, april, scheduler.StateOK, allOnly(t, april, 500)); err != nil {
		t.Fatal(err)
	}
	if err := b.EvaluateChanges(ctx, april); err != nil || len(openChanges(f)) != 0 {
		t.Errorf("no compared month, yet flagged: %v %+v", err, openChanges(f))
	}
	// A week with no stored snapshot isn't evaluated.
	if err := b.EvaluateChanges(ctx, calendar.Next(april)); err == nil {
		t.Error("a week with no snapshot evaluated")
	}
}

// Each wake evaluates every stored week not yet evaluated, in order, once;
// a failed evaluation is retried on the next wake.
func TestSchedulerEvaluatesEachWeekOnce(t *testing.T) {
	f, _ := fixture(at(2026, 12, 1).Add(time.Hour), at(2026, 10, 1))
	f.FailChanges = errors.New("synthetic failure")
	s := sched(f, nil)
	if _, err := s.Tick(context.Background()); err == nil {
		t.Fatal("a failed evaluation reported no error")
	}
	if len(f.Evaluated) != 0 {
		t.Fatalf("evaluated through a failure: %v", f.Evaluated)
	}
	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	var weeks []string
	for _, st := range f.All() {
		if st.Period.Kind == calendar.Week {
			weeks = append(weeks, schedtest.Key(st.Period))
		}
	}
	if len(weeks) == 0 || len(f.Evaluated) != len(weeks) {
		t.Fatalf("evaluated %v, stored weeks %v", f.Evaluated, weeks)
	}
	for i := 1; i < len(f.Evaluated); i++ {
		if f.Evaluated[i] <= f.Evaluated[i-1] {
			t.Errorf("out of order: %v", f.Evaluated)
		}
	}
	if _, err := s.Tick(context.Background()); err != nil || len(f.Evaluated) != len(weeks) {
		t.Errorf("a third wake evaluated again: %v %v", err, f.Evaluated)
	}
}
