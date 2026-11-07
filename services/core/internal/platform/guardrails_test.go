package platform

import (
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// R43: a rolling window ends at the start of the UTC day 15 minutes before
// now.
func TestGuardWindowGrace(t *testing.T) {
	day := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		now  time.Time
		want time.Time
	}{
		{day.Add(5 * time.Minute), day.AddDate(0, 0, -1)},
		{day.Add(14*time.Minute + 59*time.Second), day.AddDate(0, 0, -1)},
		{day.Add(15 * time.Minute), day},
		{day.Add(23 * time.Hour), day},
	} {
		from, to := guardWindow(c.now, 30)
		if !to.Equal(c.want) || !from.Equal(c.want.AddDate(0, 0, -30)) {
			t.Errorf("at %v: from %v to %v, want to %v", c.now, from, to, c.want)
		}
	}
}

// A9 (unit): the guardrails render is the body's QG cells as stored, shown
// and hidden alike; an unnamed (fully hidden) row is left out.
func TestGuardrailsFromBody(t *testing.T) {
	p := calendar.Of(calendar.Month, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	from, to := analytics.Dates(p)
	h := smallgroup.Guarded{Suppressed: true}
	row := analytics.Row{Answers: smallgroup.Count(30), Pass: smallgroup.Count(20), Regenerate: h, Escalate: h, Degraded: smallgroup.Count(0),
		PassShare: smallgroup.Share(20, 30), RegenerateShare: smallgroup.GuardedShare{Suppressed: true}}
	b := analytics.Body{Kind: "month", From: from, To: to, All: row, Hospitals: []analytics.HospitalRow{
		{Hospital: analytics.Ref{ID: uuid.NewString(), Name: "Synthetic North"}, Row: row},
		{Hospital: analytics.Ref{ID: uuid.NewString(), Name: ""}},
	}}
	computed := time.Date(2026, 11, 1, 0, 20, 0, 0, time.UTC)
	out := GuardrailsFromBody(b, p, computed)
	if out.Period != "month" || out.From != "2026-10-01T00:00:00.000Z" || out.To != "2026-11-01T00:00:00.000Z" || out.UpdatedAt != "2026-11-01T00:20:00.000Z" {
		t.Errorf("bounds %+v", out)
	}
	if len(out.Hospitals) != 1 {
		t.Fatalf("%d rows, want the named one", len(out.Hospitals))
	}
	for _, r := range []Rates{out.All, out.Hospitals[0].Rates} {
		if r.Answers != row.Answers || r.Pass != row.Pass || r.Regenerate != row.Regenerate || r.Escalate != row.Escalate ||
			r.Degraded != row.Degraded || r.PassShare != row.PassShare || r.RegenerateShare != row.RegenerateShare {
			t.Errorf("cells differ from the body: %+v", r)
		}
	}
	raw, _ := json.Marshal(HiddenRates())
	if regexp.MustCompile(`"value":[^n]`).Match(raw) || regexp.MustCompile(`"suppressed":false`).Match(raw) {
		t.Errorf("a hidden row shows something: %s", raw)
	}
}

// ADR 0012: the rolling values sunset at least 90 days after their release.
func TestRollingSunsetNotice(t *testing.T) {
	if RollingSunset.Sub(RollingDeprecatedAt) < 90*24*time.Hour {
		t.Errorf("sunset %v is less than 90 days after %v", RollingSunset, RollingDeprecatedAt)
	}
	if GuardrailsGrace != 15*time.Minute {
		t.Errorf("grace %v", GuardrailsGrace)
	}
}
