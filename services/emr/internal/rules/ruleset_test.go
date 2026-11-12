package rules

import (
	"crypto/sha256"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// base is a minimal valid pilot rule set; the tests below break one thing
// at a time.
const base = `version: "9"
status: pilot
clinical_signoff: deferred
description: test
categories:
  blood_chemistry: {label: Blood chemistry, freshness_hours: 72}
  vital_signs: {label: Vital signs, freshness_hours: 6}
analytes:
  - {name: potassium, loinc: ["2823-3"], unit: mmol/L, category: blood_chemistry}
rules:
  - id: k-high
    kind: threshold
    analyte: potassium
    severity: critical
    op: ">="
    value: 6.5
    source: "a cited reference"
    causes:
      - id: c1
        label: A cause
        confidence: review
        evidence:
          - {medication_atc: [C03DA]}
  - id: flag
    kind: source_flag
    severity: critical
    flags: [LL, HH, AA]
    unmapped_category: {laboratory: blood_chemistry, vital-signs: vital_signs}
    source: "the lab's own flag"
`

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBaseLoadsAndHashesTheBytes(t *testing.T) {
	p := write(t, "ruleset-9.yaml", base)
	rs, err := Load(p, false)
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	if rs.Version() != "9" || rs.SHA256() != sha256.Sum256([]byte(base)) || rs.Status() != "pilot" {
		t.Fatalf("version %q status %q", rs.Version(), rs.Status())
	}
	if ids := rs.RuleIDs(); len(ids) != 2 || ids[0] != "k-high" {
		t.Fatalf("rule ids %v", ids)
	}
}

func TestRuleSetSchemaRefusals(t *testing.T) {
	for name, body := range map[string]string{
		"threshold without source":   strings.Replace(base, `    source: "a cited reference"`+"\n", "", 1),
		"empty source":               strings.Replace(base, `"a cited reference"`, `""`, 1),
		"high confidence":            strings.Replace(base, "confidence: review", "confidence: high", 1),
		"other confidence":           strings.Replace(base, "confidence: review", "confidence: certain", 1),
		"unknown key":                strings.Replace(base, "    op: \">=\"\n", "    op: \">=\"\n    fuzzy: true\n", 1),
		"unknown kind":               strings.Replace(base, "kind: threshold", "kind: ml_score", 1),
		"unknown severity":           strings.Replace(base, "severity: critical\n    op", "severity: high\n    op", 1),
		"unknown category":           strings.Replace(base, "category: blood_chemistry}", "category: chemistry}", 1),
		"category without window":    strings.Replace(base, "  vital_signs: {label: Vital signs, freshness_hours: 6}\n", "", 1),
		"no op":                      strings.Replace(base, "    op: \">=\"\n", "", 1),
		"bad op":                     strings.Replace(base, `op: ">="`, `op: "=="`, 1),
		"no value":                   strings.Replace(base, "    value: 6.5\n", "", 1),
		"unknown analyte":            strings.Replace(base, "analyte: potassium", "analyte: kalium", 1),
		"duplicate rule id":          strings.Replace(base, "id: flag", "id: k-high", 1),
		"bad rule id":                strings.Replace(base, "id: k-high", "id: K High", 1),
		"bad loinc":                  strings.Replace(base, `"2823-3"`, `"2823"`, 1),
		"bad atc":                    strings.Replace(base, "[C03DA]", "[c03da]", 1),
		"cause without evidence":     strings.Replace(base, "        evidence:\n          - {medication_atc: [C03DA]}\n", "", 1),
		"flag not critical":          strings.Replace(base, "flags: [LL, HH, AA]", "flags: [LL, N]", 1),
		"pilot without deferral":     strings.Replace(base, "clinical_signoff: deferred", "clinical_signoff: done", 1),
		"reviewed without reviewers": strings.Replace(strings.Replace(base, "status: pilot", "status: reviewed", 1), "clinical_signoff: deferred", "clinical_signoff: done", 1),
		"synthetic source in pilot":  strings.Replace(base, `"a cited reference"`, "synthetic", 1),
		"no rules":                   base[:strings.Index(base, "rules:")] + "rules: []\n",
		"two documents":              base + "---\nversion: x\n",
		"freshness zero":             strings.Replace(base, "freshness_hours: 72", "freshness_hours: 0", 1),
		"bad version":                strings.Replace(base, `version: "9"`, `version: "Nine Point"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, "ruleset-9.yaml", body), true); !errors.Is(err, ErrRuleSet) {
				t.Fatalf("loaded: %v", err)
			}
		})
	}
	// A reviewed set needs reviewers on every rule.
	reviewed := strings.Replace(strings.Replace(base, "status: pilot", "status: reviewed", 1), "clinical_signoff: deferred", "clinical_signoff: done", 1)
	reviewed = strings.ReplaceAll(reviewed, "    source: ", "    reviewed_by: [Clinical Lead, Second Clinician]\n    source: ")
	if _, err := Load(write(t, "ruleset-9.yaml", reviewed), false); err != nil {
		t.Fatalf("reviewed set refused: %v", err)
	}
}

// Plan Task 27: every threshold has a source.
func TestThresholdWithoutSourceRefused(t *testing.T) {
	body := strings.Replace(base, `    source: "a cited reference"`+"\n", "", 1)
	if _, err := Load(write(t, "ruleset-9.yaml", body), true); !errors.Is(err, ErrRuleSet) {
		t.Fatalf("a threshold without a source loaded: %v", err)
	}
}

// §21 question 3: no "high" confidence.
func TestHighConfidenceRefused(t *testing.T) {
	body := strings.Replace(base, "confidence: review", "confidence: high", 1)
	if _, err := Load(write(t, "ruleset-9.yaml", body), true); !errors.Is(err, ErrRuleSet) {
		t.Fatalf("a high-confidence cause loaded: %v", err)
	}
}

// K10: the synthetic set loads in development only, and a set marked
// synthetic must be named so (and the reverse).
func TestSyntheticRuleSetRefusedOutsideDevelopment(t *testing.T) {
	synth := filepath.Join("..", "..", "rules", "testdata", "ruleset-0-synthetic.yaml")
	if _, err := Load(synth, true); err != nil {
		t.Fatalf("synthetic in development: %v", err)
	}
	if _, err := Load(synth, false); !errors.Is(err, ErrSynthetic) {
		t.Fatalf("synthetic outside development: %v", err)
	}
	b, err := os.ReadFile(synth)
	if err != nil {
		t.Fatal(err)
	}
	// The same content under another name, or the name on a pilot set.
	if _, err := Load(write(t, "ruleset-0.yaml", string(b)), true); !errors.Is(err, ErrRuleSet) {
		t.Fatalf("synthetic content without the -synthetic name: %v", err)
	}
	if _, err := Load(write(t, "ruleset-9-synthetic.yaml", base), true); !errors.Is(err, ErrRuleSet) {
		t.Fatalf("a pilot set named -synthetic: %v", err)
	}
}

// v1 loads anywhere (pilot), and is marked so.
func TestRuleSetV1IsPilotSignoffDeferred(t *testing.T) {
	rs, err := Load(filepath.Join("..", "..", "rules", "ruleset-1.yaml"), false)
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	if rs.Version() != "1" || rs.Status() != "pilot" || rs.Signoff() != "deferred" {
		t.Fatalf("v1 is %s %s %s", rs.Version(), rs.Status(), rs.Signoff())
	}
}

func hist(obs ...Observation) History {
	return History{Patient: Patient{ID: uuid.New(), Sex: "female"}, Observations: obs}
}

func f64(v float64) *float64 { return &v }

func ago(d time.Duration) *time.Time {
	t := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC).Add(-d)
	return &t
}

var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func loadBase(t *testing.T) *RuleSet {
	t.Helper()
	rs, err := Load(write(t, "ruleset-9.yaml", base), false)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// ">7" is at least 7; "<0.5" at most 0.5; ">6" can't be said to be >= 6.5.
func TestComparatorBounds(t *testing.T) {
	rs := loadBase(t)
	for _, c := range []struct {
		cmp   string
		bound float64
		hit   bool
	}{{">", 7, true}, {">=", 6.5, true}, {">", 6.5, true}, {">", 6, false}, {">=", 6.4, false}, {"<", 7, false}, {"<=", 9, false}} {
		o := Observation{ID: uuid.New(), LOINC: "2823-3", Category: "laboratory", Status: "final", Comparator: c.cmp,
			BoundSI: f64(c.bound), BoundNum: f64(c.bound), ValueText: c.cmp + "x", EffectiveAt: ago(time.Hour), Standardised: true}
		fs, err := Evaluate(rs, hist(o), o.ID, now)
		if err != nil {
			t.Fatal(err)
		}
		if (len(fs) == 1 && fs[0].RuleID == "k-high") != c.hit {
			t.Errorf("%s%v: findings %v, want hit %v", c.cmp, c.bound, fs, c.hit)
		}
	}
}

// §21 question 2: an unmapped observation runs the source flag rule only,
// even with a value that would cross a threshold.
func TestUnmappedRunsSourceFlagOnly(t *testing.T) {
	rs := loadBase(t)
	o := Observation{ID: uuid.New(), Category: "laboratory", Status: "final", ValueNum: f64(9), ValueSI: f64(9),
		EffectiveAt: ago(time.Hour)}
	fs, err := Evaluate(rs, hist(o), o.ID, now)
	if err != nil || len(fs) != 0 {
		t.Fatalf("unmapped without a flag: %v %v", fs, err)
	}
	o.SourceFlag = "HH"
	fs, err = Evaluate(rs, hist(o), o.ID, now)
	if err != nil || len(fs) != 1 || fs[0].RuleID != "flag" || fs[0].Severity != "critical" || fs[0].Standardised ||
		fs[0].Category != "blood_chemistry" {
		t.Fatalf("unmapped HH: %+v %v", fs, err)
	}
	// A mapped value with no conversion (no value_si) never reaches a
	// numeric rule either.
	m := Observation{ID: uuid.New(), LOINC: "2823-3", Category: "laboratory", Status: "final", ValueNum: f64(30),
		EffectiveAt: ago(time.Hour)}
	if fs, err := Evaluate(rs, hist(m), m.ID, now); err != nil || len(fs) != 0 {
		t.Fatalf("no value_si: %v %v", fs, err)
	}
}

// A trigger not in the history, or a value that isn't a number, is an
// error: the consumer stops the hospital rather than skip it.
func TestEvaluateErrors(t *testing.T) {
	rs := loadBase(t)
	o := Observation{ID: uuid.New(), LOINC: "2823-3", Category: "laboratory", Status: "final", ValueNum: f64(7), ValueSI: f64(7),
		EffectiveAt: ago(time.Hour), Standardised: true}
	if _, err := Evaluate(rs, hist(o), uuid.New(), now); err == nil {
		t.Fatal("a trigger outside the history evaluated")
	}
	nan := o
	nan.ValueSI = f64(math.Inf(1))
	if _, err := Evaluate(rs, hist(nan), nan.ID, now); err == nil {
		t.Fatal("an infinite value evaluated")
	}
}
