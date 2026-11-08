package disclose

import (
	"sort"
	"strings"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// The final check: what this context publishes together with every known
// body, as linear equations over the groups (one unknown per group).
// Known: every shown group, every set of every body, and, where a body and
// all its child periods are known with the same filters, every additive
// group of the parent as the sum of the children's (D2, D3). The check
// fails when a hidden group is determined now and wasn't before, or a set's
// hidden members break the whole-set rule (R41a-5).

// sheetState is a laid-out body and which of its cells are hidden (a
// member never sent counts as hidden).
type sheetState struct {
	sh    *sheet
	hide  []bool
	whole bool
}

func stateOf(b *Body) *sheetState {
	sh, cs, _, _ := layout(&b.Report)
	h := make([]bool, len(cs))
	for i, c := range cs {
		h[i] = c != nil && !c.Shown()
	}
	hideVirtual(sh, h, func(i int) bool { return cs[i] == nil })
	return &sheetState{sh: sh, hide: h, whole: b.Whole}
}

// hideVirtual: a member never sent is known exactly when the rest of the
// one set it is alone in is shown; it counts as hidden otherwise.
func hideVirtual(sh *sheet, h []bool, virtual func(int) bool) {
	for _, r := range sh.rels {
		v, n, any := -1, 0, h[r.total]
		for _, p := range r.parts {
			if virtual(p) {
				v, n = p, n+1
			} else {
				any = any || h[p]
			}
		}
		if n == 1 && any {
			h[v] = true
		}
	}
}

type system struct {
	vars map[string]int
}

func (s *system) v(g string) int {
	if i, ok := s.vars[g]; ok {
		return i
	}
	s.vars[g] = len(s.vars)
	return s.vars[g]
}

// eqn is an equation total − Σ parts = 0 over the groups (zero groups are
// constants and left out).
type eqn struct {
	vars    []int
	coefs   []int64
	hidden  [][]int // the parts hidden (or never sent): the whole-set rule's members
}

func (s *system) eq(total string, parts []string, hidden []bool) eqn {
	var e eqn
	if total != zeroGroup {
		e.vars, e.coefs = append(e.vars, s.v(total)), append(e.coefs, 1)
	}
	for i, p := range parts {
		if p == zeroGroup {
			continue
		}
		e.vars, e.coefs = append(e.vars, s.v(p)), append(e.coefs, -1)
		if hidden[i] {
			e.hidden = append(e.hidden, []int{s.v(p)})
		}
	}
	return e
}

// learn adds a body's shown groups and sets to k, and returns its sets.
func (s *system) learn(k *smallgroup.Knowledge, st *sheetState) []eqn {
	for i, c := range st.sh.cells {
		if !st.hide[i] && c.group != zeroGroup {
			k.Add([]int{s.v(c.group)}, nil)
		}
	}
	var out []eqn
	for _, r := range st.sh.rels {
		parts := make([]string, len(r.parts))
		hid := make([]bool, len(r.parts))
		for i, p := range r.parts {
			parts[i], hid[i] = st.sh.cells[p].group, st.hide[p]
		}
		e := s.eq(st.sh.cells[r.total].group, parts, hid)
		k.Add(e.vars, e.coefs)
		out = append(out, e)
	}
	return out
}

// descKey is a desc without its period.
func descKey(d desc) string {
	return d.m + "|" + strings.Join(d.attrs[:], "|") + "|" + strings.Join(d.conds, ",") + "|" + d.not + "|" + d.d + "|" + d.x
}

func sameFilters(a, b *sheetState) bool { return a.sh.ctx.Filters == b.sh.ctx.Filters }

// crossSets are the additive equations parent = Σ children for every body
// in all whose child periods are all in all, with the same filters.
// involving, if ≥ 0, keeps only those that involve body all[involving].
func (s *system) crossSets(all []*sheetState, involving int) []eqn {
	var out []eqn
	for pi, p := range all {
		kids := calendar.Children(p.sh.ctx.Period)
		if len(kids) == 0 {
			continue
		}
		cs := make([]*sheetState, len(kids))
		in := pi == involving
		for ki, kp := range kids {
			for oi, o := range all {
				if sameFilters(o, p) && o.sh.ctx.Period.Equal(kp) {
					cs[ki] = o
					in = in || oi == involving
				}
			}
		}
		complete := true
		for _, c := range cs {
			complete = complete && c != nil
		}
		if !complete || (involving >= 0 && !in) {
			continue
		}
		descs := map[string]desc{}
		for _, st := range append([]*sheetState{p}, cs...) {
			for _, c := range st.sh.cells {
				if additive(c.d.m) && c.group != zeroGroup {
					descs[descKey(c.d)] = c.d
				}
			}
		}
		keys := make([]string, 0, len(descs))
		for k := range descs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		// in reports whether st has the group (and if it is hidden there); a
		// group a body doesn't list is 0, unless the body is hidden whole.
		lookup := func(st *sheetState, g string) (present, hidden bool) {
			for i, c := range st.sh.cells {
				if c.group == g {
					return true, st.hide[i]
				}
			}
			return st.whole, true
		}
		for _, k := range keys {
			d := descs[k]
			pg := group(d, p.sh.ctx.Period, p.sh.ctx.First)
			if ok, _ := lookup(p, pg); !ok {
				pg = zeroGroup
			}
			var parts []string
			var hid []bool
			for ki, c := range cs {
				first := p.sh.ctx.First && kids[ki].From.Equal(p.sh.ctx.Period.From)
				g := group(d, kids[ki], first)
				ok, h := lookup(c, g)
				if !ok {
					g = zeroGroup
				}
				parts, hid = append(parts, g), append(hid, h)
			}
			out = append(out, s.eq(pg, parts, hid))
		}
	}
	return out
}

// check returns a cell of the current context to hide, or −1 when what it
// publishes with the known bodies passes.
func (g *guarding) check(hide []bool, known []*sheetState) int {
	ch := make([]bool, len(hide))
	for i, c := range g.sh.cells {
		ch[i] = hide[i] && !c.virtual
	}
	hideVirtual(g.sh, ch, func(i int) bool { return g.sh.cells[i].virtual })
	cur := &sheetState{sh: g.sh, hide: ch}
	s := &system{vars: map[string]int{}}
	before := smallgroup.NewKnowledge()
	var sets []eqn
	for _, k := range known {
		sets = append(sets, s.learn(before, k)...)
	}
	for _, e := range s.crossSets(known, -1) {
		before.Add(e.vars, e.coefs)
		sets = append(sets, e)
	}
	after := before.Clone()
	sets = append(sets, s.learn(after, cur)...)
	all := append(append([]*sheetState{}, known...), cur)
	for _, e := range s.crossSets(all, len(all)-1) {
		after.Add(e.vars, e.coefs)
		sets = append(sets, e)
	}
	bad := map[int]bool{}
	for _, st := range all {
		for i, c := range st.sh.cells {
			if !st.hide[i] || c.virtual || c.group == zeroGroup {
				continue
			}
			v := s.v(c.group)
			if after.Exposed([]int{v}, nil) && !before.Exposed([]int{v}, nil) {
				bad[v] = true
			}
		}
	}
	for _, e := range sets {
		if len(e.hidden) >= 2 && smallgroup.SetChanged(before, after, e.hidden) {
			for _, m := range e.hidden {
				bad[m[0]] = true
			}
		}
	}
	if len(bad) == 0 {
		return -1
	}
	// The shown cell with the smallest non-zero value in a set with a
	// failing group; else the smallest shown cell in such a set; else any.
	near := map[int]bool{}
	for _, e := range sets {
		hit := false
		for _, v := range e.vars {
			hit = hit || bad[v]
		}
		if hit {
			for _, v := range e.vars {
				near[v] = true
			}
		}
	}
	best, tier := -1, 3
	for i, c := range g.sh.cells {
		if hide[i] || c.virtual || c.group == zeroGroup || g.pinned[c.group] {
			continue
		}
		t := 2
		if near[s.v(c.group)] {
			t = 1
			if g.vals[i] != 0 {
				t = 0
			}
		}
		if t < tier || (t == tier && g.vals[i] < g.vals[best]) {
			best, tier = i, t
		}
	}
	if best < 0 {
		return g.sh.pat
	}
	return best
}
