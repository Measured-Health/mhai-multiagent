package synth

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A late critical result (synth-fhir --late-critical): a new critical
// potassium for patient 1, served only a while after the first
// Observation search, so a first sync's backfill (historical by design,
// A18) is followed by a fresh result a live check can see raise an alert.
func TestLateCriticalIsServedOnlyFromItsTime(t *testing.T) {
	ps := GenerateAt(7, 3, time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	at := time.Date(2026, 11, 14, 9, 5, 0, 0, time.UTC)
	late := LateCritical(ps[0], at)
	if late["id"] != "syn-o-001-k-late" || lastUpdated(late) != "2026-11-14T09:05:00Z" {
		t.Fatalf("late observation: id %v, lastUpdated %v", late["id"], lastUpdated(late))
	}
	if !strings.Contains(late["effectiveDateTime"].(string), "2026-11-14T09:55") { // 10 min before, in +01:00
		t.Fatalf("taken: %v", late["effectiveDateTime"])
	}
	clock := at.Add(-3 * time.Minute)
	s := &store{opts: Options{Now: func() time.Time { return clock }, LateAfter: 3 * time.Minute,
		Late: func(t time.Time) []map[string]any { return []map[string]any{LateCritical(ps[0], t)} }},
		byType: map[string][]map[string]any{"Observation": ps[0].Observations}}
	search := func() string {
		w := httptest.NewRecorder()
		s.search(w, httptest.NewRequest("GET", "/r4/Observation?_lastUpdated=gt2026-11-14T00:00:00Z&_sort=_lastUpdated", nil), "Observation")
		return w.Body.String()
	}
	// The first search (the backfill) starts the clock and hasn't it.
	if strings.Contains(search(), "syn-o-001-k-late") {
		t.Fatal("served in the first search")
	}
	clock = at.Add(-time.Second)
	if strings.Contains(search(), "syn-o-001-k-late") {
		t.Fatal("served before its time")
	}
	clock = at
	got := search()
	if !strings.Contains(got, "syn-o-001-k-late") || !strings.Contains(got, `"lastUpdated":"2026-11-14T09:05:00Z"`) {
		t.Fatalf("not served at its time, last updated then: %s", got)
	}
	// Off by default: no LateAt, nothing late.
	s.opts = Options{Now: func() time.Time { return at.Add(time.Hour) }}
	if strings.Contains(search(), "syn-o-001-k-late") {
		t.Fatal("served with the option off")
	}
}
