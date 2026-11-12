package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/codes"
)

// emr rules check prints the version and hash a platform admin activates;
// a synthetic set is refused outside development.
func TestRulesCheck(t *testing.T) {
	v1 := filepath.Join("..", "..", "rules", "ruleset-1.yaml")
	b, err := os.ReadFile(v1)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	var out bytes.Buffer
	if err := run([]string{"rules", "check", v1}, func(string) string { return "production" }, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"version: 1\n", "status: pilot\n", "clinical_signoff: deferred\n", "sha256: " + hex.EncodeToString(sum[:])} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q has no %q", out.String(), want)
		}
	}
	synth := filepath.Join("..", "..", "rules", "testdata", "ruleset-0-synthetic.yaml")
	if err := run([]string{"rules", "check", synth}, func(string) string { return "production" }, &out); err == nil {
		t.Fatal("the synthetic set checked in production")
	}
	env := func(k string) string {
		if k == "APP_ENV" {
			return "development"
		}
		return ""
	}
	if err := run([]string{"rules", "check", synth}, env, &out); err != nil {
		t.Fatalf("synthetic in development: %v", err)
	}
}

// The service refuses a rule set whose analyte unit differs from the
// conversions' canonical unit, and a synthetic set outside development.
func TestLoadRulesChecksUnits(t *testing.T) {
	tables, err := codes.Load(filepath.Join("..", "..", "codes", "testdata"), true)
	if err != nil {
		t.Fatal(err)
	}
	maps, err := codes.LoadMaps(filepath.Join("..", "..", "maps"), tables, true)
	if err != nil {
		t.Fatal(err)
	}
	synth := filepath.Join("..", "..", "rules", "testdata", "ruleset-0-synthetic.yaml")
	if _, missing, err := loadRules(synth, maps, true); err != nil || len(missing) != 0 {
		t.Fatalf("synthetic: %v %v", missing, err)
	}
	if _, _, err := loadRules(synth, maps, false); err == nil || !strings.Contains(err.Error(), "development only") {
		t.Fatalf("synthetic outside development: %v", err)
	}
	b, err := os.ReadFile(synth)
	if err != nil {
		t.Fatal(err)
	}
	wrong := filepath.Join(t.TempDir(), "ruleset-0-synthetic.yaml")
	if err := os.WriteFile(wrong, []byte(strings.Replace(string(b), `loinc: ["99901-1"], unit: mmol/L`, `loinc: ["99901-1"], unit: mg/dL`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadRules(wrong, maps, true); err == nil || !strings.Contains(err.Error(), "unit") {
		t.Fatalf("glucose in mg/dL: %v", err)
	}
	// v1 against the synthetic conversions: its real codes aren't listed
	// (Task 21), so it loads with every code missing a conversion.
	if _, missing, err := loadRules(filepath.Join("..", "..", "rules", "ruleset-1.yaml"), maps, false); err != nil || len(missing) == 0 {
		t.Fatalf("v1: %v %v", missing, err)
	}
}
