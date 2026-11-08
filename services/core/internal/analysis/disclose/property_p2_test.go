package disclose

import (
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose/attack"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// P2 (design §5.7, D6): every context of a period at the cap, on small
// dimensions (2 conditions, 2 age bands and null, 2 sexes, 2 locations in
// 2 settings: 47 contexts), guarded parents first as the orchestrator
// will. The attacker writes every number published, across all contexts
// and views, over the atoms (the full cross-classification).
//
//   - Asserted, within each family (a context and its filter subsets): no
//     hidden cell of 1 or more can be solved, and every group has one state.
//   - Reported, across all families of the period: the share of hidden
//     cells of 1 or more that can be solved. That is R-A1.
//   - Reported, the drill-capable attacker: hidden subgroups of 1 to 4 that
//     the rows of shown groups of 5 or more pick out (the §5.2 trade).
func TestP2FamilyExhaustive(t *testing.T) {
	var hidden, solvable, small, drilled, families int
	for _, seed := range seeds(100, 6, 2) {
		periods := []calendar.Period{octWeek}
		blocks := []calendar.Period{octWeek}
		if long() {
			blocks = octBlocksOf()
			periods = append(periods, oct2026)
		}
		w := attack.Generate(seed, smallDims(2), blocks, false, attack.Options{Subjects: 140, Presence: 0.9, Rare: 0.6})
		sets := filterSets(w.Dims)
		for _, p := range periods {
			bodies := guardFamily(t, &w, p, sets)
			docs, idx := docsOf(t, bodies, sets)
			m, err := attack.New(&w, docs)
			if err != nil {
				t.Fatal(err)
			}
			for _, x := range m.Mismatches() {
				t.Errorf("seed %d %v: %s", seed, p, x)
			}
			for _, f := range sets {
				fam := familyDocs(f, idx)
				families++
				for _, i := range m.Exposed(fam) {
					c := m.Cells[i]
					t.Errorf("seed %d %v family %v: hidden %s (doc %d, %d) is solvable", seed, p, f, c.Name, c.Doc, c.Truth)
				}
				for _, x := range m.OneState(fam) {
					t.Errorf("seed %d %v family %v: %s", seed, p, f, x)
				}
			}
			var all []int
			for i := range docs {
				all = append(all, i)
			}
			hidden += len(m.HiddenProtected(all))
			solvable += len(m.Exposed(all))
			e, s := m.DrillExposed(all)
			drilled, small = drilled+e, small+s
		}
	}
	share := func(a, b int) float64 {
		if b == 0 {
			return 0
		}
		return float64(a) / float64(b)
	}
	t.Logf("P2: %d families checked, none leaks within itself", families)
	t.Logf("R-A1: %d of %d hidden cells of 1 or more solvable across families (%.2f%%)", solvable, hidden, 100*share(solvable, hidden))
	t.Logf("drill run: %d of %d hidden groups of 1 to 4 picked out by the rows of shown groups of 5 or more (%.2f%%)", drilled, small, 100*share(drilled, small))
}
