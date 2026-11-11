package disclose

import (
	"errors"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose/attack"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// sexWorld is one block of patients by sex (with condition c1).
func sexWorld(bySex map[string]int) attack.World {
	d := smallDims(1)
	d.Sexes = []string{"female", "male", "other"}
	w := attack.World{Dims: d, Blocks: []calendar.Period{octWeek}}
	for si, s := range d.Sexes {
		for i := 0; i < bySex[s]; i++ {
			w.Subjects = append(w.Subjects, attack.Subject{Sex: si, Age: i % 2, Blocks: map[int]*attack.Block{0: {Conds: 1, Results: map[int]int{}}}})
		}
	}
	return w
}

// guardFamily guards every context of p within the cap over dims, parents
// first, each with every body already published for the period (ruling
// C10: one record per period).
func guardFamily(t *testing.T, w *attack.World, p calendar.Period, sets []Filters) map[Filters]Body {
	t.Helper()
	out := map[Filters]Body{}
	var published []Body
	for n := 0; n <= FilterCap; n++ {
		for _, f := range sets {
			if f.Count() != n {
				continue
			}
			b, err := Guard(rawOf(w, p, f, false), nil, published)
			if err != nil {
				t.Fatalf("%v: %v", f, err)
			}
			out[f] = b
			published = append(published, b)
		}
	}
	return out
}

// guardPeriodAll guards every context of p, parents first (the
// orchestrator's order).
func guardPeriodAll(t *testing.T, w *attack.World, p calendar.Period, sets []Filters) map[Filters]Body {
	t.Helper()
	return guardFamily(t, w, p, sets)
}

// subsets are f's proper subsets: its family above it.
func subsets(f Filters) []Filters {
	var set []int
	for d, v := range f {
		if v != "" {
			set = append(set, d)
		}
	}
	var out []Filters
	for mask := 0; mask < 1<<len(set)-1; mask++ {
		var g Filters
		for i, d := range set {
			if mask&(1<<i) != 0 {
				g[d] = f[d]
			}
		}
		out = append(out, g)
	}
	return out
}

// T5: asking for a complementary cell through a filter. The unfiltered
// view hides "other" (3) and the next-smallest, male (12); asking for
// sex=male directly is answered hidden whole.
func TestT5ComplementByFilterHiddenWhole(t *testing.T) {
	w := sexWorld(map[string]int{"female": 200, "male": 12, "other": 3})
	var male Filters
	male[DimSex] = "male"
	bodies := guardFamily(t, &w, octWeek, []Filters{{}, male})
	for _, p := range bodies[Filters{}].BySex {
		if p.Key == "male" && p.Patients.Shown() {
			t.Fatal("the parent shows male: the test proves nothing")
		}
	}
	if b := bodies[male]; !b.Whole || b.Patients.Shown() {
		t.Errorf("sex=male answers %+v: 12 = the parent's hidden male", b.Patients)
	}
}

// Review focus 1: a filtered request whose own group is hidden in a stored
// parent is hidden whole, at the second filter too.
func TestRequestHiddenInParentIsHiddenWhole(t *testing.T) {
	w := sexWorld(map[string]int{"female": 200, "male": 24, "other": 6})
	var age, both Filters
	age[DimAgeBand] = "18-39"
	both[DimAgeBand], both[DimSex] = "18-39", "male"
	bodies := guardFamily(t, &w, octWeek, []Filters{{}, age, both})
	parent := bodies[age]
	hidden := false
	for _, p := range parent.BySex {
		hidden = hidden || (p.Key == "male" && !p.Patients.Shown())
	}
	if !hidden {
		t.Fatalf("age=18-39 shows its male row: %+v", parent.BySex)
	}
	if b := bodies[both]; !b.Whole {
		t.Errorf("age=18-39, sex=male answers %+v though its parent hides it", b.Patients)
	}
}

// T6: a third filter is refused, and so is a code that isn't one.
func TestT6ThirdFilterRefused(t *testing.T) {
	var f Filters
	f[DimAgeBand], f[DimSex], f[DimSetting] = "18-39", "male", "admitted"
	if err := CheckFilters(f); !errors.Is(err, ErrTooManyFilters) {
		t.Errorf("three filters: %v", err)
	}
	r := flat(oct2026, 50)
	r.Context.Filters = f
	if b, err := Guard(r, nil, nil); !errors.Is(err, ErrTooManyFilters) || !b.Whole {
		t.Errorf("Guard with three filters: %v", err)
	}
	for _, bad := range []Filters{{DimSex: "unknown"}, {DimCondition: "c 1"}, {DimSetting: "ward"}, {DimLocation: "L1", DimSetting: "admitted"}} {
		if err := CheckFilters(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	f[DimSetting] = ""
	if err := CheckFilters(f); err != nil {
		t.Errorf("two filters: %v", err)
	}
}

// D5: within a family, each group has one state, in every view and
// context, on random worlds.
func TestOneStatePerGroupInFamily(t *testing.T) {
	for _, seed := range seeds(200, 10, 2) {
		w := attack.Generate(seed, smallDims(2), []calendar.Period{octWeek}, false, attack.Options{Subjects: 120, Presence: 1, Rare: 0.5})
		sets := filterSets(w.Dims)
		bodies := guardFamily(t, &w, octWeek, sets)
		docs, idx := docsOf(t, bodies, sets)
		m, err := attack.New(&w, docs)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range sets {
			for _, x := range m.OneState(familyDocs(f, idx)) {
				t.Errorf("seed %d %v: %s", seed, f, x)
			}
		}
	}
}

func docsOf(t *testing.T, bodies map[Filters]Body, sets []Filters) ([]attack.Doc, map[Filters]int) {
	t.Helper()
	var docs []attack.Doc
	idx := map[Filters]int{}
	for _, f := range sets {
		d, err := docOf(bodies[f])
		if err != nil {
			t.Fatal(err)
		}
		idx[f] = len(docs)
		docs = append(docs, d)
	}
	return docs, idx
}

// familyDocs is f's family: f and its subsets.
func familyDocs(f Filters, idx map[Filters]int) []int {
	out := []int{idx[f]}
	for _, s := range subsets(f) {
		out = append(out, idx[s])
	}
	return out
}
