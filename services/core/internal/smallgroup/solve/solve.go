// Package solve is the exact reference solver of the small-group tests
// (Plan 4 C20, Plan 7c spec §6.3 and §6.4): exact rational elimination
// (math/big) over sparse linear equations. A box (a linear combination of
// the unknowns) is exposed when it is in the row space of the equations:
// what is known determines its value exactly.
//
// It is the tests' solver, independent of the guard's modular one in
// smallgroup/joint.go, so it shares no code with it. The equations split
// into connected components first (two components share no unknown), which
// is exact: a box is in the row space exactly when each component's part of
// it is in that component's row space.
package solve

import (
	"math/big"
	"sort"
)

// Term is coefficient × unknown.
type Term struct {
	Var  int
	Coef int64
}

// Exposed is ExposedLinear for 0/1 vectors: each equation and box lists its
// unknowns, each with coefficient 1 (an unknown listed twice counts twice).
func Exposed(nVars int, eqs [][]int, boxes [][]int) []bool {
	return ExposedLinear(nVars, ones(eqs), ones(boxes))
}

func ones(vs [][]int) [][]Term {
	out := make([][]Term, len(vs))
	for i, v := range vs {
		t := make([]Term, len(v))
		for j, x := range v {
			t[j] = Term{x, 1}
		}
		out[i] = t
	}
	return out
}

// entry is one non-zero coefficient of a sparse row.
type entry struct {
	col int
	v   *big.Rat
}

type vec []entry

// norm sorts terms by unknown, adds repeats and drops zeros. Unknowns out
// of [0, nVars) are dropped.
func norm(ts []Term, nVars int) vec {
	m := map[int]*big.Rat{}
	for _, t := range ts {
		if t.Var < 0 || t.Var >= nVars || t.Coef == 0 {
			continue
		}
		if m[t.Var] == nil {
			m[t.Var] = new(big.Rat)
		}
		m[t.Var].Add(m[t.Var], new(big.Rat).SetInt64(t.Coef))
	}
	out := make(vec, 0, len(m))
	for c, v := range m {
		if v.Sign() != 0 {
			out = append(out, entry{c, v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].col < out[j].col })
	return out
}

// sub is a − f·b, both sorted.
func sub(a, b vec, f *big.Rat) vec {
	out := make(vec, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case j >= len(b) || (i < len(a) && a[i].col < b[j].col):
			out = append(out, a[i])
			i++
		case i >= len(a) || b[j].col < a[i].col:
			out = append(out, entry{b[j].col, new(big.Rat).Neg(new(big.Rat).Mul(f, b[j].v))})
			j++
		default:
			v := new(big.Rat).Sub(a[i].v, new(big.Rat).Mul(f, b[j].v))
			if v.Sign() != 0 {
				out = append(out, entry{a[i].col, v})
			}
			i++
			j++
		}
	}
	return out
}

// basis is a row echelon form: each pivot row has its smallest unknown as
// pivot, with coefficient 1.
type basis map[int]vec

func (b basis) reduce(v vec) vec {
	for len(v) > 0 {
		p, ok := b[v[0].col]
		if !ok {
			return v
		}
		v = sub(v, p, v[0].v)
	}
	return v
}

func (b basis) add(v vec) {
	v = b.reduce(v)
	if len(v) == 0 {
		return
	}
	inv := new(big.Rat).Inv(v[0].v)
	for i := range v {
		v[i].v = new(big.Rat).Mul(v[i].v, inv)
	}
	b[v[0].col] = v
}

// ExposedLinear reports, for each box, whether it is in the row space of
// eqs over the rationals.
func ExposedLinear(nVars int, eqs [][]Term, boxes [][]Term) []bool {
	// Connected components of the unknowns, joined by the equations.
	parent := make([]int, nVars)
	for i := range parent {
		parent[i] = i
	}
	find := func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	es := make([]vec, len(eqs))
	for i, e := range eqs {
		es[i] = norm(e, nVars)
		for _, t := range es[i][min(1, len(es[i])):] {
			a, b := find(es[i][0].col), find(t.col)
			if a != b {
				parent[a] = b
			}
		}
	}
	// One basis per component, shortest equations first (less fill).
	order := make([]int, len(es))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return len(es[order[a]]) < len(es[order[b]]) })
	bases := map[int]basis{}
	for _, i := range order {
		e := es[i]
		if len(e) == 0 {
			continue
		}
		r := find(e[0].col)
		if bases[r] == nil {
			bases[r] = basis{}
		}
		bases[r].add(e)
	}
	out := make([]bool, len(boxes))
	for k, bx := range boxes {
		v := norm(bx, nVars)
		parts := map[int]vec{}
		var roots []int
		for _, t := range v {
			r := find(t.col)
			if _, ok := parts[r]; !ok {
				roots = append(roots, r)
			}
			parts[r] = append(parts[r], t)
		}
		exposed := true
		for _, r := range roots {
			b := bases[r]
			if b == nil || len(b.reduce(parts[r])) != 0 {
				exposed = false
				break
			}
		}
		out[k] = exposed
	}
	return out
}
