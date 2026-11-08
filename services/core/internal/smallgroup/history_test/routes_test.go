package historytest

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// The JSON-side check of the analytics routes catches a response that
// isn't the stored bodies: a trend point that totals two periods, a series
// point shown where the body hides it, a median over a hidden value, a
// cell that differs, a row the body doesn't name. Fixed dates only.

type routeCase struct {
	p      *Parser
	months []calendar.Period
	rows   []analytics.Snapshot
	ids    []string
}

func row(n int64, hideEsc bool) analytics.Row {
	r := analytics.Row{Questions: smallgroup.Count(12 * n), Answers: smallgroup.Count(10 * n), Pass: smallgroup.Count(5 * n),
		Regenerate: smallgroup.Count(0), Escalate: smallgroup.Count(5 * n), Degraded: smallgroup.Count(0),
		PassShare: smallgroup.Share(5*n, 10*n), RegenerateShare: smallgroup.Share(0, 10*n), EscalateShare: smallgroup.Share(5*n, 10*n),
		Reviews: smallgroup.Count(0), AgreementRate: smallgroup.GuardedShare{}, ActiveClinicians: smallgroup.Count(6 * n),
		QuestionsPerClinician: analytics.RatioOf(2), MedianReviewHours: analytics.Hours{Number: analytics.Number{Suppressed: true}}}
	if hideEsc {
		r.Escalate, r.Pass = smallgroup.Guarded{Suppressed: true}, smallgroup.Guarded{Suppressed: true}
		r.EscalateShare, r.PassShare = smallgroup.GuardedShare{Suppressed: true}, smallgroup.GuardedShare{Suppressed: true}
	}
	return r
}

func newRouteCase(t *testing.T) *routeCase {
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
	for i, m := range calendar.Children(calendar.Of(calendar.Quarter, start)) {
		from, to := analytics.Dates(m)
		all := row(4, false)
		all.Questions = smallgroup.Count(int64(100 + i))
		b := analytics.Body{Kind: "month", From: from, To: to, All: all, Hospitals: []analytics.HospitalRow{
			{Hospital: analytics.Ref{ID: c.ids[0], Name: "Synthetic A"}, Staff: analytics.StaffFigure(9), Row: row(1, false)},
			{Hospital: analytics.Ref{ID: c.ids[1], Name: "Synthetic B"}, Staff: analytics.StaffFigure(9), Row: row(2, false)},
			{Hospital: analytics.Ref{ID: c.ids[2], Name: "Synthetic C"}, Staff: analytics.StaffFigure(9), Row: row(3, true)},
		}}
		raw, _ := json.Marshal(b)
		if err := c.p.Add("stored", raw, m); err != nil {
			t.Fatal(err)
		}
		c.months = append(c.months, m)
		c.rows = append(c.rows, analytics.Snapshot{Period: m, State: analytics.StateOK, Body: raw, ComputedAt: m.To.Add(time.Hour)})
	}
	return c
}

func (c *routeCase) analytics(t *testing.T) []byte {
	t.Helper()
	out, _ := analytics.Render(calendar.Month, c.rows, time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC))
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

func TestRouteCheckAcceptsTheRoutes(t *testing.T) {
	c := newRouteCase(t)
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

func TestRouteCheckCatches(t *testing.T) {
	cases := map[string]func(c *routeCase) error{
		// June's trend point totals May and June (101 + 102).
		"a trend point over two periods": func(c *routeCase) error {
			raw := bytes.Replace(c.analytics(t), []byte(`"value":102,`), []byte(`"value":203,`), 1)
			return c.p.AddAnalytics(raw)
		},
		// A point for a period that isn't stored.
		"a trend point too many": func(c *routeCase) error {
			raw := bytes.Replace(c.analytics(t), []byte(`"trend":[`), []byte(`"trend":[{"start":"2026-03-01T00:00:00.000Z","value":{"value":0,"suppressed":false}},`), 1)
			return c.p.AddAnalytics(raw)
		},
		// C's needs-review share shown where the body hides it.
		"a series point shown where hidden": func(c *routeCase) error {
			raw := bytes.Replace(c.series(t, c.ids[2], "needs_review_share"), []byte(`"value":{"value":null,"suppressed":true}`),
				[]byte(`"value":{"value":0.5000,"suppressed":false}`), 1)
			return c.p.AddSeries(raw, calendar.Month)
		},
		// A's median taken over C's hidden share too.
		"a series median over a hidden value": func(c *routeCase) error {
			raw := bytes.Replace(c.series(t, c.ids[0], "needs_review_share"), []byte(`"median":0.5000`), []byte(`"median":0.4000`), 1)
			return c.p.AddSeries(raw, calendar.Month)
		},
		"a cell that differs": func(c *routeCase) error {
			raw := bytes.Replace(c.analytics(t), []byte(`"questions":{"value":12,`), []byte(`"questions":{"value":13,`), 1)
			return c.p.AddAnalytics(raw)
		},
		// The use median of questions (12, 24, 36) sent as another value.
		"a median that isn't the shown values'": func(c *routeCase) error {
			raw := bytes.Replace(c.analytics(t), []byte(`"median":{"questions":{"value":24,`), []byte(`"median":{"questions":{"value":30,`), 1)
			return c.p.AddAnalytics(raw)
		},
		"a row the body doesn't name": func(c *routeCase) error {
			raw := bytes.Replace(c.analytics(t), []byte(`"name":"Synthetic A"`), []byte(`"name":"Synthetic Z"`), 1)
			return c.p.AddAnalytics(raw)
		},
		"an older period served as the latest": func(c *routeCase) error {
			out, _ := analytics.Render(calendar.Month, c.rows[:2], time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC))
			raw, _ := json.Marshal(out)
			return c.p.AddAnalytics(raw)
		},
	}
	for name, f := range cases {
		if err := f(newRouteCase(t)); err == nil {
			t.Errorf("%s: not caught", name)
		}
	}
}
