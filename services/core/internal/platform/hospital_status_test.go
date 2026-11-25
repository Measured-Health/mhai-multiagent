package platform_test

import (
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/platform"
)

func sp(s string) *string { return &s }

// The deactivation's reason and note, as mhai-web's sample API takes them.
func TestCheckDeactivation(t *testing.T) {
	for _, c := range []struct {
		reason string
		note   *string
		want   *string
	}{
		{"contract_ended", nil, nil},
		{"security_concern", sp("   "), nil},
		{"hospital_request", sp(" Synthetic note "), sp("Synthetic note")},
		{"other", sp("Synthetic\nreason"), sp("Synthetic\nreason")},
		{"other", sp(strings.Repeat("x", 500)), sp(strings.Repeat("x", 500))},
	} {
		got, ok := platform.CheckDeactivation(c.reason, c.note)
		if !ok || (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s %v: %v %v", c.reason, c.note, got, ok)
		}
	}
	for _, c := range []struct {
		reason string
		note   *string
	}{
		{"", nil}, {"bored", nil}, {"Contract_ended", nil}, {"other", nil}, {"other", sp(" \n ")},
		{"other", sp(strings.Repeat("x", 501))}, {"contract_ended", sp("bell\x07")}, {"contract_ended", sp("zero​width")},
		{"contract_ended", sp("see P-AB12CD")},
	} {
		if got, ok := platform.CheckDeactivation(c.reason, c.note); ok {
			t.Errorf("%q %q: accepted as %v", c.reason, *nonNil(c.note), got)
		}
	}
	if len(platform.DeactivateReasons) != len(platform.DeactivateReasonWords) {
		t.Error("every reason needs its words")
	}
	for _, r := range platform.DeactivateReasons {
		if platform.DeactivateReasonWords[r] == "" {
			t.Errorf("%s has no words", r)
		}
	}
}

func nonNil(s *string) *string {
	if s == nil {
		return sp("<nil>")
	}
	return s
}

func TestStatusText(t *testing.T) {
	if got, ok := platform.StatusText("  Synthetic renewal \n"); !ok || got != "Synthetic renewal" {
		t.Errorf("%q %v", got, ok)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("y", 501), "tab\tinside", "P-ZZ9999"} {
		if got, ok := platform.StatusText(bad); ok {
			t.Errorf("%q: accepted as %q", bad, got)
		}
	}
}
