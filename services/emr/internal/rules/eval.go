package rules

import (
	"bytes"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/codes"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/normalise"
)

// ErrEvaluate: the history can't be evaluated as given (a trigger not in
// it, a value that isn't a finite number). The consumer stops the
// hospital's evaluation rather than skip a record (design §15.1).
var ErrEvaluate = errors.New("rules: evaluation error")

// Patient is what population bands need of a patient.
type Patient struct {
	ID        uuid.UUID
	Sex       string // female, male, other, unknown
	BirthDate *time.Time
}

// Observation is a canonical observation as the rules see it. ValueSI is
// in the analyte's canonical unit (the normaliser's conversion); ValueNum
// and the reference range are in the source unit. A comparator value
// ("<0.5") has no ValueNum or ValueSI: its bound is BoundNum and, converted
// the same way, BoundSI.
type Observation struct {
	ID              uuid.UUID
	LOINC           string // empty when unmapped
	Category        string // laboratory, vital-signs
	Status          string
	ValueNum        *float64
	ValueSI         *float64
	Comparator      string
	BoundNum        *float64
	BoundSI         *float64
	ValueText       string
	RefLow, RefHigh *float64
	SourceFlag      string
	EffectiveAt     *time.Time
	EffectiveDate   *time.Time // a date-only source time: no hour is invented
	Standardised    bool
	Superseded      bool
}

// Medication is a canonical medication as the rules see it.
type Medication struct {
	ID             uuid.UUID
	ATC            string
	Status         string
	StartAt, EndAt *time.Time
}

// History is one patient's current canonical rows.
type History struct {
	Patient      Patient
	Observations []Observation
	Medications  []Medication
}

// Cause is a likely cause with the rows that support it (at most three;
// ids only, the app shows each row's label, value, flag and time).
type Cause struct {
	ID, Label, Confidence string
	Observations          []uuid.UUID
	Medications           []uuid.UUID
}

// Finding is one rule hit on one primary observation.
type Finding struct {
	RuleID, Kind, Category, Severity string
	Primary                          uuid.UUID
	// Observations and Medications are the other rows the rule used
	// (a delta's baseline, a trend's earlier values, a score's set, a
	// combination's medicine).
	Observations []uuid.UUID
	Medications  []uuid.UUID
	Causes       []Cause
	Preliminary  bool
	Standardised bool
	Score        *int
	Scale        int // NEWS2 SpO2 scale, 1 or 2; 0 otherwise
}

var rank = map[string]int{"info": 1, "urgent": 2, "critical": 3}

func stepUp(s string) string {
	switch s {
	case "info":
		return "urgent"
	case "urgent", "critical":
		return "critical"
	}
	return s
}

// evaluable statuses: only final, amended and corrected raise findings;
// preliminary runs critical thresholds and source flags (design §11.4).
var evaluable = []string{"preliminary", "final", "amended", "corrected"}
var settled = []string{"final", "amended", "corrected"}

func (o Observation) time() (time.Time, bool) {
	if o.EffectiveAt != nil {
		return *o.EffectiveAt, true
	}
	return time.Time{}, false
}

// at is the observation's time for medication and age checks: its time, or
// the start of its date.
func (o Observation) at() time.Time {
	if o.EffectiveAt != nil {
		return *o.EffectiveAt
	}
	if o.EffectiveDate != nil {
		return *o.EffectiveDate
	}
	return time.Time{}
}

func bad(v *float64) bool { return v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0)) }

func eps(x float64) float64 { return 1e-9 * math.Max(1, math.Abs(x)) }

// holds reports whether the observation's value certainly satisfies op x:
// a value, or a comparator's interval (">100" is above 100, "<0.5" below
// 0.5). A value it can't know (no value_si) never holds.
func (o Observation) holds(op string, x float64) bool {
	lo, hi := math.Inf(-1), math.Inf(1)
	loOpen, hiOpen := false, false
	switch {
	case o.ValueSI != nil:
		lo, hi = *o.ValueSI, *o.ValueSI
	case o.Comparator != "" && o.BoundSI != nil:
		b := *o.BoundSI
		switch o.Comparator {
		case ">":
			lo, loOpen = b, true
		case ">=":
			lo = b
		case "<":
			hi, hiOpen = b, true
		case "<=":
			hi = b
		default:
			return false
		}
	default:
		return false
	}
	e := eps(x)
	switch op {
	case ">=":
		return lo >= x-e
	case ">":
		return lo > x+e || (loOpen && lo >= x-e)
	case "<=":
		return hi <= x+e
	case "<":
		return hi < x-e || (hiOpen && hi <= x+e)
	}
	return false
}

// limit is a threshold's value for the patient: the value, or the band.
func limit(r Rule, p Patient, at time.Time) float64 {
	if r.Value != nil {
		return *r.Value
	}
	var unknown float64
	age := -1
	if p.BirthDate != nil && !at.IsZero() {
		b := *p.BirthDate
		age = at.Year() - b.Year()
		if at.YearDay() < b.YearDay() {
			age--
		}
	}
	for _, b := range r.Bands {
		if b.Sex == "unknown" {
			unknown = *b.Value
		}
	}
	if age < 0 || (p.Sex != "female" && p.Sex != "male") {
		return unknown
	}
	for _, b := range r.Bands {
		if (b.Sex == p.Sex || b.Sex == "any") && (b.MinAge == nil || age >= *b.MinAge) && (b.MaxAge == nil || age <= *b.MaxAge) {
			return *b.Value
		}
	}
	return unknown
}

// Evaluate runs the rule set over patient history h for the trigger
// observation (design §11.1). It is pure: the same input gives the same
// findings. Findings name the trigger as their primary observation, except
// NEWS2, whose primary is the newest observation of the set.
func Evaluate(rs *RuleSet, h History, trigger uuid.UUID, _ time.Time) ([]Finding, error) {
	var o Observation
	found := false
	for _, x := range h.Observations {
		if bad(x.ValueSI) || bad(x.ValueNum) || bad(x.BoundSI) || bad(x.BoundNum) || bad(x.RefLow) || bad(x.RefHigh) {
			return nil, ErrEvaluate
		}
		if x.ID == trigger {
			o, found = x, true
		}
	}
	if !found {
		return nil, ErrEvaluate
	}
	if o.Superseded || !slices.Contains(evaluable, o.Status) {
		return nil, nil
	}
	prelim := o.Status == "preliminary"
	analyte, mapped := Analyte{}, false
	if o.LOINC != "" {
		analyte, mapped = rs.AnalyteOf(o.LOINC)
	}
	var out []Finding
	mk := func(r Rule, sev, category string) Finding {
		return Finding{RuleID: r.ID, Kind: r.Kind, Category: category, Severity: sev, Primary: o.ID,
			Preliminary: prelim, Standardised: o.Standardised}
	}

	// Thresholds: the most severe hit per analyte (one finding a value).
	var best *Rule
	if mapped && !analyte.Text {
		for i, r := range rs.f.Rules {
			if r.Kind != "threshold" || r.Analyte != analyte.Name || (prelim && r.Severity != "critical") {
				continue
			}
			if o.holds(r.Op, limit(r, h.Patient, o.at())) && (best == nil || rank[r.Severity] > rank[best.Severity]) {
				best = &rs.f.Rules[i]
			}
		}
	}
	thresholdSev := ""
	if best != nil {
		thresholdSev = best.Severity
		out = append(out, mk(*best, best.Severity, analyte.Category))
	}

	// The source's own critical flag: also unmapped; not when MHAI's own
	// critical threshold already hit.
	flagged := false
	for _, r := range rs.f.Rules {
		if r.Kind != "source_flag" || o.SourceFlag == "" || !slices.Contains(r.Flags, o.SourceFlag) || thresholdSev == "critical" {
			continue
		}
		cat := r.UnmappedCategory[o.Category]
		if mapped {
			cat = analyte.Category
		}
		if cat == "" {
			continue
		}
		out = append(out, mk(r, r.Severity, cat))
		flagged = true
	}
	if prelim || !mapped || analyte.Text && !isNews2Param(rs, analyte.Name) {
		return withCauses(rs, h, o, out), nil
	}

	for _, r := range rs.f.Rules {
		switch r.Kind {
		case "reference_range":
			if thresholdSev != "" || flagged || !slices.Contains(r.Analytes, analyte.Name) || o.ValueNum == nil {
				continue
			}
			v := *o.ValueNum
			if (o.RefLow != nil && v < *o.RefLow-eps(*o.RefLow)) || (o.RefHigh != nil && v > *o.RefHigh+eps(*o.RefHigh)) {
				out = append(out, mk(r, r.Severity, analyte.Category))
			}
		case "delta":
			if r.Analyte != analyte.Name {
				continue
			}
			if f, ok := delta(rs, r, h, o); ok {
				f.Category = analyte.Category
				out = append(out, f)
			}
		case "trend":
			if r.Analyte != analyte.Name {
				continue
			}
			if f, ok := trend(rs, r, h, o); ok {
				f.Category = analyte.Category
				out = append(out, f)
			}
		case "combination":
			if r.Analyte != analyte.Name || !o.holds(r.Op, *r.Value) {
				continue
			}
			meds := activeMeds(h.Medications, r.WithATC, o.at())
			if len(meds) == 0 {
				continue
			}
			sev := r.Severity
			if thresholdSev != "" && rank[stepUp(thresholdSev)] > rank[sev] {
				sev = stepUp(thresholdSev)
			}
			if rank[sev] <= rank[thresholdSev] {
				continue // nothing to raise: the threshold already says as much
			}
			f := mk(r, sev, analyte.Category)
			f.Medications = meds
			out = append(out, f)
		case "score":
			if f, ok := news2(rs, r, h, o); ok {
				out = append(out, f)
			}
		}
	}
	return withCauses(rs, h, o, out), nil
}

// sameAnalyte reports whether x is a value of analyte a.
func sameAnalyte(a Analyte, x Observation) bool {
	return x.LOINC != "" && slices.Contains(a.LOINC, x.LOINC)
}

// history is the analyte's settled, timed, current values with a value_si,
// in [from, to] (to inclusive only when inclusive), oldest first.
func history(rs *RuleSet, name string, h History, from, to time.Time, inclusive bool) []Observation {
	a, _ := rs.Analyte(name)
	var out []Observation
	for _, x := range h.Observations {
		t, ok := x.time()
		if !ok || x.Superseded || !slices.Contains(settled, x.Status) || x.ValueSI == nil || !sameAnalyte(a, x) ||
			t.Before(from) || t.After(to) || (!inclusive && t.Equal(to)) {
			continue
		}
		out = append(out, x)
	}
	slices.SortStableFunc(out, func(p, q Observation) int {
		if c := p.EffectiveAt.Compare(*q.EffectiveAt); c != 0 {
			return c
		}
		return bytes.Compare(p.ID[:], q.ID[:])
	})
	return out
}

func delta(rs *RuleSet, r Rule, h History, o Observation) (Finding, bool) {
	t, ok := o.time()
	if !ok || o.ValueSI == nil {
		return Finding{}, false
	}
	v := *o.ValueSI
	base := history(rs, r.Analyte, h, t.Add(-time.Duration(r.WithinHours)*time.Hour), t, false)
	if len(base) == 0 {
		return Finding{}, false
	}
	pick := base[0]
	for _, b := range base[1:] {
		if (r.Fall != nil && *b.ValueSI > *pick.ValueSI) || (r.Fall == nil && *b.ValueSI < *pick.ValueSI) {
			pick = b
		}
	}
	p := *pick.ValueSI
	hit := false
	switch {
	case r.Rise != nil:
		hit = v-p >= *r.Rise-eps(*r.Rise)
	case r.Fall != nil:
		hit = p-v >= *r.Fall-eps(*r.Fall)
	case r.Ratio != nil:
		hit = p > 0 && v/p >= *r.Ratio-eps(*r.Ratio)
	}
	if !hit {
		return Finding{}, false
	}
	return Finding{RuleID: r.ID, Kind: r.Kind, Severity: r.Severity, Primary: o.ID, Observations: []uuid.UUID{pick.ID},
		Standardised: o.Standardised}, true
}

func trend(rs *RuleSet, r Rule, h History, o Observation) (Finding, bool) {
	t, ok := o.time()
	if !ok || o.ValueSI == nil || !slices.Contains(settled, o.Status) {
		return Finding{}, false
	}
	vals := history(rs, r.Analyte, h, t.Add(-time.Duration(r.WithinHours)*time.Hour), t, true)
	i := slices.IndexFunc(vals, func(x Observation) bool { return x.ID == o.ID })
	if i < 0 || i+1 < r.Count {
		return Finding{}, false
	}
	run := vals[i+1-r.Count : i+1]
	for k := 1; k < len(run); k++ {
		a, b := *run[k-1].ValueSI, *run[k].ValueSI
		if (r.Direction == "rising" && !(b > a+eps(a))) || (r.Direction == "falling" && !(b < a-eps(a))) {
			return Finding{}, false
		}
	}
	var ev []uuid.UUID
	for _, x := range run[:len(run)-1] {
		ev = append(ev, x.ID)
	}
	return Finding{RuleID: r.ID, Kind: r.Kind, Severity: r.Severity, Primary: o.ID, Observations: ev,
		Standardised: o.Standardised}, true
}

// activeMeds are the medications active at t whose ATC code starts with
// one of the classes.
func activeMeds(meds []Medication, classes []string, t time.Time) []uuid.UUID {
	var out []uuid.UUID
	for _, m := range meds {
		if m.Status != "active" || m.ATC == "" || (m.StartAt != nil && m.StartAt.After(t)) || (m.EndAt != nil && !m.EndAt.After(t)) {
			continue
		}
		for _, c := range classes {
			if strings.HasPrefix(m.ATC, c) {
				out = append(out, m.ID)
				break
			}
		}
	}
	return out
}

// withCauses adds each finding's likely causes: those with evidence in the
// patient's own rows, at most three evidence items each, in the rule's
// order (strongest first).
func withCauses(rs *RuleSet, h History, o Observation, fs []Finding) []Finding {
	for i := range fs {
		var r Rule
		for _, x := range rs.f.Rules {
			if x.ID == fs[i].RuleID {
				r = x
			}
		}
		for _, c := range r.Causes {
			cause := Cause{ID: c.ID, Label: c.Label, Confidence: c.Confidence}
			n := 0
			for _, e := range c.Evidence {
				if len(e.MedicationATC) > 0 {
					for _, id := range activeMeds(h.Medications, e.MedicationATC, o.at()) {
						if n < 3 {
							cause.Medications = append(cause.Medications, id)
							n++
						}
					}
					continue
				}
				t := o.at()
				for _, x := range history(rs, e.Analyte, h, t.Add(-time.Duration(e.WithinHours)*time.Hour), t, true) {
					if x.ID != o.ID && x.holds(e.Op, *e.Value) && n < 3 {
						cause.Observations = append(cause.Observations, x.ID)
						n++
					}
				}
			}
			if n > 0 {
				fs[i].Causes = append(fs[i].Causes, cause)
			}
		}
	}
	return fs
}

// Bound reads a comparator value's bound ("<0.5", in the connection's
// decimal separator) and converts it to the analyte's canonical unit with
// the normaliser's reviewed conversions. Either may be nil: a bound it
// can't read or convert never reaches a numeric rule.
func Bound(m *codes.Maps, loinc, unitUCUM, valueText string, sep rune) (num, si *float64) {
	v, cmp, err := normalise.ParseDecimal(valueText, sep)
	if err != nil || cmp == "" {
		return nil, nil
	}
	num = &v
	if m != nil && loinc != "" && unitUCUM != "" {
		if c, ok := m.Convert(loinc, v, unitUCUM); ok {
			si = &c
		}
	}
	return num, si
}
