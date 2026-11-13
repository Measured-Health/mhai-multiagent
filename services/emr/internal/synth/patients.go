// Package synth is the synthetic FHIR R4 source (EMR plan Task 25): made-up
// patients with timed labs, vital signs and medications, and a FHIR server
// that serves them with SMART Backend Services sign-in. Test and
// development only: the emr binary's synth-fhir command refuses anything
// but APP_ENV=development, and nothing here contacts a real server.
//
// Every name comes from the lists below, marked synthetic; every MRN is
// SYN-<digits>. No real person's data.
package synth

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"time"
)

// SyntheticGiven and SyntheticFamily are the only names used: invented,
// and marked as such.
var (
	SyntheticGiven  = []string{"Synthia", "Testa", "Fakeola", "Mocka", "Dummar", "Placido", "Sampla", "Fixtur"}
	SyntheticFamily = []string{"Testperson", "Synthetica", "Mockingbard", "Fixtureson", "Samplewood", "Dummyfield"}
)

// The systems the generated codes use (maps/testdata/synthetic/fhir.yaml
// maps the lab and drug codes for the synthetic connection).
const (
	LabSystem   = "urn:synthetic:lab"
	VitalSystem = "urn:synthetic:vital"
	DrugSystem  = "urn:synthetic:drug"
	MRNSystem   = "urn:synthetic:mrn"
)

// The cases a generated set always holds.
const (
	CaseCriticalPotassium = "critical_potassium"
	CaseCreatinineRise    = "creatinine_rise"
	CaseAmended           = "amended"
)

// Patient is one synthetic patient and their records, as FHIR resources.
type Patient struct {
	ID, MRN, Given, Family, BirthDate, Sex string
	Resource                               map[string]any
	Encounter                              map[string]any
	Observations, Medications              []map[string]any
	cases                                  []string
	values                                 []string
}

// Has reports whether the patient carries a case.
func (p Patient) Has(c string) bool { return slices.Contains(p.cases, c) }

// Values are the patient's result values as written (for telemetry
// checks).
func (p Patient) Values() []string { return slices.Clone(p.values) }

// Locations are the synthetic wards.
func Locations(anchor time.Time) []map[string]any {
	var out []map[string]any
	for i, name := range []string{"Synthetic Ward A", "Synthetic Ward B"} {
		out = append(out, map[string]any{"resourceType": "Location", "id": fmt.Sprintf("syn-l-%d", i+1),
			"meta": meta("1", anchor.Add(-30*24*time.Hour)), "name": name,
			"physicalType": map[string]any{"coding": []any{map[string]any{"code": "wa"}}}})
	}
	return out
}

func meta(version string, at time.Time) map[string]any {
	return map[string]any{"versionId": version, "lastUpdated": at.UTC().Format(time.RFC3339)}
}

// Generate makes n patients from seed, timed relative to now.
func Generate(seed int64, n int) []Patient {
	return GenerateAt(seed, n, time.Now().UTC().Truncate(time.Minute))
}

// GenerateAt makes n patients from seed, timed relative to anchor (every
// record within the last 3 days, none after anchor). Patient 0 has a
// critical potassium, patient 1 a creatinine rise and a loop diuretic,
// patient 2 an amended glucose.
func GenerateAt(seed int64, n int, anchor time.Time) []Patient {
	r := rand.New(rand.NewPCG(uint64(seed), uint64(seed)^0x5eed))
	lagos := time.FixedZone("+01:00", 3600)
	var out []Patient
	for i := 0; i < n; i++ {
		p := Patient{
			ID:     fmt.Sprintf("syn-p-%03d", i+1),
			MRN:    fmt.Sprintf("SYN-%06d", r.IntN(1000000)),
			Given:  SyntheticGiven[r.IntN(len(SyntheticGiven))],
			Family: SyntheticFamily[r.IntN(len(SyntheticFamily))],
			BirthDate: time.Date(1940+r.IntN(65), time.Month(1+r.IntN(12)), 1+r.IntN(28), 0, 0, 0, 0, time.UTC).
				Format("2006-01-02"),
			Sex: []string{"female", "male"}[r.IntN(2)],
		}
		p.Resource = map[string]any{"resourceType": "Patient", "id": p.ID, "meta": meta("1", anchor.Add(-72*time.Hour)),
			"identifier": []any{map[string]any{"system": MRNSystem, "value": p.MRN}},
			"name":       []any{map[string]any{"use": "official", "given": []any{p.Given}, "family": p.Family}},
			"gender":     p.Sex, "birthDate": p.BirthDate, "active": true}
		enc := fmt.Sprintf("syn-e-%03d", i+1)
		p.Encounter = map[string]any{"resourceType": "Encounter", "id": enc, "meta": meta("1", anchor.Add(-70*time.Hour)),
			"status": "in-progress", "class": map[string]any{"code": "IMP"},
			"subject":  map[string]any{"reference": "Patient/" + p.ID},
			"period":   map[string]any{"start": anchor.Add(-70 * time.Hour).In(lagos).Format(time.RFC3339)},
			"location": []any{map[string]any{"location": map[string]any{"reference": fmt.Sprintf("Location/syn-l-%d", 1+i%2)}}}}
		obs := func(id, system, code, display, category, value, unit, flag, status, version string, takenAgo time.Duration) {
			taken := anchor.Add(-takenAgo)
			o := map[string]any{"resourceType": "Observation", "id": id, "meta": meta(version, taken.Add(20*time.Minute)),
				"status": status,
				"category": []any{map[string]any{"coding": []any{map[string]any{
					"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": category}}}},
				"code":              map[string]any{"coding": []any{map[string]any{"system": system, "code": code, "display": display}}},
				"subject":           map[string]any{"reference": "Patient/" + p.ID},
				"encounter":         map[string]any{"reference": "Encounter/" + enc},
				"effectiveDateTime": taken.In(lagos).Format(time.RFC3339),
				"issued":            taken.Add(15 * time.Minute).UTC().Format(time.RFC3339),
				"valueQuantity":     map[string]any{"value": jsonNumber(value), "unit": unit},
			}
			if flag != "" {
				o["interpretation"] = []any{map[string]any{"coding": []any{map[string]any{"code": flag}}}}
			}
			p.Observations = append(p.Observations, o)
			p.values = append(p.values, value)
		}
		dec := func(lo, hi float64) string { return strconv.FormatFloat(lo+r.Float64()*(hi-lo), 'f', 2, 64) }
		k := dec(3.61, 4.89)
		flag := ""
		if i == 0 {
			k, flag = "6.93", "HH"
			p.cases = append(p.cases, CaseCriticalPotassium)
		}
		obs(fmt.Sprintf("syn-o-%03d-k", i+1), LabSystem, "K", "Potassium", "laboratory", k, "mmol/L", flag, "final", "1", 6*time.Hour)
		if i == 1 {
			obs(fmt.Sprintf("syn-o-%03d-cr1", i+1), LabSystem, "CREA", "Creatinine", "laboratory", "81.25", "umol/L", "", "final", "1", 50*time.Hour)
			obs(fmt.Sprintf("syn-o-%03d-cr2", i+1), LabSystem, "CREA", "Creatinine", "laboratory", "183.75", "umol/L", "H", "final", "1", 4*time.Hour)
			p.cases = append(p.cases, CaseCreatinineRise)
			p.Medications = append(p.Medications, map[string]any{"resourceType": "MedicationRequest", "id": fmt.Sprintf("syn-m-%03d", i+1),
				"meta": meta("1", anchor.Add(-3*time.Hour)), "status": "active", "intent": "order",
				"medicationCodeableConcept": map[string]any{"coding": []any{map[string]any{"system": DrugSystem, "code": "FUROS"}},
					"text": "Loop diuretic one"},
				"subject":    map[string]any{"reference": "Patient/" + p.ID},
				"authoredOn": anchor.Add(-3 * time.Hour).UTC().Format(time.RFC3339),
				"dosageInstruction": []any{map[string]any{"route": map[string]any{"text": "oral"},
					"doseAndRate": []any{map[string]any{"doseQuantity": map[string]any{"value": jsonNumber("40"), "unit": "mg"}}}}}})
		} else {
			obs(fmt.Sprintf("syn-o-%03d-cr", i+1), LabSystem, "CREA", "Creatinine", "laboratory", dec(61.1, 98.9), "umol/L", "", "final", "1", 5*time.Hour)
		}
		if i == 2 {
			// Version 2 of a glucose: amended after the first report.
			obs(fmt.Sprintf("syn-o-%03d-glu", i+1), LabSystem, "GLU", "Glucose", "laboratory", "11.42", "mmol/L", "H", "amended", "2", 3*time.Hour)
			p.cases = append(p.cases, CaseAmended)
		} else {
			obs(fmt.Sprintf("syn-o-%03d-glu", i+1), LabSystem, "GLU", "Glucose", "laboratory", dec(4.11, 6.89), "mmol/L", "", "final", "1", 3*time.Hour)
		}
		obs(fmt.Sprintf("syn-o-%03d-hr", i+1), VitalSystem, "HR", "Heart rate", "vital-signs", strconv.Itoa(61+r.IntN(37))+".5", "bpm", "", "final", "1", 2*time.Hour)
		out = append(out, p)
	}
	return out
}
