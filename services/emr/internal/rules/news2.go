package rules

import (
	"bytes"
	"slices"
	"time"

	"github.com/google/uuid"
)

// NEWS2 (Royal College of Physicians, National Early Warning Score 2,
// 2017, chart 1). The scoring bands are the published chart, fixed here;
// the rule set chooses which analytes carry each parameter and what a total
// means. A score is computed only from one set of vitals taken within one
// hour with every parameter present, consciousness and air or oxygen
// included; a missing or unreadable parameter means no score, never
// "normal" (design §11.1). SpO2 Scale 2 is used only when the source
// records it for the patient.

func isNews2Param(rs *RuleSet, name string) bool {
	for _, r := range rs.f.Rules {
		if r.Kind != "score" {
			continue
		}
		p := r.Params
		if slices.Contains([]string{p.RespiratoryRate, p.SpO2, p.AirOrOxygen, p.SystolicBP, p.HeartRate, p.Consciousness,
			p.Temperature}, name) {
			return true
		}
	}
	return false
}

func band(v float64, edges []float64, scores []int) int {
	for i, e := range edges {
		if v <= e+eps(e) {
			return scores[i]
		}
	}
	return scores[len(scores)-1]
}

func respScore(v float64) int  { return band(v, []float64{8, 11, 20, 24}, []int{3, 1, 0, 2, 3}) }
func spo2Scale1(v float64) int { return band(v, []float64{91, 93, 95}, []int{3, 2, 1, 0}) }
func sbpScore(v float64) int   { return band(v, []float64{90, 100, 110, 219}, []int{3, 2, 1, 0, 3}) }
func pulseScore(v float64) int {
	return band(v, []float64{40, 50, 90, 110, 130}, []int{3, 1, 0, 1, 2, 3})
}
func tempScore(v float64) int {
	return band(v, []float64{35.0, 36.0, 38.0, 39.0}, []int{3, 1, 0, 1, 2})
}

func spo2Scale2(v float64, oxygen bool) int {
	if v <= 92+eps(92) || !oxygen {
		return band(v, []float64{83, 85, 87}, []int{3, 2, 1, 0})
	}
	return band(v, []float64{94, 96}, []int{1, 2, 3})
}

func news2(rs *RuleSet, r Rule, h History, o Observation) (Finding, bool) {
	p := r.Params
	params := []string{p.RespiratoryRate, p.SpO2, p.AirOrOxygen, p.SystolicBP, p.HeartRate, p.Consciousness, p.Temperature}
	trig, _ := rs.AnalyteOf(o.LOINC)
	t, ok := o.time()
	if !ok || !slices.Contains(params, trig.Name) || !slices.Contains(settled, o.Status) {
		return Finding{}, false
	}
	usable := func(name string, x Observation) bool {
		a, _ := rs.Analyte(name)
		_, timed := x.time()
		return timed && !x.Superseded && slices.Contains(settled, x.Status) && sameAnalyte(a, x) &&
			((a.Text && x.ValueText != "" && x.Comparator == "") || (!a.Text && x.ValueSI != nil))
	}
	// The set's time: the newest parameter within an hour of the trigger,
	// so every observation of one set names the same set (and primary).
	anchor := t
	for _, x := range h.Observations {
		for _, name := range params {
			if usable(name, x) && x.EffectiveAt.After(anchor) && x.EffectiveAt.Sub(t) <= time.Hour {
				anchor = *x.EffectiveAt
			}
		}
	}
	set := map[string]Observation{}
	for _, name := range params {
		for _, x := range h.Observations {
			if !usable(name, x) || x.EffectiveAt.After(anchor) || anchor.Sub(*x.EffectiveAt) > time.Hour {
				continue
			}
			if cur, ok := set[name]; !ok || x.EffectiveAt.After(*cur.EffectiveAt) ||
				(x.EffectiveAt.Equal(*cur.EffectiveAt) && bytes.Compare(x.ID[:], cur.ID[:]) > 0) {
				set[name] = x
			}
		}
		if _, ok := set[name]; !ok {
			return Finding{}, false
		}
	}
	// Scale 2 only when recorded: the newest scale record at or before the
	// set; none is Scale 1; an unreadable one is no score.
	scale := 1
	var scaleObs *Observation
	sa, _ := rs.Analyte(p.SpO2Scale)
	for i, x := range h.Observations {
		if _, timed := x.time(); timed && !x.Superseded && slices.Contains(settled, x.Status) && sameAnalyte(sa, x) &&
			!x.EffectiveAt.After(anchor) && (scaleObs == nil || x.EffectiveAt.After(*scaleObs.EffectiveAt)) {
			scaleObs = &h.Observations[i]
		}
	}
	if scaleObs != nil {
		switch scaleObs.ValueText {
		case "1":
		case "2":
			scale = 2
		default:
			return Finding{}, false
		}
	}
	var oxygen bool
	switch set[p.AirOrOxygen].ValueText {
	case "air":
	case "oxygen":
		oxygen = true
	default:
		return Finding{}, false
	}
	var conscious int
	switch set[p.Consciousness].ValueText {
	case "A":
	case "C", "V", "P", "U":
		conscious = 3
	default:
		return Finding{}, false
	}
	spo2 := *set[p.SpO2].ValueSI
	scores := []int{respScore(*set[p.RespiratoryRate].ValueSI), sbpScore(*set[p.SystolicBP].ValueSI),
		pulseScore(*set[p.HeartRate].ValueSI), tempScore(*set[p.Temperature].ValueSI), conscious}
	if scale == 2 {
		scores = append(scores, spo2Scale2(spo2, oxygen))
	} else {
		scores = append(scores, spo2Scale1(spo2))
	}
	if oxygen {
		scores = append(scores, 2)
	}
	total, three := 0, false
	for _, s := range scores {
		total += s
		three = three || s == 3
	}
	sev := ""
	switch {
	case total >= r.TotalCritical:
		sev = "critical"
	case total >= r.TotalUrgent:
		sev = "urgent"
	case three:
		sev = r.SingleThree
	}
	if sev == "" {
		return Finding{}, false
	}
	// The primary is the newest observation of the set.
	var primary Observation
	first := true
	for _, x := range set {
		if first || x.EffectiveAt.After(*primary.EffectiveAt) ||
			(x.EffectiveAt.Equal(*primary.EffectiveAt) && bytes.Compare(x.ID[:], primary.ID[:]) > 0) {
			primary, first = x, false
		}
	}
	var ev []uuid.UUID
	for _, name := range params {
		if set[name].ID != primary.ID {
			ev = append(ev, set[name].ID)
		}
	}
	if scaleObs != nil {
		ev = append(ev, scaleObs.ID)
	}
	return Finding{RuleID: r.ID, Kind: r.Kind, Category: "vital_signs", Severity: sev, Primary: primary.ID, Observations: ev,
		Standardised: true, Score: &total, Scale: scale}, true
}
