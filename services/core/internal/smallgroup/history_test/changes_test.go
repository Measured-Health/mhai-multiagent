package historytest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// The JSON-side check of What changed and the change series (changes.go)
// catches every change that isn't the stored, shown cells: each check has a
// case here that only it stops, and the untampered routes pass. Fixed dates
// only: the week of 1–7 April 2026 against March 2026, and the week of 1–7
// March 2026, whose month (February) isn't stored.

var (
	chWeek     = calendar.Of(calendar.Week, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	chMonth    = calendar.Of(calendar.Month, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	chEarlyWk  = calendar.Of(calendar.Week, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	chRunAt    = time.Date(2026, 4, 8, 0, 20, 0, 0, time.UTC)
	chFirstDay = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
)

// figures is a row with every change measure shown.
func figures(q, c, e, a int64, r float64) analytics.Row {
	return analytics.Row{Questions: smallgroup.Count(q), Answers: smallgroup.Count(40), Pass: smallgroup.Count(40 - e),
		Regenerate: smallgroup.Count(0), Escalate: smallgroup.Count(e), Degraded: smallgroup.Count(0),
		PassShare: smallgroup.Share(40-e, 40), RegenerateShare: smallgroup.Share(0, 40), EscalateShare: smallgroup.Share(e, 40),
		Reviews: smallgroup.Count(40), AgreementRate: smallgroup.Share(a, 40), ActiveClinicians: smallgroup.Count(c),
		QuestionsPerClinician: analytics.RatioOf(r), MedianReviewHours: analytics.HoursOf(2.5)}
}

type changeCase struct {
	p       *Parser
	ids     []string // A, B (both periods), C (the week only)
	area    []byte   // the changes area as rendered
	items   []map[string]any
	weekRow []analytics.Snapshot
	changes []analytics.ChangeRow
}

// newChangeCase stores March, the April week and the early March week, and
// renders the area from analytics.Changes; tweak may change the stored
// bodies after the flags are made (so the area no longer matches them).
func newChangeCase(t *testing.T, tweak func(week, month *analytics.Body)) *changeCase {
	t.Helper()
	c := &changeCase{ids: []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}}
	var weeks []calendar.Period
	for w := calendar.Of(calendar.Week, chFirstDay); w.From.Before(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)); w = calendar.Next(w) {
		weeks = append(weeks, w)
	}
	onboard := map[string]time.Time{}
	for _, id := range c.ids {
		onboard[id] = chFirstDay.AddDate(-1, 0, 0)
	}
	c.p = New(weeks, onboard)
	ref := func(i int) analytics.Ref {
		return analytics.Ref{ID: c.ids[i], Name: "Synthetic " + string(rune('A'+i))}
	}
	body := func(p calendar.Period, all analytics.Row, rows ...analytics.HospitalRow) analytics.Body {
		from, to := analytics.Dates(p)
		return analytics.Body{Kind: string(p.Kind), From: from, To: to, All: all, Hospitals: rows}
	}
	monthRow := figures(62, 12, 10, 30, 4.67)
	steadyB := figures(62, 12, 10, 30, 4.67)
	weekRow := figures(40, 30, 30, 10, 3.33)
	weekB := figures(14, 12, 10, 30, 1.05)
	month := body(chMonth, monthRow, analytics.HospitalRow{Hospital: ref(0), Staff: analytics.StaffFigure(9), Row: monthRow},
		analytics.HospitalRow{Hospital: ref(1), Staff: analytics.StaffFigure(9), Row: steadyB})
	week := body(chWeek, weekRow, analytics.HospitalRow{Hospital: ref(0), Staff: analytics.StaffFigure(9), Row: weekRow},
		analytics.HospitalRow{Hospital: ref(1), Staff: analytics.StaffFigure(9), Row: weekB},
		analytics.HospitalRow{Hospital: ref(2), Staff: analytics.StaffFigure(9), Row: weekRow})
	early := body(chEarlyWk, weekRow, analytics.HospitalRow{Hospital: ref(0), Staff: analytics.StaffFigure(9), Row: weekRow})
	flags := analytics.Changes(chWeek, week, chMonth, month)
	if tweak != nil {
		tweak(&week, &month)
	}
	for _, x := range []struct {
		p calendar.Period
		b analytics.Body
	}{{chMonth, month}, {chEarlyWk, early}, {chWeek, week}} {
		raw, _ := json.Marshal(x.b)
		if err := c.p.Add("stored", raw, x.p); err != nil {
			t.Fatal(err)
		}
		if x.p.Kind == calendar.Week {
			c.weekRow = append(c.weekRow, analytics.Snapshot{Period: x.p, State: analytics.StateOK, Body: raw, ComputedAt: x.p.To.Add(time.Hour)})
		}
	}
	for i, f := range flags {
		cur, _ := f.Current.MarshalJSON()
		prev, _ := f.Previous.MarshalJSON()
		c.changes = append(c.changes, analytics.ChangeRow{ID: fmt.Sprintf("00000000-0000-4000-8000-%012x", i+1), Hospital: f.Hospital,
			Area: f.Area, Measure: f.Measure, Direction: f.Direction, Current: cur, Previous: prev, Summary: f.Summary, Version: 1})
	}
	area, _ := analytics.RenderChanges(&analytics.ChangesRun{Week: chWeek, Month: chMonth, EvaluatedAt: chRunAt}, c.changes, "")
	c.area, _ = json.Marshal(area)
	var m map[string]any
	_ = json.Unmarshal(c.area, &m)
	for _, x := range m["items"].([]any) {
		c.items = append(c.items, x.(map[string]any))
	}
	return c
}

// item is the rendered item of a row (all: "") and measure.
func (c *changeCase) item(t *testing.T, row, measure string) map[string]any {
	t.Helper()
	for _, it := range c.items {
		h, _ := it["hospital"].(map[string]any)
		id := ""
		if h != nil {
			id = h["id"].(string)
		}
		if id == row && it["measure"] == measure {
			return it
		}
	}
	t.Fatalf("no %s %s item", row, measure)
	return nil
}

// withItems is the area with items replaced.
func (c *changeCase) withItems(items ...map[string]any) []byte {
	var m map[string]any
	_ = json.Unmarshal(c.area, &m)
	list := make([]any, 0, len(items))
	for _, it := range items {
		list = append(list, it)
	}
	m["items"] = list
	out, _ := json.Marshal(m)
	return out
}

// copied is a deep copy of an item with f applied.
func copied(it map[string]any, f func(m map[string]any)) map[string]any {
	raw, _ := json.Marshal(it)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	f(m)
	return m
}

func (c *changeCase) series(t *testing.T, id string) ([]byte, []byte) {
	t.Helper()
	for _, row := range c.changes {
		if row.ID != id {
			continue
		}
		s, err := analytics.ChangeSeries(calendar.Week, c.weekRow, row.Hospital, row.Measure)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(s)
		item, _ := analytics.ChangeOutOf(row, "")
		iraw, _ := json.Marshal(item)
		return raw, iraw
	}
	t.Fatalf("no change %s", id)
	return nil, nil
}

func TestChangeCheckAcceptsTheRoutes(t *testing.T) {
	c := newChangeCase(t, nil)
	if len(c.items) != 10 {
		t.Fatalf("%d changes flagged, want five each for all and A: %s", len(c.items), c.area)
	}
	if err := c.p.AddChanges(c.area); err != nil {
		t.Fatalf("changes: %v", err)
	}
	for _, row := range c.changes {
		raw, item := c.series(t, row.ID)
		if err := c.p.AddChangeSeries(raw, calendar.Week, item); err != nil {
			t.Fatalf("series %s %s: %v", row.Measure, row.ID, err)
		}
	}
	if c.p.Changes() != 10 {
		t.Errorf("%d changes counted", c.p.Changes())
	}
}

// Each case names the check (changes.go) it must trip; want is a piece of
// that check's message, so a case passing on another check fails.
func TestChangeCheckCatches(t *testing.T) {
	type tc struct {
		want string
		run  func(t *testing.T, c *changeCase) error
	}
	one := func(row int, measure string, f func(m map[string]any)) func(t *testing.T, c *changeCase) error {
		return func(t *testing.T, c *changeCase) error {
			id := ""
			if row >= 0 {
				id = c.ids[row]
			}
			return c.p.AddChanges(c.withItems(copied(c.item(t, id, measure), f)))
		}
	}
	area := func(f func(m map[string]any)) func(t *testing.T, c *changeCase) error {
		return func(t *testing.T, c *changeCase) error {
			var m map[string]any
			_ = json.Unmarshal(c.area, &m)
			f(m)
			raw, _ := json.Marshal(m)
			return c.p.AddChanges(raw)
		}
	}
	tweaked := func(tweak func(week, month *analytics.Body)) func(t *testing.T, c *changeCase) error {
		return func(t *testing.T, _ *changeCase) error {
			c := newChangeCase(t, tweak)
			return c.p.AddChanges(c.area)
		}
	}
	shown := func(v float64) map[string]any { return map[string]any{"value": v, "suppressed": false} }
	cases := map[string]tc{
		// The area.
		"an area key missing": {"keys of", area(func(m map[string]any) { delete(m, "items") })},
		"a window not a week and its month": {"isn't a week and the month before it", area(func(m map[string]any) {
			m["window"].(map[string]any)["from"] = "2026-02-01T00:00:00.000Z"
		})},
		"a window week not stored": {"isn't stored", area(func(m map[string]any) {
			m["window"] = map[string]any{"from": "2026-03-01T00:00:00.000Z", "to": "2026-04-15T00:00:00.000Z"}
		})},
		"changes against a month not stored": {"which isn't stored", area(func(m map[string]any) {
			m["window"] = map[string]any{"from": "2026-02-01T00:00:00.000Z", "to": "2026-03-08T00:00:00.000Z"}
		})},
		"an item twice": {"twice", func(t *testing.T, c *changeCase) error {
			it := c.item(t, "", "questions")
			return c.p.AddChanges(c.withItems(it, it))
		}},
		// The item.
		"an extra key":        {"change: keys of", one(-1, "questions", func(m map[string]any) { m["raw"] = 3 })},
		"an unknown measure":  {`measure "median_review_hours"`, one(-1, "questions", func(m map[string]any) { m["measure"] = "median_review_hours" })},
		"the wrong area":      {"in area", one(-1, "questions", func(m map[string]any) { m["area"] = "quality" })},
		"serious":             {"serious", one(-1, "questions", func(m map[string]any) { m["serious"] = true })},
		"a quality direction": {"isn't the measure's", one(-1, "questions", func(m map[string]any) { m["direction"] = "worse" })},
		"version 0":           {"version", one(-1, "questions", func(m map[string]any) { m["version"] = 0 })},
		"a hospital not named so": {"isn't named", one(0, "questions", func(m map[string]any) {
			m["hospital"].(map[string]any)["name"] = "Synthetic Z"
		})},
		"a hospital with no previous month": {"has no previous month", func(t *testing.T, c *changeCase) error {
			it := copied(c.item(t, c.ids[0], "questions"), func(m map[string]any) {
				m["hospital"] = map[string]any{"id": c.ids[2], "name": "Synthetic C"}
			})
			return c.p.AddChanges(c.withItems(it))
		}},
		"current hidden": {"not shown", one(-1, "questions", func(m map[string]any) {
			m["current"] = map[string]any{"value": nil, "suppressed": true}
		})},
		"current not the week's": {"the week's stored cell", one(-1, "questions", func(m map[string]any) { m["current"] = shown(41) })},
		"previous not scaled":    {"the month's stored cell gives", one(-1, "questions", func(m map[string]any) { m["previous"] = shown(62) })},
		"previous over a month cell hidden": {"which isn't shown", tweaked(func(_, month *analytics.Body) {
			month.All.Questions = smallgroup.Guarded{Suppressed: true}
		})},
		"a base under the minimum": {"under the minimum", tweaked(func(week, _ *analytics.Body) {
			week.All.Answers = smallgroup.Count(19)
		})},
		"a base hidden": {"under the minimum", tweaked(func(week, _ *analytics.Body) {
			week.All.Reviews = smallgroup.Guarded{Suppressed: true}
		})},
		"under the threshold": {"under the threshold", func(t *testing.T, c *changeCase) error {
			it := copied(c.item(t, c.ids[0], "active_clinicians"), func(m map[string]any) {
				m["hospital"] = map[string]any{"id": c.ids[1], "name": "Synthetic B"}
				m["current"], m["previous"] = shown(12), shown(12)
				m["summary"] = "Active clinicians up: 12 to 12"
			})
			return c.p.AddChanges(c.withItems(it))
		}},
		"the direction turned round": {"→", one(-1, "questions", func(m map[string]any) { m["direction"] = "falling" })},
		"a summary naming the hospital": {"names a hospital", one(0, "questions", func(m map[string]any) {
			m["summary"] = "Questions up at Synthetic A: 14.0 to 40 a week"
		})},
		"a summary with another number": {"3 numbers", one(-1, "questions", func(m map[string]any) {
			m["summary"] = "Questions up 186%: 14.0 to 40 a week"
		})},
		"a summary with the wrong number": {"isn't", one(-1, "questions", func(m map[string]any) {
			m["summary"] = "Questions up: 14.0 to 41 a week"
		})},
		"a share summary not in percent": {"isn't", one(-1, "needs_review_share", func(m map[string]any) {
			m["summary"] = "Needs-review share worse: 0.25 to 0.75"
		})},
		"a follow-up's keys": {"follow_up", one(-1, "questions", func(m map[string]any) {
			m["follow_up"] = map[string]any{"by": map[string]any{"id": "x", "name": "y"}, "at": "x", "note": nil, "by_you": false}
		})},
		"a follow-up's author keys": {"follow_up by", one(-1, "questions", func(m map[string]any) {
			m["follow_up"] = map[string]any{"by": map[string]any{"id": "x"}, "at": "x", "note": nil, "by_you": false, "version": 1}
		})},
	}
	// The change series.
	ser := func(measure string, hospital int, f func(t *testing.T, raw, item []byte) ([]byte, []byte)) func(t *testing.T, c *changeCase) error {
		return func(t *testing.T, c *changeCase) error {
			for _, row := range c.changes {
				if row.Measure == measure && (hospital < 0) == (row.Hospital == nil) {
					raw, item := c.series(t, row.ID)
					raw, item = f(t, raw, item)
					return c.p.AddChangeSeries(raw, calendar.Week, item)
				}
			}
			t.Fatalf("no %s change", measure)
			return nil
		}
	}
	edits := func(f func(m map[string]any)) func(t *testing.T, raw, item []byte) ([]byte, []byte) {
		return func(t *testing.T, raw, item []byte) ([]byte, []byte) { return edit(t, raw, f), item }
	}
	cases["a series of another measure"] = tc{"series measure", ser("questions", -1, edits(func(m map[string]any) { m["measure"] = "active_clinicians" }))}
	cases["a series of another hospital"] = tc{"series hospital", ser("questions", 0, func(t *testing.T, raw, item []byte) ([]byte, []byte) {
		return edit(t, raw, func(m map[string]any) { m["hospital"] = nil }), item
	})}
	cases["a median for all hospitals"] = tc{"for all hospitals", ser("questions", -1, edits(func(m map[string]any) {
		m["points"].([]any)[0].(map[string]any)["median"] = 14
	}))}
	cases["a hospital median not the others'"] = tc{"the other hospitals' shown values give", ser("questions", 0, edits(func(m map[string]any) {
		pts := m["points"].([]any)
		pts[len(pts)-1].(map[string]any)["median"] = 99
	}))}
	cases["a point not the stored cell"] = tc{"stored", ser("questions", -1, edits(func(m map[string]any) {
		pts := m["points"].([]any)
		pts[len(pts)-1].(map[string]any)["value"] = map[string]any{"value": 41, "suppressed": false}
	}))}
	cases["a point missing"] = tc{"points for", ser("questions", -1, edits(func(m map[string]any) { m["points"] = m["points"].([]any)[1:] }))}
	cases["a series that isn't an object"] = tc{"not an object", ser("questions", -1, func(t *testing.T, raw, item []byte) ([]byte, []byte) {
		return []byte(`[]`), item
	})}
	for name, k := range cases {
		err := k.run(t, newChangeCase(t, nil))
		if err == nil || !strings.Contains(err.Error(), k.want) {
			t.Errorf("%s: got %v, want an error with %q", name, err, k.want)
		}
	}
	// Through GET /platform/analytics: a what_changed area that isn't one is
	// refused with the area's error.
	c := newChangeCase(t, nil)
	raw := []byte(`{"period":"week","from":null,"to":null,"changes":{"unavailable":true},"use":{},"alerts":null,"quality":{},"hospitals":{},"what_changed":{"updated_at":"x","window":{},"items":[]}}`)
	if err := c.p.AddAnalytics(raw); err == nil || !strings.Contains(err.Error(), "what_changed: updated_at") {
		t.Errorf("a broken changes area through AddAnalytics: %v", err)
	}
}
