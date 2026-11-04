// Package smallgroup holds the small-group rules (Plan 4 spec §6.3;
// contract §9.1, §14.7). Every count that leaves the service across
// hospitals goes through it, so a raw 1 to 4 never reaches a response
// writer: a value is guarded here, before it is encoded.
//
//  1. Count: 0 is shown as 0, 1 to 4 are hidden, 5 and up are shown.
//  2. Set: in a set that adds up (a total and its parts), if any part is
//     hidden, enough others are hidden that the hidden parts together are
//     at least 5 when the total is shown (so total − Σ shown is never 1 to
//     4), and never exactly one part is hidden.
//  3. Share: a share is hidden when its numerator or its complement
//     (denominator − numerator) is 1 to 4, or when the denominator is
//     hidden; ShareOf also hides it when the numerator is hidden.
//  4. AcrossRows: a table of per-hospital rows and the all-hospitals row
//     (the column totals). Rule 2 holds on every row and every column,
//     and no hidden cell can be worked out from the shown ones by any
//     linear combination of the row, column and total equations: the
//     table is checked as a graph (below) and more cells are hidden until
//     none can.
//
// The determinability check: write the table with its margins negated
// (each hospital row's total and each column's all-hospitals total, with
// the grand total positive), and every row and every column sums to zero.
// The hidden cells are the unknowns; each row and column is one equation
// in them. That is the incidence matrix of a bipartite graph (rows and
// columns the vertices, a hidden cell the edge between its row and its
// column), whose null space is the graph's cycle space. So a hidden cell
// is determined exactly when its edge is on no cycle: a bridge. A table
// with no bridge among its hidden cells gives no hidden cell away. The
// property test (C20) checks this against a solver of its own.
package smallgroup

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
)

// Min is the smallest group shown: a count of 1 to Min−1 is hidden.
const Min = 5

// Guarded is a count under the small-group rule. Shown: Value is set and
// Suppressed false. Hidden: Value nil and Suppressed true. Nothing to show
// (not used for counts): both empty. It encodes as {value, suppressed}.
type Guarded struct {
	Value      *int64
	Suppressed bool
}

// MarshalJSON writes {"value": n | null, "suppressed": bool}. A hidden
// value is always null, whatever Value holds.
func (g Guarded) MarshalJSON() ([]byte, error) {
	if g.Suppressed || g.Value == nil {
		return json.Marshal(struct {
			Value      *int64 `json:"value"`
			Suppressed bool   `json:"suppressed"`
		}{nil, g.Suppressed})
	}
	return []byte(`{"value":` + strconv.FormatInt(*g.Value, 10) + `,"suppressed":false}`), nil
}

// UnmarshalJSON reads {value, suppressed} strictly, for a stored response
// read back: a shown count of 1 to 4 (or below 0), a hidden one with a
// value, or a count with nothing to show is refused, so a stored body that
// breaks rule 1 is never served.
func (g *Guarded) UnmarshalJSON(b []byte) error {
	var v struct {
		Value      *int64 `json:"value"`
		Suppressed *bool  `json:"suppressed"`
	}
	if err := strictDecode(b, &v); err != nil {
		return err
	}
	switch {
	case v.Suppressed == nil:
		return errGuarded
	case *v.Suppressed && v.Value == nil:
		*g = hidden
	case !*v.Suppressed && v.Value != nil && *v.Value >= 0 && !small(*v.Value):
		*g = shown(*v.Value)
	default:
		return errGuarded
	}
	return nil
}

// Shown reports whether the value is sent.
func (g Guarded) Shown() bool { return !g.Suppressed && g.Value != nil }

// ShareDigits is the decimal places a share is sent with.
const ShareDigits = 4

// GuardedShare is a share (0 to 1) under the small-group rule, encoded as
// {value, suppressed}. A shown share is always written with ShareDigits
// decimal places (1 is 1.0000), so no share can read as a small count.
type GuardedShare struct {
	Share      *float64
	Suppressed bool
}

// MarshalJSON writes {"value": 0.0000–1.0000 | null, "suppressed": bool}.
func (g GuardedShare) MarshalJSON() ([]byte, error) {
	if g.Suppressed || g.Share == nil || math.IsNaN(*g.Share) || *g.Share < 0 || *g.Share > 1 {
		return json.Marshal(struct {
			Value      *float64 `json:"value"`
			Suppressed bool     `json:"suppressed"`
		}{nil, g.Suppressed || g.Share != nil})
	}
	return []byte(`{"value":` + strconv.FormatFloat(*g.Share, 'f', ShareDigits, 64) + `,"suppressed":false}`), nil
}

// UnmarshalJSON reads {value, suppressed} strictly: a shown share is 0 to
// 1, a hidden one has no value.
func (g *GuardedShare) UnmarshalJSON(b []byte) error {
	var v struct {
		Value      *float64 `json:"value"`
		Suppressed *bool    `json:"suppressed"`
	}
	if err := strictDecode(b, &v); err != nil {
		return err
	}
	switch {
	case v.Suppressed == nil:
		return errGuarded
	case v.Value == nil:
		*g = GuardedShare{Suppressed: *v.Suppressed}
	case !*v.Suppressed && *v.Value >= 0 && *v.Value <= 1:
		x := *v.Value
		*g = GuardedShare{Share: &x}
	default:
		return errGuarded
	}
	return nil
}

var errGuarded = errors.New("smallgroup: not a guarded value")

func strictDecode(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		return errGuarded
	}
	return nil
}

// Shown reports whether the share is sent.
func (g GuardedShare) Shown() bool { return !g.Suppressed && g.Share != nil }

func small(n int64) bool { return n >= 1 && n < Min }

func shown(n int64) Guarded { return Guarded{Value: &n} }

var hidden = Guarded{Suppressed: true}

// Count is rule 1: 0 is 0, 1 to 4 are hidden, 5 and up are shown. A
// negative count is no count: hidden (fail closed).
func Count(n int64) Guarded {
	if n < 0 || small(n) {
		return hidden
	}
	return shown(n)
}

// Share is rule 3 over raw counts: num / den, hidden when num or den − num
// is 1 to 4, or den is (or num is outside 0..den). A den of 0 is nothing
// to divide by: {null, false}.
func Share(num, den int64) GuardedShare {
	switch {
	case den == 0 && num == 0:
		return GuardedShare{}
	case den < 0 || num < 0 || num > den || small(den) || small(num) || small(den-num):
		return GuardedShare{Suppressed: true}
	}
	v := math.Round(float64(num)/float64(den)*math.Pow10(ShareDigits)) / math.Pow10(ShareDigits)
	return GuardedShare{Share: &v}
}

// ShareOf is rule 3 as a response applies it: Share(num, den), and hidden
// as well when the guarded numerator or denominator is hidden (a shown
// share and a shown denominator would give the hidden numerator back).
func ShareOf(num, den int64, numG, denG Guarded) GuardedShare {
	if !numG.Shown() || !denG.Shown() {
		if den == 0 && !denG.Suppressed {
			return GuardedShare{}
		}
		return GuardedShare{Suppressed: true}
	}
	return Share(num, den)
}

// skipBridge turns the bridge repair off, so a test can show the property
// test catches rule 4 taken literally (two hidden per column). Never set
// outside tests. It stays an unexported variable because the tests that set it
// are in this package (an export_test.go setter would not hide it from them).
var skipBridge bool

// line is one set that adds up: total = Σ parts, as cell indexes.
type line struct {
	total int
	parts []int
}

// grid is the cells being guarded and which are hidden. pri orders the
// cells to hide when more must be: lower first.
type grid struct {
	val  []int64
	hide []bool
	pri  []int
}

func newGrid(vals []int64) *grid {
	g := &grid{val: vals, hide: make([]bool, len(vals)), pri: make([]int, len(vals))}
	for i, v := range vals {
		g.hide[i] = v < 0 || small(v)
	}
	return g
}

// better is whether cell a is a better one to hide next than b: lower
// priority, then a non-zero before a zero (a zero adds nothing to the
// hidden sum), then the smaller value, then the lower index.
func (g *grid) better(a, b int) bool {
	if g.pri[a] != g.pri[b] {
		return g.pri[a] < g.pri[b]
	}
	if (g.val[a] == 0) != (g.val[b] == 0) {
		return g.val[a] != 0
	}
	if g.val[a] != g.val[b] {
		return g.val[a] < g.val[b]
	}
	return a < b
}

// pick is the best shown cell of cells, or -1.
func (g *grid) pick(cells []int) int {
	best := -1
	for _, c := range cells {
		if !g.hide[c] && (best < 0 || g.better(c, best)) {
			best = c
		}
	}
	return best
}

// enforce applies rule 2 to one line and reports whether it hid anything.
// The total shown: the hidden parts are none, or at least two summing to 5
// or more. The total hidden: never exactly one hidden part while another
// part can be hidden. When the parts run out, the total is hidden.
func (g *grid) enforce(l line) bool {
	changed := false
	for {
		var n int
		var sum int64
		for _, p := range l.parts {
			if g.hide[p] {
				n++
				sum += g.val[p]
			}
		}
		if n == 0 {
			return changed
		}
		if g.hide[l.total] {
			if n != 1 {
				return changed
			}
			c := g.pick(l.parts)
			if c < 0 {
				return changed
			}
			g.hide[c], changed = true, true
			continue
		}
		if n >= 2 && !small(sum) {
			return changed
		}
		c := g.pick(l.parts)
		if c < 0 {
			c = l.total
		}
		g.hide[c], changed = true, true
	}
}

// Set is rules 1 and 2 over one set: total = Σ parts. It returns the
// guarded total first, then each part in order.
func Set(total int64, parts ...int64) []Guarded {
	vals := append([]int64{total}, parts...)
	g := newGrid(vals)
	l := line{total: 0}
	for i := range parts {
		l.parts = append(l.parts, i+1)
	}
	g.enforce(l)
	return g.out()
}

func (g *grid) out() []Guarded {
	out := make([]Guarded, len(g.val))
	for i, v := range g.val {
		if g.hide[i] {
			out[i] = hidden
		} else {
			out[i] = shown(v)
		}
	}
	return out
}

// AcrossRows is rules 1, 2 and 4 over a table. Each row is [total,
// part1, …, partK]: a set that adds up. all is the column totals in the
// same shape (all[j] = Σ rows[i][j]), itself a row and, column by column,
// the total of the hospital rows. It returns the guarded all row and rows.
//
// Every row (all included) and every column keeps rule 2; then while any
// hidden cell is a bridge of the hidden-cell graph (it could be solved for),
// one more cell is hidden, a hospital's before the all row's. It ends: the
// table entirely hidden has no bridge. Rows of the wrong width, or an all
// row that isn't the column sums, hide everything (fail closed).
func AcrossRows(all []int64, rows [][]int64) ([]Guarded, [][]Guarded) {
	w := len(all)
	consistent := w >= 2
	for j := 0; consistent && j < w; j++ {
		var s int64
		for _, r := range rows {
			if len(r) != w {
				consistent = false
				break
			}
			s += r[j]
		}
		consistent = consistent && s == all[j]
	}
	for _, r := range append([][]int64{all}, rows...) {
		if !consistent || len(r) != w {
			consistent = false
			break
		}
		var s int64
		for _, v := range r[1:] {
			s += v
		}
		consistent = s == r[0]
	}
	if !consistent {
		a := make([]Guarded, len(all))
		for j := range a {
			a[j] = hidden
		}
		out := make([][]Guarded, len(rows))
		for i, r := range rows {
			out[i] = make([]Guarded, len(r))
			for j := range r {
				out[i][j] = hidden
			}
		}
		return a, out
	}

	// Cell (i, j) is i*w + j; row 0 is all, rows 1.. the hospitals.
	h := len(rows) + 1
	vals := make([]int64, 0, h*w)
	vals = append(vals, all...)
	for _, r := range rows {
		vals = append(vals, r...)
	}
	g := newGrid(vals)
	for j := 0; j < w; j++ {
		g.pri[j] = 1 // the all row last
	}
	cell := func(i, j int) int { return i*w + j }
	var lines []line
	for i := 0; i < h; i++ {
		l := line{total: cell(i, 0)}
		for j := 1; j < w; j++ {
			l.parts = append(l.parts, cell(i, j))
		}
		lines = append(lines, l)
	}
	for j := 0; j < w; j++ {
		l := line{total: cell(0, j)}
		for i := 1; i < h; i++ {
			l.parts = append(l.parts, cell(i, j))
		}
		lines = append(lines, l)
	}
	for {
		changed := false
		for _, l := range lines {
			if g.enforce(l) {
				changed = true
			}
		}
		if changed {
			continue
		}
		b, ok := bridge(g.hide, h, w)
		if !ok || skipBridge {
			break
		}
		g.hide[g.breakBridge(b, h, w)] = true
	}
	o := g.out()
	allOut := o[:w]
	out := make([][]Guarded, len(rows))
	for i := range rows {
		out[i] = o[(i+1)*w : (i+2)*w]
	}
	return allOut, out
}

// The hidden-cell graph: vertex i (0 ≤ i < h) is row i, vertex h + j is
// column j; hidden cell (i, j) is the edge between them.

// bridge finds a hidden cell whose edge is a bridge (Tarjan's low-link,
// iteratively), as its cell index.
func bridge(hide []bool, h, w int) (int, bool) {
	n := h + w
	adj := make([][][2]int, n) // (neighbour, cell)
	for c, x := range hide {
		if !x {
			continue
		}
		i, j := c/w, c%w
		adj[i] = append(adj[i], [2]int{h + j, c})
		adj[h+j] = append(adj[h+j], [2]int{i, c})
	}
	disc := make([]int, n)
	low := make([]int, n)
	for i := range disc {
		disc[i] = -1
	}
	t := 0
	type frame struct{ v, viaCell, next int }
	for s := 0; s < n; s++ {
		if disc[s] >= 0 || len(adj[s]) == 0 {
			continue
		}
		stack := []frame{{v: s, viaCell: -1}}
		disc[s], low[s] = t, t
		t++
		for len(stack) > 0 {
			f := &stack[len(stack)-1]
			if f.next < len(adj[f.v]) {
				e := adj[f.v][f.next]
				f.next++
				u, c := e[0], e[1]
				if c == f.viaCell {
					continue
				}
				if disc[u] >= 0 {
					low[f.v] = min(low[f.v], disc[u])
					continue
				}
				disc[u], low[u] = t, t
				t++
				stack = append(stack, frame{v: u, viaCell: c})
				continue
			}
			done := *f
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				continue
			}
			p := &stack[len(stack)-1]
			low[p.v] = min(low[p.v], low[done.v])
			if low[done.v] > disc[p.v] {
				return done.viaCell, true
			}
		}
	}
	return 0, false
}

// breakBridge is the cell to hide so that bridge cell b lies on a cycle:
// with b's edge removed, its row's side and its column's side of the
// graph are apart, and a cell joining a row of the column's side to a
// column of the row's side closes a cycle through b. With none shown, the
// best shown cell of b's row or column (the loop goes on from there).
func (g *grid) breakBridge(b, h, w int) int {
	bi, bj := b/w, b%w
	n := h + w
	adj := make([][]int, n)
	for c, x := range g.hide {
		if !x || c == b {
			continue
		}
		i, j := c/w, c%w
		adj[i] = append(adj[i], h+j)
		adj[h+j] = append(adj[h+j], i)
	}
	side := func(s int) []bool {
		seen := make([]bool, n)
		seen[s] = true
		q := []int{s}
		for len(q) > 0 {
			v := q[0]
			q = q[1:]
			for _, u := range adj[v] {
				if !seen[u] {
					seen[u] = true
					q = append(q, u)
				}
			}
		}
		return seen
	}
	rowSide, colSide := side(bi), side(h+bj)
	var closing []int
	for i := 0; i < h; i++ {
		if !colSide[i] {
			continue
		}
		for j := 0; j < w; j++ {
			if rowSide[h+j] {
				closing = append(closing, i*w+j)
			}
		}
	}
	if c := g.pick(closing); c >= 0 {
		return c
	}
	var near []int
	for j := 0; j < w; j++ {
		near = append(near, bi*w+j)
	}
	for i := 0; i < h; i++ {
		near = append(near, i*w+bj)
	}
	if c := g.pick(near); c >= 0 {
		return c
	}
	// Every cell of b's row and column is hidden: hide the best shown cell
	// anywhere (the table entirely hidden has no bridge).
	all := make([]int, len(g.hide))
	for i := range all {
		all[i] = i
	}
	return g.pick(all)
}

// Within is rules 1 and 2 over a set that adds up (total = Σ parts) whose
// every member is a subset of another shown or hidden count: within[0]
// holds the total, within[i+1] part i (degraded within answers). A member
// is hidden first when its container is hidden, or when container −
// member is 1 to 4 (the two shown would give a small group away); then
// rule 2 runs on the set.
func Within(total int64, parts []int64, within []int64, withinG []Guarded) []Guarded {
	vals := append([]int64{total}, parts...)
	g := newGrid(vals)
	if len(within) != len(vals) || len(withinG) != len(vals) {
		for i := range g.hide {
			g.hide[i] = true
		}
		return g.out()
	}
	for i, v := range vals {
		if !withinG[i].Shown() || v > within[i] || small(within[i]-v) {
			g.hide[i] = true
		}
	}
	l := line{total: 0}
	for i := range parts {
		l.parts = append(l.parts, i+1)
	}
	g.enforce(l)
	return g.out()
}

// TableOut is a guarded table: the all row, each row, and the subset
// column (all first, then each row's).
type TableOut struct {
	All    []Guarded
	Rows   [][]Guarded
	Subset []Guarded
}

// Table guards a table of rows [total, part1, …], each width wide, with
// the all row their column sums (AcrossRows), and a subset count per row
// (sub[i] is within rows[i][0], as degraded is within answers), whose
// column is a set that adds up of its own (Within). The route and the
// property test both run this. The width is the caller's, never taken from
// the rows, so a table with no rows still has a full all row; a row of
// another width, or a sub of another length, hides everything (AcrossRows
// and Within fail closed). The result always has an all row of width
// cells, one row per row, and len(rows)+1 subset cells.
func Table(width int, rows [][]int64, sub []int64) TableOut {
	if width < 2 {
		width = 2
	}
	all := make([]int64, width)
	for _, r := range rows {
		for j, v := range r {
			if j < len(all) {
				all[j] += v
			}
		}
	}
	ga, gr := AcrossRows(all, rows)
	var subAll int64
	for _, v := range sub {
		subAll += v
	}
	within := []int64{all[0]}
	withinG := []Guarded{ga[0]}
	for i := range sub {
		if i < len(rows) && len(rows[i]) > 0 && len(gr[i]) > 0 {
			within = append(within, rows[i][0])
			withinG = append(withinG, gr[i][0])
		}
	}
	subG := Within(subAll, sub, within, withinG)
	if len(sub) != len(rows) {
		subG = make([]Guarded, len(rows)+1)
		for i := range subG {
			subG[i] = hidden
		}
	}
	return TableOut{All: ga, Rows: gr, Subset: subG}
}
