// Package rules is the abnormality rules engine (EMR design §11; plan Tasks
// 27-29): a deterministic, versioned rule set read from a YAML file and
// pinned by its SHA-256, a pure evaluator over one patient's canonical rows,
// the rules consumer that writes findings, and the per-source breaker.
//
// Rules run only on the canonical model: LOINC, canonical units (the
// normaliser's value_si, converted by the reviewed conversions) and ATC
// classes. A value the normaliser couldn't convert never reaches a numeric
// rule; an unmapped observation runs the source flag rule only.
package rules

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/codes"
)

var (
	// ErrRuleSet: the file is not a valid rule set (fixed text plus the
	// rule's id or the field; never a patient value).
	ErrRuleSet = errors.New("rules: rule set refused")
	// ErrSynthetic: a synthetic rule set outside development (K10).
	ErrSynthetic = errors.New("rules: a synthetic rule set is for development only")
)

// The closed vocabularies (plan Global Constraints; design §11.3).
var (
	Categories = []string{"blood_chemistry", "blood_count", "kidney_function", "blood_gases_lactate", "heart_markers",
		"blood_sugar", "vital_signs", "infection_markers"}
	Severities  = []string{"critical", "urgent", "info"}
	Kinds       = []string{"threshold", "source_flag", "reference_range", "delta", "trend", "score", "combination"}
	Confidences = []string{"caution", "review"} // never "high" (§21 question 3)
	ops         = []string{">=", ">", "<=", "<"}
	flags       = []string{"LL", "HH", "AA", "L", "H", "A"}
	statuses    = []string{"synthetic", "pilot", "reviewed"}
)

var (
	versionShape = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	idShape      = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	loincShape   = regexp.MustCompile(`^[0-9]{1,7}-[0-9]$`)
	atcPrefix    = regexp.MustCompile(`^[A-Z]([0-9]{2}([A-Z]([A-Z]([0-9]{2})?)?)?)?$`)
)

// Analyte is a measured quantity the rules name: its LOINC codes and the
// canonical unit its values are in.
type Analyte struct {
	Name     string   `yaml:"name"`
	LOINC    []string `yaml:"loinc"`
	Unit     string   `yaml:"unit"`
	Text     bool     `yaml:"text"`
	Category string   `yaml:"category"`
}

// Category is one of the eight with its freshness window.
type Category struct {
	Label          string `yaml:"label"`
	FreshnessHours int    `yaml:"freshness_hours"`
}

// Band is a population band of a threshold: by sex and age in years
// (inclusive). The sex unknown band is used whenever the patient's sex or
// age is unknown, or no other band matches (design §11.1).
type Band struct {
	Sex    string   `yaml:"sex"`
	MinAge *int     `yaml:"min_age"`
	MaxAge *int     `yaml:"max_age"`
	Value  *float64 `yaml:"value"`
}

// Evidence is one query for a cause: an active medication of an ATC class,
// or an observation of an analyte crossing a value within a window.
type Evidence struct {
	MedicationATC []string `yaml:"medication_atc"`
	Analyte       string   `yaml:"analyte"`
	Op            string   `yaml:"op"`
	Value         *float64 `yaml:"value"`
	WithinHours   int      `yaml:"within_hours"`
}

// CauseRule is a likely cause a rule may suggest, shown only when evidence
// is found.
type CauseRule struct {
	ID         string     `yaml:"id"`
	Label      string     `yaml:"label"`
	Confidence string     `yaml:"confidence"`
	Evidence   []Evidence `yaml:"evidence"`
}

// News2Params names the analyte of each NEWS2 parameter.
type News2Params struct {
	RespiratoryRate string `yaml:"respiratory_rate"`
	SpO2            string `yaml:"spo2"`
	SpO2Scale       string `yaml:"spo2_scale"`
	AirOrOxygen     string `yaml:"air_or_oxygen"`
	SystolicBP      string `yaml:"systolic_bp"`
	HeartRate       string `yaml:"heart_rate"`
	Consciousness   string `yaml:"consciousness"`
	Temperature     string `yaml:"temperature"`
}

// Rule is one rule as written in the file.
type Rule struct {
	ID       string `yaml:"id"`
	Kind     string `yaml:"kind"`
	Severity string `yaml:"severity"`
	Analyte  string `yaml:"analyte"`
	// threshold, combination
	Op    string   `yaml:"op"`
	Value *float64 `yaml:"value"`
	Bands []Band   `yaml:"bands"`
	// source_flag
	Flags            []string          `yaml:"flags"`
	UnmappedCategory map[string]string `yaml:"unmapped_category"`
	// reference_range
	Analytes []string `yaml:"analytes"`
	// delta, trend, score
	Rise        *float64 `yaml:"rise"`
	Fall        *float64 `yaml:"fall"`
	Ratio       *float64 `yaml:"ratio"`
	WithinHours int      `yaml:"within_hours"`
	Direction   string   `yaml:"direction"`
	Count       int      `yaml:"count"`
	// score
	Score         string      `yaml:"score"`
	Params        News2Params `yaml:"params"`
	TotalCritical int         `yaml:"total_critical"`
	TotalUrgent   int         `yaml:"total_urgent"`
	SingleThree   string      `yaml:"single_three"`
	// combination
	WithATC []string `yaml:"with_atc"`

	Causes     []CauseRule `yaml:"causes"`
	Source     string      `yaml:"source"`
	ReviewedBy []string    `yaml:"reviewed_by"`
}

type file struct {
	Version         string              `yaml:"version"`
	Status          string              `yaml:"status"`
	ClinicalSignoff string              `yaml:"clinical_signoff"`
	Description     string              `yaml:"description"`
	Categories      map[string]Category `yaml:"categories"`
	Analytes        []Analyte           `yaml:"analytes"`
	Rules           []Rule              `yaml:"rules"`
}

// RuleSet is a loaded, checked rule set. It is immutable and safe for
// concurrent use.
type RuleSet struct {
	f        file
	sha      [32]byte
	analytes map[string]Analyte
	byLOINC  map[string]string
}

// Version is the rule set's version, recorded on every finding.
func (rs *RuleSet) Version() string { return rs.f.Version }

// SHA256 is the hash of the file's bytes, recorded on every finding and
// compared with a hospital's activation.
func (rs *RuleSet) SHA256() [32]byte { return rs.sha }

// Status is synthetic, pilot or reviewed.
func (rs *RuleSet) Status() string { return rs.f.Status }

// Signoff is the clinical sign-off state: none (synthetic), deferred
// (pilot) or done (reviewed).
func (rs *RuleSet) Signoff() string { return rs.f.ClinicalSignoff }

// RuleIDs are the rules' ids in file order.
func (rs *RuleSet) RuleIDs() []string {
	out := make([]string, len(rs.f.Rules))
	for i, r := range rs.f.Rules {
		out[i] = r.ID
	}
	return out
}

// Analyte is the named analyte.
func (rs *RuleSet) Analyte(name string) (Analyte, bool) {
	a, ok := rs.analytes[name]
	return a, ok
}

// AnalyteOf is the analyte a LOINC code belongs to.
func (rs *RuleSet) AnalyteOf(loinc string) (Analyte, bool) {
	n, ok := rs.byLOINC[loinc]
	if !ok {
		return Analyte{}, false
	}
	return rs.analytes[n], true
}

// FreshnessHours is the category's freshness window.
func (rs *RuleSet) FreshnessHours(category string) (int, bool) {
	c, ok := rs.f.Categories[category]
	return c.FreshnessHours, ok
}

// Analytes are every analyte, in file order.
func (rs *RuleSet) Analytes() []Analyte { return slices.Clone(rs.f.Analytes) }

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrRuleSet}, a...)...)
}

// Load reads and checks a rule set file. Strict: an unknown key, a second
// document, a value outside a closed vocabulary or a rule without its
// source is refused. A synthetic set (status synthetic, version and file
// name ending -synthetic, every source "synthetic") loads in development
// only. A pilot set has its clinical sign-off deferred; a reviewed set
// names its reviewers on every rule.
func Load(path string, development bool) (*RuleSet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, refuse("the file is unreadable")
	}
	var f file
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(&f); err != nil {
		return nil, refuse("not a valid rule set file")
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, refuse("one document only")
	}
	rs := &RuleSet{f: f, sha: sha256.Sum256(b), analytes: map[string]Analyte{}, byLOINC: map[string]string{}}
	synthName := strings.HasSuffix(filepath.Base(path), "-synthetic.yaml")
	if err := rs.check(synthName); err != nil {
		return nil, err
	}
	if f.Status == "synthetic" && !development {
		return nil, ErrSynthetic
	}
	return rs, nil
}

func (rs *RuleSet) check(synthName bool) error {
	f := rs.f
	if !versionShape.MatchString(f.Version) || !slices.Contains(statuses, f.Status) {
		return refuse("version or status")
	}
	synthetic := f.Status == "synthetic"
	if synthetic != synthName || synthetic != strings.HasSuffix(f.Version, "-synthetic") {
		return refuse("a synthetic set has status synthetic, a -synthetic version and a -synthetic file name; no other does")
	}
	want := map[string]string{"synthetic": "none", "pilot": "deferred", "reviewed": "done"}[f.Status]
	if f.ClinicalSignoff != want {
		return refuse("clinical_signoff must be %s for a %s set", want, f.Status)
	}
	for name, c := range f.Categories {
		if !slices.Contains(Categories, name) || c.Label == "" || c.FreshnessHours < 1 || c.FreshnessHours > 24*30 {
			return refuse("category %q", name)
		}
	}
	for i, a := range f.Analytes {
		if !idShape.MatchString(a.Name) || len(a.LOINC) == 0 {
			return refuse("analyte %d", i)
		}
		if _, dup := rs.analytes[a.Name]; dup {
			return refuse("analyte %s listed twice", a.Name)
		}
		if _, ok := f.Categories[a.Category]; !ok {
			return refuse("analyte %s: category without a freshness window", a.Name)
		}
		if a.Text != (a.Unit == "") {
			return refuse("analyte %s: a numeric analyte has a unit, a text one none", a.Name)
		}
		for _, l := range a.LOINC {
			if !loincShape.MatchString(l) {
				return refuse("analyte %s: LOINC", a.Name)
			}
			if _, dup := rs.byLOINC[l]; dup {
				return refuse("analyte %s: a LOINC code in two analytes", a.Name)
			}
			rs.byLOINC[l] = a.Name
		}
		rs.analytes[a.Name] = a
	}
	if len(f.Rules) == 0 {
		return refuse("no rules")
	}
	seen := map[string]bool{}
	for _, r := range f.Rules {
		if !idShape.MatchString(r.ID) || seen[r.ID] {
			return refuse("rule id %q", r.ID)
		}
		seen[r.ID] = true
		if strings.TrimSpace(r.Source) == "" || (synthetic != (r.Source == "synthetic")) {
			return refuse("rule %s: every rule cites its source (\"synthetic\" only in a synthetic set)", r.ID)
		}
		if f.Status == "reviewed" && len(r.ReviewedBy) < 2 {
			return refuse("rule %s: a reviewed set names two reviewers on every rule", r.ID)
		}
		if err := rs.checkRule(r); err != nil {
			return err
		}
		if len(r.Causes) > 3 {
			return refuse("rule %s: at most three causes", r.ID)
		}
		for _, c := range r.Causes {
			if !idShape.MatchString(c.ID) || strings.TrimSpace(c.Label) == "" || len(c.Label) > 60 ||
				!slices.Contains(Confidences, c.Confidence) || len(c.Evidence) == 0 || len(c.Evidence) > 3 {
				return refuse("rule %s cause %q: an id, a label, caution or review, one to three evidence queries", r.ID, c.ID)
			}
			for _, e := range c.Evidence {
				if err := rs.checkEvidence(r.ID, e); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func finite(v *float64) bool { return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) }

func (rs *RuleSet) numeric(name string) bool {
	a, ok := rs.analytes[name]
	return ok && !a.Text
}

func (rs *RuleSet) checkRule(r Rule) error {
	if !slices.Contains(Kinds, r.Kind) {
		return refuse("rule %s: kind", r.ID)
	}
	if r.Kind != "score" && !slices.Contains(Severities, r.Severity) {
		return refuse("rule %s: severity", r.ID)
	}
	switch r.Kind {
	case "threshold":
		if !rs.numeric(r.Analyte) || !slices.Contains(ops, r.Op) {
			return refuse("rule %s: a numeric analyte and an op", r.ID)
		}
		if (r.Value == nil) == (len(r.Bands) == 0) || (r.Value != nil && !finite(r.Value)) {
			return refuse("rule %s: a value or bands", r.ID)
		}
		unknown := 0
		for _, b := range r.Bands {
			if !finite(b.Value) || !slices.Contains([]string{"female", "male", "any", "unknown"}, b.Sex) ||
				(b.MinAge != nil && *b.MinAge < 0) || (b.MaxAge != nil && *b.MaxAge > 150) ||
				(b.MinAge != nil && b.MaxAge != nil && *b.MinAge > *b.MaxAge) {
				return refuse("rule %s: band", r.ID)
			}
			if b.Sex == "unknown" {
				unknown++
				if b.MinAge != nil || b.MaxAge != nil {
					return refuse("rule %s: the unknown band has no ages", r.ID)
				}
			}
		}
		if len(r.Bands) > 0 && unknown != 1 {
			return refuse("rule %s: bands need exactly one unknown band", r.ID)
		}
	case "source_flag":
		if len(r.Flags) == 0 || r.Severity != "critical" {
			return refuse("rule %s: flags, critical", r.ID)
		}
		for _, fl := range r.Flags {
			if !slices.Contains(flags, fl) {
				return refuse("rule %s: flag %q", r.ID, fl)
			}
		}
		for _, c := range []string{"laboratory", "vital-signs"} {
			if _, ok := rs.f.Categories[r.UnmappedCategory[c]]; !ok {
				return refuse("rule %s: unmapped_category for %s", r.ID, c)
			}
		}
		if len(r.UnmappedCategory) != 2 {
			return refuse("rule %s: unmapped_category", r.ID)
		}
	case "reference_range":
		if r.Severity != "info" || len(r.Analytes) == 0 {
			return refuse("rule %s: info, analytes", r.ID)
		}
		for _, a := range r.Analytes {
			if !rs.numeric(a) {
				return refuse("rule %s: analyte %q", r.ID, a)
			}
		}
	case "delta":
		set := 0
		for _, v := range []*float64{r.Rise, r.Fall, r.Ratio} {
			if v != nil {
				set++
				if !finite(v) || *v <= 0 {
					return refuse("rule %s: change", r.ID)
				}
			}
		}
		if set != 1 || !rs.numeric(r.Analyte) || r.WithinHours < 1 || r.WithinHours > 24*14 {
			return refuse("rule %s: one of rise, fall, ratio; an analyte; a window", r.ID)
		}
	case "trend":
		if !rs.numeric(r.Analyte) || (r.Direction != "rising" && r.Direction != "falling") || r.Count < 3 || r.Count > 10 ||
			r.WithinHours < 1 || r.WithinHours > 24*14 {
			return refuse("rule %s: analyte, direction, count 3 to 10, window", r.ID)
		}
	case "score":
		if r.Score != "news2" || r.Severity != "" || r.WithinHours != 1 || r.TotalCritical < 1 || r.TotalUrgent < 1 ||
			r.TotalUrgent >= r.TotalCritical || !slices.Contains(Severities, r.SingleThree) {
			return refuse("rule %s: news2 within 1 h with its severities", r.ID)
		}
		p := r.Params
		for _, n := range []string{p.RespiratoryRate, p.SpO2, p.SystolicBP, p.HeartRate, p.Temperature} {
			if !rs.numeric(n) {
				return refuse("rule %s: parameter %q", r.ID, n)
			}
		}
		for _, n := range []string{p.SpO2Scale, p.AirOrOxygen, p.Consciousness} {
			if a, ok := rs.analytes[n]; !ok || !a.Text {
				return refuse("rule %s: parameter %q", r.ID, n)
			}
		}
	case "combination":
		if !rs.numeric(r.Analyte) || !slices.Contains(ops, r.Op) || !finite(r.Value) || len(r.WithATC) == 0 {
			return refuse("rule %s: analyte, op, value, with_atc", r.ID)
		}
		for _, a := range r.WithATC {
			if !atcPrefix.MatchString(a) {
				return refuse("rule %s: ATC %q", r.ID, a)
			}
		}
	}
	return nil
}

func (rs *RuleSet) checkEvidence(rule string, e Evidence) error {
	switch {
	case len(e.MedicationATC) > 0 && e.Analyte == "":
		for _, a := range e.MedicationATC {
			if !atcPrefix.MatchString(a) {
				return refuse("rule %s: evidence ATC %q", rule, a)
			}
		}
		if e.Op != "" || e.Value != nil || e.WithinHours != 0 {
			return refuse("rule %s: medication evidence has ATC classes only", rule)
		}
	case e.Analyte != "" && len(e.MedicationATC) == 0:
		if !rs.numeric(e.Analyte) || !slices.Contains(ops, e.Op) || !finite(e.Value) || e.WithinHours < 1 ||
			e.WithinHours > 24*30 {
			return refuse("rule %s: observation evidence needs analyte, op, value, within_hours", rule)
		}
	default:
		return refuse("rule %s: evidence is a medication class or an observation", rule)
	}
	return nil
}

// CheckUnits checks the rule set against the normaliser's conversions: an
// analyte the conversions know must have the same canonical unit as the
// rule set says, or the rules would compare a value in one unit against a
// threshold in another. It returns the analytes the conversions don't know
// (their numeric rules can't fire until the conversions list them).
func (rs *RuleSet) CheckUnits(m *codes.Maps) error {
	_, err := rs.UnitsMissing(m)
	return err
}

// UnitsMissing is CheckUnits, also returning the LOINC codes the
// conversions don't list.
func (rs *RuleSet) UnitsMissing(m *codes.Maps) ([]string, error) {
	var missing []string
	for _, a := range rs.f.Analytes {
		if a.Text {
			continue
		}
		for _, l := range a.LOINC {
			u, ok := m.Canonical(l)
			if !ok {
				missing = append(missing, l)
				continue
			}
			if u != a.Unit {
				return nil, refuse("analyte %s: LOINC %s is in %s in the conversions, %s in the rule set", a.Name, l, u, a.Unit)
			}
		}
	}
	return missing, nil
}
