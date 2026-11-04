package smallgroup

import (
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"strings"
	"testing"
)

// C20 (property): for random per-hospital tables, no hidden cell can be
// worked out from what is shown. The solver below is the test's own, not
// the package's bridge check: it writes every equation the response
// implies (each row's total = Σ its parts, all included; each column's all
// = Σ hospitals; degraded's all = Σ hospitals), puts the shown cells on the
// right-hand side, and runs exact Gaussian elimination over the hidden
// cells. A hidden cell is recovered when its reduced row has no other
// unknown in it: that covers a row sum, all − Σ shown, and any longer
// chain of them.

// table is one random table: per hospital [answers, pass, regenerate,
// escalate] and degraded.
type table struct {
	rows     [][]int64
	degraded []int64
}

// count draws a count biased to the small values that matter.
func count(r *rand.Rand) int64 {
	switch r.Intn(4) {
	case 0:
		return int64(r.Intn(5)) // 0..4
	case 1:
		return int64(r.Intn(8)) // 0..7
	case 2:
		return int64(r.Intn(15))
	default:
		return int64(r.Intn(60))
	}
}

func randomTable(r *rand.Rand) table {
	n := 1 + r.Intn(8)
	var t table
	for i := 0; i < n; i++ {
		p, g, e := count(r), count(r), count(r)
		if r.Intn(6) == 0 {
			p, g, e = 0, 0, 0
		}
		ans := p + g + e
		t.rows = append(t.rows, []int64{ans, p, g, e})
		d := int64(0)
		if ans > 0 {
			d = int64(r.Intn(int(min(ans, 9)) + 1))
		}
		t.degraded = append(t.degraded, d)
	}
	return t
}

func sums(rows [][]int64) []int64 {
	all := make([]int64, len(rows[0]))
	for _, row := range rows {
		for j, v := range row {
			all[j] += v
		}
	}
	return all
}

// recovered runs the solver: equations eqs over cells (each a list of
// (cell, coefficient)), with known values for shown cells. It returns the
// hidden cells the equations pin down.
func recovered(nCells int, eqs [][][2]int, hide []bool) []int {
	var unknown []int
	col := make(map[int]int)
	for c := 0; c < nCells; c++ {
		if hide[c] {
			col[c] = len(unknown)
			unknown = append(unknown, c)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	// The right-hand side doesn't matter for whether a cell is pinned.
	var m [][]*big.Rat
	for _, eq := range eqs {
		row := make([]*big.Rat, len(unknown))
		for i := range row {
			row[i] = new(big.Rat)
		}
		any := false
		for _, t := range eq {
			if j, ok := col[t[0]]; ok {
				row[j].Add(row[j], big.NewRat(int64(t[1]), 1))
				any = true
			}
		}
		if any {
			m = append(m, row)
		}
	}
	// Reduced row echelon form.
	pivotRow := 0
	pivots := map[int]int{} // column → row
	for c := 0; c < len(unknown) && pivotRow < len(m); c++ {
		p := -1
		for r := pivotRow; r < len(m); r++ {
			if m[r][c].Sign() != 0 {
				p = r
				break
			}
		}
		if p < 0 {
			continue
		}
		m[pivotRow], m[p] = m[p], m[pivotRow]
		inv := new(big.Rat).Inv(m[pivotRow][c])
		for j := range m[pivotRow] {
			m[pivotRow][j].Mul(m[pivotRow][j], inv)
		}
		for r := range m {
			if r == pivotRow || m[r][c].Sign() == 0 {
				continue
			}
			f := new(big.Rat).Set(m[r][c])
			for j := range m[r] {
				m[r][j].Sub(m[r][j], new(big.Rat).Mul(f, m[pivotRow][j]))
			}
		}
		pivots[c] = pivotRow
		pivotRow++
	}
	var out []int
	for c, r := range pivots {
		alone := true
		for j, v := range m[r] {
			if j != c && v.Sign() != 0 {
				alone = false
				break
			}
		}
		if alone {
			out = append(out, unknown[c])
		}
	}
	return out
}

// guard runs the rules as the route does (Table). Cells: the all row (4),
// each hospital (4), then degraded's all, then each hospital's degraded.
func guard(t table) ([]int64, []Guarded) {
	all := sums(t.rows)
	o := Table(4, t.rows, t.degraded)
	var dAll int64
	for _, d := range t.degraded {
		dAll += d
	}
	vals := append([]int64{}, all...)
	out := append([]Guarded{}, o.All...)
	for i, row := range t.rows {
		vals = append(vals, row...)
		out = append(out, o.Rows[i]...)
	}
	vals = append(vals, dAll)
	vals = append(vals, t.degraded...)
	out = append(out, o.Subset...)
	return vals, out
}

// equations are every equation the response implies, over guard's cells.
func equations(h int) [][][2]int {
	const w = 4
	cell := func(i, j int) int { return i*w + j } // i = 0 the all row
	var eqs [][][2]int
	for i := 0; i <= h; i++ {
		eq := [][2]int{{cell(i, 0), 1}}
		for j := 1; j < w; j++ {
			eq = append(eq, [2]int{cell(i, j), -1})
		}
		eqs = append(eqs, eq)
	}
	for j := 0; j < w; j++ {
		eq := [][2]int{{cell(0, j), 1}}
		for i := 1; i <= h; i++ {
			eq = append(eq, [2]int{cell(i, j), -1})
		}
		eqs = append(eqs, eq)
	}
	d0 := (h + 1) * w
	eq := [][2]int{{d0, 1}}
	for i := 1; i <= h; i++ {
		eq = append(eq, [2]int{d0 + i, -1})
	}
	return append(eqs, eq)
}

// atoms lists, for each cell, the hospital-level atoms it adds up, so
// nested shown pairs can be compared (secure-build: a shown pair where one
// contains the other must not differ by 1 to 4). An atom is (hospital,
// part, degraded or not): degraded is within answers, not within any one
// part, so a part and degraded are never nested, but answers and degraded
// are.
func atoms(h int) [][]int {
	const w = 4
	atom := func(hi, pj, d int) int { return ((hi-1)*(w-1)+(pj-1))*2 + d }
	var out [][]int
	for i := 0; i <= h; i++ {
		for j := 0; j < w; j++ {
			var a []int
			for hi := 1; hi <= h; hi++ {
				if i != 0 && hi != i {
					continue
				}
				for pj := 1; pj < w; pj++ {
					if j != 0 && pj != j {
						continue
					}
					a = append(a, atom(hi, pj, 0), atom(hi, pj, 1))
				}
			}
			out = append(out, a)
		}
	}
	deg := func(hi int) []int {
		var a []int
		for pj := 1; pj < w; pj++ {
			a = append(a, atom(hi, pj, 1))
		}
		return a
	}
	var dAll []int
	for hi := 1; hi <= h; hi++ {
		dAll = append(dAll, deg(hi)...)
	}
	out = append(out, dAll)
	for hi := 1; hi <= h; hi++ {
		out = append(out, deg(hi))
	}
	return out
}

func subset(a, b []int) bool {
	in := map[int]bool{}
	for _, x := range b {
		in[x] = true
	}
	for _, x := range a {
		if !in[x] {
			return false
		}
	}
	return true
}

// leak runs the property over n random tables and describes the first
// table that gives a hidden value away, or returns "".
func leak(seed int64, n int) string {
	r := rand.New(rand.NewSource(seed))
	hiddenCells := 0
	for k := 0; k < n; k++ {
		tb := randomTable(r)
		h := len(tb.rows)
		vals, g := guard(tb)
		hide := make([]bool, len(vals))
		for i, x := range g {
			hide[i] = x.Suppressed
			if x.Suppressed {
				hiddenCells++
			}
			if x.Shown() && (*x.Value != vals[i] || small(vals[i])) {
				return fmt.Sprintf("table %d %v: cell %d shown as %d (raw %d)", k, tb, i, *x.Value, vals[i])
			}
			if !x.Shown() && !x.Suppressed {
				return fmt.Sprintf("table %d: cell %d neither shown nor hidden", k, i)
			}
		}
		if got := recovered(len(vals), equations(h), hide); len(got) > 0 {
			return fmt.Sprintf("table %d %v (degraded %v): hidden cells %v recovered", k, tb.rows, tb.degraded, got)
		}
		at := atoms(h)
		for i := range vals {
			for j := range vals {
				if i == j || hide[i] || hide[j] || !subset(at[j], at[i]) {
					continue
				}
				if d := vals[i] - vals[j]; small(d) {
					return fmt.Sprintf("table %d %v (degraded %v): shown cells %d and %d differ by %d", k, tb.rows, tb.degraded, i, j, d)
				}
			}
		}
		b, _ := json.Marshal(g)
		if smallValue.Match(b) {
			return fmt.Sprintf("table %d encodes a small value: %s", k, b)
		}
	}
	if hiddenCells == 0 {
		return "no cell was ever hidden: the generator tests nothing"
	}
	return ""
}

func TestNoHiddenValueDerivable(t *testing.T) {
	if msg := leak(20261103, 10000); msg != "" {
		t.Fatal(msg)
	}
}

// With the bridge repair off (rule 4 taken literally: two hidden per
// column, hidden sums at least 5), the same property finds a hidden value
// it can solve for: the repair is what C20 rests on.
func TestPropertyCatchesLiteralRule4(t *testing.T) {
	skipBridge = true
	defer func() { skipBridge = false }()
	msg := leak(20261103, 10000)
	if !strings.Contains(msg, "recovered") {
		t.Fatalf("the property passed rule 4 taken literally: %q", msg)
	}
}

// The solver itself finds what a naive table gives away: one hidden cell in
// a row, and all − Σ shown down a column.
func TestSolverFindsLeaks(t *testing.T) {
	// One hospital, [10, 7, 3, 0], degraded 0: hide only the 3.
	h := 1
	n := (h+1)*4 + h + 1
	hide := make([]bool, n)
	hide[4+2] = true
	if got := recovered(n, equations(h), hide); len(got) != 1 || got[0] != 6 {
		t.Errorf("one hidden cell in a row: recovered %v", got)
	}
	// Two hospitals; hospital 1's regenerate and escalate hidden (a pair in
	// its row), nothing else: the columns give both back.
	h = 2
	n = (h+1)*4 + h + 1
	hide = make([]bool, n)
	hide[4+2], hide[4+3] = true, true
	if got := recovered(n, equations(h), hide); len(got) != 2 {
		t.Errorf("a row pair, columns shown: recovered %v", got)
	}
	// A 2x2 cycle of hidden cells gives nothing back.
	hide = make([]bool, n)
	hide[4+2], hide[4+3], hide[8+2], hide[8+3] = true, true, true, true
	if got := recovered(n, equations(h), hide); len(got) != 0 {
		t.Errorf("a hidden cycle: recovered %v", got)
	}
}
