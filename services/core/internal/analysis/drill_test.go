package analysis

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

func shown(n int64) smallgroup.Guarded { return smallgroup.Guarded{Value: &n} }

var hidden = smallgroup.Guarded{Suppressed: true}

const (
	condA = "0b6c6d4e-8a9f-4f6e-b2d3-1c2a3b4c5d6e"
	condB = "6f1c1c3e-2c0b-4c55-9a52-0d2e5b0b7a11"
)

// drillBody is a guarded body with shown and hidden cells in every
// drillable list.
func drillBody() disclose.Body {
	var b disclose.Body
	b.Patients = shown(40)
	b.ByCondition = []disclose.ConditionRow[smallgroup.Guarded]{{Condition: condA, CNE: disclose.CNE[smallgroup.Guarded]{Cases: shown(30)}}}
	b.ByAge = []disclose.Part[smallgroup.Guarded]{{Key: "40-64", Patients: shown(12)}, {Key: "18-39", Patients: hidden}, {Key: "", Patients: shown(28)}}
	b.BySex = []disclose.Part[smallgroup.Guarded]{{Key: "female", Patients: shown(35)}, {Key: "male", Patients: shown(5)}}
	b.BySetting = []disclose.Part[smallgroup.Guarded]{{Key: "admitted", Patients: shown(40)}}
	b.ByLocation = []disclose.Part[smallgroup.Guarded]{{Key: condB, Setting: "admitted", Patients: shown(40)}}
	b.BySpecialty = []disclose.Part[smallgroup.Guarded]{{Key: "cardiology", Patients: shown(40)}}
	b.Pairs = []disclose.Pair[smallgroup.Guarded, smallgroup.GuardedShare]{
		{A: condA, B: condB, APatients: shown(30), Patients: shown(6)},
		{A: condB, B: condA, APatients: shown(30), Patients: shown(9)}, // not a < b: never drillable
	}
	b.Findings = []disclose.FindingRow[smallgroup.Guarded]{{Name: "", Flag: "high", Patients: shown(9)}, {Name: condA, Flag: "", Patients: hidden}}
	return b
}

func seededKeys(t *testing.T, seed byte) *Keys {
	t.Helper()
	ks := make([][]byte, 4)
	for i := range ks {
		ks[i] = bytes.Repeat([]byte{seed + byte(i)}, KeyLen)
	}
	k, err := NewKeys(ks[0], ks[1], ks[2], ks[3])
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// A8 and departure 19: drills only on shown counts of 5 or more of a
// group the rows describe; none on prevalence, a location or specialty
// row, or anything under a location, specialty or source filter, or in a
// body hidden whole.
func TestDrillGroupsOnlyRowVisibleShownFiveOrMore(t *testing.T) {
	b := drillBody()
	var keys []string
	for _, g := range DrillGroups(b, disclose.Filters{}) {
		keys = append(keys, string(g.View)+" "+g.RowKey)
		if g.Count < disclose.Min {
			t.Errorf("%s %s: count %d", g.View, g.RowKey, g.Count)
		}
	}
	want := "affected by_age:40-64,affected by_age:,affected by_sex:female,affected by_sex:male,affected by_setting:admitted," +
		"co-occurring " + condA + "|" + condB + ",findings |high"
	if strings.Join(keys, ",") != want {
		t.Errorf("drillable:\n%s\nwant\n%s", strings.Join(keys, ","), want)
	}
	for _, d := range []int{disclose.DimLocation, disclose.DimSpecialty, disclose.DimSource} {
		var f disclose.Filters
		f[d] = "x"
		if g := DrillGroups(b, f); len(g) != 0 {
			t.Errorf("under a %s filter: %d drills", disclose.DimNames[d], len(g))
		}
	}
	var f disclose.Filters
	f[disclose.DimCondition], f[disclose.DimSex] = condA, "female"
	if len(DrillGroups(b, f)) == 0 {
		t.Error("no drill under the row's own dimensions")
	}
	b.Whole = true
	if len(DrillGroups(b, disclose.Filters{})) != 0 {
		t.Error("a body hidden whole has drills")
	}
}

var pidIn = regexp.MustCompile(`(^|[^A-Za-z0-9])P-[A-Z0-9]{4,12}([^A-Za-z0-9]|$)`)

// Departure 13, N6, T10: a key is bound to the hospital, the period, the
// filters, the view and the row; another key set gives other keys; no
// keys, no drill. No key holds the pid's shape (P7).
func TestDrillKeysBound(t *testing.T) {
	k := seededKeys(t, 1)
	h, p := uuid.New(), uuid.New()
	g := DrillGroups(drillBody(), disclose.Filters{})[0]
	key := DrillKey(k, h, p, disclose.Filters{}, g)
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(key) || pidIn.MatchString(key) {
		t.Fatalf("key %q", key)
	}
	var f disclose.Filters
	f[disclose.DimSex] = "female"
	other := g
	other.RowKey = "by_sex:female"
	for name, k2 := range map[string]string{
		"hospital": DrillKey(k, uuid.New(), p, disclose.Filters{}, g),
		"period":   DrillKey(k, h, uuid.New(), disclose.Filters{}, g),
		"filters":  DrillKey(k, h, p, f, g),
		"row":      DrillKey(k, h, p, disclose.Filters{}, other),
		"key set":  DrillKey(seededKeys(t, 9), h, p, disclose.Filters{}, g),
	} {
		if k2 == key {
			t.Errorf("another %s, the same key", name)
		}
	}
	if DrillKey(nil, h, p, disclose.Filters{}, g) != "" {
		t.Error("a key without keys")
	}
	if got, ok := FindDrill(k, h, p, disclose.Filters{}, drillBody(), key); !ok || got.RowKey != g.RowKey {
		t.Errorf("FindDrill: %v %v", got.RowKey, ok)
	}
	for _, bad := range []string{DrillKey(k, uuid.New(), p, disclose.Filters{}, g), DrillKey(seededKeys(t, 9), h, p, disclose.Filters{}, g), ""} {
		if _, ok := FindDrill(k, h, p, disclose.Filters{}, drillBody(), bad); ok {
			t.Error("a foreign or rotated key found a group")
		}
	}
	// Over many contexts, no key holds the pid's shape.
	for i := 0; i < 20000; i++ {
		if key := DrillKey(k, uuid.New(), uuid.New(), disclose.Filters{}, g); key != "" && pidIn.MatchString(key) {
			t.Fatalf("key %q holds a pid's shape", key)
		}
	}
}

// SetDrills sets keys exactly on the drillable counts of the view, and
// leaves every other drill null; the same bytes each time.
func TestSetDrills(t *testing.T) {
	b := drillBody()
	k := seededKeys(t, 1)
	h, p := uuid.New(), uuid.New()
	for _, v := range disclose.Views {
		raw, err := renderView(b, v, names{conds: map[string]condName{}, locs: map[string]locName{}})
		if err != nil {
			t.Fatal(err)
		}
		key := func(g DrillGroup) string { return DrillKey(k, h, p, disclose.Filters{}, g) }
		out, err := SetDrills(raw, v, b, disclose.Filters{}, key)
		if err != nil {
			t.Fatal(err)
		}
		again, _ := SetDrills(raw, v, b, disclose.Filters{}, key)
		if !bytes.Equal(out, again) {
			t.Errorf("%s: two renders differ", v)
		}
		var o map[string]any
		if err := json.Unmarshal(out, &o); err != nil {
			t.Fatal(err)
		}
		n := countDrills(o)
		want := map[disclose.View]int{disclose.Affected: 5, disclose.CoOccurring: 1, disclose.Findings: 1}[v]
		if n != want {
			t.Errorf("%s: %d drills set, want %d", v, n, want)
		}
	}
}

func countDrills(v any) int {
	n := 0
	switch x := v.(type) {
	case map[string]any:
		if d, ok := x["drill"].(string); ok && d != "" {
			n++
		}
		for _, e := range x {
			n += countDrills(e)
		}
	case []any:
		for _, e := range x {
			n += countDrills(e)
		}
	}
	return n
}

// The reply's row has exactly the contract's keys (no name, MRN, birth
// date or address can appear).
func TestRowsCarryExactKeys(t *testing.T) {
	raw, err := json.Marshal(PatientRow{PID: "P-ABCDEFGH", Sex: "female", Conditions: []ConditionOf{{ID: condA}}, Findings: []FindingOf{{}}})
	if err != nil {
		t.Fatal(err)
	}
	var o map[string]any
	_ = json.Unmarshal(raw, &o)
	var keys []string
	for k := range o {
		keys = append(keys, k)
	}
	if len(o) != 6 || o["pid"] == nil || !hasKeys(o, "pid", "age_band", "sex", "setting", "conditions", "findings") {
		t.Errorf("row keys %v", keys)
	}
	f := o["findings"].([]any)[0].(map[string]any)
	if len(f) != 3 || !hasKeys(f, "name", "flag", "source_label") || f["source_label"] != nil {
		t.Errorf("finding %v", f)
	}
	c := o["conditions"].([]any)[0].(map[string]any)
	if len(c) != 3 || !hasKeys(c, "id", "name", "icd10") {
		t.Errorf("condition %v", c)
	}
}

func hasKeys(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}
