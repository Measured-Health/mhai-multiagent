package disclose

import (
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose/attack"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// P2 (design §5.7, D5, ruling C10): every context of a period at the cap,
// guarded parents first as the orchestrator will. The attacker writes every
// number published, across all contexts and views, over the atoms (the full
// cross-classification), and counts hidden groups (distinct boxes, each
// once however many cells publish it):
//
//   - asserted, within each family and across the whole period: no hidden
//     group of 1 to 4 can be solved, and every group has one state;
//   - reported: hidden groups of 1 to 4 and secondary ones (5 or more), and
//     how many of each the period determines (R-A1), and the drill-capable
//     attacker's subgroups of 1 to 4 (the §5.2 trade, R-A4).
type p2Counts struct {
	families, small, smallExposed, secondary, secondaryExposed, drilled, drillSmall int
}

func (c *p2Counts) log(t *testing.T, name string) {
	t.Helper()
	pct := func(a, b int) float64 {
		if b == 0 {
			return 0
		}
		return 100 * float64(a) / float64(b)
	}
	t.Logf("%s: %d families", name, c.families)
	t.Logf("%s R-A1: hidden groups of 1–4 solvable %d of %d (%.2f%%); secondary (5+) solvable %d of %d (%.2f%%)",
		name, c.smallExposed, c.small, pct(c.smallExposed, c.small), c.secondaryExposed, c.secondary, pct(c.secondaryExposed, c.secondary))
	t.Logf("%s drill run: %d of %d hidden groups of 1–4 picked out by drill rows (%.2f%%)", name, c.drilled, c.drillSmall, pct(c.drilled, c.drillSmall))
}

// p2Period guards every context of p in w and runs the attacker on it.
func p2Period(t *testing.T, w *attack.World, p calendar.Period, c *p2Counts) {
	t.Helper()
	sets := filterSets(w.Dims)
	bodies := guardPeriodAll(t, w, p, sets)
	docs, idx := docsOf(t, bodies, sets)
	m, err := attack.New(w, docs)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range m.Mismatches() {
		t.Errorf("%v: %s", p, x)
	}
	for _, x := range m.ContractViolations() {
		t.Errorf("%v: %s", p, x)
	}
	for _, f := range sets {
		fam := familyDocs(f, idx)
		c.families++
		for _, i := range m.Exposed(fam) {
			x := m.Cells[i]
			if x.Truth <= 4 {
				t.Errorf("%v family %v: hidden %s (doc %d, %d) is solvable", p, f, x.Name, x.Doc, x.Truth)
			}
		}
	}
	var all []int
	for i := range docs {
		all = append(all, i)
	}
	for _, x := range m.OneState(all) {
		t.Errorf("%v: one state across the period: %s", p, x)
	}
	s, se, sec, sece := m.GroupExposure(all)
	c.small, c.smallExposed, c.secondary, c.secondaryExposed = c.small+s, c.smallExposed+se, c.secondary+sec, c.secondaryExposed+sece
	if se > 0 {
		t.Errorf("%v: %d hidden groups of 1 to 4 solvable across the period's families", p, se)
	}
	e, sm := m.DrillExposed(all)
	c.drilled, c.drillSmall = c.drilled+e, c.drillSmall+sm
}

// Small dimensions, many worlds (and, at full size, the month as well).
func TestP2FamilyExhaustive(t *testing.T) {
	var c p2Counts
	for _, seed := range seeds(100, 6, 2) {
		periods := []calendar.Period{octWeek}
		blocks := []calendar.Period{octWeek}
		if long() {
			blocks = octBlocksOf()
			periods = append(periods, oct2026)
		}
		w := attack.Generate(seed, smallDims(2), blocks, false, attack.Options{Subjects: 140, Presence: 0.9, Rare: 0.6})
		for _, p := range periods {
			p2Period(t, &w, p, &c)
		}
	}
	c.log(t, "P2 small")
}

// realDims are a hospital's real dimension sizes for the patient's
// attributes: every age band and null, every sex code, a location in each
// setting, two specialties; three conditions and a finding.
func realDims() attack.Dims {
	return attack.Dims{
		Conditions:  []string{"c1", "c2", "c3"},
		Ages:        []string{"0-17", "18-39", "40-64", "65-79", "80+", ""},
		Sexes:       []string{"female", "male", "other", "not_recorded"},
		Locations:   []attack.Location{{ID: "L1", Setting: "admitted"}, {ID: "L2", Setting: "outpatient"}, {ID: "L3", Setting: "emergency"}},
		Specialties: []string{"S1", "S2"},
		Sources:     []string{"X1"},
		Findings:    []attack.Finding{{Name: "Hb", Flag: "low"}},
	}
}

// Realistic worlds: real dimension sizes, hundreds of patients, several
// weeks, every week's every context. Too large for -race; CI runs it in
// the analysis property step.
func TestP2Realistic(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("realistic size: run without -race (CI's analysis property step)")
	}
	var c p2Counts
	weeks := octBlocksOf()[:3]
	for _, seed := range seeds(10, 1, 1) {
		w := attack.Generate(seed, realDims(), weeks, false, attack.Options{Subjects: 700, Presence: 0.5, Rare: 0.5})
		for _, p := range weeks {
			p2Period(t, &w, p, &c)
		}
	}
	c.log(t, "P2 realistic")
}
