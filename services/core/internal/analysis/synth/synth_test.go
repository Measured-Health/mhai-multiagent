package synth

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

var start = time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)

// The same seed gives the same cohort; it covers every kind, age band
// (and none), sex, "Not standardised", and runs of zeros; every fact is a
// day inside the window.
func TestCohortDeterministicAndComplete(t *testing.T) {
	opts := Options{Subjects: 400, Months: 24, SmallBias: 0.3, Start: start}
	a, b := Cohort(7, opts), Cohort(7, opts)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if !bytes.Equal(ja, jb) {
		t.Fatal("the same seed gave two cohorts")
	}
	if jc, _ := json.Marshal(Cohort(8, opts)); bytes.Equal(ja, jc) {
		t.Fatal("two seeds gave one cohort")
	}
	if len(a.Subjects) != 400 {
		t.Fatalf("%d subjects", len(a.Subjects))
	}
	end := start.AddDate(0, 24, 0)
	kinds, bands, sexes := map[string]bool{}, map[string]bool{}, map[string]bool{}
	unstandardised := false
	for _, c := range a.Conditions {
		if c.ICD10 == nil && c.Name == nil {
			unstandardised = true
		}
	}
	perMonth := map[string]map[string]int{} // condition -> month -> facts
	for _, s := range a.Subjects {
		if len(s.Facts) == 0 {
			t.Fatal("a subject with no facts")
		}
		for _, f := range s.Facts {
			d, err := time.Parse(time.DateOnly, f.Day)
			if err != nil || d.Before(start) || !d.Before(end) {
				t.Fatalf("fact day %q outside the window", f.Day)
			}
			kinds[f.Kind] = true
			sexes[f.Sex] = true
			if f.AgeBand == nil {
				bands["none"] = true
			} else {
				bands[*f.AgeBand] = true
			}
			if f.Condition != nil {
				k := f.Condition.String()
				if perMonth[k] == nil {
					perMonth[k] = map[string]int{}
				}
				perMonth[k][f.Day[:7]]++
			}
		}
	}
	for _, k := range []string{"consultation", "diagnosis", "finding", "alert"} {
		if !kinds[k] {
			t.Errorf("no %s fact", k)
		}
	}
	for _, b := range []string{"0-17", "18-39", "40-64", "65-79", "80+", "none"} {
		if !bands[b] {
			t.Errorf("no age band %s", b)
		}
	}
	for _, s := range []string{"female", "male", "other", "not_recorded"} {
		if !sexes[s] {
			t.Errorf("no sex %s", s)
		}
	}
	if !unstandardised {
		t.Error("no Not standardised condition")
	}
	zeros := false
	for _, months := range perMonth {
		if len(months) < 24 {
			zeros = true
		}
	}
	if !zeros {
		t.Error("no condition has a month with no facts")
	}
	body, err := a.SeedJSON()
	if err != nil || bytes.Contains(body, []byte(`"ref"`)) {
		t.Fatalf("seed body: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || len(top) != 4 {
		t.Fatalf("seed body keys %v (%v)", top, err)
	}
}
