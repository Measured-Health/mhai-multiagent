package disclose

import (
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose/attack"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// guardLedger guards every context of p parents first, as the orchestrator
// does: one Ledger for the period, each body added once published.
func guardLedger(t *testing.T, w *attack.World, p calendar.Period, sets []Filters) (map[Filters]Body, time.Duration) {
	t.Helper()
	out := map[Filters]Body{}
	l := NewLedger()
	var worst time.Duration
	for n := 0; n <= FilterCap; n++ {
		for _, f := range sets {
			if f.Count() != n {
				continue
			}
			start := time.Now()
			b, err := l.Guard(rawOf(w, p, f, false), nil)
			if err != nil {
				t.Fatalf("%v: %v", f, err)
			}
			l.Add(b)
			if d := time.Since(start); d > worst {
				worst = d
			}
			out[f] = b
		}
	}
	return out, worst
}

// P2 through the kept record (PR 3 review, condition 3): the incremental
// Ledger, sources included, exposes nothing of 1 to 4 across the period.
func TestP2LedgerIncremental(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("every context of a period: run without -race (CI's analysis property step)")
	}
	for _, d := range []attack.Dims{smallDims(2), sourceDims()} {
		for _, seed := range seeds(20, 1, 1) {
			w := attack.Generate(seed, d, []calendar.Period{octWeek}, false, attack.Options{Subjects: 120, Presence: 0.9, Rare: 0.6})
			sets := filterSets(w.Dims)
			bodies, _ := guardLedger(t, &w, octWeek, sets)
			docs, _ := docsOf(t, bodies, sets)
			m, err := attack.New(&w, docs)
			if err != nil {
				t.Fatal(err)
			}
			var all []int
			for i := range docs {
				all = append(all, i)
			}
			if _, se, _, _ := m.GroupExposure(all); se > 0 {
				t.Errorf("seed %d: %d hidden groups of 1 to 4 solvable through the ledger", seed, se)
			}
			for _, x := range m.OneState(all) {
				t.Errorf("seed %d: one state: %s", seed, x)
			}
		}
	}
}

// The ledger records each context once, and a recorded body is known to
// the next guard: a group hidden in it is hidden in its sub-context.
func TestLedgerAddOnceAndKnown(t *testing.T) {
	w := sexWorld(map[string]int{"female": 200, "male": 12, "other": 3})
	l := NewLedger()
	top, err := l.Guard(rawOf(&w, octWeek, Filters{}, false), nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Add(top)
	l.Add(top)
	if l.Len() != 1 || !l.Has(top) {
		t.Fatalf("ledger holds %d bodies", l.Len())
	}
	var male Filters
	male[DimSex] = "male"
	b, err := l.Guard(rawOf(&w, octWeek, male, false), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Whole {
		t.Errorf("sex=male shows %+v though the recorded parent hides it", b.Patients)
	}
}

// The guard's cost with the kept record (condition 3), reported: realistic
// dimensions, every context of a week guarded through one ledger. A guard
// over the cold budget makes the request answer 503 not_ready while the
// engine finishes it (TestColdFamilyBudget); the numbers go in the PR.
func TestLedgerCostPerContext(t *testing.T) {
	if !long() || raceEnabled {
		t.Skip("ANALYSIS_LONG=1, without -race: the guard's cost, reported")
	}
	w := attack.Generate(1, realDims(), octBlocksOf()[:1], false, attack.Options{Subjects: 700, Presence: 0.5, Rare: 0.5})
	sets := filterSets(w.Dims)
	start := time.Now()
	_, worst := guardLedger(t, &w, octWeek, sets)
	t.Logf("ledger: %d contexts in %v, worst %v", len(sets), time.Since(start), worst)
}
