package disclose

import (
	"sort"
	"strings"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// The layout is every number of a context, as cells, and the equations
// between them that the definitions make public. One walker serves the raw
// values and the guarded body, so a body read back from storage lays out
// exactly as it was computed.
//
// Each cell has a group: what it counts, written canonically, so that the
// same group reached through two views, two filter sets or two levels has
// one key (D5's "one state per group"). A group is a measure over a period
// with restrictions:
//
//   - pat: distinct patients with every restriction (attributes, and the
//     conditions they had in the period);
//   - the additive measures, summed over the period's blocks (D2): case,
//     new and exist of a condition (or "*", every condition), consult,
//     corr, res and alert with their categories. Under a condition filter
//     a block counts for a patient who had the condition in that block.
//
// At a week, a condition's cases are its patients (one case per patient and
// condition in a block). In a first period (Context.First) nothing came
// before: a condition's new cases are its patients, and at a week nothing
// is existing.

// The measures.
const (
	mPat     = "pat"
	mCase    = "case"
	mNew     = "new"
	mExist   = "exist"
	mConsult = "consult"
	mCorr    = "corr"
	mRes     = "res"
	mAlert   = "alert"
)

// zeroGroup is a group that is 0 by definition (contradictory restrictions).
const zeroGroup = "0"

// nullKey is a null row's value as a restriction.
const nullKey = "∅"

// desc is a group before its period is applied.
type desc struct {
	m     string
	attrs [nDims]string // DimCondition unused; "" no restriction
	conds []string      // conditions the patient had (sorted, unique)
	not   string        // a condition the patient didn't have (pairs)
	d     string        // the measure's own condition ("*" every one)
	x     string        // the measure's category
	bad   bool          // contradictory restrictions
}

func baseDesc(f Filters) desc {
	var b desc
	for i, v := range f {
		if i == DimCondition {
			if v != "" {
				b.conds = []string{v}
			}
			continue
		}
		b.attrs[i] = v
	}
	return b
}

func (b desc) clone() desc {
	b.conds = append([]string{}, b.conds...)
	return b
}

// with restricts to dim = v ("" key: the null row).
func (b desc) with(dim int, v string) desc {
	b = b.clone()
	if v == "" {
		v = nullKey
	}
	if b.attrs[dim] != "" && b.attrs[dim] != v {
		b.bad = true
	}
	b.attrs[dim] = v
	return b
}

func (b desc) has(cs ...string) desc {
	b = b.clone()
	for _, c := range cs {
		i := sort.SearchStrings(b.conds, c)
		if i < len(b.conds) && b.conds[i] == c {
			continue
		}
		b.conds = append(b.conds, "")
		copy(b.conds[i+1:], b.conds[i:])
		b.conds[i] = c
	}
	if b.not != "" && contains(b.conds, b.not) {
		b.bad = true
	}
	return b
}

func (b desc) measure(m, d, x string) desc {
	b = b.clone()
	b.m, b.d, b.x = m, d, x
	return b
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func additive(m string) bool { return m != mPat && m != "" }

// group is d's key over period p (first: nothing before p).
func group(d desc, p calendar.Period, first bool) string {
	if d.bad {
		return zeroGroup
	}
	d = d.clone()
	if additive(d.m) && d.d != "*" && d.d != "" {
		// A measure of condition c implies the patient had c in the block.
		var keep []string
		for _, c := range d.conds {
			if c != d.d {
				keep = append(keep, c)
			}
		}
		d.conds = keep
	}
	week := p.Kind == calendar.Week
	switch {
	case d.m == mCase && d.d != "*" && week,
		d.m == mNew && d.d != "*" && first && (week || len(d.conds) == 0):
		d = d.has(d.d)
		d.m, d.d, d.x = mPat, "", ""
	case d.m == mExist && first && week:
		return zeroGroup
	case d.m == mNew && d.d == "*" && first && week:
		d.m = mCase
	}
	var s strings.Builder
	s.WriteString(d.m)
	s.WriteString("|" + string(p.Kind) + ":" + p.From.Format(time.DateOnly))
	for i, v := range d.attrs {
		if v != "" {
			s.WriteString("|" + DimNames[i] + "=" + v)
		}
	}
	if len(d.conds) > 0 {
		s.WriteString("|has=" + strings.Join(d.conds, ","))
	}
	if d.not != "" {
		s.WriteString("|not=" + d.not)
	}
	if d.d != "" {
		s.WriteString("|of=" + d.d)
	}
	if d.x != "" {
		s.WriteString("|" + d.x)
	}
	return s.String()
}

// cellInfo is one number of a context.
type cellInfo struct {
	path    string
	d       desc
	period  calendar.Period
	first   bool
	group   string
	virtual bool // never sent: a set's member the definitions imply
}

// rel is an equation: total = Σ parts. checked marks the sets the
// contract's reader checks (guards.ts): a hidden total hides its non-zero
// members (A2).
type rel struct {
	total   int
	parts   []int
	checked bool
}

// table is a two-way table: rows [cases, new, existing] against the all
// row (smallgroup's rule 4).
type table struct {
	all  [3]int
	rows [][3]int
}

type shareRef struct{ num, den int }

// medRef is a median's counts: hidden when any is hidden.
type medRef struct{ counts []int }

// sheet is a context's layout.
type sheet struct {
	ctx    Context
	cells  []cellInfo
	rels   []rel
	tables []table
	shares []shareRef
	meds   []medRef
	pat    int
	keys   bool // every list's keys are distinct
	// parts are the row keys of each partition (by dimension), for the
	// partition equations between contexts.
	parts map[int][]string
	// locSetting is each listed location's setting ("" none).
	locSetting map[string]string
	// pairsFull: the pairs list reached PairsMax, so a pair it leaves out
	// isn't known to be 0.
	pairsFull bool
}

type layer[C, S, M any] struct {
	sh  *sheet
	cs  []*C
	ss  []*S
	ms  []*M
	ctx Context
}

func (l *layer[C, S, M]) cell(path string, d desc, p calendar.Period, first bool, c *C) int {
	id := len(l.sh.cells)
	l.sh.cells = append(l.sh.cells, cellInfo{path: path, d: d, period: p, first: first, group: group(d, p, first), virtual: c == nil})
	l.cs = append(l.cs, c)
	return id
}

func (l *layer[C, S, M]) here(path string, d desc, c *C) int {
	return l.cell(path, d, l.ctx.Period, l.ctx.First, c)
}

// rel records total = Σ parts. A set with no listed part is no equation:
// in a body hidden whole the lists are empty, not zero.
func (l *layer[C, S, M]) rel(checked bool, total int, parts ...int) {
	if len(parts) == 0 {
		return
	}
	l.sh.rels = append(l.sh.rels, rel{total, parts, checked})
}

// columns adds a table's column sums (all = Σ rows) as equations.
func (l *layer[C, S, M]) columns(t table) {
	if len(t.rows) == 0 {
		return
	}
	for j := 0; j < 3; j++ {
		var ps []int
		for _, r := range t.rows {
			ps = append(ps, r[j])
		}
		l.rel(false, t.all[j], ps...)
	}
}

// periodsOfSeries is a period's series points: its children, or a week
// itself.
func periodsOfSeries(p calendar.Period) []calendar.Period {
	if p.Kind == calendar.Week {
		return []calendar.Period{p}
	}
	return calendar.Children(p)
}

// layout lays out r. Lists are walked in their stored order.
func layout[C, S, M any](r *Report[C, S, M]) (*sheet, []*C, []*S, []*M) {
	l := &layer[C, S, M]{sh: &sheet{ctx: r.Context, keys: true}, ctx: r.Context}
	base := baseDesc(r.Context.Filters)
	seen := map[string]bool{}
	key := func(list string, k ...string) {
		s := list + "\x00" + strings.Join(k, "\x00")
		if seen[s] {
			l.sh.keys = false
		}
		seen[s] = true
	}
	pat := base.measure(mPat, "", "")
	l.sh.pat = l.here("patients", pat, &r.Patients)

	// Prevalence: the total, by condition (a table), the series (a table).
	cne := func(path, d string, v *CNE[C], p calendar.Period, first bool) [3]int {
		c := l.cell(path+".cases", base.measure(mCase, d, ""), p, first, &v.Cases)
		n := l.cell(path+".new", base.measure(mNew, d, ""), p, first, &v.New)
		e := l.cell(path+".existing", base.measure(mExist, d, ""), p, first, &v.Existing)
		l.rel(true, c, n, e)
		return [3]int{c, n, e}
	}
	total := cne("total", "*", &r.Total, r.Context.Period, r.Context.First)
	t1 := table{all: total}
	for i := range r.ByCondition {
		row := &r.ByCondition[i]
		key("by_condition", row.Condition)
		t1.rows = append(t1.rows, cne("by_condition."+row.Condition, row.Condition, &row.CNE, r.Context.Period, r.Context.First))
	}
	l.sh.tables = append(l.sh.tables, t1)
	l.columns(t1)
	t2 := table{all: total}
	kids := periodsOfSeries(r.Context.Period)
	for i := range r.Series {
		pt := &r.Series[i]
		key("series", pt.Start)
		p := calendar.Period{}
		if i < len(kids) && kids[i].From.Format(time.DateOnly) == pt.Start {
			p = kids[i]
		} else {
			l.sh.keys = false // a series point that isn't the period's child
		}
		first := r.Context.First && p.From.Equal(r.Context.Period.From)
		path := "series." + pt.Start
		c := l.cell(path+".cases", base.measure(mCase, "*", ""), p, first, &pt.Cases)
		n := l.cell(path+".new", base.measure(mNew, "*", ""), p, first, &pt.New)
		e := l.cell(path+".existing", base.measure(mExist, "*", ""), p, first, nil)
		l.rel(true, c, n, e)
		t2.rows = append(t2.rows, [3]int{c, n, e})
	}
	if len(r.Series) > 0 && len(r.Series) != len(kids) {
		l.sh.keys = false
	}
	l.sh.tables = append(l.sh.tables, t2)
	l.columns(t2)

	// Affected: five partitions of the patients; locations nest in settings.
	l.sh.parts = map[int][]string{}
	l.sh.locSetting = map[string]string{}
	for _, row := range r.ByLocation {
		l.sh.locSetting[row.Key] = row.Setting
	}
	l.sh.pairsFull = len(r.Pairs) >= PairsMax
	parts := func(name string, dim int, rows []Part[C]) []int {
		var ps []int
		for i := range rows {
			l.sh.parts[dim] = append(l.sh.parts[dim], rows[i].Key)
			key(name, rows[i].Key)
			ps = append(ps, l.here(name+"."+rows[i].Key, pat.with(dim, rows[i].Key), &rows[i].Patients))
		}
		l.rel(true, l.sh.pat, ps...)
		return ps
	}
	parts("by_age", DimAgeBand, r.ByAge)
	parts("by_sex", DimSex, r.BySex)
	setIDs := parts("by_setting", DimSetting, r.BySetting)
	locIDs := parts("by_location", DimLocation, r.ByLocation)
	parts("by_specialty", DimSpecialty, r.BySpecialty)
	for _, s := range Settings {
		var in []int
		for i := range r.ByLocation {
			if r.ByLocation[i].Setting == s && r.ByLocation[i].Key != "" {
				in = append(in, locIDs[i])
			}
		}
		if len(in) == 0 {
			continue
		}
		id := -1
		for i := range r.BySetting {
			if r.BySetting[i].Key == s {
				id = setIDs[i]
			}
		}
		if id < 0 {
			l.sh.keys = false // locations of a setting the partition doesn't list
			continue
		}
		d := pat.with(DimSetting, s).measure(mPat, "", "~located")
		rest := l.here("by_setting."+s+".rest", d, nil)
		l.rel(false, id, append(in, rest)...)
	}

	// Co-occurring pairs: A's patients = both + A without B.
	for i := range r.Pairs {
		pr := &r.Pairs[i]
		lo, hi := pr.A, pr.B
		if hi < lo {
			lo, hi = hi, lo
		}
		key("pairs", lo, hi)
		if pr.A == pr.B {
			l.sh.keys = false
		}
		path := "pairs." + pr.A + "." + pr.B
		a := l.here(path+".a_patients", pat.has(pr.A), &pr.APatients)
		both := l.here(path+".patients", pat.has(pr.A, pr.B), &pr.Patients)
		nb := pat.has(pr.A)
		nb.not = pr.B
		rest := l.here(path+".a_only", nb, nil)
		l.rel(true, a, both, rest)
		l.sh.shares = append(l.sh.shares, shareRef{both, a})
		l.ss = append(l.ss, &pr.ShareOfA)
	}

	// Findings: per finding, patients and results; the alert block.
	for i := range r.Findings {
		f := &r.Findings[i]
		key("findings", f.Name, f.Flag)
		x := "finding=" + orNull(f.Name) + "/" + orNull(f.Flag)
		path := "findings." + f.Name + "." + f.Flag
		l.here(path+".patients", pat.measure(mPat, "", x), &f.Patients)
		l.here(path+".results", base.measure(mRes, "", x), &f.Results)
	}
	a := &r.Alerts
	al := func(path, x string, c *C) int { return l.here("alerts."+path, base.measure(mAlert, "", x), c) }
	at := al("total", "", &a.Total)
	ac := al("critical", "severity=critical", &a.Critical)
	au := al("urgent", "severity=urgent", &a.Urgent)
	ai := al("info", "severity=info", &a.Info)
	l.rel(true, at, ac, au, ai)
	ad := al("dismissed", "dismissed", &a.Dismissed)
	ak := al("kept", "kept", nil)
	l.rel(true, at, ad, ak)
	var rs []int
	for i := range a.DismissReasons {
		key("dismiss_reasons", a.DismissReasons[i].Reason)
		rs = append(rs, al("reason."+a.DismissReasons[i].Reason, "reason="+a.DismissReasons[i].Reason, &a.DismissReasons[i].Count))
	}
	l.rel(true, ad, rs...)
	l.sh.meds = append(l.sh.meds, medRef{[]int{at}}, medRef{[]int{at, ac}})
	l.ms = append(l.ms, &a.MedianAck, &a.CriticalMedianAck)

	// Quality: per condition, consultations and their sets.
	for i := range r.Quality {
		q := &r.Quality[i]
		key("quality", q.Condition)
		c := q.Condition
		qc := func(path, m, x string, v *C) int {
			return l.here("quality."+c+"."+path, base.measure(m, c, x), v)
		}
		n := qc("consultations", mConsult, "", &q.Consultations)
		var lv []int
		for j, v := range []*C{&q.High, &q.Caution, &q.Review, &q.Failed} {
			lv = append(lv, qc(Confidences[j], mConsult, "confidence="+Confidences[j], v))
		}
		l.rel(true, n, lv...)
		rated := qc("rated", mConsult, "rated", &q.Rated)
		l.rel(true, n, rated, qc("unrated", mConsult, "unrated", nil))
		agreed := qc("agreed", mConsult, "agreed", &q.Agreed)
		l.rel(true, rated, agreed, qc("not_agreed", mConsult, "not_agreed", nil))
		l.sh.shares = append(l.sh.shares, shareRef{agreed, rated})
		l.ss = append(l.ss, &q.AgreementRate)
		ct := qc("corrections", mCorr, "", &q.Corrections.Total)
		var st []int
		for j, v := range []*C{&q.Corrections.Approved, &q.Corrections.Rejected, &q.Corrections.Waiting} {
			name := []string{"approved", "rejected", "waiting"}[j]
			st = append(st, qc("corrections."+name, mCorr, "status="+name, v))
		}
		l.rel(true, ct, st...)
	}
	return l.sh, l.cs, l.ss, l.ms
}

func orNull(s string) string {
	if s == "" {
		return nullKey
	}
	return s
}
