// Package synth makes seeded synthetic Analysis cohorts: for the demo
// hospital (core analysis seed-demo) and for the disclosure tests. Nothing
// here is, or derives from, a real patient.
package synth

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
)

// Options shape a cohort: Subjects people with facts on days in
// [Start, Start + Months). SmallBias (0 to 1) is how strongly facts favour
// the common conditions, which leaves the rare ones with small groups and
// months with none.
type Options struct {
	Subjects  int
	Months    int
	SmallBias float64
	Start     time.Time
}

// Condition, Location and Source are the demo hospital's dictionaries. A
// condition with no ICD-10 code (and no name) is "Not standardised".
type Condition struct {
	ID     uuid.UUID  `json:"id"`
	ICD10  *string    `json:"icd10"`
	Name   *string    `json:"name"`
	Source *uuid.UUID `json:"source,omitempty"`
}

type Location struct {
	ID      uuid.UUID `json:"id"`
	Label   string    `json:"label"`
	Setting string    `json:"setting"`
}

type Source struct {
	ID    uuid.UUID `json:"id"`
	Label string    `json:"label"`
}

// Subject is one synthetic person. Ref is never sent to the database: the
// seed draws a random matching key for each subject.
type Subject struct {
	Ref   string          `json:"-"`
	Facts []analysis.Fact `json:"facts"`
}

// Set is a cohort with its dictionaries.
type Set struct {
	Conditions []Condition `json:"conditions"`
	Locations  []Location  `json:"locations"`
	Sources    []Source    `json:"sources"`
	Subjects   []Subject   `json:"subjects"`
}

// SeedJSON is the body mhai_analysis_seed takes.
func (s Set) SeedJSON() ([]byte, error) { return json.Marshal(s) }

// The demo dictionary: common public ICD-10 categories with plain names.
var demoConditions = []struct{ code, name string }{
	{"I10", "Essential hypertension"},
	{"E11", "Type 2 diabetes mellitus"},
	{"J45", "Asthma"},
	{"B54", "Malaria, unspecified"},
	{"A09", "Infectious gastroenteritis and colitis"},
	{"J18", "Pneumonia, unspecified organism"},
	{"N18", "Chronic kidney disease"},
	{"D50", "Iron deficiency anaemia"},
	{"O24", "Diabetes in pregnancy"},
	{"G40", "Epilepsy"},
}

var (
	ageBands = []string{"0-17", "18-39", "40-64", "65-79", "80+"}
	sexes    = []string{"female", "male", "other", "not_recorded"}
	kinds    = []string{"consultation", "diagnosis", "finding", "alert"}
	settings = []string{"admitted", "outpatient", "emergency"}
)

func ptr[T any](v T) *T { return &v }

func pick[T any](r *rand.Rand, xs []T) T { return xs[r.IntN(len(xs))] }

func id(r *rand.Rand) uuid.UUID {
	var u uuid.UUID
	for i := range u {
		u[i] = byte(r.UintN(256))
	}
	u[6] = u[6]&0x0f | 0x40 // version 4
	u[8] = u[8]&0x3f | 0x80 // RFC 4122 variant
	return u
}

// Cohort is deterministic per seed. It covers every kind, every age band
// and none, every sex code, a "Not standardised" condition per source, and
// runs of zeros (rare conditions have months with no facts).
func Cohort(seed int64, o Options) Set {
	r := rand.New(rand.NewPCG(uint64(seed), 0x616e616c79736973)) // #nosec G404 -- synthetic data, not security
	start := time.Date(o.Start.Year(), o.Start.Month(), o.Start.Day(), 0, 0, 0, 0, time.UTC)
	days := int(start.AddDate(0, o.Months, 0).Sub(start).Hours() / 24)
	var s Set
	s.Sources = []Source{{ID: id(r), Label: "Synthetic EMR"}, {ID: id(r), Label: "Synthetic lab system"}}
	for i, l := range []string{"Synthetic Ward A", "Synthetic Outpatients", "Synthetic Emergency"} {
		s.Locations = append(s.Locations, Location{ID: id(r), Label: l, Setting: settings[i]})
	}
	for _, c := range demoConditions {
		s.Conditions = append(s.Conditions, Condition{ID: id(r), ICD10: ptr(c.code), Name: ptr(c.name)})
	}
	for i := range s.Sources {
		s.Conditions = append(s.Conditions, Condition{ID: id(r), Source: &s.Sources[i].ID})
	}
	// Condition weights: the first is common, later ones rarer (SmallBias).
	weights := make([]float64, len(s.Conditions))
	total := 0.0
	for i := range weights {
		w := 1.0 / (1.0 + o.SmallBias*float64(i)*float64(i))
		weights[i], total = w, total+w
	}
	condition := func() Condition {
		x := r.Float64() * total
		for i, w := range weights {
			if x < w {
				return s.Conditions[i]
			}
			x -= w
		}
		return s.Conditions[len(s.Conditions)-1]
	}
	for n := 0; n < o.Subjects; n++ {
		sub := Subject{Ref: fmt.Sprintf("synthetic-%d-%06d", seed, n)}
		var band *string
		if n%23 != 0 { // some subjects have no recorded age
			band = ptr(ageBands[(n+r.IntN(len(ageBands)))%len(ageBands)])
		}
		sex := sexes[(n+r.IntN(2))%len(sexes)]
		loc := pick(r, s.Locations)
		src := pick(r, s.Sources)
		facts := 1 + r.IntN(6)
		for f := 0; f < facts; f++ {
			kind := kinds[(n+f)%len(kinds)]
			c := condition()
			day := start.AddDate(0, 0, r.IntN(days))
			fact := analysis.Fact{Day: day.Format(time.DateOnly), Kind: kind, Condition: &c.ID, AgeBand: band, Sex: sex,
				Setting: ptr(loc.Setting), Location: &loc.ID, Source: &src.ID}
			switch kind {
			case "consultation":
				fact.Urgency = ptr(pick(r, []string{"routine", "urgent", "emergency"}))
				fact.Confidence = ptr(pick(r, []string{"high", "caution", "review", "failed"}))
				if r.IntN(2) == 0 {
					fact.Rating = ptr(pick(r, []string{"agreed", "not_agreed"}))
				}
				if r.IntN(5) == 0 {
					fact.Correction = ptr(pick(r, []string{"waiting", "approved", "rejected"}))
				}
				fact.Specialty = ptr(pick(r, []string{"general_medicine", "paediatrics", "obstetrics"}))
			case "finding":
				fact.Flag = ptr(pick(r, []string{"low", "high", "abnormal"}))
			case "alert":
				fact.Severity = ptr(pick(r, []string{"critical", "urgent", "info"}))
				if r.IntN(3) > 0 {
					fact.AckMinutes = ptr(r.IntN(240))
				}
				if r.IntN(4) == 0 {
					fact.Dismiss = ptr(pick(r, []string{"not_relevant", "already_known", "wrong_patient", "other"}))
				}
			}
			sub.Facts = append(sub.Facts, fact)
		}
		s.Subjects = append(s.Subjects, sub)
	}
	return s
}
