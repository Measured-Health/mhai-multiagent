package smallgroup

import (
	"context"
	"errors"
	"flag"
	"math/big"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup/solve"
)

// Plan 7c Task 2: the guard over time (spec §6). Every count is synthetic.

// newBlock is an empty block of f with slots and hospitals, onboarded at 0.
func newBlock(f *Family, slots, hospitals int) *Block {
	b := &Block{Family: f, Slots: slots, Hospitals: hospitals, Onboard: make([]int, hospitals)}
	b.X = make([][][]int64, hospitals)
	for h := range b.X {
		b.X[h] = make([][]int64, slots)
		for s := range b.X[h] {
			b.X[h][s] = make([]int64, f.Cats)
		}
	}
	return b
}

func seq(from, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = from + i
	}
	return out
}

// publish guards p and stores it in the block.
func publish(t *testing.T, b *Block, p Pending) Published {
	t.Helper()
	hide, err := GuardSnapshot(b, p)
	if err != nil {
		t.Fatalf("guard %v: %v", p.Slots, err)
	}
	pub := Published{Slots: p.Slots, Listed: p.Listed, Children: p.Children, Hidden: hide}
	b.Published = append(b.Published, pub)
	return pub
}

// ---- the test's own equations, for solve ------------------------------------

// leakReport runs solve over every published snapshot of b: shown cells as
// equations, shown zeros, unlisted hospitals and pre-onboarding slots as
// zeros. It returns the hidden cells exposed with a value of 1 to 4, the
// hidden-member sums of 1 to 4 exposed, and how many hidden cells (any
// value) are exposed.
type leakReport struct {
	smallCells, smallSums []string
	exposedCells          int
}

func cellVars(b *Block, p Published, cell int) []int {
	m := len(b.Family.Measures)
	r, k := cell/m, cell%m
	hosp := p.Listed
	if r > 0 {
		hosp = []int{p.Listed[r-1]}
	}
	var out []int
	for _, h := range hosp {
		for _, s := range p.Slots {
			for _, c := range b.Family.Measures[k] {
				out = append(out, (h*b.Slots+s)*b.Family.Cats+c)
			}
		}
	}
	return out
}

func valueOf(b *Block, vars []int) int64 {
	var n int64
	for _, x := range vars {
		c := x % b.Family.Cats
		s := (x / b.Family.Cats) % b.Slots
		n += b.X[x/b.Family.Cats/b.Slots][s][c]
	}
	return n
}

func eqsOf(b *Block, zerosKnown bool) [][]int {
	var eqs [][]int
	nc := b.Family.Cats
	zero := func(vs []int) {
		for _, v := range vs {
			eqs = append(eqs, []int{v})
		}
	}
	for h := 0; h < b.Hospitals; h++ {
		for s := 0; s < b.Onboard[h]; s++ {
			zero(seq((h*b.Slots+s)*nc, nc))
		}
	}
	for _, p := range b.Published {
		for c, hid := range p.Hidden {
			if hid {
				continue
			}
			vs := cellVars(b, p, c)
			if zerosKnown && valueOf(b, vs) == 0 {
				zero(vs)
			} else {
				eqs = append(eqs, vs)
			}
		}
		for h := 0; h < b.Hospitals; h++ {
			if indexOf(p.Listed, h) < 0 {
				for _, s := range p.Slots {
					zero(seq((h*b.Slots+s)*nc, nc))
				}
			}
		}
	}
	return eqs
}

// testSets are the sets of p (rows, columns, parent), each a list of
// (cell of a snapshot, or a virtual box).
func testSets(b *Block, pi int) [][][]int {
	p := b.Published[pi]
	m := len(b.Family.Measures)
	rows := len(p.Listed) + 1
	var out [][][]int // each set: hidden members' vars
	add := func(members [][]int, hidden []bool) {
		var hs [][]int
		for i, mv := range members {
			if hidden[i] {
				hs = append(hs, mv)
			}
		}
		out = append(out, hs)
	}
	for r := 0; r < rows; r++ {
		hosp := p.Listed
		if r > 0 {
			hosp = []int{p.Listed[r-1]}
		}
		for _, rs := range b.Family.RowSets {
			var mem [][]int
			var hid []bool
			for _, k := range rs.Parts {
				mem = append(mem, cellVars(b, p, r*m+k))
				hid = append(hid, p.Hidden[r*m+k])
			}
			for _, vc := range rs.Virtual {
				var vs []int
				for _, h := range hosp {
					for _, s := range p.Slots {
						for _, c := range vc {
							vs = append(vs, (h*b.Slots+s)*b.Family.Cats+c)
						}
					}
				}
				mem = append(mem, vs)
				hid = append(hid, true)
			}
			add(mem, hid)
		}
	}
	for k := 0; k < m; k++ {
		var mem [][]int
		var hid []bool
		for r := 1; r < rows; r++ {
			mem = append(mem, cellVars(b, p, r*m+k))
			hid = append(hid, p.Hidden[r*m+k])
		}
		add(mem, hid)
	}
	for r := 0; r < rows && len(p.Children) > 0; r++ {
		for k := 0; k < m; k++ {
			var mem [][]int
			var hid []bool
			for _, ci := range p.Children {
				c := b.Published[ci]
				cr := 0
				if r > 0 {
					cr = indexOf(c.Listed, p.Listed[r-1]) + 1
					if cr == 0 {
						continue
					}
				}
				mem = append(mem, cellVars(b, c, cr*m+k))
				hid = append(hid, c.Hidden[cr*m+k])
			}
			add(mem, hid)
		}
	}
	return out
}

func report(b *Block) leakReport { return reportAll([]*Block{b}) }

// reportAll is report over several blocks at once: one system, each
// block's unknowns after the previous ones'.
func reportAll(bs []*Block) leakReport {
	var eqs, boxes [][]int
	type what struct {
		cell  bool
		desc  string
		value int64
	}
	var whats []what
	shift := func(vs []int, off int) []int {
		out := make([]int, len(vs))
		for i, v := range vs {
			out[i] = v + off
		}
		return out
	}
	off := 0
	for bi, b := range bs {
		for _, e := range eqsOf(b, true) {
			eqs = append(eqs, shift(e, off))
		}
		for pi, p := range b.Published {
			for c, hid := range p.Hidden {
				if hid {
					vs := cellVars(b, p, c)
					boxes = append(boxes, shift(vs, off))
					whats = append(whats, what{true, "block " + itoa(int64(bi)) + " snapshot " + itoa(int64(pi)) + " cell " + itoa(int64(c)), valueOf(b, vs)})
				}
			}
			for si, st := range testSets(b, pi) {
				var vs []int
				for _, mv := range st {
					vs = append(vs, mv...)
				}
				if len(st) > 0 && small(valueOf(b, vs)) {
					boxes = append(boxes, shift(vs, off))
					whats = append(whats, what{false, "block " + itoa(int64(bi)) + " snapshot " + itoa(int64(pi)) + " set " + itoa(int64(si)), valueOf(b, vs)})
				}
			}
		}
		off += b.nVars()
	}
	ex := solve.Exposed(off, eqs, boxes)
	var r leakReport
	for i, e := range ex {
		if !e {
			continue
		}
		v := whats[i].value
		switch {
		case whats[i].cell:
			r.exposedCells++
			if small(v) {
				r.smallCells = append(r.smallCells, whats[i].desc+" = "+itoa(v))
			}
		case small(v):
			r.smallSums = append(r.smallSums, whats[i].desc+" = "+itoa(v))
		}
	}
	return r
}

func (r leakReport) leaks() string {
	return strings.Join(append(append([]string{}, r.smallCells...), r.smallSums...), "; ")
}

// ---- the primes and the reference solver ------------------------------------

func TestPrimesAre61Bit(t *testing.T) {
	for _, p := range primes {
		if !new(big.Int).SetUint64(p).ProbablyPrime(64) || p>>60 != 1 {
			t.Errorf("%d is not a 61-bit prime", p)
		}
	}
	if primes[0] == primes[1] {
		t.Error("the two primes are the same")
	}
}

// A8, hand-built: x + y and x + (1 + p1)·y are independent over the
// rationals, so x is exposed; modulo p1 they are the same row. The check
// still finds x through the second prime.
func TestModularAgreesWithRational(t *testing.T) {
	p1 := int64(primes[0])
	eqs := [][]int{{0, 1}, {0, 1}}
	coefs := [][]int64{{1, 1}, {1, 1 + p1}}
	boxes := [][]int{{0}, {1}, {0, 1}}
	bc := [][]int64{{1}, {1}, {1, 1}}
	// One prime alone misses x.
	q := newMBasis(primes[0])
	for i, e := range eqs {
		q.add(toM(e, coefs[i], q.p))
	}
	if len(q.reduce(toM([]int{0}, nil, q.p))) == 0 {
		t.Fatal("the hand-built case doesn't divide a minor by p1")
	}
	got := exposedMod(eqs, coefs, boxes, bc)
	var rat [][]solve.Term
	for i, e := range eqs {
		rat = append(rat, []solve.Term{{Var: e[0], Coef: coefs[i][0]}, {Var: e[1], Coef: coefs[i][1]}})
	}
	var rb [][]solve.Term
	for i, bx := range boxes {
		var ts []solve.Term
		for j, v := range bx {
			ts = append(ts, solve.Term{Var: v, Coef: bc[i][j]})
		}
		rb = append(rb, ts)
	}
	want := solve.ExposedLinear(2, rat, rb)
	for i := range boxes {
		if got[i] != want[i] || !got[i] {
			t.Errorf("box %v: modular %v rational %v, want both exposed", boxes[i], got[i], want[i])
		}
	}
	// On the A7 histories, the modular check and the rational one agree on
	// every protected box.
	r := rand.New(rand.NewSource(20261107))
	for k := 0; k < 30; k++ {
		h := randomHistory(r)
		for _, b := range h.run(t) {
			if msg := agreeModRational(b); msg != "" {
				t.Fatalf("history %d: %s", k, msg)
			}
		}
	}
}

// agreeModRational compares the two checks over every published cell box
// and set of b, with everything known.
func agreeModRational(b *Block) string {
	eqs := eqsOf(b, true)
	var boxes [][]int
	for pi, p := range b.Published {
		for c := range p.Hidden {
			boxes = append(boxes, cellVars(b, p, c))
		}
		for _, st := range testSets(b, pi) {
			var vs []int
			for _, mv := range st {
				vs = append(vs, mv...)
			}
			boxes = append(boxes, vs)
		}
	}
	want := solve.Exposed(b.Hospitals*b.Slots*b.Family.Cats, eqs, boxes)
	q := newPair()
	var zeros []int
	var rest [][]int
	for _, e := range eqs {
		if len(e) == 1 {
			zeros = append(zeros, e[0])
		} else {
			rest = append(rest, e)
		}
	}
	q.addAll(rest, zeros)
	for i, bx := range boxes {
		if got := q.exposed(bx, nil); got != want[i] {
			return "box " + itoa(int64(i)) + ": modular " + boolStr(got) + ", rational " + boolStr(want[i])
		}
	}
	return ""
}

func boolStr(b bool) string {
	if b {
		return "exposed"
	}
	return "not exposed"
}

// ---- the parent set ----------------------------------------------------------

// A6: frozen hidden week cells summing to 3 hide the month total; one hidden
// week cell in a hospital's row with the month shown hides the month cell;
// children are never changed.
func TestParentSetRule2AndBridge(t *testing.T) {
	// Rows all, A, B; columns month, w1..w4.
	all := []int64{83, 15, 16, 26, 26}
	rows := [][]int64{
		{23, 1, 2, 10, 10},   // A: weeks 1 and 2 hidden (1 and 2), sum 3
		{60, 14, 14, 16, 16}, // B
	}
	hid := [][]bool{
		{false, false, false, false, false},
		{false, true, true, false, false},
		{false, true, true, false, false},
	}
	got := AcrossRowsFrozen(all, rows, hid, []int{0})
	if !got[1][0] {
		t.Error("A's month (23, whose hidden weeks sum to 3) is shown")
	}
	for i := range got {
		for j := 1; j < 5; j++ {
			if got[i][j] != hid[i][j] {
				t.Errorf("child cell (%d, %d) changed", i, j)
			}
		}
	}
	// One hidden week in B's row, the month shown: the month is hidden.
	all = []int64{100, 25, 25, 25, 25}
	rows = [][]int64{{40, 10, 10, 10, 10}, {60, 15, 15, 15, 15}}
	hid = [][]bool{
		{false, false, false, false, false},
		{false, false, false, true, false},
		{false, false, false, true, false},
	}
	got = AcrossRowsFrozen(all, rows, hid, []int{0})
	if !got[1][0] || !got[2][0] {
		t.Errorf("a lone hidden week next to a shown month: month cells %v %v", got[1][0], got[2][0])
	}
	// Only the parent column hid, and no bridge is left.
	b, ok := bridge(flat(got), 3, 5)
	if ok {
		t.Errorf("a bridge left at cell %d", b)
	}
	// A table that doesn't add up hides the parent column.
	got = AcrossRowsFrozen([]int64{9, 1, 1, 1, 1}, [][]int64{{9, 1, 1, 1, 1}}, [][]bool{make([]bool, 5), make([]bool, 5)}, []int{0})
	if !got[0][0] || !got[1][0] {
		t.Error("an inconsistent parent set shows its parent")
	}
}

func flat(b [][]bool) []bool {
	var out []bool
	for _, r := range b {
		out = append(out, r...)
	}
	return out
}

// With every parent cell hidden and the children guarded by their own
// tables, no child cell of the parent set is a bridge (spec §6.3 step 4): a
// parent cell can be, but only as the sum of shown children, which tells
// nothing new.
func TestParentFullyHiddenHasNoBridge(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for k := 0; k < 3000; k++ {
		n := 1 + r.Intn(6)
		weeks := 2 + r.Intn(3)
		// One measure's column per week, guarded as Set (all = Σ hospitals).
		vals := make([][]int64, n+1) // [row][week]
		for i := range vals {
			vals[i] = make([]int64, weeks)
		}
		hidW := make([][]bool, n+1)
		for i := range hidW {
			hidW[i] = make([]bool, weeks)
		}
		for w := 0; w < weeks; w++ {
			parts := make([]int64, n)
			var tot int64
			for i := range parts {
				parts[i] = count(r)
				tot += parts[i]
			}
			g := Set(tot, parts...)
			vals[0][w] = tot
			hidW[0][w] = g[0].Suppressed
			for i := range parts {
				vals[i+1][w] = parts[i]
				hidW[i+1][w] = g[i+1].Suppressed
			}
		}
		hide := make([]bool, (n+1)*(weeks+1))
		for i := 0; i <= n; i++ {
			hide[i*(weeks+1)] = true
			for w := 0; w < weeks; w++ {
				hide[i*(weeks+1)+w+1] = hidW[i][w]
			}
		}
		for _, c := range bridges(hide, n+1, weeks+1, false) {
			if c%(weeks+1) != 0 {
				t.Fatalf("case %d: child cell %d is a bridge: %v %v", k, c, vals, hidW)
			}
		}
	}
}

// ---- the history check -------------------------------------------------------

// rBlockOneHospital fills reviews per slot for hospital h (agreed only).
func setR(b *Block, h int, agreed []int64) {
	for s, v := range agreed {
		b.X[h][s][CatAgreed] = v
	}
}

// A5: a quarter whose months show every cell but one, where Q − M1 − M2
// would give a hidden 1 to 4: the quarter cell is hidden, and nothing is
// exposed.
func TestQuarterMinusMonthsHidden(t *testing.T) {
	b := newBlock(R, 3, 2) // slots are months here: no weeks are published
	setR(b, 0, []int64{3, 20, 20})
	setR(b, 1, []int64{30, 30, 30})
	var months []int
	for s := 0; s < 3; s++ {
		publish(t, b, Pending{Slots: []int{s}, Listed: []int{0, 1}})
		months = append(months, s)
	}
	if !b.Published[0].Hidden[1*3+RReviews] {
		t.Fatal("month 1's 3 shown")
	}
	q := publish(t, b, Pending{Slots: seq(0, 3), Listed: []int{0, 1}, Children: months})
	if !q.Hidden[1*3+RReviews] {
		t.Error("the quarter's A reviews (Q − M2 − M3 = 3) is shown")
	}
	if r := report(b); r.leaks() != "" {
		t.Errorf("exposed: %s", r.leaks())
	}
}

// A hidden cell solvable only through a shown 0 (non-negativity): the guard
// catches it and so does solve. Two weeks hide A's degraded 0 and B's
// degraded 7 as a pair; a month showing A's degraded as 0 gives both away.
func TestShownZeroIsKnown(t *testing.T) {
	b := newBlock(QG, 4, 2)
	for s := 0; s < 4; s++ {
		b.X[0][s][CatPass] = 6
		if s < 2 {
			b.X[1][s][CatPassDegraded] = 7
		} else {
			b.X[1][s][CatPass] = 6
		}
	}
	m := len(QG.Measures)
	for s := 0; s < 4; s++ {
		hid := make([]bool, 3*m)
		if s < 2 {
			hid[1*m+QGDegraded], hid[2*m+QGDegraded] = true, true
		}
		b.Published = append(b.Published, Published{Slots: []int{s}, Listed: []int{0, 1}, Hidden: hid})
	}
	// Published week 1 alone gives nothing away.
	if r := report(b); r.exposedCells != 0 {
		t.Fatalf("the weeks alone expose %d cells", r.exposedCells)
	}
	monthAllShown := Published{Slots: seq(0, 4), Listed: []int{0, 1}, Children: seq(0, 4), Hidden: make([]bool, 3*m)}
	withMonth := *b
	withMonth.Published = append(append([]Published{}, b.Published...), monthAllShown)
	// solve finds B's week-1 degraded through A's month 0 ...
	if r := report(&withMonth); r.exposedCells == 0 {
		t.Fatal("solve misses the shown-zero chain")
	}
	// ... and only through the zero: as a plain equation it is not exposed.
	eqs := eqsOf(&withMonth, false)
	if ex := solve.Exposed(withMonth.Hospitals*withMonth.Slots*QG.Cats, eqs, [][]int{cellVars(&withMonth, withMonth.Published[0], 2*m+QGDegraded)}); ex[0] {
		t.Fatal("the case doesn't need the zero")
	}
	// The guard hides enough: B's week-1 degraded stays unknown.
	publish(t, b, Pending{Slots: seq(0, 4), Listed: []int{0, 1}, Children: seq(0, 4)})
	if r := report(b); r.leaks() != "" {
		t.Errorf("after the guard: %s", r.leaks())
	}
	target := cellVars(b, b.Published[0], 2*m+QGDegraded)
	if ex := solve.Exposed(b.Hospitals*b.Slots*QG.Cats, eqsOf(b, true), [][]int{target}); ex[0] {
		t.Errorf("after the guard B's week-1 degraded is exposed; month hidden %v", b.Published[4].Hidden)
	}
}

// A26, the review's case: a hospital onboarded mid-month (20 Oct), October
// shown, November shown as 0, the week holding the month edge hidden. With
// Monday-to-Sunday weeks (a test-only calendar) the chain from onboarding
// and the zero solves the hidden edge week; with nested weeks the guard
// hides enough that nothing is exposed.
func TestReviewChainingCase(t *testing.T) {
	// Days 0..60 are 1 Oct to 30 Nov 2026. e is the Monday of the
	// Monday-to-Sunday week that holds 31 Oct (day 30) and 1 Nov.
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	off := (int(start.Weekday()) + 6) % 7 // days from the Monday on or before 1 Oct
	e := -off
	for e+7 <= 30 {
		e += 7
	}
	if e+6 < 31 {
		t.Fatalf("the week from day %d doesn't cross the month edge", e)
	}
	// One hospital, onboarded on the Tuesday before: 10 reviews in that
	// week, 3 in the edge week's October days, none in November.
	onboard := e - 6
	daily := make([]int64, 61)
	for d := e - 6; d < e-1; d++ {
		daily[d] = 2
	}
	daily[e] = 1
	daily[30] += 2
	sum := func(from, to int) int64 {
		var n int64
		for d := max(from, 0); d < to && d < 61; d++ {
			n += daily[d]
		}
		return n
	}
	type cell struct{ from, to int }
	var cells []cell
	for d := -off; d < 61; d += 7 {
		cells = append(cells, cell{max(d, 0), min(d+7, 61)})
	}
	cells = append(cells, cell{0, 31}, cell{31, 61}) // October, November
	var eqs [][]int
	for d := 0; d < onboard; d++ {
		eqs = append(eqs, []int{d})
	}
	var hiddenCells []cell
	for _, c := range cells {
		v := sum(c.from, c.to)
		if small(v) {
			hiddenCells = append(hiddenCells, c)
			continue
		}
		vs := seq(c.from, c.to-c.from)
		if v == 0 {
			for _, x := range vs {
				eqs = append(eqs, []int{x})
			}
		} else {
			eqs = append(eqs, vs)
		}
	}
	var boxes [][]int
	for _, c := range hiddenCells {
		boxes = append(boxes, seq(c.from, c.to-c.from))
	}
	exposed := false
	for _, e := range solve.Exposed(61, eqs, boxes) {
		exposed = exposed || e
	}
	if !exposed {
		t.Fatal("Monday-to-Sunday weeks: the chain isn't exposed, so the test proves nothing")
	}

	// Nested: October and November's weeks (8 slots), one hospital, the
	// same days; onboarding in the third week of October.
	b := newBlock(R, 8, 1)
	bounds := []int{0, 7, 14, 21, 31, 38, 45, 52, 61}
	for b.Onboard[0] < 8 && bounds[b.Onboard[0]+1] <= onboard {
		b.Onboard[0]++
	}
	for s := 0; s < 8; s++ {
		b.X[0][s][CatAgreed] = sum(bounds[s], bounds[s+1])
	}
	for mo := 0; mo < 2; mo++ {
		var weeks []int
		for w := 0; w < 4; w++ {
			publish(t, b, Pending{Slots: []int{mo*4 + w}, Listed: []int{0}})
			weeks = append(weeks, len(b.Published)-1)
		}
		publish(t, b, Pending{Slots: seq(mo*4, 4), Listed: []int{0}, Children: weeks})
	}
	if r := report(b); r.leaks() != "" || r.exposedCells != 0 {
		t.Errorf("nested, guarded: %d hidden cells exposed (%s)", r.exposedCells, r.leaks())
	}
}

// A snapshot forced fully hidden adds no new exposure.
func TestFallbackAllHiddenAddsNothing(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	for k := 0; k < 20; k++ {
		h := randomHistory(r)
		h.checkFullyHidden = true
		h.run(t)
	}
}

// TestFullyHiddenAlwaysPasses: for every A7 block, S fully hidden passes the
// check (newlyExposed is empty), so the guard's loop always ends with a
// snapshot to publish.
func TestFullyHiddenAlwaysPasses(t *testing.T) {
	r := rand.New(rand.NewSource(12))
	for k := 0; k < historyCount()/5; k++ {
		h := randomHistory(r)
		h.checkFullyHidden = true
		h.run(t)
	}
}

// A hidden month whose four weeks are all shown is exposed before and
// after: only new exposures count, and the guard publishes.
func TestOnlyNewExposuresCount(t *testing.T) {
	b := newBlock(R, 4, 2)
	setR(b, 0, []int64{10, 10, 10, 10})
	setR(b, 1, []int64{20, 20, 20, 20})
	var weeks []int
	for s := 0; s < 4; s++ {
		publish(t, b, Pending{Slots: []int{s}, Listed: []int{0, 1}})
		weeks = append(weeks, s)
	}
	m := len(R.Measures)
	forced := make([]bool, 3*m)
	forced[1*m+RReviews] = true // A's month, hidden though its weeks are shown
	hide, err := GuardSnapshot(b, Pending{Slots: seq(0, 4), Listed: []int{0, 1}, Children: weeks, Forced: forced})
	if err != nil {
		t.Fatalf("the guard gave up: %v", err)
	}
	shownCells := 0
	for _, h := range hide {
		if !h {
			shownCells++
		}
	}
	if !hide[1*m+RReviews] || shownCells == 0 {
		t.Errorf("hidden %v: the month was hidden whole for an exposure that wasn't new", hide)
	}
}

// A28, linked families: questions 7 and answers 4 for one hospital-week.
// Neither answers nor the unanswered 3 is exposed, whichever side the
// rules hid first. With questions and answers guarded as two independent
// families, the link reopens a bridge: answers 5 of 7 shows, and the
// unanswered 2 is solved from the row.
func TestLinkedFamilies(t *testing.T) {
	m := len(QG.Measures)
	build := func(qA, aA int64) *Block {
		b := newBlock(QG, 1, 3)
		b.X[0][0][CatPass], b.X[0][0][CatUnanswered] = aA, qA-aA
		b.X[1][0][CatPass] = 20
		b.X[2][0][CatPass], b.X[2][0][CatEscalate] = 12, 9
		return b
	}
	for _, forcedFirst := range []int{-1, QGQuestions, QGAnswers} {
		b := build(7, 4)
		forced := make([]bool, 4*m)
		if forcedFirst >= 0 {
			forced[1*m+forcedFirst] = true
		}
		publish(t, b, Pending{Slots: []int{0}, Listed: []int{0, 1, 2}, Forced: forced})
		pub := b.Published[0]
		eqs := eqsOf(b, true)
		ans := cellVars(b, pub, 1*m+QGAnswers)
		unanswered := []int{0*QG.Cats + CatUnanswered}
		ex := solve.Exposed(3*QG.Cats, eqs, [][]int{ans, unanswered})
		if (pub.Hidden[1*m+QGAnswers] && ex[0]) || ex[1] {
			t.Errorf("forced %d: answers exposed %v, unanswered exposed %v (%v)", forcedFirst, ex[0], ex[1], pub.Hidden)
		}
		if r := report(b); r.leaks() != "" {
			t.Errorf("forced %d: %s", forcedFirst, r.leaks())
		}
	}
	// Split into two families, the link is missed: questions as a family of
	// its own (Set over the column) and answers as another (Table).
	b := build(7, 5)
	qs := []int64{7, 20, 21}
	qg := Set(48, qs...)
	rows := [][]int64{{5, 5, 0, 0}, {20, 20, 0, 0}, {21, 12, 0, 9}}
	tb := Table(4, rows, []int64{0, 0, 0})
	hide := make([]bool, 4*m)
	for r := 0; r < 4; r++ {
		hide[r*m+QGQuestions] = qg[r].Suppressed
		g := tb.All
		if r > 0 {
			g = tb.Rows[r-1]
		}
		for k := 0; k < 4; k++ {
			hide[r*m+QGAnswers+k] = g[k].Suppressed
		}
		hide[r*m+QGDegraded] = tb.Subset[r].Suppressed
	}
	b.Published = []Published{{Slots: []int{0}, Listed: []int{0, 1, 2}, Hidden: hide}}
	if r := report(b); r.leaks() == "" {
		t.Error("split families: the unanswered 2 isn't exposed, so the test proves nothing")
	}
	// The joint guard on the same counts hides it.
	b.Published = nil
	publish(t, b, Pending{Slots: []int{0}, Listed: []int{0, 1, 2}})
	if r := report(b); r.leaks() != "" {
		t.Errorf("joint guard: %s", r.leaks())
	}
}

// A24: a full block at 50 hospitals, QG, every snapshot inside 60 s.
// Skipped unless named with -run or ANALYTICS_BUDGET=1 (the build host).
func TestJointBudget(t *testing.T) {
	if os.Getenv("ANALYTICS_BUDGET") != "1" && !strings.Contains(flag.Lookup("test.run").Value.String(), "TestJointBudget") {
		t.Skip("set ANALYTICS_BUDGET=1 or -run TestJointBudget")
	}
	r := rand.New(rand.NewSource(50))
	const hosp = 50
	b := newBlock(QG, 12, hosp)
	for h := 0; h < hosp; h++ {
		for s := 0; s < 12; s++ {
			fillQG(r, b.X[h][s])
		}
	}
	listed := seq(0, hosp)
	var months []int
	for mo := 0; mo < 3; mo++ {
		var weeks []int
		for w := 0; w < 4; w++ {
			start := time.Now()
			publish(t, b, Pending{Slots: []int{mo*4 + w}, Listed: listed})
			if d := time.Since(start); d > 60*time.Second {
				t.Fatalf("week %d took %v", mo*4+w, d)
			}
			weeks = append(weeks, len(b.Published)-1)
		}
		start := time.Now()
		publish(t, b, Pending{Slots: seq(mo*4, 4), Listed: listed, Children: weeks})
		if d := time.Since(start); d > 60*time.Second {
			t.Fatalf("month %d took %v", mo, d)
		}
		months = append(months, len(b.Published)-1)
	}
	start := time.Now()
	publish(t, b, Pending{Slots: seq(0, 12), Listed: listed, Children: months})
	t.Logf("quarter took %v", time.Since(start))
	if d := time.Since(start); d > 60*time.Second {
		t.Fatalf("quarter took %v", d)
	}
}

// T17: the guard checks its context between sets and stops with its error.
func TestGuardSnapshotStopsOnContext(t *testing.T) {
	b := newBlock(QG, 4, 3)
	r := rand.New(rand.NewSource(5))
	for h := 0; h < 3; h++ {
		for s := 0; s < 4; s++ {
			fillQG(r, b.X[h][s])
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := GuardSnapshotCtx(ctx, b, Pending{Slots: []int{0}, Listed: []int{0, 1, 2}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled guard returned %v, want context.Canceled", err)
	}
	if _, err := GuardSnapshotCtx(context.Background(), b, Pending{Slots: []int{0}, Listed: []int{0, 1, 2}}); err != nil {
		t.Fatalf("a live guard: %v", err)
	}
}

// The guard's exposure check as a library (Analysis, plan Task 5):
// Knowledge decides exposure and the whole-set rule modulo the two primes,
// as GuardSnapshot does, and agrees with the exact solver.
func TestKnowledgeExposedAndSetChanged(t *testing.T) {
	before := NewKnowledge()
	before.Add([]int{0, 1, 2}, nil) // a total of three hidden members
	after := before.Clone()
	after.Add([]int{0, 1}, nil) // the first two's sum
	if before.Exposed([]int{2}, nil) || !after.Exposed([]int{2}, nil) {
		t.Fatal("x2 = total − (x0 + x1) isn't exposed after, or is before")
	}
	if !after.Exposed([]int{0, 1}, nil) || after.Exposed([]int{0}, nil) {
		t.Fatal("x0 + x1 is known, x0 alone isn't")
	}
	if after.Exposed([]int{3, 4}, []int64{2, -2}) || after.Exposed([]int{3}, nil) {
		t.Fatal("unknowns in no equation are exposed")
	}
	// The whole-set rule: the set {x0, x1} goes from nothing determined to
	// its full sum alone (allowed); {x0, x2} gains a part (refused).
	if SetChanged(before, after, [][]int{{0}, {1}}) {
		t.Error("nothing → the full sum alone is allowed")
	}
	if !SetChanged(before, after, [][]int{{0, 1}, {2}}) {
		t.Error("a member newly determined is a violation")
	}
	if SetChanged(after, after, [][]int{{0}, {2}}) {
		t.Error("no change is no violation")
	}
	// Against the exact solver on a random system.
	r := rand.New(rand.NewSource(7))
	for trial := 0; trial < 50; trial++ {
		k := NewKnowledge()
		var eqs [][]int
		for i := 0; i < 4; i++ {
			var e []int
			for v := 0; v < 6; v++ {
				if r.Intn(2) == 0 {
					e = append(e, v)
				}
			}
			eqs = append(eqs, e)
			k.Add(e, nil)
		}
		boxes := [][]int{{0}, {1}, {2, 3}, {4, 5}, {0, 5}}
		want := solve.Exposed(6, eqs, boxes)
		for i, bx := range boxes {
			if got := k.Exposed(bx, nil); got != want[i] {
				t.Fatalf("trial %d box %v: %v, exact %v", trial, bx, got, want[i])
			}
		}
	}
}
