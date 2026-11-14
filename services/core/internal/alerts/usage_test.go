package alerts_test

import (
	"encoding/json"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/alerts"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// Task 41: every breakdown of the alert counts keeps the small-group rule:
// no shown count is 1 to 4, a breakdown with a hidden part hides at least
// 5 together (or the total), and the total is hidden everywhere once any
// breakdown hides it.
func TestUsageSmallGroupsSuppressed(t *testing.T) {
	cases := map[string]alerts.UsageCounts{
		"two small severities": {Total: 12,
			Severity: map[string]int64{"critical": 7, "urgent": 2, "info": 3},
			Category: map[string]int64{"blood_chemistry": 12},
			Route:    map[string]int64{"queue": 9, "named": 3},
			Handling: map[string]int64{"open": 6, "acknowledged": 5, "dismissed": 1}},
		"one small part": {Total: 23,
			Severity: map[string]int64{"critical": 22, "urgent": 1},
			Category: map[string]int64{"blood_sugar": 11, "vital_signs": 12},
			Route:    map[string]int64{"queue": 23},
			Handling: map[string]int64{"open": 23}},
		"small total": {Total: 3,
			Severity: map[string]int64{"critical": 3},
			Category: map[string]int64{"blood_sugar": 3},
			Route:    map[string]int64{"named": 3},
			Handling: map[string]int64{"withdrawn": 3}},
		"none": {Severity: map[string]int64{}, Category: map[string]int64{}, Route: map[string]int64{}, Handling: map[string]int64{}},
	}
	for name, c := range cases {
		out := alerts.GuardUsage(c)
		for bname, b := range map[string]map[string]smallgroup.Guarded{"severity": out.BySeverity, "category": out.ByCategory,
			"route": out.ByRoute, "handling": out.ByHandling} {
			raw := map[string]map[string]int64{"severity": c.Severity, "category": c.Category, "route": c.Route,
				"handling": c.Handling}[bname]
			var hiddenSum int64
			hidden := 0
			for k, g := range b {
				if g.Shown() {
					if v := *g.Value; (v >= 1 && v < smallgroup.Min) || v != raw[k] {
						t.Errorf("%s %s %s: shown %d (raw %d)", name, bname, k, v, raw[k])
					}
					continue
				}
				hidden++
				hiddenSum += raw[k]
			}
			if out.Total.Shown() && hidden > 0 && (hidden == 1 || hiddenSum < smallgroup.Min) {
				t.Errorf("%s %s: %d hidden summing %d with the total shown", name, bname, hidden, hiddenSum)
			}
		}
		if (c.Total >= 1 && c.Total < smallgroup.Min) && out.Total.Shown() {
			t.Errorf("%s: a small total shown", name)
		}
		js, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		var back map[string]any
		if err := json.Unmarshal(js, &back); err != nil {
			t.Fatal(err)
		}
	}
	// The small total is hidden in every breakdown, not shown by another.
	small := alerts.GuardUsage(cases["small total"])
	if small.Total.Shown() || small.ByRoute["named"].Shown() || small.BySeverity["critical"].Shown() {
		t.Error("a small total leaks through a breakdown")
	}
	if g := alerts.GuardUsage(cases["two small severities"]); !g.Total.Shown() || *g.Total.Value != 12 ||
		g.BySeverity["urgent"].Shown() || g.BySeverity["info"].Shown() || !g.BySeverity["critical"].Shown() {
		t.Errorf("two small severities: %+v", g.BySeverity)
	}
}
