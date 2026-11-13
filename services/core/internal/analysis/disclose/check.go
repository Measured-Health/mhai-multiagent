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
	vals  []int64 // shown cells' values
	whole bool
	body  Body           // the body itself (a recorded one), for D5's parents
	at    map[string]int // a cell of each group (built on first use)
}

// cell is a cell of group g in st, or −1.
func (st *sheetState) cell(g string) int {
	if st.at == nil {
		st.at = make(map[string]int, len(st.sh.cells))
		for i, c := range st.sh.cells {
			if _, ok := st.at[c.group]; !ok {
				st.at[c.group] = i
			}
		}
	}
	if i, ok := st.at[g]; ok {
		return i
	}
	return -1
}

func stateOf(b *Body) *sheetState {
	sh, cs, _, _ := layout(&b.Report)
	h := make([]bool, len(cs))
	vals := make([]int64, len(cs))
	for i, c := range cs {
		h[i] = c != nil && !c.Shown()
		if c != nil && c.Shown() {
			vals[i] = *c.Value
		}
	}
	hideVirtual(sh, h, func(i int) bool { return cs[i] == nil })
	return &sheetState{sh: sh, hide: h, vals: vals, whole: b.Whole, body: *b}
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

// system numbers the groups: shown is those shown in some body (their
// values known), hidden those a body publishes hidden.
type system struct {
	vars          map[string]int
	shown, hidden map[int]bool
}

func newSystem() *system {
	return &system{vars: map[string]int{}, shown: map[int]bool{}, hidden: map[int]bool{}}
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
	vars   []int
	coefs  []int64
	hidden [][]int // the parts hidden (or never sent): the whole-set rule's members
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

// learn adds a body's shown groups (with their values) and sets to k,
// and returns its sets.
func (s *system) learn(k *smallgroup.Knowledge, st *sheetState) []eqn {
	for i, c := range st.sh.cells {
		// A member never sent is known through its equation, never by a
		// value of its own (a stored body doesn't carry one).
		if !st.hide[i] && c.group != zeroGroup && !c.virtual {
			v := s.v(c.group)
			k.AddValued([]int{v}, nil, st.vals[i])
			s.shown[v] = true
		} else if st.hide[i] && !c.virtual && c.group != zeroGroup {
			s.hidden[s.v(c.group)] = true
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
		k.AddValued(e.vars, e.coefs, 0)
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
			if i := st.cell(g); i >= 0 {
				return true, st.hide[i]
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

// zeroParts adds what non-negative counts give: a set whose total is
// known to be 0 has every part 0 (a month showing 0 approved corrections
// says every block had none). Run to a fixed point.
func zeroParts(k *smallgroup.Knowledge, sets []eqn) {
	for changed := true; changed; {
		changed = false
		for _, e := range sets {
			zero := true
			if len(e.vars) > 0 && e.coefs[0] == 1 {
				x, ok := k.Value(e.vars[:1], nil)
				zero = ok && x == 0
			}
			if !zero {
				continue
			}
			for i, v := range e.vars {
				if e.coefs[i] != -1 {
					continue
				}
				if x, ok := k.Value([]int{v}, nil); ok && x == 0 {
					continue
				}
				k.AddValued([]int{v}, nil, 0)
				changed = true
			}
		}
	}
}

// prior is what the known bodies publish, built once per GuardResponse:
// the groups, what they determine, and the shape of each of their sets.
type prior struct {
	known  []*sheetState
	sys    *system
	before *smallgroup.Knowledge
	sets   []eqn
	shapes []smallgroup.SetShape
}

func newPrior(known []*sheetState) *prior {
	p := &prior{known: known, sys: newSystem(), before: smallgroup.NewKnowledge()}
	for _, k := range known {
		p.sets = append(p.sets, p.sys.learn(p.before, k)...)
	}
	for _, e := range append(append(p.sys.crossSets(known, -1), p.sys.partitionSets(known, -1)...), p.sys.settingSets(known, -1)...) {
		p.before.AddValued(e.vars, e.coefs, 0)
		p.sets = append(p.sets, e)
	}
	zeroParts(p.before, p.sets)
	for _, e := range p.sets {
		p.shapes = append(p.shapes, p.before.Shape(e.hidden))
	}
	return p
}

// partDims are the attribute dimensions: each patient has one value of
// each, so a context is the sum of its sub-contexts over any of them.
var partDims = []int{DimAgeBand, DimSex, DimLocation, DimSetting, DimSpecialty, DimSource}

func ctxKey(st *sheetState) string {
	return st.sh.ctx.Period.String() + "|" + strings.Join(st.sh.ctx.Filters[:], "|")
}

// absentIsZero: a group a body doesn't list is 0 there only when the row
// that would carry it is listed whenever it isn't 0, and the body isn't
// hidden whole. A pair's "A without B" and a setting's unlocated patients
// aren't such rows (an unlisted pair says only that "both" is 0), nor is a
// condition's patients beyond a week (only pairs carry it), nor a pair
// beyond a full list.
func absentIsZero(st *sheetState, d desc) bool {
	if st.whole || d.not != "" || d.x == "~located" {
		return false
	}
	if d.m != mPat {
		return true
	}
	extra := 0
	for _, c := range d.conds {
		if c != st.sh.ctx.Filters[DimCondition] {
			extra++
		}
	}
	switch {
	case extra == 0:
		return true
	case extra == 1:
		return st.sh.ctx.Period.Kind == calendar.Week // by_condition lists it
	default:
		return !st.sh.pairsFull
	}
}

// partitionSets are the equations between a context C and its published
// sub-contexts C + {d = v} over an attribute dimension d (ruling C10): for
// every group, C's = Σ over the values C lists of the sub-context's (a value
// C doesn't list is 0; the null value and an unpublished sub-context are
// unknowns). This is how {A} − {A, male} gives {A, female} away, across
// families. involving, if ≥ 0, keeps only those that involve all[involving].
func (s *system) partitionSets(all []*sheetState, involving int) []eqn {
	byKey := map[string]int{}
	for i, st := range all {
		byKey[ctxKey(st)] = i
	}
	var out []eqn
	for ci, c := range all {
		F := c.sh.ctx.Filters
		if c.whole || F.Count() >= FilterCap {
			continue
		}
		for _, d := range partDims {
			// Only a partition the context lists (source has none in the
			// views): no keys is no equation, not a sum of nothing.
			if F[d] != "" || len(c.sh.parts[d]) == 0 {
				continue
			}
			subs := map[string]*sheetState{}
			in := ci == involving
			for _, v := range c.sh.parts[d] {
				G := F
				G[d] = v
				if v == "" || CheckFilters(G) != nil {
					continue
				}
				probe := &sheetState{sh: &sheet{ctx: Context{Period: c.sh.ctx.Period, Filters: G}}}
				if si, ok := byKey[ctxKey(probe)]; ok {
					subs[v] = all[si]
					in = in || si == involving
				}
			}
			// The equation holds whether or not a sub-context is published
			// (the attacker always has it: chains run through unpublished
			// ones).
			if involving >= 0 && !in {
				continue
			}
			type pd struct {
				d     desc
				cell  cellInfo
				valid bool
			}
			descs := map[string]pd{}
			add := func(cell cellInfo, clear bool) {
				if cell.group == zeroGroup {
					return
				}
				dd := cell.d.clone()
				if clear {
					dd.attrs[d] = ""
				} else if dd.attrs[d] != "" {
					return
				}
				descs[descKey(dd)+"@"+cell.period.String()] = pd{dd, cell, true}
			}
			for _, cell := range c.sh.cells {
				add(cell, false)
			}
			for _, st := range subs {
				for _, cell := range st.sh.cells {
					add(cell, true)
				}
			}
			keys := make([]string, 0, len(descs))
			for k := range descs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			lookup := func(st *sheetState, dd desc, g string) (string, bool) {
				if i := st.cell(g); i >= 0 {
					return g, st.hide[i]
				}
				if absentIsZero(st, dd) {
					return zeroGroup, false
				}
				// Not published: an unknown, protected by its value (1 to 4),
				// not a member of the whole-set rule.
				return g, false
			}
			for _, k := range keys {
				x := descs[k]
				total, _ := lookup(c, x.d, group(x.d, x.cell.period, x.cell.first))
				var parts []string
				var hid []bool
				for _, v := range c.sh.parts[d] {
					dv := x.d.with(d, v)
					g := group(dv, x.cell.period, x.cell.first)
					h := false
					if st, ok := subs[v]; ok {
						g, h = lookup(st, dv, g)
					}
					parts, hid = append(parts, g), append(hid, h)
				}
				out = append(out, s.eq(total, parts, hid))
			}
		}
	}
	return out
}

// inconsistent is check's answer when what is known contradicts itself.
const inconsistent = -2

// settingSets are the equations a location's setting makes between
// contexts (ruling C10): in a context C with neither filter, a group at
// setting s is the sum of the same group at each location C lists with
// setting s, plus the patients at no location (0 when C lists no null
// location and no location without a setting). So {male, outpatient} is
// {male} less {male, L1} when L1 is the only admitted location.
func (s *system) settingSets(all []*sheetState, involving int) []eqn {
	byKey := map[string]int{}
	for i, st := range all {
		byKey[ctxKey(st)] = i
	}
	find := func(per calendar.Period, f Filters) (*sheetState, int) {
		probe := &sheetState{sh: &sheet{ctx: Context{Period: per, Filters: f}}}
		if i, ok := byKey[ctxKey(probe)]; ok {
			return all[i], i
		}
		return nil, -1
	}
	var out []eqn
	for ci, c := range all {
		F := c.sh.ctx.Filters
		if c.whole || F.Count() >= FilterCap || F[DimSetting] != "" || F[DimLocation] != "" {
			continue
		}
		restZero := true
		for l, set := range c.sh.locSetting {
			if l == "" || set == "" {
				restZero = false
			}
		}
		for _, set := range c.sh.parts[DimSetting] {
			if set == "" {
				continue
			}
			in := ci == involving
			G := F
			G[DimSetting] = set
			sb, si := find(c.sh.ctx.Period, G)
			in = in || (si >= 0 && si == involving)
			type lb struct {
				l  string
				st *sheetState
			}
			var locs []lb
			for _, l := range c.sh.parts[DimLocation] {
				if l == "" || c.sh.locSetting[l] != set {
					continue
				}
				H := F
				H[DimLocation] = l
				st, i := find(c.sh.ctx.Period, H)
				in = in || (i >= 0 && i == involving)
				locs = append(locs, lb{l, st})
			}
			if len(locs) == 0 || (involving >= 0 && !in) {
				continue
			}
			type pd struct {
				d    desc
				cell cellInfo
			}
			descs := map[string]pd{}
			add := func(cell cellInfo, clear int) {
				if cell.group == zeroGroup || cell.virtual {
					return
				}
				dd := cell.d.clone()
				if clear >= 0 {
					dd.attrs[clear] = ""
				}
				if dd.attrs[DimSetting] != "" || dd.attrs[DimLocation] != "" {
					return
				}
				descs[descKey(dd)+"@"+cell.period.String()] = pd{dd, cell}
			}
			for _, cell := range c.sh.cells {
				add(cell, -1)
			}
			if sb != nil {
				for _, cell := range sb.sh.cells {
					add(cell, DimSetting)
				}
			}
			for _, x := range locs {
				if x.st != nil {
					for _, cell := range x.st.sh.cells {
						add(cell, DimLocation)
					}
				}
			}
			keys := make([]string, 0, len(descs))
			for k := range descs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			lookup := func(st *sheetState, dd desc, g string) (string, bool) {
				if st == nil {
					return g, false
				}
				if i := st.cell(g); i >= 0 {
					return g, st.hide[i]
				}
				if absentIsZero(st, dd) {
					return zeroGroup, false
				}
				return g, false
			}
			for _, k := range keys {
				x := descs[k]
				ds := x.d.with(DimSetting, set)
				total, _ := lookup(sb, ds, group(ds, x.cell.period, x.cell.first))
				var parts []string
				var hid []bool
				for _, l := range locs {
					dl := x.d.with(DimLocation, l.l)
					g, h := lookup(l.st, dl, group(dl, x.cell.period, x.cell.first))
					parts, hid = append(parts, g), append(hid, h)
				}
				if !restZero {
					dr := ds.with(DimLocation, "")
					parts, hid = append(parts, group(dr, x.cell.period, x.cell.first)), append(hid, false)
				}
				out = append(out, s.eq(total, parts, hid))
			}
		}
	}
	return out
}

// check returns a cell of the current context to hide, −1 when what it
// publishes with the known bodies passes, or inconsistent.
func (g *guarding) check(hide []bool, p *prior) int {
	ch := make([]bool, len(hide))
	for i, c := range g.sh.cells {
		ch[i] = hide[i] && !c.virtual
	}
	hideVirtual(g.sh, ch, func(i int) bool { return g.sh.cells[i].virtual })
	cur := &sheetState{sh: g.sh, hide: ch, vals: g.vals}
	s, before := p.sys, p.before
	// The current context's groups are learnt into a copy of the numbering.
	s = &system{vars: copyInts(s.vars), shown: copyBools(s.shown), hidden: copyBools(s.hidden)}
	after := before.Clone()
	sets := append([]eqn{}, p.sets...)
	shapes := append([]smallgroup.SetShape{}, p.shapes...)
	own := s.learn(after, cur)
	all := append(append([]*sheetState{}, p.known...), cur)
	for _, e := range append(append(s.crossSets(all, len(all)-1), s.partitionSets(all, len(all)-1)...), s.settingSets(all, len(all)-1)...) {
		after.AddValued(e.vars, e.coefs, 0)
		own = append(own, e)
	}
	for _, e := range own {
		sets = append(sets, e)
		shapes = append(shapes, before.Shape(e.hidden))
	}
	zeroParts(after, sets)
	if !before.Consistent() || !after.Consistent() {
		return inconsistent // the model is wrong: fail closed
	}
	// Newly determined: a group published hidden (whatever its value), or
	// one never shown (a member never sent, or a sub-context not yet
	// published, ruling C10) at 1 to 4.
	bad := map[int]bool{}
	for _, v := range s.vars {
		if s.shown[v] {
			continue
		}
		x, ok := after.Value([]int{v}, nil)
		if !ok {
			continue
		}
		if _, was := before.Value([]int{v}, nil); was {
			continue
		}
		if s.hidden[v] || (x >= 1 && x < Min) {
			bad[v] = true
		}
	}
	for i, e := range sets {
		if len(e.hidden) >= 2 && shapes[i].Changed(after, e.hidden) {
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
		if hide[i] || c.virtual || c.group == zeroGroup || g.pinned[c.group] || g.public[c.group] {
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

func copyInts(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyBools(m map[int]bool) map[int]bool {
	out := make(map[int]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
