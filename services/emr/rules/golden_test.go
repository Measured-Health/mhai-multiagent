package rules_test

// The golden tests (EMR design §11.5; plan Tasks 27 and 30). Each file in
// testdata/golden/<set>/ holds synthetic patients with timed results, vital
// signs and medications, and the findings each trigger must raise. The
// expectation is exact: a finding not listed in expect fails the test, and
// so does a listed one that isn't raised. Every rule of a set needs at
// least one positive and one negative case across its files.
//
// Values go through the normaliser's own parsing and conversions
// (normalise.ParseDecimal, Maps.ParseUnit, Maps.Convert), so a rule sees
// exactly what it would see from a stored observation.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/codes"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/normalise"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/rules"
)

type goldenFile struct {
	Description string          `yaml:"description"`
	RuleSet     string          `yaml:"rule_set"`
	Now         time.Time       `yaml:"now"`
	Patients    []goldenPatient `yaml:"patients"`
	Expect      []goldenExpect  `yaml:"expect"`
	ExpectNone  []goldenExpect  `yaml:"expect_none"`
}

type goldenPatient struct {
	ID           string      `yaml:"id"`
	Sex          string      `yaml:"sex"`
	BirthDate    string      `yaml:"birth_date"`
	Observations []goldenObs `yaml:"observations"`
	Medications  []goldenMed `yaml:"medications"`
	Triggers     []string    `yaml:"triggers"`
}

type goldenObs struct {
	ID         string `yaml:"id"`
	Analyte    string `yaml:"analyte"`
	LOINC      string `yaml:"loinc"`
	LocalCode  string `yaml:"local_code"`
	Category   string `yaml:"category"`
	Value      string `yaml:"value"`
	Text       string `yaml:"text"`
	Unit       string `yaml:"unit"`
	RefLow     string `yaml:"ref_low"`
	RefHigh    string `yaml:"ref_high"`
	Flag       string `yaml:"flag"`
	At         string `yaml:"at"`
	Status     string `yaml:"status"`
	Superseded bool   `yaml:"superseded"`
}

type goldenMed struct {
	ID     string `yaml:"id"`
	ATC    string `yaml:"atc"`
	Status string `yaml:"status"`
	Start  string `yaml:"start"`
	End    string `yaml:"end"`
}

type goldenExpect struct {
	Patient      string   `yaml:"patient"`
	Observation  string   `yaml:"observation"`
	RuleID       string   `yaml:"rule_id"`
	Severity     string   `yaml:"severity"`
	Category     string   `yaml:"category"`
	Standardised *bool    `yaml:"standardised"`
	Preliminary  *bool    `yaml:"preliminary"`
	Score        *int     `yaml:"score"`
	Scale        *int     `yaml:"scale"`
	Causes       []string `yaml:"causes"`
	Evidence     []string `yaml:"evidence"`
	Medications  []string `yaml:"medications"`
}

// goldenSet is one rule set with the code tables and conversions its
// golden files run through.
type goldenSet struct {
	dir  string
	rs   *rules.RuleSet
	maps *codes.Maps
}

func loadSets(t *testing.T) map[string]goldenSet {
	t.Helper()
	synthTables, err := codes.Load(filepath.Join("..", "codes", "testdata"), true)
	if err != nil {
		t.Fatal(err)
	}
	synthMaps, err := codes.LoadMaps(filepath.Join("..", "maps"), synthTables, true)
	if err != nil {
		t.Fatal(err)
	}
	synth, err := rules.Load(filepath.Join("testdata", "ruleset-0-synthetic.yaml"), true)
	if err != nil {
		t.Fatalf("synthetic rule set: %v", err)
	}
	// v1 runs through the real unit table and the fixture's conversions for
	// the real LOINC codes it names (the production ones come with Task 21).
	v1Tables, err := codes.Load(filepath.Join("testdata", "v1-fixture"), true)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "common"), 0o700); err != nil {
		t.Fatal(err)
	}
	for src, dst := range map[string]string{
		filepath.Join("..", "maps", "common", "units.yaml"):         "units.yaml",
		filepath.Join("testdata", "v1-fixture", "conversions.yaml"): "conversions.yaml",
	} {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "common", dst), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	v1Maps, err := codes.LoadMaps(dir, v1Tables, true)
	if err != nil {
		t.Fatal(err)
	}
	// Production loading: v1 is a pilot set, accepted outside development.
	v1, err := rules.Load("ruleset-1.yaml", false)
	if err != nil {
		t.Fatalf("rule set v1: %v", err)
	}
	if err := v1.CheckUnits(v1Maps); err != nil {
		t.Fatalf("rule set v1 units against its conversions: %v", err)
	}
	if err := synth.CheckUnits(synthMaps); err != nil {
		t.Fatalf("synthetic rule set units: %v", err)
	}
	return map[string]goldenSet{
		synth.Version(): {dir: "synthetic", rs: synth, maps: synthMaps},
		v1.Version():    {dir: "v1", rs: v1, maps: v1Maps},
	}
}

func decodeGolden(t *testing.T, path string) goldenFile {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var g goldenFile
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(&g); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return g
}

// at reads a time relative to now ("-1h", "-30m", "-3d") or a date-only
// value ("date:2026-11-06").
func at(t *testing.T, now time.Time, s string) (*time.Time, *time.Time) {
	t.Helper()
	if d, ok := strings.CutPrefix(s, "date:"); ok {
		v, err := time.Parse("2006-01-02", d)
		if err != nil {
			t.Fatalf("date %q", s)
		}
		return nil, &v
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		var n int
		if _, err := fmt.Sscanf(days, "%d", &n); err != nil {
			t.Fatalf("time %q", s)
		}
		v := now.Add(time.Duration(n) * 24 * time.Hour)
		return &v, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatalf("time %q", s)
	}
	v := now.Add(d)
	return &v, nil
}

func num(t *testing.T, s string) *float64 {
	t.Helper()
	if s == "" {
		return nil
	}
	v, cmp, err := normalise.ParseDecimal(s, '.')
	if err != nil || cmp != "" {
		t.Fatalf("number %q", s)
	}
	return &v
}

type key struct{ patient, obs, rule string }

func TestGolden(t *testing.T) {
	sets := loadSets(t)
	positive, negative := map[string]map[string]bool{}, map[string]map[string]bool{}
	for version, set := range sets {
		positive[version], negative[version] = map[string]bool{}, map[string]bool{}
		files, err := filepath.Glob(filepath.Join("testdata", "golden", set.dir, "*.yaml"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no golden files for %s", version)
		}
		for _, file := range files {
			g := decodeGolden(t, file)
			if g.RuleSet != version {
				t.Fatalf("%s names rule set %q, in the %s folder", file, g.RuleSet, set.dir)
			}
			t.Run(set.dir+"/"+filepath.Base(file), func(t *testing.T) {
				runGolden(t, set, g, positive[version], negative[version])
			})
		}
	}
	for version, set := range sets {
		for _, id := range set.rs.RuleIDs() {
			if !positive[version][id] {
				t.Errorf("rule set %s: rule %s has no positive golden case", version, id)
			}
			if !negative[version][id] {
				t.Errorf("rule set %s: rule %s has no negative golden case", version, id)
			}
		}
	}
}

func runGolden(t *testing.T, set goldenSet, g goldenFile, positive, negative map[string]bool) {
	ids := map[uuid.UUID]string{}
	got := map[key]rules.Finding{}
	for _, gp := range g.Patients {
		h := rules.History{Patient: rules.Patient{ID: uuid.New(), Sex: gp.Sex}}
		if gp.BirthDate != "" {
			b, err := time.Parse("2006-01-02", gp.BirthDate)
			if err != nil {
				t.Fatal(err)
			}
			h.Patient.BirthDate = &b
		}
		byName := map[string]uuid.UUID{}
		for _, o := range gp.Observations {
			id := uuid.New()
			ids[id], byName[o.ID] = o.ID, id
			h.Observations = append(h.Observations, buildObs(t, set, g.Now, id, o))
		}
		for _, m := range gp.Medications {
			id := uuid.New()
			ids[id] = m.ID
			med := rules.Medication{ID: id, ATC: m.ATC, Status: m.Status}
			if m.Start != "" {
				med.StartAt, _ = at(t, g.Now, m.Start)
			}
			if m.End != "" {
				med.EndAt, _ = at(t, g.Now, m.End)
			}
			h.Medications = append(h.Medications, med)
		}
		triggers := gp.Triggers
		if len(triggers) == 0 {
			for _, o := range gp.Observations {
				triggers = append(triggers, o.ID)
			}
		}
		for _, tr := range triggers {
			id, ok := byName[tr]
			if !ok {
				t.Fatalf("patient %s: trigger %s is no observation", gp.ID, tr)
			}
			fs, err := rules.Evaluate(set.rs, h, id, g.Now)
			if err != nil {
				t.Fatalf("patient %s trigger %s: %v", gp.ID, tr, err)
			}
			for _, f := range fs {
				k := key{gp.ID, ids[f.Primary], f.RuleID}
				if prev, dup := got[k]; dup && prev.Severity != f.Severity {
					t.Errorf("%v raised twice with different severities", k)
				}
				got[k] = f
			}
		}
	}
	want := map[key]goldenExpect{}
	for _, e := range g.Expect {
		k := key{e.Patient, e.Observation, e.RuleID}
		want[k] = e
		positive[e.RuleID] = true
		f, ok := got[k]
		if !ok {
			t.Errorf("missing finding %v", k)
			continue
		}
		checkFinding(t, k, e, f, ids)
	}
	for k, f := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected finding %v (%s)", k, f.Severity)
		}
	}
	for _, e := range g.ExpectNone {
		negative[e.RuleID] = true
		k := key{e.Patient, e.Observation, e.RuleID}
		if _, ok := got[k]; ok {
			t.Errorf("finding %v raised; the file says none", k)
		}
	}
}

func names(ids []uuid.UUID, by map[uuid.UUID]string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, by[id])
	}
	sort.Strings(out)
	return out
}

func checkFinding(t *testing.T, k key, e goldenExpect, f rules.Finding, ids map[uuid.UUID]string) {
	t.Helper()
	if f.Severity != e.Severity {
		t.Errorf("%v severity %s, want %s", k, f.Severity, e.Severity)
	}
	if e.Category != "" && f.Category != e.Category {
		t.Errorf("%v category %s, want %s", k, f.Category, e.Category)
	}
	if e.Standardised != nil && f.Standardised != *e.Standardised {
		t.Errorf("%v standardised %v", k, f.Standardised)
	}
	if e.Preliminary != nil && f.Preliminary != *e.Preliminary {
		t.Errorf("%v preliminary %v", k, f.Preliminary)
	}
	if e.Score != nil && (f.Score == nil || *f.Score != *e.Score) {
		t.Errorf("%v score %v, want %d", k, f.Score, *e.Score)
	}
	if e.Scale != nil && f.Scale != *e.Scale {
		t.Errorf("%v scale %d, want %d", k, f.Scale, *e.Scale)
	}
	if e.Causes != nil {
		var got []string
		for _, c := range f.Causes {
			got = append(got, c.ID)
			if c.Confidence != "caution" && c.Confidence != "review" {
				t.Errorf("%v cause %s confidence %q", k, c.ID, c.Confidence)
			}
			if len(c.Observations)+len(c.Medications) == 0 || len(c.Observations)+len(c.Medications) > 3 {
				t.Errorf("%v cause %s has %d evidence items", k, c.ID, len(c.Observations)+len(c.Medications))
			}
		}
		if !slices.Equal(got, e.Causes) && !(len(got) == 0 && len(e.Causes) == 0) {
			t.Errorf("%v causes %v, want %v", k, got, e.Causes)
		}
	}
	if e.Evidence != nil {
		want := slices.Clone(e.Evidence)
		sort.Strings(want)
		if got := names(f.Observations, ids); !slices.Equal(got, want) {
			t.Errorf("%v evidence %v, want %v", k, got, want)
		}
	}
	if e.Medications != nil {
		want := slices.Clone(e.Medications)
		sort.Strings(want)
		if got := names(f.Medications, ids); !slices.Equal(got, want) {
			t.Errorf("%v medications %v, want %v", k, got, want)
		}
	}
}

// buildObs makes the observation the normaliser would have stored: the
// analyte's first LOINC (or the given one, or none for a local code), the
// unit by the unit table, the value converted by the conversions, a
// comparator kept apart with its bound.
func buildObs(t *testing.T, set goldenSet, now time.Time, id uuid.UUID, o goldenObs) rules.Observation {
	t.Helper()
	loinc := o.LOINC
	category := o.Category
	if o.Analyte != "" {
		a, ok := set.rs.Analyte(o.Analyte)
		if !ok {
			t.Fatalf("observation %s: no analyte %q", o.ID, o.Analyte)
		}
		loinc = a.LOINC[0]
		if category == "" && a.Category == "vital_signs" {
			category = "vital-signs"
		}
	}
	if category == "" {
		category = "laboratory"
	}
	status := o.Status
	if status == "" {
		status = "final"
	}
	obs := rules.Observation{ID: id, LOINC: loinc, Category: category, Status: status, SourceFlag: o.Flag,
		ValueText: o.Text, Superseded: o.Superseded, RefLow: num(t, o.RefLow), RefHigh: num(t, o.RefHigh)}
	obs.EffectiveAt, obs.EffectiveDate = at(t, now, o.At)
	ucum, unitOK := set.maps.ParseUnit(o.Unit)
	if o.Value != "" {
		v, cmp, err := normalise.ParseDecimal(o.Value, '.')
		if err != nil {
			t.Fatalf("observation %s: value %q", o.ID, o.Value)
		}
		if cmp == "" {
			obs.ValueNum = &v
		} else {
			obs.Comparator, obs.ValueText = cmp, strings.TrimSpace(o.Value)
		}
		if loinc != "" && unitOK {
			if cmp == "" {
				if si, ok := set.maps.Convert(loinc, v, ucum); ok {
					obs.ValueSI = &si
				}
			} else {
				obs.BoundNum, obs.BoundSI = rules.Bound(set.maps, loinc, ucum, obs.ValueText, '.')
			}
		}
	}
	if loinc != "" {
		if _, known := set.rs.AnalyteOf(loinc); !known && o.LOINC == "" {
			t.Fatalf("observation %s: LOINC %s is no analyte's", o.ID, loinc)
		}
	}
	obs.Standardised = loinc != "" && (obs.ValueNum == nil || obs.ValueSI != nil)
	return obs
}
