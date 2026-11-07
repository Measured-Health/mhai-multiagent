package historytest

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/scheduler"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/scheduler/schedtest"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/platform"
)

// A7, the review's form (Plan 7c spec §6.4, §11.5; Task 4): random
// histories of 1 to 3 years from each hospital's onboarding, the release
// epoch mid-month inside each, fed to the scheduler through the fake
// source, a wake at every week's end plus the grace. The JSON every route
// would send (GET /platform/guardrails/periods on week, month and quarter,
// GET /platform/guardrails on the deprecated 7d, 30d and 90d; until 7c-2 builds the analytics routes, the stored bodies stand
// in for them: they hold every cell any analytics route sends) is parsed by
// this package alone. The exact
// solver over all of it exposes no hidden cell of 1 to 4 and no
// hidden-member sum of 1 to 4; no period is unavailable; ineligible weeks,
// months and quarters are in no response.

// historyCount is the number of histories: fewer under -race or -short.
func historyCount() int {
	switch {
	case testing.Short():
		return 8
	case raceEnabled:
		return 15
	}
	return 100
}

type world struct {
	weeks   []calendar.Period
	epoch   time.Time
	onboard map[string]time.Time
	fake    *schedtest.Fake
	truth   map[string]map[string]scheduler.FrozenRow // week key → hospital → counts
}

func count(r *rand.Rand) int64 {
	switch r.Intn(4) {
	case 0:
		return int64(r.Intn(5))
	case 1:
		return int64(r.Intn(8))
	case 2:
		return int64(r.Intn(15))
	}
	return int64(r.Intn(60))
}

func randomWorld(r *rand.Rand) *world {
	start := time.Date(2024+r.Intn(2), time.Month(1+3*r.Intn(4)), 1, 0, 0, 0, 0, time.UTC)
	months := 12 + r.Intn(25)
	end := start.AddDate(0, months, 0)
	w := &world{onboard: map[string]time.Time{}, truth: map[string]map[string]scheduler.FrozenRow{}}
	for p := calendar.Of(calendar.Week, start); p.From.Before(end); p = calendar.Next(p) {
		w.weeks = append(w.weeks, p)
	}
	em := start.AddDate(0, r.Intn(max(1, months/3)), 0)
	w.epoch = em.AddDate(0, 0, 3+r.Intn(24)).Add(12 * time.Hour) // mid-month
	w.fake = schedtest.New(start, w.epoch)
	n := 1 + r.Intn(8)
	span := end.Sub(start)
	for i := 0; i < n; i++ {
		id := uuid.New()
		created := start.Add(-time.Duration(r.Intn(400)) * time.Hour)
		if r.Intn(3) > 0 {
			created = start.Add(time.Duration(r.Int63n(int64(span))))
		}
		var gone time.Time
		if r.Intn(10) < 3 {
			gone = created.Add(time.Duration(r.Int63n(int64(end.Sub(created)) + 1)))
		}
		w.onboard[id.String()] = created
		w.fake.Hosp = append(w.fake.Hosp, scheduler.Hospital{ID: id, Name: "Synthetic Hospital " + string(rune('A'+i)), Status: "active", CreatedAt: created})
		w.fake.StaffN[id] = int64(r.Intn(30))
		quiet := r.Intn(4) == 0
		for _, wk := range w.weeks {
			if r.Intn(5) == 0 {
				quiet = !quiet
			}
			if !created.Before(wk.To) || (!gone.IsZero() && !gone.After(wk.From)) {
				continue // not listed: nothing
			}
			var row scheduler.FrozenRow
			if !quiet {
				for _, c := range []int{0, 2, 4} {
					n := count(r)
					d := int64(0)
					if r.Intn(3) == 0 && n > 0 {
						d = int64(r.Intn(int(n) + 1))
					}
					row.QG[c], row.QG[c+1] = n-d, d
				}
				if r.Intn(5) == 0 {
					row.QG[6] = int64(r.Intn(4))
				}
				var answers int64
				for c := 0; c < 6; c++ {
					answers += row.QG[c]
				}
				if answers > 0 {
					row.R[0] = int64(r.Intn(int(min(answers, 9)) + 1))
					row.R[1] = int64(r.Intn(int(min(answers-row.R[0], 4)) + 1))
					row.Clinicians = 1 + int64(r.Intn(int(min(answers, 6))))
				}
				row.Decided = int64(r.Intn(8))
				if row.Decided > 0 {
					m := float64(r.Intn(400)) / 10
					row.Median = &m
				}
			}
			w.fake.Set(wk, id, row)
			key := schedtest.Key(wk)
			if w.truth[key] == nil {
				w.truth[key] = map[string]scheduler.FrozenRow{}
			}
			w.truth[key][id.String()] = row
		}
	}
	return w
}

// value is the ground truth of a box.
func (w *world) value(p *Parser) func([]int) int64 {
	ids := map[int]string{}
	for id := range w.onboard {
		h, _ := p.HospitalIndex(id)
		ids[h] = id
	}
	return func(vars []int) int64 {
		var n int64
		for _, v := range vars {
			f, h, wk, c := p.Decode(v)
			row := w.truth[schedtest.Key(w.weeks[wk])][ids[h]]
			if f == 0 {
				n += row.QG[c]
			} else {
				n += row.R[c]
			}
		}
		return n
	}
}

// responses are the JSON documents an attacker records after each wake:
// the source label, the period and the bytes.
type response struct {
	source  string
	period  calendar.Period
	raw     []byte
	rolling bool
}

// routes renders what each route would send for the stored snapshot s:
// GET /platform/guardrails/periods for its kind (Task 5, R41a-4), and the stored body, which
// stands in for the analytics routes until 7c-2 (it holds every cell they
// send).
var routes = []func(t *testing.T, s scheduler.Stored, at time.Time) []response{
	func(t *testing.T, s scheduler.Stored, _ time.Time) []response {
		return []response{{source: "stored", period: s.Period, raw: s.Body}}
	},
	func(t *testing.T, s scheduler.Stored, at time.Time) []response {
		b, err := analytics.DecodeBody(s.Body, s.Period)
		if err != nil {
			t.Fatalf("%v: %v", s.Period, err)
		}
		raw, _ := json.Marshal(platform.GuardrailsFromBody(b, s.Period, at))
		return []response{{source: "guardrails", period: s.Period, raw: raw}}
	},
}

// rolling is GET /platform/guardrails?period=7d|30d|90d at a wake: the
// hospitals listed, every figure hidden (R41a-3).
func (w *world) rolling(at time.Time) []response {
	var refs []platform.Ref
	for _, h := range w.fake.Hosp {
		if !h.CreatedAt.After(at) {
			refs = append(refs, platform.Ref{ID: h.ID.String(), Name: h.Name})
		}
	}
	var out []response
	for _, days := range []int{7, 30, 90} {
		to := at.Add(-calendar.Grace).Truncate(24 * time.Hour)
		raw, _ := json.Marshal(platform.RollingGuardrails(fmt.Sprintf("%dd", days), to.AddDate(0, 0, -days), to, refs))
		out = append(out, response{source: "rolling", raw: raw, rolling: true})
	}
	return out
}

func (w *world) run(t *testing.T) (*Parser, []response) {
	t.Helper()
	s := scheduler.Scheduler{Builder: scheduler.Builder{Src: w.fake}, Lock: schedtest.NoLock}
	var out []response
	seen := map[string]bool{}
	for _, wk := range w.weeks {
		w.fake.Clock = wk.To.Add(calendar.Grace)
		if _, err := s.Tick(context.Background()); err != nil {
			t.Fatalf("tick at %v: %v", w.fake.Clock, err)
		}
		for _, st := range w.fake.All() {
			if seen[schedtest.Key(st.Period)] {
				continue
			}
			seen[schedtest.Key(st.Period)] = true
			if st.State != scheduler.StateOK {
				t.Fatalf("%v stored %s: the guard gave up", st.Period, st.State)
			}
			if st.Period.From.Before(w.epoch) {
				t.Fatalf("%v is stored though it starts before the epoch %v", st.Period, w.epoch)
			}
			for _, route := range routes {
				out = append(out, route(t, st, w.fake.Clock)...)
			}
		}
		out = append(out, w.rolling(w.fake.Clock)...)
	}
	// The first published month and quarter are the next full ones.
	for _, k := range []calendar.Kind{calendar.Month, calendar.Quarter} {
		first := calendar.Of(k, w.epoch)
		if first.From.Before(w.epoch) {
			first = calendar.Next(first)
		}
		for _, st := range w.fake.All() {
			if st.Period.Kind == k && st.Period.From.Before(first.From) {
				t.Fatalf("%v published before the first full %s %v", st.Period, k, first)
			}
		}
		if _, ok := w.fake.Snapshot(first); !ok && first.Finished(w.fake.Clock) {
			t.Fatalf("the first full %s %v isn't published", k, first)
		}
	}
	p := New(w.weeks, w.onboard)
	for _, r := range out {
		var err error
		if r.rolling {
			err = p.AddRolling(r.raw)
		} else {
			err = p.Add(r.source, r.raw, r.period)
		}
		if err != nil {
			t.Fatalf("parse %s %v: %v", r.source, r.period, err)
		}
	}
	return p, out
}

func TestHistoryFromRouteJSON(t *testing.T) {
	r := rand.New(rand.NewSource(20261107))
	cells, hidden, exposed := 0, 0, 0
	for k := 0; k < historyCount(); k++ {
		w := randomWorld(r)
		p, _ := w.run(t)
		rep := p.Check(w.value(p))
		if len(rep.SmallCells)+len(rep.SmallSums) > 0 {
			t.Fatalf("history %d (epoch %v): %s", k, w.epoch, strings.Join(append(rep.SmallCells, rep.SmallSums...), "; "))
		}
		cells += rep.Cells
		hidden += rep.Hidden
		exposed += rep.ExposedCells
	}
	if hidden == 0 {
		t.Fatal("nothing was hidden: the generator tests nothing")
	}
	t.Logf("%d histories: %d published cells, %d hidden, %d hidden cells exposed with 0 or 5 and up (known before they were hidden)",
		historyCount(), cells, hidden, exposed)
}
