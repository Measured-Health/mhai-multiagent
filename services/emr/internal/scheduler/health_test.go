package scheduler

import (
	"testing"
	"time"
)

// A23, the emr side (design §15.4): when a source counts as stale, by type.
func TestStaleRules(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	every := func(m int) *int { return &m }
	file := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	for name, c := range map[string]struct {
		typ      string
		lastOK   time.Time
		poll     int
		expected *int
		newFile  *time.Time
		stale    bool
	}{
		"fhir fresh":             {"fhir_r4", ago(5 * time.Minute), 120, nil, nil, false},
		"fhir at 3 polls":        {"fhir_r4", ago(6 * time.Minute), 120, nil, nil, false},
		"fhir past 3 polls":      {"fhir_r4", ago(6*time.Minute + time.Second), 120, nil, nil, true},
		"fhir never":             {"fhir_r4", time.Time{}, 120, nil, nil, true},
		"archive quiet":          {"document_archive", ago(2 * time.Hour), 3600, nil, file(10 * 24 * time.Hour), false},
		"archive silent":         {"document_archive", ago(4 * time.Hour), 3600, nil, nil, true},
		"export fresh":           {"file_export", ago(5 * time.Minute), 300, every(60), file(80 * time.Minute), false},
		"export missing file":    {"file_export", ago(5 * time.Minute), 300, every(60), file(91 * time.Minute), true},
		"export no interval":     {"file_export", ago(5 * time.Minute), 300, nil, file(time.Minute), true},
		"export never had file":  {"file_export", ago(5 * time.Minute), 300, every(60), nil, true},
		"export checks stopped":  {"file_export", ago(16 * time.Minute), 300, every(60), file(time.Minute), true},
		"unknown type":           {"hl7v2_feed", ago(time.Second), 120, nil, nil, true},
		"no poll interval":       {"fhir_r4", ago(time.Second), 0, nil, nil, true},
		"future last ok (clock)": {"fhir_r4", now.Add(time.Hour), 120, nil, nil, true},
	} {
		if got := Stale(c.typ, c.lastOK, c.poll, c.expected, c.newFile, now); got != c.stale {
			t.Errorf("%s: stale %v, want %v", name, got, c.stale)
		}
	}
}

// Design §7.1: the next poll after a run. ok: one interval, ±10%; a 429
// waits what it names (capped at 15 minutes, never sooner than a poll);
// other failures double from the interval up to 15 minutes.
func TestNextDue(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	poll := 120 * time.Second
	for name, c := range map[string]struct {
		code     string
		failures int
		ra       time.Duration
		jitter   float64
		want     time.Duration
	}{
		"ok":            {"ok", 0, 0, 0, poll},
		"ok jitter +":   {"ok", 0, 0, 1, 132 * time.Second},
		"ok jitter -":   {"ok", 0, 0, -1, 108 * time.Second},
		"429 120s":      {"too_many_requests", 1, 120 * time.Second, 1, 120 * time.Second},
		"429 30s":       {"too_many_requests", 1, 30 * time.Second, 0, poll},
		"429 a day":     {"too_many_requests", 1, 24 * time.Hour, 0, 15 * time.Minute},
		"429 unnamed":   {"too_many_requests", 2, 0, 0, 4 * poll},
		"5xx once":      {"unreachable", 1, 0, 0, 2 * poll},
		"5xx twice":     {"unreachable", 2, 0, 0, 4 * poll},
		"5xx 3 times":   {"unreachable", 3, 0, 0, 15 * time.Minute},
		"5xx 10 times":  {"unreachable", 10, 0, 0, 15 * time.Minute},
		"5xx 100 times": {"unreachable", 100, 0, 0, 15 * time.Minute},
		"sign in":       {"sign_in_refused", 1, 0, 0, 2 * poll},
	} {
		if got := NextDue(now, poll, c.code, c.failures, c.ra, c.jitter); !got.Equal(now.Add(c.want)) {
			t.Errorf("%s: due in %v, want %v", name, got.Sub(now), c.want)
		}
	}
	// An hourly source's interval is above 15 minutes: it never polls
	// sooner than its interval, nor later on failure.
	if got := NextDue(now, time.Hour, "unreachable", 5, 0, 0); !got.Equal(now.Add(time.Hour)) {
		t.Errorf("hourly cap: %v", got.Sub(now))
	}
}
