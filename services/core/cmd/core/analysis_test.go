package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// N2: the CLI only seeds the demo or lowers a switch; anything else, and
// any malformed argument, is refused before the database is touched.
func TestAnalysisCLIRefusesBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"seed-demo"},
		{"seed-demo", "hav", "--signoff", "SO-2026-0001"},
		{"seed-demo", "hav", "--months", "25"},
		{"seed-demo", "hav", "--months", "0"},
		{"seed-demo", "hav", "--seed", "x"},
		{"seed-demo", "hav", "extra"},
		{"seed-demo", "not a ref!"},
		{"switch", "on", "hav"},
		{"switch", "off"},
		{"switch", "off", "hav", "--global"},
		{"switch", "raise", "--global"},
		{"raise", "hav"},
		{"named-access", "on", "hav"},
		{"named-access", "letters_only", "hav"},
		{"named-access", "demo"},
		{"named-access", "demo", "--global"},
		{"named-access", "demo", "not a ref!"},
		{"named-access", "off"},
		{"named-access", "off", "hav", "--global"},
	} {
		var out bytes.Buffer
		err := analysisCmd(context.Background(), args, "", &out, nil)
		if err == nil {
			t.Errorf("%v: accepted", args)
			continue
		}
		if !errors.Is(err, errAnalysisArgs) {
			t.Errorf("%v: not the usage refusal (reached the database step?): %v", args, err)
		}
		if strings.Contains(err.Error(), "SO-2026") || out.Len() != 0 {
			t.Errorf("%v: echoed input: %v %q", args, err, out.String())
		}
	}
}
