package attack

import (
	"encoding/json"
	"fmt"
	mbits "math/bits"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup/solve"
)

// The atoms (the attacker's unknowns), each a count of patients or facts:
//
//   - block: patients in one block with attributes a, conditions S (a case
//     each), new ones N ⊆ S and findings F. At a week these are the
//     patients; summed over a period's blocks they give every additive
//     case count (D2).
//   - dist: patients of one month or quarter with attributes a, the
//     conditions S and findings F they had in it (its distinct counts).
//     Linked to the blocks by zeros only (bounds are P5's).
//   - consult, result, alert: facts in one block, of a patient with
//     attributes a and block conditions S (for a condition filter), with
//     their categories.
//
// What the attacker knows: every shown cell; a shown 0 makes every atom in
// it 0, and the zeros that follow (a fact needs its patient, a patient of a
// month needs blocks, a block's patient needs a month that can hold it, an
// existing case needs an earlier case once the start is known); and, from
// the start, a first period's new cases of a condition are its patients.
const (
	kBlock = iota
	kDist
	kConsult
	kResult
	kAlert
)

type atom struct {
	kind       int8
	b, p, a    int // block, period (dist), attribute tuple
	S, N, F    uint32
	d          int // consult: condition; result: finding
	c1, c2, c3 int // consult: confidence, rating, correction; alert: severity, dismissal
}

// The attribute dimensions, as the contract names the filters.
const (
	dAge = iota
	dSex
	dLoc
	dSetting
	dSpec
	dSource
	nAttr
)

var attrNames = map[string]int{"age_band": dAge, "sex": dSex, "location": dLoc, "setting": dSetting, "specialty": dSpec, "source": dSource}

// Doc is one published body: its context and each view's JSON.
type Doc struct {
	Period  calendar.Period
	Filters map[string]string
	First   bool
	Views   map[string][]byte
}

type term struct {
	atom int
	coef int64
}

// Cell is one published number.
type Cell struct {
	Doc   int
	Name  string
	box   []term
	Shown bool
	Value int64
	Truth int64
}

type member struct {
	box    []term
	hidden bool
}

type pset struct {
	doc     int
	name    string
	members []member
}

// Model is the attacker over a world and the documents published from it.
type Model struct {
	w       *World
	tuples  [][nAttr]string
	tupleOf map[[5]int]int
	atoms   []atom
	count   []int64
	blockA  map[int][]int // block → block atoms
	factA   map[int][]int // block → fact atoms
	periods []calendar.Period
	distA   map[int][]int // period → dist atoms
	Docs    []Doc
	Cells   []Cell
	sets    []pset
	start   [][]term // the start's equations (= 0)
}

// New lays out the universe of w's dimensions over the blocks the docs
// cover, and reads every doc.
func New(w *World, docs []Doc) (*Model, error) {
	m := &Model{w: w, tupleOf: map[[5]int]int{}, blockA: map[int][]int{}, factA: map[int][]int{}, distA: map[int][]int{}, Docs: docs}
	d := w.Dims
	for ai := range d.Ages {
		for si := range d.Sexes {
			for li, l := range d.Locations {
				for pi := range d.Specialties {
					for oi := range d.Sources {
						m.tupleOf[[5]int{ai, si, li, pi, oi}] = len(m.tuples)
						m.tuples = append(m.tuples, [nAttr]string{d.Ages[ai], d.Sexes[si], l.ID, l.Setting, d.Specialties[pi], d.Sources[oi]})
					}
				}
			}
		}
	}
	need := map[int]bool{}
	for _, doc := range docs {
		bs := m.blocksOf(doc.Period)
		if len(bs) == 0 {
			return nil, fmt.Errorf("attack: %v covers no block of the world", doc.Period)
		}
		for _, b := range bs {
			need[b] = true
		}
		if doc.Period.Kind != calendar.Week && m.periodIndex(doc.Period) < 0 {
			m.periods = append(m.periods, doc.Period)
		}
	}
	k, nf := len(d.Conditions), len(d.Findings)
	full := uint32(1)<<k - 1
	var bl []int
	for b := range need {
		bl = append(bl, b)
	}
	sort.Ints(bl)
	for _, b := range bl {
		for a := range m.tuples {
			for S := uint32(0); S <= full; S++ {
				for N := S; ; N = (N - 1) & S {
					for F := uint32(0); F < 1<<nf; F++ {
						m.add(atom{kind: kBlock, b: b, a: a, S: S, N: N, F: F}, m.blockA, b)
					}
					if N == 0 {
						break
					}
				}
				for _, c := range setBits(S) {
					for c1 := range confidences {
						for c2 := range ratings {
							for c3 := range corrections {
								m.add(atom{kind: kConsult, b: b, a: a, S: S, d: c, c1: c1, c2: c2, c3: c3}, m.factA, b)
							}
						}
					}
				}
				for f := 0; f < nf; f++ {
					m.add(atom{kind: kResult, b: b, a: a, S: S, d: f}, m.factA, b)
				}
				for c1 := range severities {
					for c2 := range dismissals {
						m.add(atom{kind: kAlert, b: b, a: a, S: S, c1: c1, c2: c2}, m.factA, b)
					}
				}
			}
		}
	}
	for p := range m.periods {
		for a := range m.tuples {
			for S := uint32(0); S <= full; S++ {
				for F := uint32(0); F < 1<<nf; F++ {
					m.add(atom{kind: kDist, b: -1, p: p, a: a, S: S, F: F}, m.distA, p)
				}
			}
		}
	}
	m.truth()
	for i := range docs {
		if err := m.read(i); err != nil {
			return nil, fmt.Errorf("attack: doc %d (%v): %w", i, docs[i].Period, err)
		}
	}
	m.startEquations()
	return m, nil
}

func (m *Model) add(a atom, idx map[int][]int, key int) {
	idx[key] = append(idx[key], len(m.atoms))
	m.atoms = append(m.atoms, a)
}

func (m *Model) periodIndex(p calendar.Period) int {
	for i, q := range m.periods {
		if q.Equal(p) {
			return i
		}
	}
	return -1
}

func (m *Model) blocksOf(p calendar.Period) []int {
	var out []int
	for i, b := range m.w.Blocks {
		if !b.From.Before(p.From) && !b.To.After(p.To) {
			out = append(out, i)
		}
	}
	return out
}

// truth counts the world's patients and facts into the atoms, so a test
// can compare shown values with the attacker's definitions and know which
// hidden cells hold 1 or more.
func (m *Model) truth() {
	index := map[atom]int{}
	for i, a := range m.atoms {
		index[a] = i
	}
	m.count = make([]int64, len(m.atoms))
	inc := func(a atom, n int64) {
		if i, ok := index[a]; ok {
			m.count[i] += n
		}
	}
	for si := range m.w.Subjects {
		s := &m.w.Subjects[si]
		t := m.tupleOf[[5]int{s.Age, s.Sex, s.Loc, s.Spec, s.Source}]
		for b, blk := range s.Blocks {
			inc(atom{kind: kBlock, b: b, a: t, S: blk.Conds, N: blk.New, F: blk.Finds}, 1)
			for _, c := range blk.Consults {
				inc(atom{kind: kConsult, b: b, a: t, S: blk.Conds, d: c.Cond, c1: c.Confidence, c2: c.Rating, c3: c.Correction}, 1)
			}
			for f, n := range blk.Results {
				inc(atom{kind: kResult, b: b, a: t, S: blk.Conds, d: f}, int64(n))
			}
			for _, al := range blk.Alerts {
				inc(atom{kind: kAlert, b: b, a: t, S: blk.Conds, c1: al.Severity, c2: al.Dismiss}, 1)
			}
		}
		for p, per := range m.periods {
			var S, F uint32
			in := false
			for _, b := range m.blocksOf(per) {
				if blk, ok := s.Blocks[b]; ok {
					in = true
					S |= blk.Conds
					F |= blk.Finds
				}
			}
			if in {
				inc(atom{kind: kDist, b: -1, p: p, a: t, S: S, F: F}, 1)
			}
		}
	}
}

func (m *Model) value(box []term) int64 {
	var v int64
	for _, t := range box {
		v += t.coef * m.count[t.atom]
	}
	return v
}

// ---- reading a document -----------------------------------------------------

type filt struct {
	attrs map[int]string
	cond  int // −1 none
}

func (m *Model) matches(a int, f filt, extra map[int]string) bool {
	t := m.tuples[a]
	for d, v := range f.attrs {
		if t[d] != v {
			return false
		}
	}
	for d, v := range extra {
		if t[d] != v {
			return false
		}
	}
	return true
}

// dist is the distinct patients of p under f with the extra attributes,
// every condition of conds, and finding find (−1 any).
func (m *Model) dist(p calendar.Period, f filt, extra map[int]string, conds uint32, find int) []term {
	var ids []int
	if p.Kind == calendar.Week {
		bs := m.blocksOf(p)
		ids = m.blockA[bs[0]]
	} else {
		ids = m.distA[m.periodIndex(p)]
	}
	if f.cond >= 0 {
		conds |= 1 << f.cond
	}
	var out []term
	for _, id := range ids {
		a := m.atoms[id]
		if m.matches(a.a, f, extra) && a.S&conds == conds && (find < 0 || a.F&(1<<find) != 0) {
			out = append(out, term{id, 1})
		}
	}
	return out
}

// additive is a case count over p's blocks: which 0 cases, 1 new, 2
// existing; of condition d (−1 every condition).
func (m *Model) additive(p calendar.Period, f filt, which, d int) []term {
	var out []term
	for _, b := range m.blocksOf(p) {
		for _, id := range m.blockA[b] {
			a := m.atoms[id]
			if !m.matches(a.a, f, nil) || (f.cond >= 0 && a.S&(1<<f.cond) == 0) {
				continue
			}
			x := []uint32{a.S, a.N, a.S &^ a.N}[which]
			var c int64
			if d < 0 {
				c = int64(mbits.OnesCount32(x))
			} else if x&(1<<d) != 0 {
				c = 1
			}
			if c > 0 {
				out = append(out, term{id, c})
			}
		}
	}
	return out
}

// facts is the facts of kind over p's blocks under f that pred keeps.
func (m *Model) facts(p calendar.Period, f filt, kind int8, pred func(atom) bool) []term {
	var out []term
	for _, b := range m.blocksOf(p) {
		for _, id := range m.factA[b] {
			a := m.atoms[id]
			if a.kind == kind && m.matches(a.a, f, nil) && (f.cond < 0 || a.S&(1<<f.cond) != 0) && pred(a) {
				out = append(out, term{id, 1})
			}
		}
	}
	return out
}

func minus(a, b []term) []term {
	m := map[int]int64{}
	for _, t := range a {
		m[t.atom] += t.coef
	}
	for _, t := range b {
		m[t.atom] -= t.coef
	}
	var out []term
	for k, v := range m {
		if v != 0 {
			out = append(out, term{k, v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].atom < out[j].atom })
	return out
}

type obj = map[string]any

// guarded reads {value, suppressed}: shown with its value, or hidden.
func guarded(v any) (shown bool, val int64, err error) {
	o, ok := v.(obj)
	if !ok {
		return false, 0, fmt.Errorf("not a guarded value: %v", v)
	}
	sup, ok := o["suppressed"].(bool)
	if !ok {
		return false, 0, fmt.Errorf("no suppressed: %v", v)
	}
	if sup {
		if o["value"] != nil {
			return false, 0, fmt.Errorf("hidden with a value: %v", v)
		}
		return false, 0, nil
	}
	x, ok := o["value"].(float64)
	if !ok || x != float64(int64(x)) || x < 0 || (x >= 1 && x < 5) {
		return false, 0, fmt.Errorf("a shown count that isn't one: %v", v)
	}
	return true, int64(x), nil
}

func (m *Model) condIndex(v any) (int, error) {
	o, _ := v.(obj)
	id, _ := o["id"].(string)
	for i, c := range m.w.Dims.Conditions {
		if c == id {
			return i, nil
		}
	}
	return -1, fmt.Errorf("unknown condition %v", v)
}

func (m *Model) findIndex(name, flag any) (int, error) {
	n, _ := name.(string)
	f, _ := flag.(string)
	for i, x := range m.w.Dims.Findings {
		if x.Name == n && x.Flag == f {
			return i, nil
		}
	}
	return -1, fmt.Errorf("unknown finding %v/%v", name, flag)
}

func strOr(v any) string {
	s, _ := v.(string)
	return s
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func (m *Model) read(di int) error {
	doc := m.Docs[di]
	f := filt{attrs: map[int]string{}, cond: -1}
	for k, v := range doc.Filters {
		if k == "condition" {
			c, err := m.condIndex(obj{"id": v})
			if err != nil {
				return err
			}
			f.cond = c
			continue
		}
		d, ok := attrNames[k]
		if !ok {
			return fmt.Errorf("unknown filter %q", k)
		}
		f.attrs[d] = v
	}
	P := doc.Period
	views := map[string]obj{}
	for _, v := range []string{"prevalence", "affected", "co-occurring", "findings", "quality"} {
		var o obj
		if err := json.Unmarshal(doc.Views[v], &o); err != nil {
			return fmt.Errorf("%s: %w", v, err)
		}
		views[v] = o
	}
	var firstErr error
	cell := func(name string, v any, box []term) (int, bool) {
		shown, val, err := guarded(v)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", name, err)
		}
		m.Cells = append(m.Cells, Cell{Doc: di, Name: name, box: box, Shown: shown, Value: val, Truth: m.value(box)})
		return len(m.Cells) - 1, shown
	}
	mem := func(i int) member { return member{m.Cells[i].box, !m.Cells[i].Shown} }
	// A member never sent is known exactly when everything else in the
	// row that defines it is shown; it counts as hidden otherwise.
	virtual := func(box []term, deps ...int) member {
		h := false
		for _, d := range deps {
			h = h || !m.Cells[d].Shown
		}
		return member{box, h}
	}
	addSet := func(name string, ms ...member) {
		m.sets = append(m.sets, pset{di, name, ms})
	}
	pat := m.dist(P, f, nil, 0, -1)
	var patCell int
	for _, v := range []string{"prevalence", "affected", "co-occurring", "findings", "quality"} {
		patCell, _ = cell(v+".patients", views[v]["patients"], pat)
	}

	// Prevalence.
	pv := views["prevalence"]
	tot, _ := pv["total"].(obj)
	var totals [3]int
	for w, k := range []string{"cases", "new", "existing"} {
		totals[w], _ = cell("total."+k, tot[k], m.additive(P, f, w, -1))
	}
	addSet("total", mem(totals[1]), mem(totals[2]))
	cols := [3][]member{}
	for _, r := range list(pv["by_condition"]) {
		ro, _ := r.(obj)
		c, err := m.condIndex(ro["condition"])
		if err != nil {
			return err
		}
		var ids [3]int
		for w, k := range []string{"cases", "new", "existing"} {
			ids[w], _ = cell("by_condition."+m.w.Dims.Conditions[c]+"."+k, ro[k], m.additive(P, f, w, c))
			cols[w] = append(cols[w], mem(ids[w]))
		}
		addSet("by_condition row", mem(ids[1]), mem(ids[2]))
	}
	for w := range cols {
		if len(cols[w]) > 0 {
			addSet("by_condition column", cols[w]...)
		}
	}
	kids := []calendar.Period{P}
	if P.Kind != calendar.Week {
		kids = calendar.Children(P)
	}
	scols := [3][]member{}
	for _, r := range list(pv["series"]) {
		ro, _ := r.(obj)
		start := strOr(ro["start"])
		var kp calendar.Period
		for _, k := range kids {
			if k.From.Format(time.DateOnly) == start {
				kp = k
			}
		}
		if kp.Kind == "" {
			return fmt.Errorf("series point %s isn't a child", start)
		}
		c, _ := cell("series."+start+".cases", ro["cases"], m.additive(kp, f, 0, -1))
		n, _ := cell("series."+start+".new", ro["new"], m.additive(kp, f, 1, -1))
		e := virtual(m.additive(kp, f, 2, -1), c, n)
		addSet("series point", mem(n), e)
		scols[0], scols[1], scols[2] = append(scols[0], mem(c)), append(scols[1], mem(n)), append(scols[2], e)
	}
	for w := range scols {
		if len(scols[w]) > 0 {
			addSet("series column", scols[w]...)
		}
	}

	// Affected: the partitions, and locations within settings.
	af := views["affected"]
	byLoc := map[string][]member{}
	locIDs := map[string]bool{}
	for _, part := range []struct{ field, key string }{{"by_age", "band"}, {"by_sex", "sex"}, {"by_setting", "setting"}, {"by_location", "location"}, {"by_specialty", "specialty"}} {
		var ms []member
		for _, r := range list(af[part.field]) {
			ro, _ := r.(obj)
			var key string
			var d int
			switch part.field {
			case "by_location":
				lo, _ := ro["location"].(obj)
				key, d = strOr(lo["id"]), dLoc
				if key != "" {
					locIDs[key] = true
				}
			default:
				key = strOr(ro[part.key])
				d = map[string]int{"by_age": dAge, "by_sex": dSex, "by_setting": dSetting, "by_specialty": dSpec}[part.field]
			}
			i, _ := cell(part.field+"."+key, ro["patients"], m.dist(P, f, map[int]string{d: key}, 0, -1))
			ms = append(ms, mem(i))
			if part.field == "by_location" && key != "" {
				lo, _ := ro["location"].(obj)
				byLoc[strOr(lo["setting"])] = append(byLoc[strOr(lo["setting"])], mem(i))
			}
		}
		addSet(part.field, ms...)
	}
	settingCell := map[string]int{}
	for i := range m.Cells {
		if m.Cells[i].Doc == di && strings.HasPrefix(m.Cells[i].Name, "by_setting.") {
			settingCell[strings.TrimPrefix(m.Cells[i].Name, "by_setting.")] = i
		}
	}
	for _, s := range sortedKeys(byLoc) {
		if s == "" {
			continue
		}
		deps := []int{}
		if i, ok := settingCell[s]; ok {
			deps = append(deps, i)
		}
		h := false
		for _, mb := range byLoc[s] {
			h = h || mb.hidden
		}
		// The setting's patients at no listed location: never sent.
		var rest []term
		for _, t := range m.dist(P, f, map[int]string{dSetting: s}, 0, -1) {
			if !locIDs[m.tuples[m.atoms[t.atom].a][dLoc]] {
				rest = append(rest, t)
			}
		}
		v := virtual(rest, deps...)
		v.hidden = v.hidden || h
		addSet("setting "+s, append(byLoc[s], v)...)
	}

	// Pairs.
	for _, r := range list(views["co-occurring"]["pairs"]) {
		ro, _ := r.(obj)
		a, err := m.condIndex(ro["a"])
		if err != nil {
			return err
		}
		b, err := m.condIndex(ro["b"])
		if err != nil {
			return err
		}
		name := "pair " + m.w.Dims.Conditions[a] + "+" + m.w.Dims.Conditions[b]
		ab := m.dist(P, f, nil, 1<<a, -1)
		both := m.dist(P, f, nil, 1<<a|1<<b, -1)
		ai, _ := cell(name+".a_patients", ro["a_patients"], ab)
		bi, _ := cell(name+".patients", ro["patients"], both)
		addSet(name, mem(bi), virtual(minus(ab, both), ai, bi))
	}

	// Findings and alerts.
	fv := views["findings"]
	for _, r := range list(fv["findings"]) {
		ro, _ := r.(obj)
		fi, err := m.findIndex(ro["name"], ro["flag"])
		if err != nil {
			return err
		}
		name := "finding " + strOr(ro["name"]) + "/" + strOr(ro["flag"])
		cell(name+".patients", ro["patients"], m.dist(P, f, nil, 0, fi))
		cell(name+".results", ro["results"], m.facts(P, f, kResult, func(a atom) bool { return a.d == fi }))
	}
	al, _ := fv["alerts"].(obj)
	sev, _ := al["by_severity"].(obj)
	var sevs []member
	for si, s := range severities {
		i, _ := cell("alerts."+s, sev[s], m.facts(P, f, kAlert, func(a atom) bool { return a.c1 == si }))
		sevs = append(sevs, mem(i))
	}
	ti, _ := cell("alerts.total", al["total"], m.facts(P, f, kAlert, func(atom) bool { return true }))
	addSet("alerts by severity", sevs...)
	di2, _ := cell("alerts.dismissed", al["dismissed"], m.facts(P, f, kAlert, func(a atom) bool { return a.c2 != 0 }))
	addSet("alerts dismissed", mem(di2), virtual(m.facts(P, f, kAlert, func(a atom) bool { return a.c2 == 0 }), ti, di2))
	var rs []member
	for _, r := range list(al["dismiss_reasons"]) {
		ro, _ := r.(obj)
		ri := -1
		for i, x := range dismissals {
			if x != "" && x == strOr(ro["reason"]) {
				ri = i
			}
		}
		if ri < 0 {
			return fmt.Errorf("unknown reason %v", ro["reason"])
		}
		i, _ := cell("alerts.reason."+dismissals[ri], ro["count"], m.facts(P, f, kAlert, func(a atom) bool { return a.c2 == ri }))
		rs = append(rs, mem(i))
	}
	if len(rs) > 0 {
		addSet("dismiss reasons", rs...)
	}

	// Quality.
	for _, r := range list(views["quality"]["by_condition"]) {
		ro, _ := r.(obj)
		c, err := m.condIndex(ro["condition"])
		if err != nil {
			return err
		}
		name := "quality " + m.w.Dims.Conditions[c]
		q := func(pred func(atom) bool) []term {
			return m.facts(P, f, kConsult, func(a atom) bool { return a.d == c && pred(a) })
		}
		ci0, _ := cell(name+".consultations", ro["consultations"], q(func(atom) bool { return true }))
		conf, _ := ro["confidence"].(obj)
		var lv []member
		for ci, x := range confidences {
			i, _ := cell(name+"."+x, conf[x], q(func(a atom) bool { return a.c1 == ci }))
			lv = append(lv, mem(i))
		}
		addSet(name+" confidence", lv...)
		ratedBox := q(func(a atom) bool { return a.c2 != 0 })
		ri, ratedShown := cell(name+".rated", ro["rated"], ratedBox)
		addSet(name+" rated", mem(ri), virtual(q(func(a atom) bool { return a.c2 == 0 }), ci0, ri))
		// A shown rate beside a shown rated gives agreed (rule 3 keeps it
		// off 1–4 and its complement).
		rate, _ := ro["agreement_rate"].(obj)
		rsup, _ := rate["suppressed"].(bool)
		agreedBox := q(func(a atom) bool { return a.c2 == 1 })
		agreedShown := ratedShown && !rsup && rate["value"] != nil
		m.Cells = append(m.Cells, Cell{Doc: di, Name: name + ".agreed", box: agreedBox, Shown: agreedShown, Truth: m.value(agreedBox), Value: m.value(agreedBox)})
		addSet(name+" agreed", member{agreedBox, !agreedShown}, virtual(q(func(a atom) bool { return a.c2 == 2 }), ri, len(m.Cells)-1))
		co, _ := ro["corrections"].(obj)
		cell(name+".corrections", co["total"], q(func(a atom) bool { return a.c3 != 0 }))
		var st []member
		for ci, x := range corrections {
			if x == "" {
				continue
			}
			i, _ := cell(name+".corrections."+x, co[x], q(func(a atom) bool { return a.c3 == ci }))
			st = append(st, mem(i))
		}
		addSet(name+" corrections", st...)
	}
	_ = patCell
	return firstErr
}

// startEquations: in a period starting at the world's start, a
// condition's new cases over its blocks are its patients, attributes by
// attributes (a month or quarter; at a week the zeros say it).
func (m *Model) startEquations() {
	if !m.w.Start || len(m.w.Blocks) == 0 {
		return
	}
	for p, per := range m.periods {
		if !per.From.Equal(m.w.Blocks[0].From) {
			continue
		}
		for a := range m.tuples {
			for c := range m.w.Dims.Conditions {
				var eq []term
				for _, b := range m.blocksOf(per) {
					for _, id := range m.blockA[b] {
						if x := m.atoms[id]; x.a == a && x.N&(1<<c) != 0 {
							eq = append(eq, term{id, 1})
						}
					}
				}
				for _, id := range m.distA[p] {
					if x := m.atoms[id]; x.a == a && x.S&(1<<c) != 0 {
						eq = append(eq, term{id, -1})
					}
				}
				m.start = append(m.start, eq)
			}
		}
	}
}

// Mismatches are shown values that differ from the attacker's definitions
// counted on the world: the engine and the attacker disagree.
func (m *Model) Mismatches() []string {
	var out []string
	for _, c := range m.Cells {
		if c.Shown && c.Value != c.Truth {
			out = append(out, fmt.Sprintf("doc %d %s: shown %d, defined %d", c.Doc, c.Name, c.Value, c.Truth))
		}
	}
	return out
}

func boxKey(box []term) string {
	var s strings.Builder
	for _, t := range box {
		s.WriteString(strconv.Itoa(t.atom) + ":" + strconv.FormatInt(t.coef, 10) + ",")
	}
	return s.String()
}

// OneState lists groups (identical boxes) shown in one of docs and hidden
// in another (D5) — boxes that are 0 by definition aside.
func (m *Model) OneState(docs []int) []string {
	in := map[int]bool{}
	for _, d := range docs {
		in[d] = true
	}
	state := map[string]int{}
	name := map[string]string{}
	var out []string
	for _, c := range m.Cells {
		if !in[c.Doc] || len(c.box) == 0 {
			continue
		}
		k := boxKey(c.box)
		s := 1
		if c.Shown {
			s = 2
		}
		if prev, ok := state[k]; ok && prev != s {
			out = append(out, fmt.Sprintf("%s (doc %d) and %s differ", c.Name, c.Doc, name[k]))
		}
		state[k], name[k] = s, fmt.Sprintf("%s (doc %d)", c.Name, c.Doc)
	}
	return out
}

// HiddenProtected is the hidden cells of docs holding 1 or more.
func (m *Model) HiddenProtected(docs []int) []int {
	in := map[int]bool{}
	for _, d := range docs {
		in[d] = true
	}
	var out []int
	for i, c := range m.Cells {
		if in[c.Doc] && !c.Shown && c.Truth >= 1 {
			out = append(out, i)
		}
	}
	return out
}

// Exposed is the hidden cells of docs holding 1 or more that the docs,
// together, determine exactly.
func (m *Model) Exposed(docs []int) []int {
	z := m.zeros(docs, nil)
	in := map[int]bool{}
	for _, d := range docs {
		in[d] = true
	}
	var eqs, boxes [][]term
	var prot []int
	for i, c := range m.Cells {
		if !in[c.Doc] {
			continue
		}
		if c.Shown {
			eqs = append(eqs, c.box)
		} else if c.Truth >= 1 {
			boxes = append(boxes, c.box)
			prot = append(prot, i)
		}
	}
	eqs = append(eqs, m.start...)
	u := merge(z, nil, append(append([][]term{}, eqs...), boxes...))
	got := solve.ExposedLinear(u.n, u.conv(eqs), u.conv(boxes))
	var out []int
	for i, x := range got {
		if x {
			out = append(out, prot[i])
		}
	}
	return out
}

// SetViolations replays docs in order (ruling R41a-5, the design's whole-
// set rule): before each doc's knowledge is added, the shape of each of
// its sets is taken; at the end every set is checked.
func (m *Model) SetViolations(order []int) []string {
	at := make([]int, len(m.atoms))
	z := m.zeros(nil, nil)
	for i := range at {
		at[i] = 1 << 30
		if z[i] {
			at[i] = -1
		}
	}
	for i := range order {
		zi := m.zeros(order[:i+1], z)
		for a := range at {
			if zi[a] && at[a] > i {
				at[a] = i
			}
		}
		z = zi
	}
	var all [][]term
	pos := map[int]int{}
	for i, d := range order {
		pos[d] = i
	}
	for _, c := range m.Cells {
		if _, ok := pos[c.Doc]; ok {
			all = append(all, c.box)
		}
	}
	for _, s := range m.sets {
		if _, ok := pos[s.doc]; ok {
			for _, mb := range s.members {
				all = append(all, mb.box)
			}
		}
	}
	all = append(all, m.start...)
	u := merge(nil, at, all)
	known := solve.NewBasis()
	for _, e := range m.start {
		known.AddTerms(u.terms(e))
	}
	learnZeros := func(i int) {
		for v, t := range u.zeroAt {
			if t == i {
				known.Add([]int{v})
			}
		}
	}
	learnZeros(-1)
	type pending struct {
		s       pset
		members [][]int
		dim     int
		sum     bool
	}
	var ps []pending
	for i, d := range order {
		for _, s := range m.sets {
			if s.doc != d {
				continue
			}
			var hid [][]int
			var truth int64
			for _, mb := range s.members {
				if mb.hidden {
					hid = append(hid, u.ones(mb.box))
					truth += m.value(mb.box)
				}
			}
			// Hidden members that are all 0 protect nothing (a body hidden
			// whole whose zeros a parent shows).
			if len(hid) < 2 || truth == 0 {
				continue
			}
			dim, sum := known.Shape(hid)
			ps = append(ps, pending{s, hid, dim, sum})
		}
		for _, c := range m.Cells {
			if c.Doc == d && c.Shown {
				known.AddTerms(u.terms(c.box))
			}
		}
		learnZeros(i)
	}
	var out []string
	for _, p := range ps {
		dim, sum := known.Shape(p.members)
		if solve.Violates(p.dim, p.sum, dim, sum) {
			out = append(out, fmt.Sprintf("doc %d %s: %d hidden members, determined %d → %d, sum %v → %v",
				p.s.doc, p.s.name, len(p.members), p.dim, dim, p.sum, sum))
		}
	}
	return out
}
