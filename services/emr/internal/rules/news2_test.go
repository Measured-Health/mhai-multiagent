package rules

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// NEWS2 needs every parameter, consciousness and air or oxygen included: a
// missing one means no score, never "normal" (design §11.1).
func TestNEWS2MissingParameterNoScore(t *testing.T) {
	rs, err := Load(filepath.Join("..", "..", "rules", "testdata", "ruleset-0-synthetic.yaml"), true)
	if err != nil {
		t.Fatal(err)
	}
	type p struct {
		loinc, text string
		v           float64
	}
	// A set scoring 9 (critical) when whole.
	full := map[string]p{
		"respiratory_rate": {"99908-6", "", 26}, "spo2": {"99910-2", "", 90}, "air_or_oxygen": {"99913-6", "air", 0},
		"systolic_bp": {"99907-8", "", 105}, "heart_rate": {"99906-0", "", 115}, "consciousness": {"99912-8", "A", 0},
		"temperature": {"99909-4", "", 37},
	}
	build := func(skip string) (History, uuid.UUID) {
		h := History{Patient: Patient{ID: uuid.New(), Sex: "male"}}
		var last uuid.UUID
		for name, x := range full {
			if name == skip {
				continue
			}
			o := Observation{ID: uuid.New(), LOINC: x.loinc, Category: "vital-signs", Status: "final", EffectiveAt: ago(30 * time.Minute),
				Standardised: true}
			if x.text != "" {
				o.ValueText = x.text
			} else {
				o.ValueNum, o.ValueSI = f64(x.v), f64(x.v)
			}
			h.Observations = append(h.Observations, o)
			last = o.ID
		}
		return h, last
	}
	h, trig := build("")
	if fs, err := Evaluate(rs, h, trig, now); err != nil || !hasScore(fs, 9) {
		t.Fatalf("whole set: %+v %v", fs, err)
	}
	for name := range full {
		h, trig := build(name)
		fs, err := Evaluate(rs, h, trig, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range fs {
			if f.RuleID == "news2" {
				t.Errorf("without %s: scored %v", name, *f.Score)
			}
		}
	}
	// A consciousness value outside A, C, V, P, U is not a value: no score.
	h, trig = build("")
	for i := range h.Observations {
		if h.Observations[i].LOINC == "99912-8" {
			h.Observations[i].ValueText = "alert"
		}
	}
	if fs, _ := Evaluate(rs, h, trig, now); hasScore(fs, -1) {
		t.Error("an unreadable consciousness level scored")
	}
}

func hasScore(fs []Finding, want int) bool {
	for _, f := range fs {
		if f.RuleID == "news2" && f.Score != nil && (want < 0 || *f.Score == want) {
			return true
		}
	}
	return false
}
