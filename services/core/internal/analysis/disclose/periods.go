package disclose

import (
	"sort"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// D3: a period is guarded with its frozen children (design §5.4). For each
// additive measure, parent = Σ children holds exactly (D2); the children
// were published first and never change, so only the parent's cell may be
// hidden further: smallgroup.AcrossRowsFrozen, with prevalence's cases,
// new and existing as tables (conditions against the total) and every
// other additive count as a line of its own. Distinct counts get no
// equation (R-A2, measured by P5).

// groupVals is a layout's raw values by group: the raw value (raw) or whether it
// is hidden (a body).
type groupVals map[string]int64

func rawGroups(r *Raw) groupVals {
	sh, cs, _, _ := layout(r)
	out := groupVals{}
	for i, c := range sh.cells {
		if cs[i] != nil {
			out[c.group] = *cs[i]
		}
	}
	return out
}

func hiddenGroups(b *Body) map[string]bool {
	st := stateOf(b)
	out := map[string]bool{}
	for i, c := range st.sh.cells {
		if !c.virtual {
			out[c.group] = st.hide[i]
		}
	}
	return out
}

// ParentForced is D3's forced cells for parent, given its current body
// (nil before its first guard) and its children's raw values and stored
// bodies, in order. A child that doesn't list a group has 0 there, shown;
// a child hidden whole hides everything.
func ParentForced(parent Raw, body *Body, children []ChildBodies) Forced {
	kids := calendar.Children(parent.Context.Period)
	out := Forced{}
	if len(kids) == 0 || len(children) != len(kids) {
		return out
	}
	psh, pcs, _, _ := layout(&parent)
	var phid map[string]bool
	if body != nil {
		phid = hiddenGroups(body)
	}
	cv := make([]groupVals, len(children))
	ch := make([]map[string]bool, len(children))
	for i := range children {
		cv[i] = rawGroups(&children[i].Raw)
		ch[i] = hiddenGroups(&children[i].Body)
	}
	childGroup := func(d desc, i int) string {
		first := parent.Context.First && kids[i].From.Equal(parent.Context.Period.From)
		return group(d, kids[i], first)
	}
	childState := func(d desc, i int) (int64, bool) {
		g := childGroup(d, i)
		if children[i].Body.Whole {
			return cv[i][g], true
		}
		return cv[i][g], ch[i][g]
	}
	parentHidden := func(i int) bool {
		return phid[psh.cells[i].group] || smallgroup.Count(*pcs[i]).Suppressed
	}
	// Prevalence: conditions against the total, per measure.
	inTable := map[int]bool{}
	for w := 0; w < 3; w++ {
		t := psh.tables[0]
		all := []int64{*pcs[t.all[w]]}
		hall := []bool{parentHidden(t.all[w])}
		for i := range children {
			v, h := childState(psh.cells[t.all[w]].d, i)
			all, hall = append(all, v), append(hall, h)
		}
		inTable[t.all[w]] = true
		var rows [][]int64
		hid := [][]bool{hall}
		var ids []int
		for _, row := range t.rows {
			c := row[w]
			inTable[c] = true
			r := []int64{*pcs[c]}
			h := []bool{parentHidden(c)}
			for i := range children {
				v, hh := childState(psh.cells[c].d, i)
				r, h = append(r, v), append(h, hh)
			}
			rows, hid, ids = append(rows, r), append(hid, h), append(ids, c)
		}
		// A condition a child has but the parent doesn't list can't be.
		res := smallgroup.AcrossRowsFrozen(all, rows, hid, []int{0})
		if res[0][0] {
			out[psh.cells[t.all[w]].group] = true
		}
		for k, c := range ids {
			if res[k+1][0] {
				out[psh.cells[c].group] = true
			}
		}
	}
	// Every other additive count: a line parent = Σ children.
	for i, c := range psh.cells {
		// The series points are the children's own cells, not the parent's.
		if c.virtual || inTable[i] || !additive(c.d.m) || c.group == zeroGroup || !c.period.Equal(parent.Context.Period) {
			continue
		}
		line := []int64{*pcs[i]}
		h := []bool{parentHidden(i)}
		for k := range children {
			v, hh := childState(c.d, k)
			line, h = append(line, v), append(h, hh)
		}
		res := smallgroup.AcrossRowsFrozen(line, [][]int64{line}, [][]bool{h, h}, []int{0})
		if res[0][0] || res[1][0] {
			out[c.group] = true
		}
	}
	return out
}

// GuardPeriod guards an unfiltered context with its children (D3): Layer A
// with the children known, then ParentForced on the result, until it adds
// nothing.
func GuardPeriod(raw Raw, children []ChildBodies) (Body, error) {
	return Guard(raw, children, nil)
}

// Guard is the whole guard of one context (D3, D5): FamilyForced against
// its stored filter parents (hidden whole if its own group is hidden in
// one), then GuardResponse with every child and parent body known and
// ParentForced, repeated until nothing changes. The children are the
// period's, with the same filters; both lists may be empty.
func Guard(raw Raw, children []ChildBodies, parents []ParentContext) (Body, error) {
	if err := CheckFilters(raw.Context.Filters); err != nil {
		return HiddenWhole(raw.Context), err
	}
	forced, whole := FamilyForced(raw, parents)
	if whole {
		return HiddenWhole(raw.Context), nil
	}
	var known []Body
	for _, c := range children {
		known = append(known, c.Body)
	}
	for _, p := range parents {
		known = append(known, p.Body)
	}
	forced = forced.Union(ParentForced(raw, nil, children))
	for {
		b, err := GuardResponse(raw, forced, known...)
		if err != nil || b.Whole {
			return b, err
		}
		more := ParentForced(raw, &b, children)
		n := len(forced)
		forced = forced.Union(more)
		if len(forced) == n {
			return b, nil
		}
	}
}

// HistoryPointOf is a stored body's point (D3): its cases, new and patients
// as published; hidden whole when the body is.
func HistoryPointOf(b Body) HistoryPoint {
	p := HistoryPoint{Start: b.Context.Period.From.Format(time.DateOnly), Cases: b.Total.Cases, New: b.Total.New, Patients: b.Patients}
	if b.Whole {
		h := smallgroup.Guarded{Suppressed: true}
		p.Cases, p.New, p.Patients = h, h, h
	}
	return p
}

// History is the points of the stored bodies of earlier periods of one
// level (the same view and filters), oldest first: at most HistoryPoints,
// the latest, and no total over them (I2).
func History(bodies []Body) []HistoryPoint {
	bs := append([]Body{}, bodies...)
	sort.SliceStable(bs, func(i, j int) bool { return bs[i].Context.Period.From.Before(bs[j].Context.Period.From) })
	if len(bs) > HistoryPoints {
		bs = bs[len(bs)-HistoryPoints:]
	}
	out := make([]HistoryPoint, 0, len(bs))
	for _, b := range bs {
		out = append(out, HistoryPointOf(b))
	}
	return out
}
