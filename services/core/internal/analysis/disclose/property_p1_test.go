package disclose

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose/attack"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// seeds is how many seeded worlds a property runs: ANALYSIS_SEED runs one;
// ANALYSIS_LONG=1 the full size; fewer under -race or -short.
func seeds(full, normal, small int) []uint64 {
	if s := os.Getenv("ANALYSIS_SEED"); s != "" {
		n, err := strconv.ParseUint(s, 10, 64)
		if err == nil {
			return []uint64{n}
		}
	}
	n := normal
	switch {
	case long():
		n = full
	case testing.Short() || raceEnabled:
		n = small
	}
	out := make([]uint64, n)
	for i := range out {
		out[i] = uint64(i + 1)
	}
	return out
}

// filterSets are every filter set within the cap over dims' filterable
// values (none of them null).
func filterSets(d attack.Dims) []Filters {
	type fv struct {
		dim int
		v   string
	}
	var vals []fv
	for _, c := range d.Conditions {
		vals = append(vals, fv{DimCondition, c})
	}
	for _, a := range d.Ages {
		if a != "" {
			vals = append(vals, fv{DimAgeBand, a})
		}
	}
	for _, s := range d.Sexes {
		vals = append(vals, fv{DimSex, s})
	}
	for _, l := range d.Locations {
		vals = append(vals, fv{DimLocation, l.ID}, fv{DimSetting, l.Setting})
	}
	seen := map[Filters]bool{}
	out := []Filters{{}}
	for i, a := range vals {
		var f Filters
		f[a.dim] = a.v
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
		for _, b := range vals[i+1:] {
			if b.dim == a.dim {
				continue
			}
			g := f
			g[b.dim] = b.v
			if !seen[g] {
				seen[g] = true
				out = append(out, g)
			}
		}
	}
	return out
}

// checkOne guards one context on its own and runs the attacker on it.
func checkOne(t *testing.T, w *attack.World, p calendar.Period, f Filters, first bool) (hidden, exposed int) {
	t.Helper()
	raw := rawOf(w, p, f, first)
	b, err := GuardResponse(raw, nil)
	if err != nil {
		t.Fatalf("%v %v: %v", p, f, err)
	}
	doc, err := docOf(b)
	if err != nil {
		t.Fatal(err)
	}
	m, err := attack.New(w, []attack.Doc{doc})
	if err != nil {
		t.Fatalf("%v %v: %v", p, f, err)
	}
	for _, x := range m.Mismatches() {
		t.Errorf("%v %v: %s", p, f, x)
	}
	for _, x := range m.OneState([]int{0}) {
		t.Errorf("%v %v: one state: %s", p, f, x)
	}
	ex := m.Exposed([]int{0})
	for _, i := range ex {
		t.Errorf("%v %v: hidden %s (%d) is solvable", p, f, m.Cells[i].Name, m.Cells[i].Truth)
	}
	for _, x := range m.SetViolations([]int{0}) {
		t.Errorf("%v %v: %s", p, f, x)
	}
	return len(m.HiddenProtected([]int{0})), len(ex)
}

// P1 (design §5.7): one response, every view at once, at each level and
// filter sets within the cap, on random worlds biased to small groups: no
// hidden cell holding 1 or more can be solved from the response's own
// equations (sets, partitions, the two tables, the series, cross-view
// equalities, shares beside their denominators, the definitions' zeros),
// no set's hidden members break the whole-set rule, and every shown value
// is what the attacker's own definitions count on the world.
func TestP1NoHiddenCellSolvable(t *testing.T) {
	octBlocks := attack.Blocks(calendar.Of(calendar.Week, day("2026-10-01")), calendar.Of(calendar.Week, day("2026-10-31")))
	q4Blocks := attack.Blocks(calendar.Of(calendar.Week, day("2026-10-01")), calendar.Of(calendar.Week, day("2026-12-31")))
	hidden := 0
	for _, seed := range seeds(2000, 20, 3) {
		t.Run(fmt.Sprint("seed=", seed), func(t *testing.T) {
			r := rand.New(rand.NewPCG(seed, 1))
			w := attack.Generate(seed, smallDims(3), octBlocks, false, attack.Options{Subjects: 20 + r.IntN(60), Presence: 0.35, Rare: 0.8})
			// The unfiltered context and random filter sets within the cap:
			// at full size, 2,000 worlds × 6 sets reach each of the 79 sets
			// about 150 times.
			sets := filterSets(w.Dims)
			r.Shuffle(len(sets)-1, func(i, j int) { sets[i+1], sets[j+1] = sets[j+1], sets[i+1] })
			sets = sets[:6]
			for _, f := range sets {
				for _, p := range []calendar.Period{octBlocks[r.IntN(len(octBlocks))], oct2026} {
					h, _ := checkOne(t, &w, p, f, false)
					hidden += h
				}
			}
			// A quarter, on two conditions (its universe is 13 blocks).
			wq := attack.Generate(seed, smallDims(2), q4Blocks, false, attack.Options{Subjects: 30 + r.IntN(60), Presence: 0.12, Rare: 0.8})
			qs := filterSets(wq.Dims)
			h, _ := checkOne(t, &wq, calendar.Of(calendar.Quarter, day("2026-10-01")), qs[r.IntN(len(qs))], false)
			hidden += h
		})
	}
	t.Logf("P1: %d hidden cells of 1 or more checked; none solvable", hidden)
}

// The attacker's checks each fire on a body built to fail them.
func TestP1AttackerChecksFire(t *testing.T) {
	blocks := attack.Blocks(octWeek, octWeek)
	w := attack.Generate(3, smallDims(2), blocks, false, attack.Options{Subjects: 40, Presence: 1, Rare: 0})
	raw := rawOf(&w, octWeek, Filters{}, false)
	b, err := GuardResponse(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	run := func(edit func(*Body), docs ...Body) *attack.Model {
		c := b
		c.ByAge = append([]Part[gd]{}, b.ByAge...)
		c.BySex = append([]Part[gd]{}, b.BySex...)
		edit(&c)
		var ds []attack.Doc
		for _, x := range append([]Body{c}, docs...) {
			d, err := docOf(x)
			if err != nil {
				t.Fatal(err)
			}
			ds = append(ds, d)
		}
		m, err := attack.New(&w, ds)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := run(func(*Body) {}); len(m.Exposed([]int{0}))+len(m.Mismatches())+len(m.SetViolations([]int{0})) > 0 {
		t.Fatal("the guarded body fails the attacker")
	}
	// One partition member hidden alone: solvable from patients.
	m := run(func(c *Body) {
		for i := range c.BySex {
			c.BySex[i].Patients = rawCount(raw, "sex", c.BySex[i].Key)
		}
		c.BySex[0].Patients = gd{Suppressed: true}
	})
	if len(m.Exposed([]int{0})) == 0 {
		t.Error("Exposed misses a lone hidden partition member")
	}
	// A shown value that isn't the definition's.
	m = run(func(c *Body) { v := *c.Patients.Value + 1; c.Patients.Value = &v })
	if len(m.Mismatches()) == 0 {
		t.Error("Mismatches misses a wrong patients count")
	}
	// The whole-set rule: two hidden sexes, and one of them shown in a
	// second document.
	m = run(func(c *Body) {
		for i := range c.BySex {
			c.BySex[i].Patients = gd{Suppressed: true}
		}
	}, func() Body {
		c := b
		c.BySex = []Part[gd]{{Key: b.BySex[0].Key, Patients: rawCount(raw, "sex", b.BySex[0].Key)}}
		return c
	}())
	if len(m.SetViolations([]int{0, 1})) == 0 || len(m.OneState([]int{0, 1})) == 0 {
		t.Error("SetViolations or OneState misses a member shown later")
	}
}

// rawCount is a partition row's raw count, shown.
func rawCount(r Raw, dim, key string) gd {
	for _, p := range map[string][]Part[int64]{"sex": r.BySex, "age": r.ByAge}[dim] {
		if p.Key == key {
			v := p.Patients
			return gd{Value: &v}
		}
	}
	return gd{Suppressed: true}
}
