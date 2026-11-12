package historytest

// The set check (ruling R41a-5), the attacker's side, in exact rationals
// with none of the guard's code (the exact rank is smallgroup/solve).
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
// The rule: since just before the set was published, D either stayed as it
// was, or went from nothing to the full sum alone. (Once part of H is
// known, a newly determined full sum minus the known part is a union of the
// rest, which may be small: the first build of this check let a 2+2 through
// that way.) The exact rank code is solve.Basis, shared with Analysis.
// (What was determined before, such as a hidden month whose weeks are all
// shown, is a combination of shown values and tells nothing new; spec §6.3,
// E0.)

import (
	"fmt"
	"sort"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup/solve"
)

func calendarChildren(p calendar.Period) []calendar.Period { return calendar.Children(p) }

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
	known := solve.NewBasis()
	zeroAll := func(vs []int) {
		for _, v := range vs {
			known.Add([]int{v})
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
			st.dimPre, st.sumPre = known.Shape(st.members)
			sets = append(sets, st)
		}
		eqs, zeros := p.docKnowledge(di)
		zeroAll(zeros)
		for _, e := range eqs {
			known.Add(e)
		}
	}
	var out []string
	for _, st := range sets {
		dim, sum := known.Shape(st.members)
		if solve.Violates(st.dimPre, st.sumPre, dim, sum) {
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
