package historytest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
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
// GET /platform/guardrails on the deprecated 7d, 30d and 90d, GET
// /platform/analytics on week, month and quarter, and every hospital's
// series on both measures, What changed and every open change's series;
// plus the stored bodies, which hold every cell any route sends) is parsed
// by this package alone, and every analytics, series and change response is
// checked against the stored bodies of its periods. The exact
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

func randomWorld(r *rand.Rand) *world { return randomWorldN(r, 1, 8, 12+r.Intn(25), false) }

// randomWorldN is a history of months months with nMin to nMax hospitals;
// crowded draws counts of 0 to 4 in most weeks, so columns hold 11 or more
// hidden hospitals and rows many hidden parts (R41a-5).
func randomWorldN(r *rand.Rand, nMin, nMax, months int, crowded bool) *world {
	start := time.Date(2024+r.Intn(2), time.Month(1+3*r.Intn(4)), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, months, 0)
	w := &world{onboard: map[string]time.Time{}, truth: map[string]map[string]scheduler.FrozenRow{}}
	for p := calendar.Of(calendar.Week, start); p.From.Before(end); p = calendar.Next(p) {
		w.weeks = append(w.weeks, p)
	}
	em := start.AddDate(0, r.Intn(max(1, months/3)), 0)
	w.epoch = em.AddDate(0, 0, 3+r.Intn(24)).Add(12 * time.Hour) // mid-month
	w.fake = schedtest.New(start, w.epoch)
	n := nMin + r.Intn(nMax-nMin+1)
	span := end.Sub(start)
	draw := count
	if crowded {
		draw = func(r *rand.Rand) int64 {
			if r.Intn(6) == 0 {
				return count(r)
			}
			return int64(r.Intn(5))
		}
	}
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
					n := draw(r)
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
	kind    calendar.Kind // the analytics routes' period
	item    []byte        // a change series' change, as the changes area sent it
}

// routes renders what each route would send for the stored snapshot s:
// the stored body (every cell any route sends) and GET
// /platform/guardrails/periods for its kind (Task 5, R41a-4). The analytics
// routes, which read every stored period of the kind, are rendered in run.
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
	notYet := map[calendar.Kind]bool{}
	for _, wk := range w.weeks {
		w.fake.Clock = wk.To.Add(calendar.Grace)
		if _, err := s.Tick(context.Background()); err != nil {
			t.Fatalf("tick at %v: %v", w.fake.Clock, err)
		}
		// In publication order: by end, then week, month, quarter.
		stored := w.fake.All()
		order := map[calendar.Kind]int{calendar.Week: 0, calendar.Month: 1, calendar.Quarter: 2}
		sort.SliceStable(stored, func(i, j int) bool {
			a, b := stored[i].Period, stored[j].Period
			if !a.To.Equal(b.To) {
				return a.To.Before(b.To)
			}
			return order[a.Kind] < order[b.Kind]
		})
		for _, st := range stored {
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
			out = append(out, w.analytics(t, st.Period.Kind)...)
		}
		for _, k := range []calendar.Kind{calendar.Week, calendar.Month, calendar.Quarter} {
			if !notYet[k] && !w.anyStored(k) {
				notYet[k] = true // GET /platform/analytics before its first period (F19)
				out = append(out, w.analytics(t, k)...)
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
	p.Epoch = w.epoch
	for _, r := range out {
		var err error
		switch {
		case r.rolling:
			err = p.AddRolling(r.raw)
		case r.source == "analytics":
			err = p.AddAnalytics(r.raw)
		case r.source == "series":
			err = p.AddSeries(r.raw, r.kind)
		case r.source == "change_series":
			err = p.AddChangeSeries(r.raw, r.kind, r.item)
		default:
			err = p.Add(r.source, r.raw, r.period)
		}
		if err != nil {
			t.Fatalf("parse %s %v: %v", r.source, r.period, err)
		}
	}
	if n := p.Unavailable(); n != 0 {
		t.Fatalf("%d analytics areas unavailable", n)
	}
	return p, out
}

// anyStored reports whether a period of kind k is stored.
func (w *world) anyStored(k calendar.Kind) bool {
	for _, s := range w.fake.All() {
		if s.Period.Kind == k {
			return true
		}
	}
	return false
}

// analytics renders GET /platform/analytics?period=k and every hospital's
// series of kind k over what is stored now, as the routes read it.
func (w *world) analytics(t *testing.T, k calendar.Kind) []response {
	t.Helper()
	var rows []analytics.Snapshot
	for _, s := range w.fake.All() {
		if s.Period.Kind == k {
			rows = append(rows, analytics.Snapshot{Period: s.Period, State: s.State, Body: s.Body, ComputedAt: s.Period.To.Add(calendar.Grace)})
		}
	}
	a, degraded := analytics.Render(k, rows, w.epoch)
	if degraded {
		t.Fatalf("%s: a stored body is unreadable", k)
	}
	// What changed (Plan 7c-3): the newest evaluation, as the route reads
	// it.
	run, changes := w.fake.ChangesNow()
	a.WhatChanged, degraded = analytics.RenderChanges(run, changes, "")
	if degraded {
		t.Fatalf("a stored change is unreadable")
	}
	raw, _ := json.Marshal(a)
	out := []response{{source: "analytics", raw: raw, kind: k}}
	if len(rows) == 0 {
		return out
	}
	// Every open change's series of kind k.
	for _, c := range changes {
		item, err := analytics.ChangeOutOf(c, "")
		if err != nil {
			t.Fatalf("change %s: %v", c.ID, err)
		}
		s, err := analytics.ChangeSeries(k, rows, c.Hospital, c.Measure)
		if err != nil {
			t.Fatalf("change series %s: %v", c.Measure, err)
		}
		sraw, _ := json.Marshal(s)
		iraw, _ := json.Marshal(item)
		out = append(out, response{source: "change_series", raw: sraw, kind: k, item: iraw})
	}
	ids := make([]string, 0, len(w.onboard))
	for id := range w.onboard {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, m := range analytics.SeriesMeasures {
			s, err := analytics.HospitalSeries(k, rows, id, m)
			if errors.Is(err, analytics.ErrNoHospital) {
				continue
			}
			if err != nil {
				t.Fatalf("series %s %s: %v", k, m, err)
			}
			raw, _ := json.Marshal(s)
			out = append(out, response{source: "series", raw: raw, kind: k})
		}
	}
	return out
}

func TestHistoryFromRouteJSON(t *testing.T) {
	r := rand.New(rand.NewSource(20261107))
	cells, hidden, exposed, changes := 0, 0, 0, 0
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
		changes += p.Changes()
	}
	if hidden == 0 {
		t.Fatal("nothing was hidden: the generator tests nothing")
	}
	if changes == 0 {
		t.Fatal("no change was flagged: the changes check tests nothing")
	}
	t.Logf("%d histories: %d published cells, %d hidden, %d hidden cells exposed with 0 or 5 and up (known before they were hidden); "+
		"%d changes sent, each checked against the stored cells", historyCount(), cells, hidden, exposed, changes)
}

// R41a-5, the whole set: replayed in publication order from the JSON, no
// published set's hidden members have anything determined since just
// before it was published but their full sum (no member, no proper union of
// any size, no other combination); and the cell and sum checks hold. Normal
// histories, and crowded ones: 15 to 20 hospitals with counts of 0 to 4 in
// most weeks (columns with 11 or more hidden hospitals, rows with every
// part hidden).
func TestHistorySetsFromRouteJSON(t *testing.T) {
	r := rand.New(rand.NewSource(41))
	normal, crowded := 6, 6
	switch {
	case testing.Short():
		normal, crowded = 1, 1
	case raceEnabled:
		normal, crowded = 2, 2
	}
	sets, wide := 0, 0
	for k := 0; k < normal+crowded; k++ {
		w := randomWorld(r)
		if k >= normal {
			w = randomWorldN(r, 15, 20, 6+r.Intn(7), true)
		}
		p, _ := w.run(t)
		if v := p.CheckSets(); len(v) > 0 {
			t.Fatalf("history %d (%d hospitals): %d sets with more than their full sum determined; first: %s", k, len(w.onboard), len(v), v[0])
		}
		rep := p.Check(w.value(p))
		if len(rep.SmallCells)+len(rep.SmallSums) > 0 {
			t.Fatalf("history %d: %s", k, strings.Join(append(rep.SmallCells, rep.SmallSums...), "; "))
		}
		s, wd := p.setStats()
		sets += s
		wide += wd
	}
	if wide == 0 {
		t.Fatal("no set had 11 or more hidden members: the crowded histories test nothing")
	}
	t.Logf("%d histories: %d published sets with 2 or more hidden members checked, %d with 11 or more", normal+crowded, sets, wide)
}
