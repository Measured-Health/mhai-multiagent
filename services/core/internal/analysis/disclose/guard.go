package disclose

import (
	"math"
	"sort"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// GuardResponse is Layer A (design §5.3): every number of one context, all
// five views together. forced lists groups to hide whatever the rules say
// (D3 and D5); known are stored bodies whose published numbers are already
// out (the period's children, the request's filter parents): the final
// check runs on them and the context together. It runs until nothing
// changes:
//
//   - A1: smallgroup.Count on every count;
//   - one state per group: a group hidden anywhere (in this context, or in
//     a known body) is hidden everywhere;
//   - A2: by_condition and the series against the total, each a table
//     (smallgroup.TableForced), sharing the total;
//   - A2, A3, A5 and the contract's other sets: smallgroup.SetForced, and a
//     hidden total hides its non-zero members (the contract's reader);
//   - the final check (smallgroup.Knowledge): no hidden group newly
//     determined, and the whole-set rule on every set, over this context
//     and the known bodies together; on a failure one more cell is hidden;
//   - A7: patients hidden, or a filtered context under QuerySetMin: hidden
//     whole, every list empty.
//
// Then A4 (shares, medians), A6 (order), A9 (no repeated key, checked
// first) and the pairs' top 20. Inconsistent raw values give the context
// hidden whole and ErrInconsistent.
func GuardResponse(raw Raw, forced Forced, known ...Body) (Body, error) {
	return NewLedger(known...).guardResponse(raw, forced)
}

// guardResponse is GuardResponse against the ledger's record.
func (l *Ledger) guardResponse(raw Raw, forced Forced) (Body, error) {
	g, err := newGuarding(&raw)
	if err != nil {
		return HiddenWhole(raw.Context), err
	}
	if g.wholeByPatients() {
		return HiddenWhole(raw.Context), nil
	}
	ks := l.p.known
	// A group already published is fixed: hidden in a known body, hidden
	// here (D5); shown there, shown here (a series point is its child's
	// total, a filter parent's row is this context's group).
	hiddenElsewhere, shownElsewhere := map[string]bool{}, map[string]bool{}
	for _, k := range ks {
		for i, c := range k.sh.cells {
			if c.virtual || c.group == zeroGroup {
				continue
			}
			if k.hide[i] {
				hiddenElsewhere[c.group] = true
			} else {
				shownElsewhere[c.group] = true
			}
		}
	}
	for g := range hiddenElsewhere {
		delete(shownElsewhere, g)
	}
	g.pinned, g.public = shownElsewhere, map[string]bool{}
	extra := Forced{}
	pr := l.p
	// A group the published bodies already determine is public: hiding it
	// protects nothing, so it is shown (unless it is 1 to 4, a leak no
	// guard here can undo).
	for i, c := range g.sh.cells {
		if v, ok := pr.sys.vars[c.group]; ok && !c.virtual && !pr.sys.shown[v] {
			if x, det := pr.before.Value([]int{v}, nil); det && x == g.vals[i] && !small(x) {
				g.public[c.group] = true
				delete(hiddenElsewhere, c.group)
			}
		}
	}
	for {
		hide := g.rules(forced, extra, hiddenElsewhere)
		if hide[g.sh.pat] {
			return HiddenWhole(raw.Context), nil
		}
		bad := g.check(hide, pr)
		if bad == inconsistent {
			return HiddenWhole(raw.Context), ErrInconsistent
		}
		if bad < 0 {
			return g.body(hide), nil
		}
		extra[g.sh.cells[bad].group] = true
	}
}

// HiddenWhole is a context's body hidden whole: every number hidden, every
// list empty (A7).
func HiddenWhole(ctx Context) Body {
	h := smallgroup.Guarded{Suppressed: true}
	var b Body
	b.Context, b.Whole = ctx, true
	b.Patients = h
	b.Total = CNE[smallgroup.Guarded]{h, h, h}
	a := &b.Alerts
	a.Total, a.Critical, a.Urgent, a.Info, a.Dismissed = h, h, h, h, h
	a.MedianAck = GuardedMinutes{Suppressed: true}
	a.CriticalMedianAck = GuardedMinutes{Suppressed: true}
	return b
}

type guarding struct {
	// pinned groups are shown in a known body: never hidden here unless
	// forced. public ones are determined by the known bodies (not 1 to 4):
	// hiding them protects nothing, so they are always shown.
	pinned, public map[string]bool
	raw            *Raw
	sh             *sheet
	vals           []int64
	meds           []*[]int64
}

func newGuarding(raw *Raw) (*guarding, error) {
	if err := CheckFilters(raw.Context.Filters); err != nil {
		return nil, err
	}
	sh, cs, _, ms := layout(raw)
	if !sh.keys {
		return nil, ErrInconsistent
	}
	g := &guarding{raw: raw, sh: sh, vals: make([]int64, len(cs)), meds: ms}
	for i, c := range cs {
		if c != nil {
			if *c < 0 {
				return nil, ErrInconsistent
			}
			g.vals[i] = *c
		}
	}
	// A member never sent is the remainder of the set it is alone in.
	for _, r := range sh.rels {
		v, n := -1, 0
		sum := int64(0)
		for _, p := range r.parts {
			if cs[p] == nil {
				v, n = p, n+1
			} else {
				sum += g.vals[p]
			}
		}
		if n == 1 {
			g.vals[v] = g.vals[r.total] - sum
			if g.vals[v] < 0 {
				return nil, ErrInconsistent
			}
		}
	}
	for _, r := range sh.rels {
		sum := int64(0)
		for _, p := range r.parts {
			sum += g.vals[p]
		}
		if sum != g.vals[r.total] {
			return nil, ErrInconsistent
		}
	}
	for _, t := range sh.tables {
		for j := 0; j < 3; j++ {
			if len(t.rows) == 0 {
				continue
			}
			sum := int64(0)
			for _, row := range t.rows {
				sum += g.vals[row[j]]
			}
			if sum != g.vals[t.all[j]] {
				return nil, ErrInconsistent
			}
		}
	}
	// One value per group, and 0 where the definitions say 0.
	byGroup := map[string]int64{}
	for i, c := range sh.cells {
		if c.group == zeroGroup && g.vals[i] != 0 {
			return nil, ErrInconsistent
		}
		if v, ok := byGroup[c.group]; ok && v != g.vals[i] {
			return nil, ErrInconsistent
		}
		byGroup[c.group] = g.vals[i]
	}
	// Agreed within rated, the medians' values within their counts.
	a := &raw.Alerts
	for _, m := range [][]int64{a.MedianAck, a.CriticalMedianAck} {
		for _, v := range m {
			if v < 0 {
				return nil, ErrInconsistent
			}
		}
	}
	if int64(len(a.MedianAck)) > a.Total || int64(len(a.CriticalMedianAck)) > a.Critical {
		return nil, ErrInconsistent
	}
	return g, nil
}

// wholeByPatients is A7 before anything else: patients 1 to 4, or a
// filtered context under QuerySetMin.
func (g *guarding) wholeByPatients() bool {
	p := g.vals[g.sh.pat]
	return (p > 0 && p < Min) || (g.raw.Context.Filters.Count() > 0 && p < QuerySetMin)
}

func small(v int64) bool { return v >= 1 && v < Min }

// rules runs A1, one state per group, the tables and the sets until nothing
// changes.
func (g *guarding) rules(forced, extra Forced, elsewhere map[string]bool) []bool {
	sh := g.sh
	hide := make([]bool, len(sh.cells))
	for i, c := range sh.cells {
		hide[i] = small(g.vals[i]) || forced[c.group] || extra[c.group] || elsewhere[c.group]
		if (g.pinned[c.group] && !forced[c.group] && !extra[c.group]) || g.public[c.group] {
			hide[i] = false
		}
	}
	for {
		before := append([]bool{}, hide...)
		// One state per group.
		hg := map[string]bool{}
		for i, c := range sh.cells {
			if hide[i] {
				hg[c.group] = true
			}
		}
		for i, c := range sh.cells {
			if hg[c.group] && c.group != zeroGroup {
				hide[i] = true
			}
		}
		for _, t := range sh.tables {
			if len(t.rows) == 0 {
				continue
			}
			rows := make([][]int64, len(t.rows))
			mask := smallgroup.Mask{All: make([]bool, 3), Rows: make([][]bool, len(t.rows))}
			for j := 0; j < 3; j++ {
				mask.All[j] = hide[t.all[j]]
			}
			for i, row := range t.rows {
				rows[i] = []int64{g.vals[row[0]], g.vals[row[1]], g.vals[row[2]]}
				mask.Rows[i] = []bool{hide[row[0]], hide[row[1]], hide[row[2]]}
			}
			out := smallgroup.TableForced(3, rows, make([]int64, len(rows)), mask)
			for j := 0; j < 3; j++ {
				hide[t.all[j]] = hide[t.all[j]] || out.All[j].Suppressed
			}
			for i, row := range t.rows {
				for j := 0; j < 3; j++ {
					hide[row[j]] = hide[row[j]] || out.Rows[i][j].Suppressed
				}
			}
		}
		for _, r := range sh.rels {
			parts := make([]int64, len(r.parts))
			f := make([]bool, len(r.parts)+1)
			f[0] = hide[r.total]
			for i, p := range r.parts {
				parts[i] = g.vals[p]
				f[i+1] = hide[p]
			}
			out := smallgroup.SetForced(g.vals[r.total], parts, f)
			hide[r.total] = hide[r.total] || out[0].Suppressed
			for i, p := range r.parts {
				hide[p] = hide[p] || out[i+1].Suppressed
			}
			if r.checked && hide[r.total] {
				for _, p := range r.parts {
					if g.vals[p] != 0 {
						hide[p] = true
					}
				}
			}
		}
		for i, c := range sh.cells {
			if (g.pinned[c.group] && !forced[c.group] && !extra[c.group]) || g.public[c.group] {
				hide[i] = false
			}
		}
		if equal(before, hide) {
			return hide
		}
	}
}

func equal(a, b []bool) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return len(a) == len(b)
}

// body assembles the guarded body: A4, A6, the pairs' top 20.
func (g *guarding) body(hide []bool) Body {
	var b Body
	b.Report = reshape[int64, struct{}, []int64, smallgroup.Guarded, smallgroup.GuardedShare, GuardedMinutes](g.raw)
	_, cs, ss, ms := layout(&b.Report)
	for i, c := range cs {
		if c == nil {
			continue
		}
		if hide[i] {
			*c = smallgroup.Guarded{Suppressed: true}
		} else {
			*c = smallgroup.Count(g.vals[i])
		}
	}
	for i, s := range g.sh.shares {
		*ss[i] = smallgroup.ShareOf(g.vals[s.num], g.vals[s.den], guardedOf(hide, g.vals, s.num), guardedOf(hide, g.vals, s.den))
	}
	for i, m := range g.sh.meds {
		hidden := false
		for _, c := range m.counts {
			hidden = hidden || hide[c]
		}
		*ms[i] = median(*g.meds[i], hidden)
	}
	order(&b)
	return b
}

func guardedOf(hide []bool, vals []int64, i int) smallgroup.Guarded {
	if hide[i] {
		return smallgroup.Guarded{Suppressed: true}
	}
	return smallgroup.Count(vals[i])
}

// median is A4: hidden over fewer than Min values or a hidden count; a
// whole number of minutes or a half; nothing to measure with no values.
func median(vals []int64, hidden bool) GuardedMinutes {
	switch {
	case hidden || (len(vals) > 0 && len(vals) < Min):
		return GuardedMinutes{Suppressed: true}
	case len(vals) == 0:
		return GuardedMinutes{}
	}
	v := append([]int64{}, vals...)
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	n := len(v)
	m := float64(v[n/2])
	if n%2 == 0 {
		m = math.Round(float64(v[n/2-1]+v[n/2])) / 2
	}
	return GuardedMinutes{Value: &m}
}

// reshape copies a report's structure (keys, context) with zero values.
func reshape[C, S, M, C2, S2, M2 any](r *Report[C, S, M]) Report[C2, S2, M2] {
	var o Report[C2, S2, M2]
	o.Context = r.Context
	o.Sources = append([]string(nil), r.Sources...)
	for _, x := range r.ByCondition {
		o.ByCondition = append(o.ByCondition, ConditionRow[C2]{Condition: x.Condition})
	}
	for _, x := range r.Series {
		o.Series = append(o.Series, Point[C2]{Start: x.Start})
	}
	cp := func(in []Part[C]) []Part[C2] {
		var out []Part[C2]
		for _, x := range in {
			out = append(out, Part[C2]{Key: x.Key, Setting: x.Setting})
		}
		return out
	}
	o.ByAge, o.BySex, o.BySetting, o.ByLocation, o.BySpecialty = cp(r.ByAge), cp(r.BySex), cp(r.BySetting), cp(r.ByLocation), cp(r.BySpecialty)
	for _, x := range r.Pairs {
		o.Pairs = append(o.Pairs, Pair[C2, S2]{A: x.A, B: x.B})
	}
	for _, x := range r.Findings {
		o.Findings = append(o.Findings, FindingRow[C2]{Name: x.Name, Flag: x.Flag})
	}
	for _, x := range r.Alerts.DismissReasons {
		o.Alerts.DismissReasons = append(o.Alerts.DismissReasons, ReasonRow[C2]{Reason: x.Reason})
	}
	for _, x := range r.Quality {
		o.Quality = append(o.Quality, QualityRow[C2, S2]{Condition: x.Condition})
	}
	return o
}
