package attack

import (
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup/solve"
)

// DrillExposed is P2's second run (design §5.2, §5.7): an attacker who may
// drill gets every shown count of distinct patients of 5 or more in docs as
// rows (age band, sex, setting, and the conditions and findings in the
// period; no location, specialty or source). A hidden count of 1 to 4 in
// docs is exposed when it is such a group's patients picked out by what
// the rows show. It returns those exposed, and all hidden counts of 1 to 4.
func (m *Model) DrillExposed(docs []int) (exposed, small int) {
	in := map[int]bool{}
	for _, d := range docs {
		in[d] = true
	}
	type vis struct {
		age, sex, setting string
		S, F              uint32
	}
	proj := func(id int) vis {
		a := m.atoms[id]
		t := m.tuples[a.a]
		return vis{t[dAge], t[dSex], t[dSetting], a.S, a.F}
	}
	var drill [][]term
	for _, c := range m.Cells {
		if in[c.Doc] && c.Dist && c.Shown && c.Value >= 5 {
			drill = append(drill, c.box)
		}
	}
	for _, c := range m.Cells {
		if !in[c.Doc] || !c.Dist || c.Shown || c.Truth < 1 || c.Truth > 4 {
			continue
		}
		small++
		h := map[int]bool{}
		v := map[vis]bool{}
		for _, t := range c.box {
			h[t.atom] = true
			v[proj(t.atom)] = true
		}
		for _, d := range drill {
			inD := map[int]bool{}
			for _, t := range d {
				inD[t.atom] = true
			}
			ok := true
			for a := range h {
				ok = ok && inD[a]
			}
			for a := range inD {
				if v[proj(a)] != h[a] {
					ok = false
				}
			}
			if ok {
				exposed++
				break
			}
		}
	}
	return exposed, small
}

// Row is a linear form over a system's variables: Σ coef·x = Rhs (an
// equation) or ≥ Rhs (an inequality).
type Row struct {
	Terms []solve.Term
	Rhs   float64
}

// LinSys is what the attacker knows of docs, for bounds (P5): the
// variables are the atoms not known to be 0 (merged), all ≥ 0; Eq is
// independent; Ge are the distinct-count bounds between a period and its
// blocks; Targets are the hidden cells of 1 or more, in Cells order (their
// indexes in Hidden).
type LinSys struct {
	N       int
	Eq, Ge  []Row
	Targets []Row
	Hidden  []int
}

// Bounds is LinSys for docs. With bounds set, every distinct count a doc
// of a month or quarter shares (by name) with docs of its blocks adds the
// bounds largest block ≤ period ≤ Σ blocks (R-A2).
func (m *Model) Bounds(docs []int, bounds bool) LinSys {
	z := m.zeros(docs, nil)
	in := map[int]bool{}
	for _, d := range docs {
		in[d] = true
	}
	var boxes [][]term
	var eqs []int
	var hid []int
	for i, c := range m.Cells {
		if !in[c.Doc] {
			continue
		}
		boxes = append(boxes, c.box)
		if c.Shown {
			eqs = append(eqs, i)
		} else if c.Truth >= 1 {
			hid = append(hid, i)
		}
	}
	boxes = append(boxes, m.start...)
	u := merge(z, nil, boxes)
	sys := LinSys{N: u.n, Hidden: hid}
	basis := solve.NewBasis()
	for _, i := range eqs {
		ts := u.terms(m.Cells[i].box)
		if len(ts) > 0 && basis.AddTerms(ts) {
			sys.Eq = append(sys.Eq, Row{ts, float64(m.Cells[i].Value)})
		}
	}
	for _, e := range m.start {
		ts := u.terms(e)
		if len(ts) > 0 && basis.AddTerms(ts) {
			sys.Eq = append(sys.Eq, Row{Terms: ts})
		}
	}
	for _, i := range hid {
		sys.Targets = append(sys.Targets, Row{Terms: u.terms(m.Cells[i].box)})
	}
	if !bounds {
		return sys
	}
	byName := map[string]map[int]int{} // name → doc → cell
	for i, c := range m.Cells {
		if in[c.Doc] && c.Dist {
			if byName[c.Name] == nil {
				byName[c.Name] = map[int]int{}
			}
			byName[c.Name][c.Doc] = i
		}
	}
	neg := func(ts []solve.Term) []solve.Term {
		out := make([]solve.Term, len(ts))
		for i, t := range ts {
			out[i] = solve.Term{Var: t.Var, Coef: -t.Coef}
		}
		return out
	}
	add := func(a, b []solve.Term) []solve.Term {
		m := map[int]int64{}
		for _, t := range append(append([]solve.Term{}, a...), b...) {
			m[t.Var] += t.Coef
		}
		var out []solve.Term
		for v, c := range m {
			if c != 0 {
				out = append(out, solve.Term{Var: v, Coef: c})
			}
		}
		return out
	}
	for _, cells := range byName {
		for pd, pc := range cells {
			per := m.Docs[pd].Period
			if per.Kind == "week" {
				continue
			}
			var kids []int
			for kd, kc := range cells {
				kp := m.Docs[kd].Period
				if kp.Kind == "week" && !kp.From.Before(per.From) && !kp.To.After(per.To) && sameFilters(m.Docs[kd], m.Docs[pd]) {
					kids = append(kids, kc)
				}
			}
			if len(kids) == 0 {
				continue
			}
			p := u.terms(m.Cells[pc].box)
			var sum []solve.Term
			for _, k := range kids {
				kt := u.terms(m.Cells[k].box)
				sys.Ge = append(sys.Ge, Row{Terms: add(p, neg(kt))})
				sum = add(sum, kt)
			}
			sys.Ge = append(sys.Ge, Row{Terms: add(sum, neg(p))})
		}
	}
	return sys
}

func sameFilters(a, b Doc) bool {
	if len(a.Filters) != len(b.Filters) {
		return false
	}
	for k, v := range a.Filters {
		if b.Filters[k] != v {
			return false
		}
	}
	return true
}
