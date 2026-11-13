package main

import (
	"bytes"
	"strings"
	"testing"
)

// The synthetic FHIR server is test-only: it refuses to start anywhere but
// APP_ENV=development, and checks its arguments.
func TestSynthFHIRDevelopmentOnly(t *testing.T) {
	args := []string{"synth-fhir", "--addr", "127.0.0.1:0", "--seed", "1", "--name", "fhir.synthetic.example", "--out", t.TempDir()}
	for _, env := range []string{"", "production", "staging", "Development"} {
		err := run(args, func(k string) string {
			if k == "APP_ENV" {
				return env
			}
			return ""
		}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "development") {
			t.Errorf("APP_ENV=%q: %v", env, err)
		}
	}
	dev := func(k string) string {
		if k == "APP_ENV" {
			return "development"
		}
		return ""
	}
	for _, bad := range [][]string{
		{"synth-fhir", "--addr", "127.0.0.1:0", "--seed", "1", "--name", "localhost", "--out", t.TempDir()},
		{"synth-fhir", "--addr", "127.0.0.1:0", "--seed", "x", "--name", "fhir.synthetic.example", "--out", t.TempDir()},
		{"synth-fhir", "--addr", "127.0.0.1:0", "--seed", "1", "--name", "fhir.synthetic.example"},
		{"synth-fhir", "--seed", "1", "--name", "fhir.synthetic.example", "--out", t.TempDir(), "--patients", "100000"},
	} {
		if err := run(bad, dev, &bytes.Buffer{}); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
