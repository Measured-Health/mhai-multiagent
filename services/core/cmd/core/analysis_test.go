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
		{"seed-demo", "plt", "--signoff", "SO-2026-0001"},
		{"seed-demo", "plt", "--months", "25"},
		{"seed-demo", "plt", "--months", "0"},
		{"seed-demo", "plt", "--seed", "x"},
		{"seed-demo", "plt", "extra"},
		{"seed-demo", "not a ref!"},
		{"switch", "on", "plt"},
		{"switch", "off"},
		{"switch", "off", "plt", "--global"},
		{"switch", "raise", "--global"},
		{"raise", "plt"},
		{"named-access", "on", "plt"},
		{"named-access", "letters_only", "plt"},
		{"named-access", "demo"},
		{"named-access", "demo", "--global"},
		{"named-access", "demo", "not a ref!"},
		{"named-access", "demo-on"},
		{"named-access", "demo-on", "--global"},
		{"named-access", "demo-on", "not a ref!"},
		{"named-access", "off"},
		{"named-access", "off", "plt", "--global"},
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
