package disclose

import (
	"bytes"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose/attack"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup/solve"
)

// handWorld is a world with n[b] patients having condition c1 in block b
// (each a new case, the start known), one per patient.
func handWorld(blocks []calendar.Period, n []int) attack.World {
	w := attack.World{Dims: smallDims(1), Blocks: blocks, Start: true}
	for b, k := range n {
		for i := 0; i < k; i++ {
			w.Subjects = append(w.Subjects, attack.Subject{Blocks: map[int]*attack.Block{
				b: {Conds: 1, New: 1, Results: map[int]int{}},
			}})
		}
	}
	return w
}

// publish guards every block, then every month, then every quarter of w
// that its blocks cover in full, children first (as the orchestrator
// will), and returns the bodies in publication order.
func publish(t *testing.T, w *attack.World, f Filters) []Body {
	t.Helper()
	var out []Body
	byPeriod := map[calendar.Period]ChildBodies{}
	start := w.Blocks[0].From
	guard := func(p calendar.Period) {
		raw := rawOf(w, p, f, w.Start && p.From.Equal(start))
		var kids []ChildBodies
		for _, k := range calendar.Children(p) {
			c, ok := byPeriod[k]
			if !ok {
				return // a parent whose children aren't all published
			}
			kids = append(kids, c)
		}
		b, err := GuardPeriod(raw, kids)
		if err != nil {
			t.Fatalf("%v: %v", p, err)
		}
		byPeriod[p] = ChildBodies{Raw: raw, Body: b}
		out = append(out, b)
	}
	for i, b := range w.Blocks {
		guard(b)
		next := calendar.Period{}
		if i+1 < len(w.Blocks) {
			next = w.Blocks[i+1]
		}
		if m := calendar.Of(calendar.Month, b.From); next.From.IsZero() || !m.Contains(next) {
			guard(m)
			if q := calendar.Of(calendar.Quarter, b.From); next.From.IsZero() || !q.Contains(next) {
				guard(q)
			}
		}
	}
	return out
}

// attackAll runs the attacker on bodies published in order: nothing
// solvable, no whole-set break, every shown value the definition's.
func attackAll(t *testing.T, w *attack.World, bodies []Body) *attack.Model {
	t.Helper()
	var docs []attack.Doc
	var order []int
	for i, b := range bodies {
		d, err := docOf(b)
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, d)
		order = append(order, i)
	}
	m, err := attack.New(w, docs)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range m.Mismatches() {
		t.Error(x)
	}
	for _, i := range m.Exposed(order) {
		t.Errorf("hidden %s of doc %d (%v), %d, is solvable", m.Cells[i].Name, m.Cells[i].Doc, bodies[m.Cells[i].Doc].Context.Period, m.Cells[i].Truth)
	}
	for _, x := range m.SetViolations(order) {
		t.Error(x)
	}
	for _, x := range m.ContractViolations() {
		t.Error(x)
	}
	return m
}

func octBlocksOf() []calendar.Period {
	return attack.Blocks(calendar.Of(calendar.Week, day("2026-10-01")), calendar.Of(calendar.Week, day("2026-10-31")))
}

// T2: a month's lone hidden block can't be recovered from the month.
func TestT2MonthTotalHidesLoneHiddenBlock(t *testing.T) {
	w := handWorld(octBlocksOf(), []int{10, 3, 8, 9})
	bodies := publish(t, &w, Filters{})
	month := bodies[len(bodies)-1]
	if month.Context.Period.Kind != calendar.Month {
		t.Fatalf("last published: %v", month.Context.Period)
	}
	if bodies[1].Total.Cases.Shown() {
		t.Fatal("the block of 3 is shown")
	}
	if month.Total.Cases.Shown() {
		t.Errorf("month total %v with blocks 10, hidden, 8, 9 gives the 3 away", *month.Total.Cases.Value)
	}
	attackAll(t, &w, bodies)
}

// T4 (last clause): a quarter whose single hidden month the other two
// would give away is hidden.
func TestQuarterWithOneHiddenMonthIsHidden(t *testing.T) {
	blocks := attack.Blocks(calendar.Of(calendar.Week, day("2026-10-01")), calendar.Of(calendar.Week, day("2026-12-31")))
	n := make([]int, len(blocks))
	for i := range n {
		n[i] = 5
	}
	for i, b := range blocks { // November: one block of 3, the rest empty
		if b.From.Month() == 11 {
			n[i] = 0
			if b.From.Day() == 8 {
				n[i] = 3
			}
		}
	}
	w := handWorld(blocks, n)
	bodies := publish(t, &w, Filters{})
	var nov, q Body
	for _, b := range bodies {
		switch {
		case b.Context.Period.Kind == calendar.Month && b.Context.Period.From.Month() == 11:
			nov = b
		case b.Context.Period.Kind == calendar.Quarter:
			q = b
		}
	}
	if nov.Total.Cases.Shown() || q.Total.Cases.Shown() {
		t.Errorf("November %+v, quarter %+v", nov.Total.Cases, q.Total.Cases)
	}
	attackAll(t, &w, bodies)
}

// Hidden children totalling 1 to 4 hide the parent's cell.
func TestParentHiddenWhenHiddenChildrenSumOneToFour(t *testing.T) {
	w := handWorld(octBlocksOf(), []int{10, 2, 2, 9})
	bodies := publish(t, &w, Filters{})
	if m := bodies[len(bodies)-1]; m.Total.Cases.Shown() {
		t.Errorf("month total %v with blocks 10, hidden, hidden, 9 gives 2 + 2 away", *m.Total.Cases.Value)
	}
	attackAll(t, &w, bodies)
}

// The stored children are inputs: guarding the parent never changes them.
func TestChildrenNeverChange(t *testing.T) {
	w := handWorld(octBlocksOf(), []int{10, 3, 8, 9})
	var kids []ChildBodies
	var before [][]byte
	for _, b := range w.Blocks {
		raw := rawOf(&w, b, Filters{}, b.From.Equal(w.Blocks[0].From))
		body, err := GuardPeriod(raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		kids = append(kids, ChildBodies{raw, body})
		for _, v := range Views {
			j, _ := ViewJSON(body, v)
			before = append(before, j)
		}
	}
	if _, err := GuardPeriod(rawOf(&w, oct2026, Filters{}, true), kids); err != nil {
		t.Fatal(err)
	}
	i := 0
	for _, k := range kids {
		for _, v := range Views {
			j, _ := ViewJSON(k.Body, v)
			if !bytes.Equal(j, before[i]) {
				t.Fatalf("a child changed: %s → %s", before[i], j)
			}
			i++
		}
	}
}

// Review focus 1: a history point is its period's stored body, as
// published; a body hidden whole gives a point hidden whole.
func TestHistoryPointsAreStoredBodies(t *testing.T) {
	w := handWorld(octBlocksOf(), []int{10, 3, 8, 9})
	bodies := publish(t, &w, Filters{})
	weeks := bodies[:4]
	weeks[2] = HiddenWhole(weeks[2].Context)
	h := History(weeks)
	if len(h) != 4 {
		t.Fatalf("points: %d", len(h))
	}
	for i, p := range h {
		b := weeks[i]
		if p.Start != b.Context.Period.From.Format("2006-01-02") || p.Cases != b.Total.Cases || p.New != b.Total.New || p.Patients != b.Patients {
			t.Errorf("point %d: %+v, body %+v %+v", i, p, b.Total, b.Patients)
		}
	}
	if h[2].Cases.Shown() || h[2].New.Shown() || h[2].Patients.Shown() || !h[2].Cases.Suppressed {
		t.Errorf("a whole-hidden body's point: %+v", h[2])
	}
	if !h[1].Cases.Suppressed {
		t.Errorf("the block of 3: %+v", h[1])
	}
}

// I2: a history carries at most HistoryPoints points, the latest, and no
// total.
func TestHistoryHasNoTotal(t *testing.T) {
	blocks := attack.Blocks(calendar.Of(calendar.Week, day("2026-10-01")), calendar.Of(calendar.Week, day("2026-12-31")))
	n := make([]int, len(blocks))
	for i := range n {
		n[i] = 6
	}
	w := handWorld(blocks, n)
	var weeks []Body
	for _, b := range publish(t, &w, Filters{}) {
		if b.Context.Period.Kind == calendar.Week {
			weeks = append(weeks, b)
		}
	}
	h := History(weeks)
	if len(h) != HistoryPoints || h[len(h)-1].Start != weeks[len(weeks)-1].Context.Period.From.Format("2006-01-02") {
		t.Fatalf("history: %d points ending %s", len(h), h[len(h)-1].Start)
	}
	if n := countFields(HistoryPoint{}); n != 4 {
		t.Errorf("a history point has %d fields: start, cases, new, patients only", n)
	}
}

// P4 (design §5.7): a hospital from its first period, over months of
// blocks with zeros and small counts; every block, month and quarter
// published as served, children first. The attacker writes the tree of
// equations, every response's own, the zeros and the start: no hidden cell
// is solvable, and no set's hidden members break the whole-set rule.
func TestP4HistoryNoExposure(t *testing.T) {
	months := 6
	if long() {
		months = 24
	}
	d := attack.Dims{Conditions: []string{"c1"}, Ages: []string{"18-39"}, Sexes: []string{"female", "male"},
		Locations: []attack.Location{{ID: "L1", Setting: "admitted"}}, Specialties: []string{"S1"}, Sources: []string{"X1"}}
	from := calendar.Of(calendar.Week, day("2025-01-01"))
	to := calendar.Of(calendar.Week, from.From.AddDate(0, months, -1))
	hidden := 0
	for _, seed := range seeds(500, 8, 2) {
		w := attack.Generate(seed, d, attack.Blocks(from, to), true, attack.Options{Subjects: 25, Presence: 0.12, Rare: 0})
		bodies := publish(t, &w, Filters{})
		m := attackAll(t, &w, bodies)
		var all []int
		for i := range bodies {
			all = append(all, i)
		}
		hidden += len(m.HiddenProtected(all))
		if t.Failed() {
			t.Fatalf("seed %d (%s)", seed, w.Describe())
		}
	}
	t.Logf("P4: %d hidden cells of 1 or more over the histories; none solvable", hidden)
}

// C3's regression: Monday-to-Sunday weeks published beside months give
// away the days at a month's edge, anchored at the start; month-aligned
// blocks don't. Days are the unknowns; every count here is shown.
func TestP4ISOWeeksWouldFail(t *testing.T) {
	start := day("2026-10-01") // a Thursday; nothing before it
	days := 61                 // October and November
	cases := make([]int, days)
	for i := range cases {
		cases[i] = 1 // one case a day: every week and block is 5 or more
	}
	var eqs [][]int
	box := func(from, to int) []int { // [from, to) in days since start, clipped
		var out []int
		for d := max(from, 0); d < min(to, days); d++ {
			out = append(out, d)
		}
		return out
	}
	eqs = append(eqs, box(0, 31), box(31, 61)) // the months
	edge := box(25, 31)                        // 26–31 October: 6 days, 6 cases
	iso := append([][]int{}, eqs...)
	for mon := -3; mon < days; mon += 7 { // Monday 28 September, before the start
		iso = append(iso, box(mon, mon+7))
	}
	blocks := append([][]int{}, eqs...)
	for _, b := range append(octBlocksOf(), attack.Blocks(calendar.Of(calendar.Week, day("2026-11-01")), calendar.Of(calendar.Week, day("2026-11-30")))...) {
		from := int(b.From.Sub(start).Hours() / 24)
		blocks = append(blocks, box(from, from+int(b.To.Sub(b.From).Hours()/24)))
	}
	// 30 October to 1 November: three days across the edge, a group of 3.
	cross := box(29, 32)
	if got := solve.Exposed(days, iso, [][]int{edge, cross[:2]}); !got[0] {
		t.Fatal("Monday-to-Sunday weeks beside months don't expose the month's last days: the test proves nothing")
	}
	if got := solve.Exposed(days, blocks, [][]int{edge, cross[:2], cross}); got[0] || got[1] || got[2] {
		t.Errorf("month-aligned blocks expose part of a block: %v", got)
	}
}
