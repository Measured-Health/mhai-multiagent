package platform

import "testing"

// A preview is rounded up to a multiple of 10, never down, and 0 stays 0.
func TestPreviewRound(t *testing.T) {
	for in, want := range map[int64]int64{0: 0, -1: 0, 1: 10, 9: 10, 10: 10, 11: 20, 4100: 4100, 4101: 4110} {
		if got := PreviewRound(in); got != want {
			t.Errorf("PreviewRound(%d) = %d, want %d", in, got, want)
		}
	}
}

// A reason is trimmed, 1 to max characters, with no control, format or
// separator character but a newline (mhai-web's NOTE rule).
func TestSettingReason(t *testing.T) {
	for in, want := range map[string]bool{"Synthetic": true, "  two\nlines  ": true, "": false, "   ": false,
		"bell\a": false, "tab\there": false, "zero​width": false, "line sep": false, "para sep": false} {
		if _, ok := settingReason(in, 500); ok != want {
			t.Errorf("settingReason(%q) = %v", in, ok)
		}
	}
	if got, ok := settingReason("  x  ", 1); !ok || got != "x" {
		t.Errorf("trimmed %q %v", got, ok)
	}
	if _, ok := settingReason("ab", 1); ok {
		t.Error("over the limit")
	}
	if _, ok := settingReason("éé", 2); !ok {
		t.Error("characters, not bytes")
	}
}

// Periods in mhai-web's words.
func TestDaysWords(t *testing.T) {
	for in, want := range map[int]string{365: "1 year", 2190: "6 years", 30: "1 month", 180: "6 months", 45: "45 days", 1: "1 day"} {
		if got := DaysWords(in); got != want {
			t.Errorf("DaysWords(%d) = %q, want %q", in, got, want)
		}
	}
}
