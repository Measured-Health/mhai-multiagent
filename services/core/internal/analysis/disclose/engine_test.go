package disclose

import (
	"math/bits"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose/attack"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// rawOf is the tests' reference engine: one context's raw values counted
// on a synthetic world, by the design's definitions (§4, §5; layout.go).
// PR 4's definer functions compute the same in SQL. The attacker
// (disclose/attack) counts the same definitions its own way, and the
// property tests check every shown value against it.
func rawOf(w *attack.World, p calendar.Period, f Filters, first bool) Raw {
	d := w.Dims
	var r Raw
	r.Context = Context{Period: p, Filters: f, First: first}
	// The hospital's sources: every subject has one (the definer function
	// lists the period's, null included when a subject has none).
	r.Sources = append([]string{}, d.Sources...)
	in := func(per calendar.Period) []int {
		var out []int
		for i, b := range w.Blocks {
			if !b.From.Before(per.From) && !b.To.After(per.To) {
				out = append(out, i)
			}
		}
		return out
	}
	cf := -1
	for i, c := range d.Conditions {
		if f[DimCondition] == c {
			cf = i
		}
	}
	attrs := func(s *attack.Subject) [nDims]string {
		var a [nDims]string
		a[DimAgeBand], a[DimSex] = d.Ages[s.Age], d.Sexes[s.Sex]
		a[DimLocation], a[DimSetting] = d.Locations[s.Loc].ID, d.Locations[s.Loc].Setting
		a[DimSpecialty], a[DimSource] = d.Specialties[s.Spec], d.Sources[s.Source]
		return a
	}
	match := func(s *attack.Subject) bool {
		a := attrs(s)
		for i, v := range f {
			if i != DimCondition && v != "" && a[i] != v {
				return false
			}
		}
		return true
	}
	unit := func(s *attack.Subject, b int) *attack.Block {
		blk, ok := s.Blocks[b]
		if !ok || !match(s) || (cf >= 0 && blk.Conds&(1<<cf) == 0) {
			return nil
		}
		return blk
	}
	// Distinct patients: those with facts in p, filters met.
	type dist struct {
		s    *attack.Subject
		S, F uint32
	}
	var ds []dist
	for i := range w.Subjects {
		s := &w.Subjects[i]
		var S, F uint32
		has := false
		for _, b := range in(p) {
			if blk, ok := s.Blocks[b]; ok {
				has, S, F = true, S|blk.Conds, F|blk.Finds
			}
		}
		if has && match(s) && (cf < 0 || S&(1<<cf) != 0) {
			ds = append(ds, dist{s, S, F})
		}
	}
	r.Patients = int64(len(ds))

	cases := func(per calendar.Period, c int) (n, nw int64) {
		for i := range w.Subjects {
			for _, b := range in(per) {
				if blk := unit(&w.Subjects[i], b); blk != nil {
					if c < 0 {
						n += int64(bits.OnesCount32(blk.Conds))
						nw += int64(bits.OnesCount32(blk.New))
					} else {
						n += int64(blk.Conds >> c & 1)
						nw += int64(blk.New >> c & 1)
					}
				}
			}
		}
		return n, nw
	}
	n, nw := cases(p, -1)
	r.Total = CNE[int64]{n, nw, n - nw}
	for c, id := range d.Conditions {
		if n, nw := cases(p, c); n > 0 {
			r.ByCondition = append(r.ByCondition, ConditionRow[int64]{id, CNE[int64]{n, nw, n - nw}})
		}
	}
	for _, k := range periodsOfSeries(p) {
		n, nw := cases(k, -1)
		r.Series = append(r.Series, Point[int64]{k.From.Format(time.DateOnly), n, nw})
	}

	part := func(dim int) []Part[int64] {
		count := map[string]int64{}
		setting := map[string]string{}
		var keys []string
		for _, x := range ds {
			k := attrs(x.s)[dim]
			if _, ok := count[k]; !ok {
				keys = append(keys, k)
			}
			count[k]++
			if dim == DimLocation {
				setting[k] = attrs(x.s)[DimSetting]
			}
		}
		var out []Part[int64]
		for _, k := range keys {
			out = append(out, Part[int64]{Key: k, Setting: setting[k], Patients: count[k]})
		}
		return out
	}
	r.ByAge, r.BySex, r.BySetting, r.ByLocation, r.BySpecialty = part(DimAgeBand), part(DimSex), part(DimSetting), part(DimLocation), part(DimSpecialty)

	for a := range d.Conditions {
		for b := a + 1; b < len(d.Conditions); b++ {
			var na, nb int64
			for _, x := range ds {
				if x.S&(1<<a) != 0 {
					na++
					if x.S&(1<<b) != 0 {
						nb++
					}
				}
			}
			if nb > 0 {
				r.Pairs = append(r.Pairs, Pair[int64, struct{}]{A: d.Conditions[a], B: d.Conditions[b], APatients: na, Patients: nb})
			}
		}
	}

	for fi, fd := range d.Findings {
		var pts, res int64
		for _, x := range ds {
			if x.F&(1<<fi) != 0 {
				pts++
			}
		}
		for i := range w.Subjects {
			for _, b := range in(p) {
				if blk := unit(&w.Subjects[i], b); blk != nil {
					res += int64(blk.Results[fi])
				}
			}
		}
		if pts > 0 {
			r.Findings = append(r.Findings, FindingRow[int64]{Name: fd.Name, Flag: fd.Flag, Patients: pts, Results: res})
		}
	}

	a := &r.Alerts
	reasons := make([]int64, len(DismissReasons)+1)
	quality := make([]QualityRow[int64, struct{}], len(d.Conditions))
	for i := range w.Subjects {
		for _, b := range in(p) {
			blk := unit(&w.Subjects[i], b)
			if blk == nil {
				continue
			}
			for _, al := range blk.Alerts {
				a.Total++
				*[]*int64{&a.Critical, &a.Urgent, &a.Info}[al.Severity]++
				if al.Dismiss != 0 {
					a.Dismissed++
					reasons[al.Dismiss]++
				}
				if al.Ack >= 0 {
					a.MedianAck = append(a.MedianAck, int64(al.Ack))
					if al.Severity == 0 {
						a.CriticalMedianAck = append(a.CriticalMedianAck, int64(al.Ack))
					}
				}
			}
			for _, c := range blk.Consults {
				q := &quality[c.Cond]
				q.Consultations++
				*[]*int64{&q.High, &q.Caution, &q.Review, &q.Failed}[c.Confidence]++
				if c.Rating != 0 {
					q.Rated++
					if c.Rating == 1 {
						q.Agreed++
					}
				}
				if c.Correction != 0 {
					q.Corrections.Total++
					*[]*int64{nil, &q.Corrections.Approved, &q.Corrections.Rejected, &q.Corrections.Waiting}[c.Correction]++
				}
			}
		}
	}
	for i, n := range reasons[1:] {
		if n > 0 {
			a.DismissReasons = append(a.DismissReasons, ReasonRow[int64]{DismissReasons[i], n})
		}
	}
	for c, q := range quality {
		if q.Consultations > 0 {
			q.Condition = d.Conditions[c]
			r.Quality = append(r.Quality, q)
		}
	}
	return r
}

// docOf is a body as the attacker reads it: each view's JSON.
func docOf(b Body) (attack.Doc, error) {
	doc := attack.Doc{Period: b.Context.Period, First: b.Context.First, Filters: map[string]string{}, Views: map[string][]byte{}}
	for i, v := range b.Context.Filters {
		if v != "" {
			doc.Filters[DimNames[i]] = v
		}
	}
	for _, v := range Views {
		j, err := ViewJSON(b, v)
		if err != nil {
			return doc, err
		}
		doc.Views[string(v)] = j
	}
	return doc, nil
}

// The tests' small world: few values per dimension, so the attacker's
// universe stays small enough to solve exactly.
func smallDims(conditions int) attack.Dims {
	cs := []string{"c1", "c2", "c3", "c4"}[:conditions]
	return attack.Dims{
		Conditions:  cs,
		Ages:        []string{"18-39", "65-79", ""},
		Sexes:       []string{"female", "male"},
		Locations:   []attack.Location{{ID: "L1", Setting: "admitted"}, {ID: "L2", Setting: "outpatient"}},
		Specialties: []string{"S1"},
		Sources:     []string{"X1"},
		Findings:    []attack.Finding{{Name: "Hb", Flag: "low"}},
	}
}
