package platform

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Fix round 2: no active hospital, and one, give a whole response (the
// all row is four columns wide whatever the rows are), not a panic; and
// what is stored decodes back to the same bytes.
func TestGuardOutEmptyAndOne(t *testing.T) {
	to := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	from := to.AddDate(0, 0, -7)
	for name, rows := range map[string][]guardRow{
		"none":  nil,
		"one":   {{id: uuid.New(), name: "Synthetic General", answers: 12, pass: 5, regenerate: 7, degraded: 6}},
		"small": {{id: uuid.New(), name: "Synthetic Small", answers: 3, pass: 3, degraded: 1}},
	} {
		out, err := Deps{}.guardOut("7d", from, to, rows)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(out.Hospitals) != len(rows) || out.Hospitals == nil {
			t.Errorf("%s: %d hospitals", name, len(out.Hospitals))
		}
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		back, err := decodeSnapshot(b, "7d", from, to)
		if err != nil {
			t.Fatalf("%s: stored body refused: %v (%s)", name, err, b)
		}
		b2, _ := json.Marshal(back)
		if string(b) != string(b2) {
			t.Errorf("%s: round trip\n%s\n%s", name, b, b2)
		}
	}
	// A stored body for another period, day or with an unknown key is refused.
	out, _ := Deps{}.guardOut("7d", from, to, nil)
	b, _ := json.Marshal(out)
	if _, err := decodeSnapshot(b, "30d", from, to); err == nil {
		t.Error("another period accepted")
	}
	if _, err := decodeSnapshot(b, "7d", from.AddDate(0, 0, 1), to.AddDate(0, 0, 1)); err == nil {
		t.Error("another day accepted")
	}
	if _, err := decodeSnapshot([]byte(`{"x":1}`), "7d", from, to); err == nil {
		t.Error("an unknown key accepted")
	}
}

// R43: the window ends at the start of the UTC day 15 minutes before now.
func TestGuardWindowGrace(t *testing.T) {
	day := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		now  time.Time
		want time.Time
	}{
		{day.Add(5 * time.Minute), day.AddDate(0, 0, -1)},
		{day.Add(14*time.Minute + 59*time.Second), day.AddDate(0, 0, -1)},
		{day.Add(15 * time.Minute), day},
		{day.Add(23 * time.Hour), day},
	} {
		from, to := guardWindow(c.now, 30)
		if !to.Equal(c.want) || !from.Equal(c.want.AddDate(0, 0, -30)) {
			t.Errorf("at %v: from %v to %v, want to %v", c.now, from, to, c.want)
		}
	}
}
