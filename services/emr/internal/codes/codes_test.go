package codes

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func repoDir(parts ...string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(append([]string{filepath.Dir(file), "..", ".."}, parts...)...)
}

// synthConn is the connection id of maps/testdata/synthetic/fhir.yaml.
var synthConn = uuid.MustParse("00000000-0000-4000-8000-00000000f41a")

func load(t *testing.T) (*Tables, *Maps) {
	t.Helper()
	tables, err := Load(repoDir("codes", "testdata"), true)
	if err != nil {
		t.Fatal(err)
	}
	maps, err := LoadMaps(repoDir("maps"), tables, true)
	if err != nil {
		t.Fatal(err)
	}
	return tables, maps
}

// A9: glucose in mg/dL converts by the factor the table gives, not one in
// the code.
func TestGlucoseConversion(t *testing.T) {
	_, m := load(t)
	got, ok := m.Convert("99901-1", 180, "mg/dL")
	if !ok || math.Abs(got-180*0.0555) > 1e-9 {
		t.Fatalf("180 mg/dL -> %v %v", got, ok)
	}
	if got, ok := m.Convert("99901-1", 5.5, "mmol/L"); !ok || got != 5.5 {
		t.Fatalf("canonical unit -> %v %v", got, ok)
	}
	// The same analyte with another factor in the file converts by it.
	dir := copyMaps(t)
	conv := filepath.Join(dir, "common", "conversions.yaml")
	b, _ := os.ReadFile(conv)
	if err := os.WriteFile(conv, []byte(strings.Replace(string(b), "factor: 0.0555", "factor: 0.05", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	tables, _ := load(t)
	m2, err := LoadMaps(dir, tables, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := m2.Convert("99901-1", 100, "mg/dL"); math.Abs(got-5) > 1e-9 {
		t.Fatalf("the table's factor not used: %v", got)
	}
	if m2.Version() == m.Version() {
		t.Error("a changed file left the mapping version as it was")
	}
}

// A9: no reviewed conversion, no value: potassium in mg/dL, an analyte with
// no row, NaN.
func TestNoConversionNoValueSI(t *testing.T) {
	_, m := load(t)
	for _, c := range []struct {
		loinc, unit string
		v           float64
	}{{"99902-9", "mg/dL", 18}, {"99917-7", "mmol/L", 120}, {"99901-1", "g/L", 1}, {"99901-1", "mmol/L", math.NaN()}} {
		if v, ok := m.Convert(c.loinc, c.v, c.unit); ok {
			t.Errorf("%s in %s converted to %v", c.loinc, c.unit, v)
		}
	}
	for in, want := range map[string]string{"mg/dl": "mg/dL", " mmol/l ": "mmol/L", "mg/dL": "mg/dL", "x10^9/L": "10*9/L"} {
		if got, ok := m.ParseUnit(in); !ok || got != want {
			t.Errorf("ParseUnit(%q) = %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"MG/DL", "mgdl", "mmol", "", "milligrams per decilitre"} {
		if got, ok := m.ParseUnit(in); ok {
			t.Errorf("ParseUnit(%q) guessed %q", in, got)
		}
	}
}

// A9: an unknown or deprecated LOINC is unknown.
func TestUnknownLOINCIsUnmapped(t *testing.T) {
	tables, m := load(t)
	for _, c := range []string{"12345-6", "99911-0", "", "99901-1 "} {
		if _, ok := tables.LOINC(c); ok {
			t.Errorf("LOINC %q known", c)
		}
	}
	if _, ok := tables.LOINC("99901-1"); !ok {
		t.Error("synthetic glucose unknown")
	}
	if l, ok := m.LOINC(synthConn, "urn:synthetic:lab", "GLU"); !ok || l != "99901-1" {
		t.Errorf("mapped GLU: %q %v", l, ok)
	}
	// Another connection's map is not this one's.
	if _, ok := m.LOINC(uuid.New(), "urn:synthetic:lab", "GLU"); ok {
		t.Error("a map applied to another connection")
	}
	if a, ok := m.ATC(synthConn, "urn:synthetic:drug", "FUROS"); !ok || a != "Z99AA01" {
		t.Errorf("mapped drug: %q %v", a, ok)
	}
}

// §9.4: a map naming a code outside the tables (or deprecated) is refused.
func TestMapWithUnknownCodeRefused(t *testing.T) {
	tables, _ := load(t)
	for name, entry := range map[string]string{
		"unknown LOINC":    `{system: "urn:x", code: "A", loinc: "12345-6"}`,
		"deprecated LOINC": `{system: "urn:x", code: "A", loinc: "99911-0"}`,
		"unknown ATC":      `{system: "urn:x", code: "A", atc: "C03CA01"}`,
		"both codes":       `{system: "urn:x", code: "A", loinc: "99901-1", atc: "Z99AA01"}`,
		"no code":          `{system: "urn:x", code: "A"}`,
		"unknown key":      `{system: "urn:x", code: "A", loinc: "99901-1", fuzzy: true}`,
	} {
		dir := copyMaps(t)
		f := "version: x\nhospital: SYN\nconnection: " + uuid.NewString() + "\nentries:\n  - " + entry + "\n"
		if err := os.WriteFile(filepath.Join(dir, "testdata", "bad.yaml"), []byte(f), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadMaps(dir, tables, true); !errors.Is(err, ErrMap) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
}

// K10: the synthetic tables and the unreviewed conversions are refused
// outside development; test maps are not read there at all.
func TestSyntheticTablesRefusedOutsideDevelopment(t *testing.T) {
	if _, err := Load(repoDir("codes", "testdata"), false); !errors.Is(err, ErrSynthetic) {
		t.Fatalf("synthetic tables outside development: %v", err)
	}
	tables, _ := load(t)
	if _, err := LoadMaps(repoDir("maps"), tables, false); !errors.Is(err, ErrSynthetic) {
		t.Fatalf("unreviewed conversions outside development: %v", err)
	}
	// One synthetic row makes the table synthetic.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "loinc.csv"), []byte("code,display,status,source\n1-8,X,ACTIVE,loinc\n2-6,Y,ACTIVE,synthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "atc.csv"), []byte("code,display,source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, false); !errors.Is(err, ErrSynthetic) {
		t.Fatalf("a mixed table outside development: %v", err)
	}
}

// §9.1: never a match on display text. The tables and maps answer by code
// only; a local code equal to a LOINC display maps to nothing.
func TestNoFuzzyMatch(t *testing.T) {
	tables, m := load(t)
	for _, s := range []string{"Glucose [synthetic]", "Glucose", "glucose", "GLUCOSE"} {
		if _, ok := tables.LOINC(s); ok {
			t.Errorf("table answered %q", s)
		}
		if _, ok := m.LOINC(synthConn, "urn:synthetic:lab", s); ok {
			t.Errorf("map answered %q", s)
		}
	}
	if _, ok := m.LOINC(synthConn, "urn:synthetic:lab", "glu"); ok {
		t.Error("a code matched in another case")
	}
}

// copyMaps copies the repository's maps to a temp directory.
func copyMaps(t *testing.T) string {
	t.Helper()
	src, dst := repoDir("maps"), t.TempDir()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}
