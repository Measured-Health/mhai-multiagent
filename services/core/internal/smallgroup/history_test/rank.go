package historytest

// The set check (ruling R41a-5), the attacker's side, in exact rationals
// and code of this package's own (no smallgroup code).
//
// For a published set with hidden members H (each a box over the base
// counts; a never-published part such as the unanswered questions counts
// as hidden), the combinations of H that the published JSON determines are
// D = rowspace(known) ∩ span(H). With r_h the residual of member h against
// the known rows (its unique part on the non-pivot unknowns), a
// combination Σ c_h·v_h is determined exactly when Σ c_h·r_h = 0, so D is
// the null space of the residuals: dim D = |H| − rank{r_h}, and the full
// sum is determined when Σ r_h = 0.
//
// The rule: since just before the set was published, nothing about H may
// have become determined except its full sum: D_final ⊆ D_pre + span(sum).
// (What was determined before, such as a hidden month whose weeks are all
// shown, is a combination of shown values and tells nothing new; spec §6.3,
// E0.)

import (
	"fmt"
	"math/big"
	"sort"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

func calendarChildren(p calendar.Period) []calendar.Period { return calendar.Children(p) }

type rterm struct {
	col int
	v   *big.Rat
}

type rvec []rterm

func ratVec(vars []int) rvec {
	m := map[int]int64{}
	for _, v := range vars {
		m[v]++
	}
	out := make(rvec, 0, len(m))
	for c, n := range m {
		out = append(out, rterm{c, new(big.Rat).SetInt64(n)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].col < out[j].col })
	return out
}

// axpy is a − f·b, both sorted by column.
func axpy(a, b rvec, f *big.Rat) rvec {
	out := make(rvec, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case j >= len(b) || (i < len(a) && a[i].col < b[j].col):
			out = append(out, a[i])
			i++
		case i >= len(a) || b[j].col < a[i].col:
			out = append(out, rterm{b[j].col, new(big.Rat).Neg(new(big.Rat).Mul(f, b[j].v))})
			j++
		default:
			v := new(big.Rat).Sub(a[i].v, new(big.Rat).Mul(f, b[j].v))
			if v.Sign() != 0 {
				out = append(out, rterm{a[i].col, v})
			}
			i++
			j++
		}
	}
	return out
}

func addVec(a, b rvec) rvec { return axpy(a, b, big.NewRat(-1, 1)) }

// rbasis is an echelon form: each row's pivot is its smallest column, with
// coefficient 1. Rows never change once added (a clone shares them).
type rbasis map[int]rvec

func (b rbasis) clone() rbasis {
	c := make(rbasis, len(b))
	for k, v := range b {
		c[k] = v
	}
	return c
}

// residual removes every pivot column from v: the unique part of v on the
// non-pivot columns, linear in v.
func (b rbasis) residual(v rvec) rvec {
	for i := 0; i < len(v); {
		row, ok := b[v[i].col]
		if !ok {
			i++
			continue
		}
		v = axpy(v, row, v[i].v)
	}
	return v
}

func (b rbasis) add(v rvec) bool {
	v = b.residual(v)
	if len(v) == 0 {
		return false
	}
	inv := new(big.Rat).Inv(v[0].v)
	w := make(rvec, len(v))
	for i, t := range v {
		w[i] = rterm{t.col, new(big.Rat).Mul(t.v, inv)}
	}
	b[w[0].col] = w
	return true
}

// shape is what the known rows determine about members H: dim D and
// whether the full sum is determined.
func (b rbasis) shape(members [][]int) (dim int, sum bool) {
	small := rbasis{}
	var total rvec
	rank := 0
	for _, m := range members {
		r := b.residual(ratVec(m))
		total = addVec(total, r)
		if small.add(r) {
			rank++
		}
	}
	return len(members) - rank, len(total) == 0
}

// violates is the rule: D_final ⊆ D_pre + span(sum).
func violates(dimPre int, sumPre bool, dimFin int, sumFin bool) bool {
	switch {
	case dimFin == dimPre:
		return false
	case dimFin == dimPre+1:
		return !(sumFin && !sumPre)
	}
	return true
}

// pubSet is one published set's hidden members and where it comes from.
type pubSet struct {
	members [][]int
	desc    string
	dimPre  int
	sumPre  bool
}

// CheckSets replays the documents in publication order: before each one's
// knowledge is added, the shape of each of its sets is taken; at the end,
// every set is checked against the rule. It returns the violations.
func (p *Parser) CheckSets() []string {
	known := rbasis{}
	zeroAll := func(vs []int) {
		for _, v := range vs {
			known.add(rvec{{v, big.NewRat(1, 1)}})
		}
	}
	for id, h := range p.hosp {
		for w, wk := range p.weeks {
			if !wk.To.After(p.onboard[id]) {
				for f, n := range []int{qgCats, rCats} {
					for c := 0; c < n; c++ {
						zeroAll([]int{p.Var(f, h, w, c)})
					}
				}
			}
		}
	}
	var sets []pubSet
	for di := range p.docs {
		for _, st := range p.docSets(di) {
			if len(st.members) < 2 {
				continue // one hidden member: the cell check covers it
			}
			st.dimPre, st.sumPre = known.shape(st.members)
			sets = append(sets, st)
		}
		eqs, zeros := p.docKnowledge(di)
		zeroAll(zeros)
		for _, e := range eqs {
			known.add(ratVec(e))
		}
	}
	var out []string
	for _, st := range sets {
		dim, sum := known.shape(st.members)
		if violates(st.dimPre, st.sumPre, dim, sum) {
			out = append(out, fmt.Sprintf("%s: %d hidden members, determined dimension %d → %d, full sum %v → %v",
				st.desc, len(st.members), st.dimPre, dim, st.sumPre, sum))
		}
	}
	return out
}

// docKnowledge is what document di adds: its shown cells (a shown 0 as
// zeros) and its unlisted hospitals as zeros.
func (p *Parser) docKnowledge(di int) (eqs [][]int, zeros []int) {
	d := p.docs[di]
	slots, _ := p.slots(d.period)
	for _, c := range d.all {
		switch {
		case !c.shown:
		case c.value == 0:
			zeros = append(zeros, c.vars...)
		default:
			eqs = append(eqs, c.vars)
		}
	}
	listed := map[string]bool{}
	for _, id := range d.listed {
		listed[id] = true
	}
	for id, h := range p.hosp {
		if listed[id] {
			continue
		}
		for _, w := range slots {
			for f, n := range []int{qgCats, rCats} {
				for c := 0; c < n; c++ {
					zeros = append(zeros, p.Var(f, h, w, c))
				}
			}
		}
	}
	return eqs, zeros
}

// docSets are document di's sets (rows, columns, parent sets), each as its
// hidden members.
func (p *Parser) docSets(di int) []pubSet {
	d := p.docs[di]
	slots, _ := p.slots(d.period)
	var out []pubSet
	hiddenOf := func(cells []cell) [][]int {
		var hs [][]int
		for _, c := range cells {
			if !c.shown {
				hs = append(hs, c.vars)
			}
		}
		return hs
	}
	rowIDs := make([]string, 0, len(d.cells))
	for id := range d.cells {
		rowIDs = append(rowIDs, id)
	}
	sort.Strings(rowIDs)
	for _, rowID := range rowIDs {
		cells := d.cells[rowID]
		hosps := []int{}
		if rowID == "all" {
			for _, id := range d.listed {
				hosps = append(hosps, p.hosp[id])
			}
		} else {
			hosps = append(hosps, p.hosp[rowID])
		}
		for _, rs := range rowSets {
			if _, ok := cells[rs.total]; !ok {
				continue
			}
			var mem []cell
			ok := true
			for _, part := range rs.parts {
				i, has := cells[part]
				if !has {
					ok = false
					break
				}
				mem = append(mem, d.all[i])
			}
			if !ok {
				continue
			}
			for _, vc := range rs.virtual {
				mem = append(mem, cell{vars: p.box(measures[rs.total], hosps, slots, vc)})
			}
			out = append(out, pubSet{members: hiddenOf(mem), desc: fmt.Sprintf("%s %v row %s set %s", d.source, d.period, rowID, rs.total)})
		}
	}
	names := make([]string, 0, len(measures))
	for name := range measures {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := d.cells["all"][name]; !ok {
			continue
		}
		var mem []cell
		for _, id := range d.listed {
			if i, ok := d.cells[id][name]; ok {
				mem = append(mem, d.all[i])
			}
		}
		out = append(out, pubSet{members: hiddenOf(mem), desc: fmt.Sprintf("%s %v column %s", d.source, d.period, name)})
	}
	var kids []doc
	for _, c := range calendarChildren(d.period) {
		for _, o := range p.docs[:di] {
			if o.source == d.source && o.period.Equal(c) {
				kids = append(kids, o)
			}
		}
	}
	if len(kids) == 0 || len(kids) != len(calendarChildren(d.period)) {
		return out
	}
	for _, rowID := range rowIDs {
		for _, name := range names {
			if _, ok := d.cells[rowID][name]; !ok {
				continue
			}
			var mem []cell
			for _, k := range kids {
				if i, ok := k.cells[rowID][name]; ok {
					mem = append(mem, k.all[i])
				}
			}
			out = append(out, pubSet{members: hiddenOf(mem), desc: fmt.Sprintf("%s %v parent set %s %s", d.source, d.period, rowID, name)})
		}
	}
	return out
}

// setStats counts the published sets with 2 or more, and 11 or more,
// hidden members.
func (p *Parser) setStats() (sets, wide int) {
	for di := range p.docs {
		for _, st := range p.docSets(di) {
			if len(st.members) >= 2 {
				sets++
			}
			if len(st.members) >= 11 {
				wide++
			}
		}
	}
	return sets, wide
}
