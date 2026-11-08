package historytest

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// The JSON-side check of the analytics routes (routes.go) catches every
// response that isn't the stored bodies: each check has a case here that
// only it stops, and the untampered routes pass. Fixed dates only.

// renderEpoch is the release epoch of the cases: the first month after it
// is April 2026, the first quarter Q2 2026.
var renderEpoch = time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

type routeCase struct {
	p    *Parser
	rows []analytics.Snapshot
	ids  []string
}

// row is a hospital's figures, scale n. A (n 1) has 5 reviews, B (n 2)
// none, so the reviews median of A and B is 2.5 (hidden); hide hides C's
// escalations and reviews.
func row(n int64, hide bool) analytics.Row {
	reviews := int64(0)
	if n == 1 {
		reviews = 5
	}
	r := analytics.Row{Questions: smallgroup.Count(12 * n), Answers: smallgroup.Count(10 * n), Pass: smallgroup.Count(5 * n),
		Regenerate: smallgroup.Count(0), Escalate: smallgroup.Count(5 * n), Degraded: smallgroup.Count(0),
		PassShare: smallgroup.Share(5*n, 10*n), RegenerateShare: smallgroup.Share(0, 10*n), EscalateShare: smallgroup.Share(5*n, 10*n),
		Reviews: smallgroup.Count(reviews), AgreementRate: smallgroup.GuardedShare{}, ActiveClinicians: smallgroup.Count(6 * n),
		QuestionsPerClinician: analytics.RatioOf(2), MedianReviewHours: analytics.Hours{Number: analytics.Number{Suppressed: true}}}
	if hide {
		h, s := smallgroup.Guarded{Suppressed: true}, smallgroup.GuardedShare{Suppressed: true}
		r.Escalate, r.Pass, r.Reviews, r.EscalateShare, r.PassShare = h, h, h, s, s
	}
	return r
}

// newRouteCase stores April to June 2026 for hospitals A, B and C; with
// hideB, B's needs-review share is hidden too (A's series median then has
// no shown value).
func newRouteCase(t *testing.T, hideB bool) *routeCase {
	t.Helper()
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	var weeks []calendar.Period
	for w := calendar.Of(calendar.Week, start); w.From.Before(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)); w = calendar.Next(w) {
		weeks = append(weeks, w)
	}
	c := &routeCase{ids: []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}}
	onboard := map[string]time.Time{}
	for _, id := range c.ids {
		onboard[id] = start.AddDate(-1, 0, 0)
	}
	c.p = New(weeks, onboard)
	c.p.Epoch = renderEpoch
	for i, m := range calendar.Children(calendar.Of(calendar.Quarter, start)) {
		from, to := analytics.Dates(m)
		all := row(4, false)
		all.Questions = smallgroup.Count(int64(100 + i))
		b := row(2, false)
		if hideB {
			b.EscalateShare = smallgroup.GuardedShare{Suppressed: true}
		}
		body := analytics.Body{Kind: "month", From: from, To: to, All: all, Hospitals: []analytics.HospitalRow{
			{Hospital: analytics.Ref{ID: c.ids[0], Name: "Synthetic A"}, Staff: analytics.StaffFigure(9), Row: row(1, false)},
			{Hospital: analytics.Ref{ID: c.ids[1], Name: "Synthetic B"}, Staff: analytics.StaffFigure(9), Row: b},
			{Hospital: analytics.Ref{ID: c.ids[2], Name: "Synthetic C"}, Staff: analytics.StaffFigure(9), Row: row(3, true)},
		}}
		raw, _ := json.Marshal(body)
		if err := c.p.Add("stored", raw, m); err != nil {
			t.Fatal(err)
		}
		c.rows = append(c.rows, analytics.Snapshot{Period: m, State: analytics.StateOK, Body: raw, ComputedAt: m.To.Add(time.Hour)})
	}
	return c
}

func (c *routeCase) analytics(t *testing.T) []byte {
	t.Helper()
	out, _ := analytics.Render(calendar.Month, c.rows, renderEpoch)
	raw, _ := json.Marshal(out)
	return raw
}

// notYet is GET /platform/analytics?period=quarter before any quarter.
func (c *routeCase) notYet(t *testing.T) []byte {
	t.Helper()
	out, _ := analytics.Render(calendar.Quarter, nil, renderEpoch)
	raw, _ := json.Marshal(out)
	return raw
}

func (c *routeCase) series(t *testing.T, id, m string) []byte {
	t.Helper()
	s, err := analytics.HospitalSeries(calendar.Month, c.rows, id, m)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(s)
	return raw
}

// edit changes a response through its JSON maps.
func edit(t *testing.T, raw []byte, f func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	out, _ := json.Marshal(m)
	return out
}

func obj(v any, path ...any) map[string]any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			v = v.(map[string]any)[k]
		case int:
			v = v.([]any)[k]
		}
	}
	return v.(map[string]any)
}

func arr(v any, key string) []any { return v.(map[string]any)[key].([]any) }

// replace swaps the first (or, with last, the last) occurrence of old.
func replace(t *testing.T, raw []byte, old, new string, last bool) []byte {
	t.Helper()
	i := bytes.Index(raw, []byte(old))
	if last {
		i = bytes.LastIndex(raw, []byte(old))
	}
	if i < 0 {
		t.Fatalf("%s not in %s", old, raw)
	}
	return append(append(append([]byte{}, raw[:i]...), new...), raw[i+len(old):]...)
}

func TestRouteCheckAcceptsTheRoutes(t *testing.T) {
	for _, hideB := range []bool{false, true} {
		c := newRouteCase(t, hideB)
		if err := c.p.AddAnalytics(c.notYet(t)); err != nil {
			t.Fatalf("not yet: %v", err)
		}
		if err := c.p.AddAnalytics(c.analytics(t)); err != nil {
			t.Fatalf("analytics: %v", err)
		}
		for _, id := range c.ids {
			for _, m := range analytics.SeriesMeasures {
				if err := c.p.AddSeries(c.series(t, id, m), calendar.Month); err != nil {
					t.Fatalf("series %s: %v", m, err)
				}
			}
		}
	}
	// The reviews median of 5 and 0 (and C hidden) is sent hidden.
	if !strings.Contains(string(newRouteCase(t, false).analytics(t)), `"reviews":{"value":null,"suppressed":true},"agreement_rate":{"value":null,"suppressed":false},"median_review_hours":{"value":null,"suppressed":false}},"trend"`) {
		t.Fatal("the fixture's reviews median isn't hidden: the median cases test nothing")
	}
}

// Each case names the check (routes.go) it must trip; want is a piece of
// that check's message, so a case passing on another check fails.
func TestRouteCheckCatches(t *testing.T) {
	type tc struct {
		want string
		run  func(t *testing.T, c *routeCase) error
	}
	an := func(f func(t *testing.T, c *routeCase) []byte) func(t *testing.T, c *routeCase) error {
		return func(t *testing.T, c *routeCase) error { return c.p.AddAnalytics(f(t, c)) }
	}
	se := func(id int, m string, f func(t *testing.T, raw []byte) []byte) func(t *testing.T, c *routeCase) error {
		return func(t *testing.T, c *routeCase) error {
			return c.p.AddSeries(f(t, c.series(t, c.ids[id], m)), calendar.Month)
		}
	}
	cases := map[string]tc{
		// readGuarded, through a median.
		"a value not {value, suppressed}": {"not {value, suppressed}", an(func(t *testing.T, c *routeCase) []byte {
			return replace(t, c.analytics(t), `"median":{"questions":{"value":24,"suppressed":false}`, `"median":{"questions":{"value":24,"suppressed":false,"x":1}`, false)
		})},
		"a hidden value with a value": {"hidden with a value", an(func(t *testing.T, c *routeCase) []byte {
			return replace(t, c.analytics(t), `"median":{"questions":{"value":24,"suppressed":false}`, `"median":{"questions":{"value":24,"suppressed":true}`, false)
		})},
		// checkMedian.
		"a median over no shown value": {"over no shown value", an(func(t *testing.T, c *routeCase) []byte {
			return replace(t, c.analytics(t), `"agreement_rate":{"value":null,"suppressed":false}`, `"agreement_rate":{"value":0.5000,"suppressed":false}`, true)
		})},
		"a count median between 0 and 5 sent shown": {"a count median of 2.5", an(func(t *testing.T, c *routeCase) []byte {
			return replace(t, c.analytics(t), `"reviews":{"value":null,"suppressed":true},"agreement_rate"`, `"reviews":{"value":2.5,"suppressed":false},"agreement_rate"`, true)
		})},
		"a ratio median that isn't the shown values'": {"the shown values give 2", an(func(t *testing.T, c *routeCase) []byte {
			return replace(t, c.analytics(t), `"questions_per_clinician":{"value":2.00,"suppressed":false}}`, `"questions_per_clinician":{"value":2.50,"suppressed":false}}`, true)
		})},
		"a count median that isn't the shown values'": {"the shown values give 24", an(func(t *testing.T, c *routeCase) []byte {
			return replace(t, c.analytics(t), `"median":{"questions":{"value":24,`, `"median":{"questions":{"value":30,`, false)
		})},
		// strictObject and the top level.
		"not JSON": {"not an object", an(func(t *testing.T, c *routeCase) []byte { return []byte(`[1]`) })},
		"an extra top-level key": {"keys of", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { m["extra"] = 1 })
		})},
		"a top-level key missing": {"no hospitals", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { m["hospital5"] = m["hospitals"]; delete(m, "hospitals") })
		})},
		"a period that isn't a kind": {"not a finished reporting period", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { m["period"] = "30d" })
		})},
		"alerts not null": {"alerts", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { m["alerts"] = map[string]any{} })
		})},
		"changes not unavailable": {"changes", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { m["changes"] = map[string]any{"unavailable": false} })
		})},
		"a period that isn't a string": {"cannot unmarshal", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { m["period"] = 1 })
		})},
		"rows that aren't a list": {"cannot unmarshal", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { obj(m, "use")["rows"] = map[string]any{} })
		})},
		"a hospital that isn't an object": {"cannot unmarshal", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { obj(m, "use", "rows", 0)["hospital"] = 1 })
		})},
		"a trend that isn't a list": {"cannot unmarshal", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { obj(m, "use")["trend"] = map[string]any{} })
		})},
		// Not yet.
		"one bound null": {"one bound null", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { m["to"] = nil })
		})},
		"not yet while periods are stored": {"not yet, with 3 stored", an(func(t *testing.T, c *routeCase) []byte {
			out, _ := analytics.Render(calendar.Month, nil, renderEpoch)
			raw, _ := json.Marshal(out)
			return raw
		})},
		"a not-yet area that isn't": {"not yet: use", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.notYet(t), func(m map[string]any) { m["use"] = map[string]any{"unavailable": true} })
		})},
		"a first_from a quarter late": {"first_from", an(func(t *testing.T, c *routeCase) []byte {
			return replace(t, c.notYet(t), `"first_from":"2026-04-01T00:00:00.000Z"`, `"first_from":"2026-07-01T00:00:00.000Z"`, false)
		})},
		"a first_from before the epoch": {"first_from", an(func(t *testing.T, c *routeCase) []byte {
			return replace(t, c.notYet(t), `"first_from":"2026-04-01T00:00:00.000Z"`, `"first_from":"2026-01-01T00:00:00.000Z"`, false)
		})},
		"a first_from not a period start": {"first_from", an(func(t *testing.T, c *routeCase) []byte {
			return replace(t, c.notYet(t), `"first_from":"2026-04-01T00:00:00.000Z"`, `"first_from":"2026-04-02T00:00:00.000Z"`, false)
		})},
		"not yet with no epoch known": {"no epoch", func(t *testing.T, c *routeCase) error {
			c.p.Epoch = time.Time{}
			return c.p.AddAnalytics(c.notYet(t))
		}},
		// The period.
		"bounds that aren't a period": {"bounds", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { m["to"] = "2026-06-30T00:00:00.000Z" })
		})},
		"an older period served as the latest": {"is not the newest stored", an(func(t *testing.T, c *routeCase) []byte {
			out, _ := analytics.Render(calendar.Month, c.rows[:2], renderEpoch)
			raw, _ := json.Marshal(out)
			return raw
		})},
		// Areas and rows.
		"one cell sent two ways": {"sent as", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) {
				obj(m, "hospitals", "rows", 0, "size")["questions"] = map[string]any{"value": 13, "suppressed": false}
			})
		})},
		"an area's keys": {"use: no trend", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { u := obj(m, "use"); u["trendX"] = u["trend"]; delete(u, "trend") })
		})},
		"a row's keys": {"use row", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { obj(m, "use", "rows", 0)["x"] = 1 })
		})},
		"a row twice": {"twice", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { u := obj(m, "use"); u["rows"] = append(arr(u, "rows"), arr(u, "rows")[0]) })
		})},
		"the confidence keys": {"quality confidence", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { delete(obj(m, "quality", "rows", 0, "confidence"), "low") })
		})},
		"a cell that differs": {"sent {", an(func(t *testing.T, c *routeCase) []byte {
			return replace(t, c.analytics(t), `"questions":{"value":12,`, `"questions":{"value":13,`, false)
		})},
		"a hidden cell sent shown": {"sent {", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) {
				obj(m, "quality", "rows", 2, "confidence")["low"] = map[string]any{"value": 15, "suppressed": false}
			})
		})},
		"a stored named row left out": {"not sent", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { u := obj(m, "use"); u["rows"] = arr(u, "rows")[1:] })
		})},
		"a row the body doesn't name": {"isn't a named stored one", an(func(t *testing.T, c *routeCase) []byte {
			return replace(t, c.analytics(t), `"name":"Synthetic A"`, `"name":"Synthetic Z"`, false)
		})},
		// Medians.
		"a median's keys": {"use median", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { delete(obj(m, "use", "median"), "questions") })
		})},
		"the confidence median's keys": {`keys of {"high"`, an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { delete(obj(m, "quality", "median", "confidence"), "low") })
		})},
		"a confidence median that isn't the shown values'": {"quality high median", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) {
				obj(m, "quality", "median", "confidence")["high"] = map[string]any{"value": 9, "suppressed": false}
			})
		})},
		// Trends (checkPoints).
		"a trend point over two periods": {"point 2", an(func(t *testing.T, c *routeCase) []byte {
			return replace(t, c.analytics(t), `"value":102,`, `"value":203,`, false)
		})},
		"a trend point too many": {"4 points for 3 stored periods", an(func(t *testing.T, c *routeCase) []byte {
			return replace(t, c.analytics(t), `"trend":[`, `"trend":[{"start":"2026-03-01T00:00:00.000Z","value":{"value":0,"suppressed":false}},`, false)
		})},
		"a trend point's keys": {"keys of", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) { arr(obj(m, "use"), "trend")[0].(map[string]any)["median"] = 1 })
		})},
		"a trend point on another start": {"starts", an(func(t *testing.T, c *routeCase) []byte {
			return edit(t, c.analytics(t), func(m map[string]any) {
				arr(obj(m, "quality"), "trend")[0].(map[string]any)["start"] = "2026-04-02T00:00:00.000Z"
			})
		})},
		// Series.
		"a series that isn't an object": {"not an object", func(t *testing.T, c *routeCase) error {
			return c.p.AddSeries([]byte(`[]`), calendar.Month)
		}},
		"a series measure not served": {"measure", se(0, "needs_review_share", func(t *testing.T, raw []byte) []byte {
			return replace(t, raw, `"measure":"needs_review_share"`, `"measure":"questions"`, false)
		})},
		"a series hospital name mismatch": {"by that name", se(0, "needs_review_share", func(t *testing.T, raw []byte) []byte {
			return replace(t, raw, `"name":"Synthetic A"`, `"name":"Synthetic Z"`, false)
		})},
		"a series point shown where hidden": {"stored {", se(2, "needs_review_share", func(t *testing.T, raw []byte) []byte {
			return replace(t, raw, `"value":{"value":null,"suppressed":true}`, `"value":{"value":0.5000,"suppressed":false}`, false)
		})},
		"a series median over a hidden value": {"the other hospitals' shown values give", se(0, "needs_review_share", func(t *testing.T, raw []byte) []byte {
			return replace(t, raw, `"median":0.5000`, `"median":0.4000`, false)
		})},
		"a series median sent null over shown values": {"the other hospitals' shown values give", se(0, "questions_per_clinician", func(t *testing.T, raw []byte) []byte {
			return replace(t, raw, `"median":2.00`, `"median":null`, false)
		})},
		"a series hospital that isn't an object": {"cannot unmarshal", se(0, "needs_review_share", func(t *testing.T, raw []byte) []byte {
			return edit(t, raw, func(m map[string]any) { m["hospital"] = 1 })
		})},
		"a series median that isn't a number": {"cannot unmarshal", se(0, "needs_review_share", func(t *testing.T, raw []byte) []byte {
			return replace(t, raw, `"median":0.5000`, `"median":"0.5"`, false)
		})},
		"a series point missing": {"2 points for 3", se(0, "needs_review_share", func(t *testing.T, raw []byte) []byte {
			return edit(t, raw, func(m map[string]any) { m["points"] = m["points"].([]any)[1:] })
		})},
	}
	for name, k := range cases {
		err := k.run(t, newRouteCase(t, false))
		if err == nil || !strings.Contains(err.Error(), k.want) {
			t.Errorf("%s: got %v, want an error with %q", name, err, k.want)
		}
	}
	// A series median with no shown value: B's share hidden too, so A's
	// median is null; sent as a number, it is caught.
	c := newRouteCase(t, true)
	raw := c.series(t, c.ids[0], "needs_review_share")
	if !bytes.Contains(raw, []byte(`"median":null`)) {
		t.Fatalf("no null median: %s", raw)
	}
	err := c.p.AddSeries(replace(t, raw, `"median":null`, `"median":0.5000`, false), calendar.Month)
	if err == nil || !strings.Contains(err.Error(), "over no shown value") {
		t.Errorf("a series median with no shown value: %v", err)
	}
}
