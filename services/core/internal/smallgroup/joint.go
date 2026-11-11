package smallgroup

// The guard over time (Plan 7c spec §6; rulings R41a and R41a-2).
//
// A block is one quarter. Its atoms are its weeks (slots), and for each
// family the unknowns are the frozen base counts x[hospital, slot,
// category]. Every published cell is a box: the sum of the base counts over
// a period's slots, one hospital (or every hospital the period lists, for
// the all row) and a set of categories. A shown cell is an equation; a
// hidden one is nothing. What an attacker knows (§6.1):
//
//  1. every shown cell of every stored snapshot;
//  2. a shown 0 makes every base count in its box 0 (counts are never
//     negative);
//  3. a hospital's base counts before its onboarding are 0, and so are
//     those of a hospital in a period that doesn't list it;
//  4. the family links: questions and answers are boxes over the same QG
//     unknowns.
//
// A box is exposed when its indicator vector is in the row space of what is
// known. GuardSnapshot guards a new snapshot S with every earlier snapshot
// of the block frozen (§6.3): the cross-sectional rules on S, the parent set
// with its children, then the history check: no protected box (a hidden
// cell, or the sum of the hidden members of a set) that wasn't exposed
// before S may be exposed after it, if it is a hidden cell or a sum of 1 to
// 4. On a failure one more S cell is hidden and the steps run again. S fully
// hidden adds no equation, so the loop always ends with something safe to
// publish; an error means the computation itself failed.
//
// Exposure is decided modulo two fixed 61-bit primes, exposed when exposed
// under either (§6.3): it errs only by hiding more, except with negligible
// probability. smallgroup/solve is the exact rational reference the tests
// check it against.

import (
	"context"
	"errors"
	"math/big"
	"math/bits"
	"sort"
)

// Family is a linear family: its base categories, its published measures
// (each a set of categories; a cell is one measure of one row), and the
// sets that add up within a row.
type Family struct {
	Name     string
	Cats     int
	Measures [][]int
	RowSets  []RowSet
	// cross is the cross-sectional rules (§6.3 step 1) on one snapshot:
	// vals[row][measure] (row 0 all), forced cells (row*M + measure) hidden
	// first; it returns the hidden pattern.
	cross func(vals [][]int64, forced []bool) ([]bool, error)
}

// RowSet is a set that adds up within a row: the total measure is the sum
// of the part measures and of the virtual parts, category sets that are
// never published (always hidden), such as the unanswered questions.
type RowSet struct {
	Total   int
	Parts   []int
	Virtual [][]int
}

// The QG base categories: answered with pass, regenerate or escalate, each
// not degraded or degraded, and unanswered.
const (
	CatPass = iota
	CatPassDegraded
	CatRegenerate
	CatRegenerateDegraded
	CatEscalate
	CatEscalateDegraded
	CatUnanswered
)

// The QG measures, in cell order.
const (
	QGQuestions = iota
	QGAnswers
	QGPass
	QGRegenerate
	QGEscalate
	QGDegraded
)

// The R categories and measures. agreed and corrected are published
// together, as agreement_rate next to reviews: a shown rate and a shown
// review count give both.
const (
	CatAgreed = iota
	CatCorrected
)

const (
	RReviews = iota
	RAgreed
	RCorrected
)

// QG is questions and answers, one family (§5): a decision belongs to one
// question and is counted in the question's week.
var QG = &Family{
	Name: "QG",
	Cats: 7,
	Measures: [][]int{
		{0, 1, 2, 3, 4, 5, 6}, // questions
		{0, 1, 2, 3, 4, 5},    // answers
		{CatPass, CatPassDegraded},
		{CatRegenerate, CatRegenerateDegraded},
		{CatEscalate, CatEscalateDegraded},
		{CatPassDegraded, CatRegenerateDegraded, CatEscalateDegraded}, // degraded
	},
	RowSets: []RowSet{
		{Total: QGAnswers, Parts: []int{QGPass, QGRegenerate, QGEscalate}},
		{Total: QGQuestions, Parts: []int{QGAnswers}, Virtual: [][]int{{CatUnanswered}}},
		{Total: QGAnswers, Parts: []int{QGDegraded}, Virtual: [][]int{{CatPass, CatRegenerate, CatEscalate}}},
	},
	cross: qgCross,
}

// R is reviews: agreed (validate) and corrected (correct).
var R = &Family{
	Name:     "R",
	Cats:     2,
	Measures: [][]int{{CatAgreed, CatCorrected}, {CatAgreed}, {CatCorrected}},
	RowSets:  []RowSet{{Total: RReviews, Parts: []int{RAgreed, RCorrected}}},
	cross:    rCross,
}

// Published is a stored snapshot of the block, as the guard sees it: the
// slots its period covers, the block hospitals it lists (row r ≥ 1 is
// Listed[r-1]; row 0 is all, their sum), its children (indexes into
// Block.Published; empty for a week) and which cells are hidden
// (row*len(Measures) + measure).
type Published struct {
	Slots    []int
	Listed   []int
	Children []int
	Hidden   []bool
}

// Block is one family's block (a quarter): slots × hospitals × categories
// base counts, X[hospital][slot][category], the frozen counts; each
// hospital's onboarding slot (every slot before it is known zero); and the
// stored snapshots, oldest first.
type Block struct {
	Family    *Family
	Slots     int
	Hospitals int
	X         [][][]int64
	Onboard   []int
	Published []Published
}

// Pending is the new snapshot S: its slots, listed hospitals, children and
// the cells to hide whatever the rules say (an unshowable name, a test).
type Pending struct {
	Slots    []int
	Listed   []int
	Children []int
	Forced   []bool
}

// ErrGuard is a computation that can't be guarded: counts that don't add
// up, a snapshot whose listing or children don't fit the block. The
// snapshot is stored unavailable.
var ErrGuard = errors.New("smallgroup: the snapshot can't be guarded")

func (b *Block) nVars() int { return b.Hospitals * b.Slots * b.Family.Cats }

func (b *Block) v(h, s, c int) int { return (h*b.Slots+s)*b.Family.Cats + c }

// box lists the unknowns of a box.
func (b *Block) box(slots, hosp, cats []int) []int {
	out := make([]int, 0, len(slots)*len(hosp)*len(cats))
	for _, h := range hosp {
		for _, s := range slots {
			for _, c := range cats {
				out = append(out, b.v(h, s, c))
			}
		}
	}
	return out
}

func (b *Block) value(vars []int) int64 {
	var n int64
	nc := b.Family.Cats
	for _, x := range vars {
		c := x % nc
		s := (x / nc) % b.Slots
		h := x / nc / b.Slots
		n += b.X[h][s][c]
	}
	return n
}

// snap is a snapshot's shape, published or pending.
type snap struct {
	slots, listed, children []int
	hidden                  []bool
}

func (b *Block) rowHosp(s snap, r int) []int {
	if r == 0 {
		return s.listed
	}
	return []int{s.listed[r-1]}
}

func (b *Block) cellBox(s snap, cell int) []int {
	m := len(b.Family.Measures)
	return b.box(s.slots, b.rowHosp(s, cell/m), b.Family.Measures[cell%m])
}

// cellValues is vals[row][measure] of a snapshot.
func (b *Block) cellValues(s snap) [][]int64 {
	m := len(b.Family.Measures)
	out := make([][]int64, len(s.listed)+1)
	for r := range out {
		out[r] = make([]int64, m)
		for k := 0; k < m; k++ {
			out[r][k] = b.value(b.cellBox(s, r*m+k))
		}
	}
	return out
}

// member is one member of a set: a cell of a snapshot, or a virtual part.
type member struct {
	snap, cell int // snap -1: virtual
	vars       []int
}

type set struct{ parts []member }

// sets are the sets of snapshot si (index into all; all[si] is its shape):
// its row sets, its column sets and its parent set.
func (b *Block) sets(all []snap, si int) []set {
	s := all[si]
	f := b.Family
	m := len(f.Measures)
	rows := len(s.listed) + 1
	var out []set
	for r := 0; r < rows; r++ {
		for _, rs := range f.RowSets {
			var st set
			for _, p := range rs.Parts {
				st.parts = append(st.parts, member{si, r*m + p, b.cellBox(s, r*m+p)})
			}
			for _, vc := range rs.Virtual {
				st.parts = append(st.parts, member{-1, 0, b.box(s.slots, b.rowHosp(s, r), vc)})
			}
			out = append(out, st)
		}
	}
	for k := 0; k < m; k++ {
		var st set
		for r := 1; r < rows; r++ {
			st.parts = append(st.parts, member{si, r*m + k, b.cellBox(s, r*m+k)})
		}
		out = append(out, st)
	}
	for r := 0; r < rows && len(s.children) > 0; r++ {
		for k := 0; k < m; k++ {
			var st set
			for _, ci := range s.children {
				c := all[ci]
				cr := 0
				if r > 0 {
					cr = indexOf(c.listed, s.listed[r-1]) + 1
					if cr == 0 {
						continue // not listed in the child: a known zero
					}
				}
				st.parts = append(st.parts, member{ci, cr*m + k, b.cellBox(c, cr*m+k)})
			}
			out = append(out, st)
		}
	}
	return out
}

func indexOf(xs []int, x int) int {
	for i, y := range xs {
		if y == x {
			return i
		}
	}
	return -1
}

// known is what snapshot s adds to what is known: the shown cells as
// equations, and the zeros (shown zeros, hospitals of the block it doesn't
// list).
func (b *Block) known(s snap) (eqs [][]int, zeros []int) {
	for c, h := range s.hidden {
		if h {
			continue
		}
		vars := b.cellBox(s, c)
		if b.value(vars) == 0 {
			zeros = append(zeros, vars...)
		} else {
			eqs = append(eqs, vars)
		}
	}
	for h := 0; h < b.Hospitals; h++ {
		if indexOf(s.listed, h) < 0 {
			zeros = append(zeros, b.box(s.slots, []int{h}, allCats(b.Family.Cats))...)
		}
	}
	return eqs, zeros
}

func allCats(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func (b *Block) onboardingZeros() []int {
	var z []int
	for h := 0; h < b.Hospitals; h++ {
		for s := 0; s < b.Slots && s < b.Onboard[h]; s++ {
			z = append(z, b.box([]int{s}, []int{h}, allCats(b.Family.Cats))...)
		}
	}
	return z
}

// ---- the exposure check, modulo two primes ----------------------------------

// The two fixed primes: 2^61 − 1, and the largest prime below it.
var primes = func() [2]uint64 {
	p1 := uint64(1)<<61 - 1
	p2 := p1 - 2
	for !new(big.Int).SetUint64(p2).ProbablyPrime(32) {
		p2 -= 2
	}
	return [2]uint64{p1, p2}
}()

type mterm struct {
	c int
	v uint64
}

type mvec []mterm

func mulmod(a, b, p uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	_, r := bits.Div64(hi, lo, p)
	return r
}

func powmod(a, e, p uint64) uint64 {
	r := uint64(1)
	for e > 0 {
		if e&1 == 1 {
			r = mulmod(r, a, p)
		}
		a = mulmod(a, a, p)
		e >>= 1
	}
	return r
}

// msub is a − f·b mod p, both sorted.
func msub(a, b mvec, f, p uint64) mvec {
	out := make(mvec, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case j >= len(b) || (i < len(a) && a[i].c < b[j].c):
			out = append(out, a[i])
			i++
		case i >= len(a) || b[j].c < a[i].c:
			out = append(out, mterm{b[j].c, (p - mulmod(f, b[j].v, p)) % p})
			j++
		default:
			v := (a[i].v + p - mulmod(f, b[j].v, p)) % p
			if v != 0 {
				out = append(out, mterm{a[i].c, v})
			}
			i++
			j++
		}
	}
	return out
}

// mbasis is a row echelon form mod p: each row's pivot is its smallest
// unknown, with coefficient 1. Rows are never changed once added, so a
// clone shares them.
type mbasis struct {
	p   uint64
	piv map[int]mvec
}

func newMBasis(p uint64) *mbasis { return &mbasis{p: p, piv: map[int]mvec{}} }

func (b *mbasis) clone() *mbasis {
	c := &mbasis{p: b.p, piv: make(map[int]mvec, len(b.piv))}
	for k, v := range b.piv {
		c.piv[k] = v
	}
	return c
}

func (b *mbasis) reduce(v mvec) mvec {
	for len(v) > 0 {
		r, ok := b.piv[v[0].c]
		if !ok {
			return v
		}
		v = msub(v, r, v[0].v, b.p)
	}
	return v
}

// residual removes every pivot column from v: v's unique part on the
// non-pivot columns, linear in v.
func (b *mbasis) residual(v mvec) mvec {
	for i := 0; i < len(v); {
		row, ok := b.piv[v[i].c]
		if !ok {
			i++
			continue
		}
		v = msub(v, row, v[i].v, b.p)
	}
	return v
}

func madd(a, c mvec, p uint64) mvec { return msub(a, c, p-1, p) }

// addIndependent adds v and reports whether it was independent.
func (b *mbasis) addIndependent(v mvec) bool {
	n := len(b.piv)
	b.add(v)
	return len(b.piv) > n
}

// setCheck is a set's hidden members prepared against what was known
// before S: each member's residual under each prime, and the shape then.
type setCheck struct {
	vars []int
	res  [2][]mvec
	dimB [2]int
	sumB [2]bool
}

func (b *Block) prepSets(sets [][][]int, base pair) []setCheck {
	out := make([]setCheck, 0, len(sets))
	for _, hid := range sets {
		var sc setCheck
		for _, m := range hid {
			sc.vars = append(sc.vars, m...)
		}
		for i := range base {
			sc.res[i], sc.dimB[i], sc.sumB[i] = base[i].shape(hid)
		}
		out = append(out, sc)
	}
	return out
}

// shape is the members' residuals against b, the dimension of the
// combinations of them b determines, and whether their full sum is one.
func (b *mbasis) shape(members [][]int) (res []mvec, dim int, sum bool) {
	small := newMBasis(b.p)
	var total mvec
	rank := 0
	for _, m := range members {
		r := dropConst(b.residual(toM(m, nil, b.p)))
		res = append(res, r)
		total = madd(total, r, b.p)
		if small.addIndependent(r) {
			rank++
		}
	}
	return res, len(members) - rank, len(total) == 0
}

// violatesAfter is the rule once S's rows are known: sb holds S's rows'
// residuals against what was known before, so a combination of the members
// is determined after exactly when its residual reduces to 0 against sb.
func (sc setCheck) violatesAfter(sb pair) bool {
	for i := range sb {
		small := newMBasis(sb[i].p)
		var total mvec
		rank := 0
		for _, r := range sc.res[i] {
			r2 := sb[i].residual(r)
			total = madd(total, r2, sb[i].p)
			if small.addIndependent(r2) {
				rank++
			}
		}
		if SetViolates(sc.dimB[i], sc.sumB[i], len(sc.res[i])-rank, len(total) == 0) {
			return true
		}
	}
	return false
}

// setViolates is the whole-set rule (R41a-5): from before to after, the
// determined combinations of the members either stay as they were, or go
// from nothing to the full sum alone. Once anything about the members is
// known (a member already determined, say), a newly determined full sum
// gives the rest away (the full sum minus the known part), so it may not
// become determined.
func SetViolates(dimB int, _ bool, dimA int, sumA bool) bool {
	switch {
	case dimA == dimB:
		return false
	case dimB == 0 && dimA == 1:
		return !sumA
	}
	return true
}

func (b *mbasis) add(v mvec) {
	v = b.reduce(v)
	if len(v) == 0 {
		return
	}
	inv := powmod(v[0].v, b.p-2, b.p)
	w := make(mvec, len(v))
	for i, t := range v {
		w[i] = mterm{t.c, mulmod(t.v, inv, b.p)}
	}
	b.piv[w[0].c] = w
}

// toM is a vector of integer coefficients mod p, sorted, repeats added.
func toM(vars []int, coefs []int64, p uint64) mvec {
	m := map[int]uint64{}
	for i, x := range vars {
		c := int64(1)
		if coefs != nil {
			c = coefs[i]
		}
		r := uint64(c % int64(p))
		if c < 0 {
			r = (p - uint64((-c)%int64(p))) % p
		}
		m[x] = (m[x] + r) % p
	}
	out := make(mvec, 0, len(m))
	for c, v := range m {
		if v != 0 {
			out = append(out, mterm{c, v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].c < out[j].c })
	return out
}

// pair is the two bases, one per prime.
type pair [2]*mbasis

func newPair() pair { return pair{newMBasis(primes[0]), newMBasis(primes[1])} }

func (q pair) clone() pair { return pair{q[0].clone(), q[1].clone()} }

func (q pair) add(vars []int, coefs []int64) {
	for i := range q {
		q[i].add(toM(vars, coefs, q[i].p))
	}
}

// exposed: in the row space under either prime.
func (q pair) exposed(vars []int, coefs []int64) bool {
	for i := range q {
		if len(q[i].reduce(toM(vars, coefs, q[i].p))) == 0 {
			return true
		}
	}
	return false
}

// addAll adds zeros (as unit rows) then equations, shortest first.
func (q pair) addAll(eqs [][]int, zeros []int) {
	for _, z := range zeros {
		q.add([]int{z}, nil)
	}
	sorted := append([][]int{}, eqs...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i]) < len(sorted[j]) })
	for _, e := range sorted {
		q.add(e, nil)
	}
}

// exposedMod is the modular check on general integer equations, for the
// tests (A8): eqs and boxes as (unknowns, coefficients).
func exposedMod(eqs [][]int, eqCoefs [][]int64, boxes [][]int, boxCoefs [][]int64) []bool {
	q := newPair()
	for i, e := range eqs {
		q.add(e, eqCoefs[i])
	}
	out := make([]bool, len(boxes))
	for i, bx := range boxes {
		out[i] = q.exposed(bx, boxCoefs[i])
	}
	return out
}

// ---- GuardSnapshot ----------------------------------------------------------

// protected is a box the check protects: a hidden cell, or the sum of a
// set's hidden members.
type protected struct {
	vars  []int
	cell  bool
	value int64
}

func (b *Block) protectedBoxes(all []snap) []protected {
	var out []protected
	for _, s := range all {
		for c, h := range s.hidden {
			if h {
				vars := b.cellBox(s, c)
				out = append(out, protected{vars, true, b.value(vars)})
			}
		}
	}
	for si := range all {
		for _, st := range b.sets(all, si) {
			var vars []int
			for _, p := range st.parts {
				if p.snap < 0 || all[p.snap].hidden[p.cell] {
					vars = append(vars, p.vars...)
				}
			}
			if len(vars) > 0 {
				out = append(out, protected{vars, false, b.value(vars)})
			}
		}
	}
	return out
}

// hiddenMembers is each set's hidden members (virtual parts included), for
// the whole-set check (R41a-5); sets with fewer than two are left out (the
// cell check covers one).
func (b *Block) hiddenMembers(all []snap) [][][]int {
	var out [][][]int
	for si := range all {
		for _, st := range b.sets(all, si) {
			var hid [][]int
			for _, p := range st.parts {
				if p.snap < 0 || all[p.snap].hidden[p.cell] {
					hid = append(hid, p.vars)
				}
			}
			if len(hid) >= 2 {
				out = append(out, hid)
			}
		}
	}
	return out
}

// validate checks the block and S fit together: counts never negative, a
// hospital S doesn't list has no counts in S's slots, children listed and
// published.
func (b *Block) validate(s Pending) error {
	if b.Family == nil || len(b.X) != b.Hospitals || len(b.Onboard) != b.Hospitals {
		return ErrGuard
	}
	for h := range b.X {
		if len(b.X[h]) != b.Slots {
			return ErrGuard
		}
		for _, row := range b.X[h] {
			if len(row) != b.Family.Cats {
				return ErrGuard
			}
			for _, v := range row {
				if v < 0 {
					return ErrGuard
				}
			}
		}
	}
	all := append(b.Published, Published{Slots: s.Slots, Listed: s.Listed, Children: s.Children})
	m := len(b.Family.Measures)
	for i, p := range all {
		seen := map[int]bool{}
		for _, h := range p.Listed {
			if h < 0 || h >= b.Hospitals || seen[h] {
				return ErrGuard
			}
			seen[h] = true
		}
		for _, sl := range p.Slots {
			if sl < 0 || sl >= b.Slots {
				return ErrGuard
			}
		}
		for h := 0; h < b.Hospitals; h++ {
			if seen[h] {
				continue
			}
			for _, sl := range p.Slots {
				for _, v := range b.X[h][sl] {
					if v != 0 {
						return ErrGuard // an unlisted hospital must have nothing
					}
				}
			}
		}
		if i < len(b.Published) && len(p.Hidden) != (len(p.Listed)+1)*m {
			return ErrGuard
		}
		for _, c := range p.Children {
			if c < 0 || c >= len(b.Published) || c >= i {
				return ErrGuard
			}
			for _, h := range b.Published[c].Listed {
				if !seen[h] {
					return ErrGuard // a parent lists every hospital its children do
				}
			}
		}
	}
	if s.Forced != nil && len(s.Forced) != (len(s.Listed)+1)*m {
		return ErrGuard
	}
	return nil
}

func (b *Block) shapes() []snap {
	out := make([]snap, len(b.Published))
	for i, p := range b.Published {
		out[i] = snap{p.Slots, p.Listed, p.Children, p.Hidden}
	}
	return out
}

// rules runs steps 1 and 2 (§6.3) with forced cells hidden first, until
// neither hides more.
func (b *Block) rules(all []snap, si int, vals [][]int64, forced []bool) ([]bool, error) {
	s := all[si]
	m := len(b.Family.Measures)
	hide := append([]bool{}, forced...)
	for {
		h, err := b.Family.cross(vals, hide)
		if err != nil {
			return nil, err
		}
		if len(s.children) > 0 {
			s.hidden = h
			for k := 0; k < m; k++ {
				pa, rows, hid := b.parentSet(all, s, k, vals)
				out := AcrossRowsFrozen(pa, rows, hid, []int{0})
				for r := range out {
					if out[r][0] {
						h[r*m+k] = true
					}
				}
			}
		}
		same := true
		for i := range h {
			if h[i] != hide[i] {
				same = false
			}
			h[i] = h[i] || hide[i]
		}
		hide = h
		if same {
			return hide, nil
		}
	}
}

// parentSet is measure k's parent set for S: rows all and S's hospitals,
// columns S then each child, with the published hidden state.
func (b *Block) parentSet(all []snap, s snap, k int, vals [][]int64) ([]int64, [][]int64, [][]bool) {
	m := len(b.Family.Measures)
	nr := len(s.listed) + 1
	w := len(s.children) + 1
	v := make([][]int64, nr)
	hid := make([][]bool, nr)
	for r := 0; r < nr; r++ {
		v[r] = make([]int64, w)
		hid[r] = make([]bool, w)
		v[r][0] = vals[r][k]
		hid[r][0] = s.hidden[r*m+k]
		for j, ci := range s.children {
			c := all[ci]
			cr := 0
			if r > 0 {
				cr = indexOf(c.listed, s.listed[r-1]) + 1
				if cr == 0 {
					continue // 0, known
				}
			}
			v[r][j+1] = b.value(b.cellBox(c, cr*m+k))
			hid[r][j+1] = c.hidden[cr*m+k]
		}
	}
	return v[0], v[1:], hid
}

// GuardSnapshot guards the pending snapshot S of the block (§6.3) and
// returns its hidden cells (row*len(Measures) + measure, row 0 all). An
// error means S can't be computed: store it unavailable.
func GuardSnapshot(b *Block, p Pending) ([]bool, error) {
	return GuardSnapshotCtx(context.Background(), b, p)
}

// GuardSnapshotCtx is GuardSnapshot under ctx (T17): it checks ctx at every
// pass of its loop and between sets, and stops with ctx's error, so a
// caller's budget bounds it.
func GuardSnapshotCtx(ctx context.Context, b *Block, p Pending) ([]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := b.validate(p); err != nil {
		return nil, err
	}
	m := len(b.Family.Measures)
	all := append(b.shapes(), snap{p.Slots, p.Listed, p.Children, nil})
	si := len(all) - 1
	vals := b.cellValues(all[si])
	forced := make([]bool, (len(p.Listed)+1)*m)
	copy(forced, p.Forced)

	base := b.before(all, si)
	earlier := b.prepSets(b.hiddenMembers(all[:si]), base)
	for {
		hide, err := b.rules(all, si, vals, forced)
		if err != nil {
			return nil, err
		}
		all[si].hidden = hide
		newly, err := b.newlyExposedWith(ctx, all, si, base, earlier)
		if err != nil {
			return nil, err
		}
		if len(newly) == 0 {
			return hide, nil
		}
		bad := b.cellToHide(all[si], hide, newly)
		if bad < 0 {
			return nil, ErrGuard // fully hidden and still exposing: never expected
		}
		forced = append([]bool{}, hide...)
		forced[bad] = true
	}
}

// before is what is known before snapshot si: onboarding and every earlier
// snapshot (E0's equations).
func (b *Block) before(all []snap, si int) pair {
	base := newPair()
	var eqs [][]int
	zeros := b.onboardingZeros()
	for _, s := range all[:si] {
		e, z := b.known(s)
		eqs = append(eqs, e...)
		zeros = append(zeros, z...)
	}
	base.addAll(eqs, zeros)
	return base
}

// newlyExposed is every protected box (a hidden cell, or a hidden-member
// sum of 1 to 4) exposed once snapshot si is added to base but not before
// (E1 minus E0).
func (b *Block) newlyExposed(all []snap, si int, base pair) [][]int {
	newly, _ := b.newlyExposedWith(context.Background(), all, si, base, nil)
	return newly
}

// newlyExposedWith is newlyExposed with the earlier snapshots' sets
// prepared once (GuardSnapshot reuses them across its loop).
func (b *Block) newlyExposedWith(ctx context.Context, all []snap, si int, base pair, earlier []setCheck) ([][]int, error) {
	after := base.clone()
	e, z := b.known(all[si])
	after.addAll(e, z)
	var newly [][]int
	for _, pr := range b.protectedBoxes(all[:si+1]) {
		if !pr.cell && !small(pr.value) {
			continue
		}
		if after.exposed(pr.vars, nil) && !base.exposed(pr.vars, nil) {
			newly = append(newly, pr.vars)
		}
	}
	// R41a-5: the whole set, under either prime.
	if earlier == nil {
		earlier = b.prepSets(b.hiddenMembers(all[:si]), base)
	}
	sb := pair{newMBasis(primes[0]), newMBasis(primes[1])}
	for i := range sb {
		for _, zv := range z {
			sb[i].add(base[i].residual(toM([]int{zv}, nil, base[i].p)))
		}
		for _, row := range e {
			sb[i].add(base[i].residual(toM(row, nil, base[i].p)))
		}
	}
	var own [][][]int
	for _, st := range b.sets(all, si) {
		var hid [][]int
		for _, p := range st.parts {
			if p.snap < 0 || all[p.snap].hidden[p.cell] {
				hid = append(hid, p.vars)
			}
		}
		if len(hid) >= 2 {
			own = append(own, hid)
		}
	}
	for _, sc := range append(earlier, b.prepSets(own, base)...) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if sc.violatesAfter(sb) {
			newly = append(newly, sc.vars)
		}
	}
	return newly, nil
}

// cellToHide is the shown S cell with the smallest non-zero value whose box
// overlaps a newly exposed box; else the smallest overlapping one; else the
// smallest shown S cell; -1 when every cell is hidden.
func (b *Block) cellToHide(s snap, hide []bool, newly [][]int) int {
	in := map[int]bool{}
	for _, vs := range newly {
		for _, x := range vs {
			in[x] = true
		}
	}
	best := [3]int{-1, -1, -1}
	var bestV [3]int64
	for c, h := range hide {
		if h {
			continue
		}
		vars := b.cellBox(s, c)
		v := b.value(vars)
		overlap := false
		for _, x := range vars {
			if in[x] {
				overlap = true
				break
			}
		}
		tier := 2
		if overlap && v != 0 {
			tier = 0
		} else if overlap {
			tier = 1
		}
		if best[tier] < 0 || v < bestV[tier] {
			best[tier], bestV[tier] = c, v
		}
	}
	for _, c := range best {
		if c >= 0 {
			return c
		}
	}
	return -1
}

// ---- the cross-sectional rules of each family (step 1) ----------------------

func qgCross(vals [][]int64, forced []bool) ([]bool, error) {
	const m = 6
	n := len(vals) - 1
	for r := range vals {
		v := vals[r]
		if len(v) != m || v[QGAnswers] != v[QGPass]+v[QGRegenerate]+v[QGEscalate] ||
			v[QGAnswers] > v[QGQuestions] || v[QGDegraded] > v[QGAnswers] {
			return nil, ErrGuard
		}
		for _, x := range v {
			if x < 0 {
				return nil, ErrGuard
			}
		}
	}
	for k := 0; k < m; k++ {
		var s int64
		for r := 1; r <= n; r++ {
			s += vals[r][k]
		}
		if s != vals[0][k] {
			return nil, ErrGuard
		}
	}
	hide := append([]bool{}, forced...)
	for {
		// The questions column: a set that adds up (rules 1 and 2).
		qs := make([]int64, n)
		qf := make([]bool, n+1)
		qf[0] = hide[QGQuestions]
		for r := 1; r <= n; r++ {
			qs[r-1] = vals[r][QGQuestions]
			qf[r] = hide[r*m+QGQuestions]
		}
		qg := setForced(vals[0][QGQuestions], qs, qf)
		next := append([]bool{}, hide...)
		for r := 0; r <= n; r++ {
			if qg[r].Suppressed {
				next[r*m+QGQuestions] = true
			}
			// answers within questions: hidden with them, or when the
			// unanswered are 1 to 4.
			if next[r*m+QGQuestions] || small(vals[r][QGQuestions]-vals[r][QGAnswers]) {
				next[r*m+QGAnswers] = true
			}
		}
		rows := make([][]int64, n)
		sub := make([]int64, n)
		mask := Mask{All: make([]bool, 4), Rows: make([][]bool, n), Subset: make([]bool, n+1)}
		for k := 0; k < 4; k++ {
			mask.All[k] = next[QGAnswers+k]
		}
		mask.Subset[0] = next[QGDegraded]
		for r := 1; r <= n; r++ {
			rows[r-1] = []int64{vals[r][QGAnswers], vals[r][QGPass], vals[r][QGRegenerate], vals[r][QGEscalate]}
			sub[r-1] = vals[r][QGDegraded]
			mask.Rows[r-1] = make([]bool, 4)
			for k := 0; k < 4; k++ {
				mask.Rows[r-1][k] = next[r*m+QGAnswers+k]
			}
			mask.Subset[r] = next[r*m+QGDegraded]
		}
		t := TableForced(4, rows, sub, mask)
		for k := 0; k < 4; k++ {
			next[QGAnswers+k] = next[QGAnswers+k] || t.All[k].Suppressed
		}
		next[QGDegraded] = next[QGDegraded] || t.Subset[0].Suppressed
		for r := 1; r <= n; r++ {
			for k := 0; k < 4; k++ {
				next[r*m+QGAnswers+k] = next[r*m+QGAnswers+k] || t.Rows[r-1][k].Suppressed
			}
			next[r*m+QGDegraded] = next[r*m+QGDegraded] || t.Subset[r].Suppressed
		}
		if equalBools(next, hide) {
			return hide, nil
		}
		hide = next
	}
}

func rCross(vals [][]int64, forced []bool) ([]bool, error) {
	const m = 3
	n := len(vals) - 1
	for r := range vals {
		v := vals[r]
		if len(v) != m || v[RReviews] != v[RAgreed]+v[RCorrected] || v[RAgreed] < 0 || v[RCorrected] < 0 {
			return nil, ErrGuard
		}
	}
	hide := append([]bool{}, forced...)
	for {
		rows := make([][]int64, n)
		mask := Mask{All: hide[0:m], Rows: make([][]bool, n)}
		for r := 1; r <= n; r++ {
			rows[r-1] = vals[r]
			mask.Rows[r-1] = hide[r*m : (r+1)*m]
		}
		t := TableForced(m, rows, make([]int64, n), mask)
		next := append([]bool{}, hide...)
		for r := 0; r <= n; r++ {
			g := t.All
			if r > 0 {
				g = t.Rows[r-1]
			}
			for k := 0; k < m; k++ {
				next[r*m+k] = next[r*m+k] || g[k].Suppressed
			}
			// agreed and corrected are published together, and only next to
			// a shown review count (agreement_rate).
			if next[r*m+RReviews] || next[r*m+RAgreed] || next[r*m+RCorrected] {
				next[r*m+RAgreed], next[r*m+RCorrected] = true, true
			}
		}
		if equalBools(next, hide) {
			return hide, nil
		}
		hide = next
	}
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- the check as a library (Analysis, plan Task 5) -------------------------

// Knowledge is what an attacker knows, as GuardSnapshot keeps it: linear
// equations over integer unknowns, decided modulo the two primes (exposed
// when exposed under either). It errs only by finding more exposed, except
// with negligible probability. Analysis's guard (internal/analysis/disclose)
// runs its final check on it; smallgroup/solve is the exact reference.
type Knowledge struct{ q pair }

// NewKnowledge is nothing known.
func NewKnowledge() *Knowledge { return &Knowledge{newPair()} }

// Clone is a copy that learns independently.
func (k *Knowledge) Clone() *Knowledge { return &Knowledge{k.q.clone()} }

// Add learns Σ coefs[i]·x[vars[i]] (coefs nil: every coefficient 1).
func (k *Knowledge) Add(vars []int, coefs []int64) { k.q.add(vars, coefs) }

// Exposed reports whether Σ coefs[i]·x[vars[i]] is determined.
func (k *Knowledge) Exposed(vars []int, coefs []int64) bool { return k.q.exposed(vars, coefs) }

// constVar is the column of the constant 1 in a valued system: a row
// Σ a·x = rhs is kept as Σ a·x − rhs·1. It sorts last, so it is never a
// pivot while the equations are consistent. 7c's systems never use it.
const constVar = 1<<31 - 1

// dropConst is a residual without its constant: the part that depends on
// the unknowns.
func dropConst(v mvec) mvec {
	if n := len(v); n > 0 && v[n-1].c == constVar {
		return v[:n-1]
	}
	return v
}

// AddValued learns Σ coefs[i]·x[vars[i]] = rhs (coefs nil: every
// coefficient 1).
func (k *Knowledge) AddValued(vars []int, coefs []int64, rhs int64) {
	c := coefs
	if c == nil {
		c = make([]int64, len(vars))
		for i := range c {
			c[i] = 1
		}
	}
	k.Add(append(append([]int{}, vars...), constVar), append(append([]int64{}, c...), -rhs))
}

// Value is Σ coefs[i]·x[vars[i]] when what is known determines it (under
// either prime; the true value is a count far below the primes, so its
// residue is the value).
func (k *Knowledge) Value(vars []int, coefs []int64) (int64, bool) {
	for i := range k.q {
		p := k.q[i].p
		r := k.q[i].residual(toM(vars, coefs, p))
		switch {
		case len(r) == 0:
			return 0, true
		case len(r) == 1 && r[0].c == constVar:
			v := r[0].v
			if v > p/2 {
				return -int64(p - v), true
			}
			return int64(v), true
		}
	}
	return 0, false
}

// Consistent reports whether what is known can all hold at once: with
// values, a contradiction is the row 0 = c ≠ 0, and the constant becomes
// determined as 0 under both primes.
func (k *Knowledge) Consistent() bool {
	for i := range k.q {
		r := k.q[i].residual(mvec{{constVar, 1}})
		if len(r) == 1 && r[0].c == constVar {
			return true
		}
	}
	return false
}

// SetShape is what a state of knowledge determines about a set's hidden
// members, under each prime: the dimension and whether the full sum is
// determined. Taken once, it is checked against later states (hoisted out
// of a guard's retry loop).
type SetShape [2]struct {
	dim int
	sum bool
}

// Shape is members' shape under k.
func (k *Knowledge) Shape(members [][]int) SetShape {
	var s SetShape
	for i := range k.q {
		_, s[i].dim, s[i].sum = k.q[i].shape(members)
	}
	return s
}

// Changed is the whole-set rule (R41a-5, SetViolates) from this shape to
// after, under either prime.
func (s SetShape) Changed(after *Knowledge, members [][]int) bool {
	for i := range after.q {
		_, da, sa := after.q[i].shape(members)
		if SetViolates(s[i].dim, s[i].sum, da, sa) {
			return true
		}
	}
	return false
}

// SetChanged is the whole-set rule (R41a-5, SetViolates) from before to
// after, under either prime, for a set's hidden members (each the sum of
// its unknowns).
func SetChanged(before, after *Knowledge, members [][]int) bool {
	return before.Shape(members).Changed(after, members)
}
