package disclose

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

var (
	oct2026 = calendar.Of(calendar.Month, day("2026-10-01"))
	octWeek = calendar.Of(calendar.Week, day("2026-10-01"))
)

// flat is a context with n patients in one row of every partition and
// nothing else: a base the unit tests change.
func flat(p calendar.Period, n int64) Raw {
	var r Raw
	r.Context.Period = p
	r.Patients = n
	r.ByAge = []Part[int64]{{Key: "18-39", Patients: n}}
	r.BySex = []Part[int64]{{Key: "female", Patients: n}}
	r.BySetting = []Part[int64]{{Key: "admitted", Patients: n}}
	r.ByLocation = []Part[int64]{{Key: "L1", Setting: "admitted", Patients: n}}
	r.BySpecialty = []Part[int64]{{Key: "S1", Patients: n}}
	for _, k := range periodsOfSeries(p) {
		r.Series = append(r.Series, Point[int64]{Start: k.From.Format(time.DateOnly)})
	}
	return r
}

func shown(g smallgroup.Guarded, v int64) bool { return g.Shown() && *g.Value == v }

// T1: female 212, male 3, total 215: male and the next-smallest member
// are hidden.
func TestT1FemaleMaleTotal(t *testing.T) {
	r := flat(oct2026, 215)
	r.BySex = []Part[int64]{{Key: "female", Patients: 212}, {Key: "male", Patients: 3}}
	b, err := GuardResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !shown(b.Patients, 215) {
		t.Fatalf("patients: %+v", b.Patients)
	}
	for _, p := range b.BySex {
		if p.Patients.Shown() {
			t.Errorf("by_sex %s shown: 215 − it gives the other away", p.Key)
		}
	}
	if b.BySex[0].Key != "female" || b.BySex[1].Key != "male" {
		t.Errorf("hidden rows in key order: %+v", b.BySex)
	}
}

// T3: a pair whose "both" is hidden comes after every shown pair.
func TestT3HiddenPairAfterShown(t *testing.T) {
	r := flat(oct2026, 40)
	r.Pairs = []Pair[int64, struct{}]{
		{A: "c1", B: "c3", APatients: 20, Patients: 2},
		{A: "c1", B: "c2", APatients: 20, Patients: 7},
		{A: "c2", B: "c3", APatients: 15, Patients: 9},
	}
	b, err := GuardResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	seenHidden := false
	for _, p := range b.Pairs {
		if !p.Patients.Shown() {
			seenHidden = true
		} else if seenHidden {
			t.Fatalf("a shown pair after a hidden one: %+v", b.Pairs)
		}
		if !p.Patients.Shown() && p.ShareOfA.Shown() {
			t.Errorf("a share beside a hidden count: %+v", p)
		}
	}
	if !seenHidden || b.Pairs[0].B != "c3" || b.Pairs[0].A != "c2" {
		t.Errorf("order: %+v", b.Pairs)
	}
}

// T7: a filter set matching 7 patients: everything hidden, every list
// empty, and the JSON says so.
func TestT7NarrowViewEmptyLists(t *testing.T) {
	r := flat(oct2026, 7)
	r.Context.Filters[DimAgeBand] = "18-39"
	r.Pairs = []Pair[int64, struct{}]{{A: "c1", B: "c2", APatients: 7, Patients: 7}}
	b, err := GuardResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Whole || b.Patients.Shown() || len(b.BySex)+len(b.Pairs)+len(b.Series)+len(b.ByCondition) != 0 {
		t.Fatalf("not hidden whole: %+v", b)
	}
	for _, v := range Views {
		j, err := ViewJSON(b, v)
		if err != nil {
			t.Fatal(err)
		}
		if s := string(j); !contains2(s, `"patients":{"value":null,"suppressed":true}`) {
			t.Errorf("%s: %s", v, s)
		}
	}
	// Unfiltered, 7 patients is shown (QuerySetMin is for filtered views).
	r.Context.Filters = Filters{}
	if b, _ := GuardResponse(r, nil); b.Whole || !shown(b.Patients, 7) {
		t.Errorf("unfiltered 7: %+v", b.Patients)
	}
	// 1 to 4 patients: hidden whole, filtered or not.
	if b, _ := GuardResponse(flat(oct2026, 3), nil); !b.Whole {
		t.Error("3 patients aren't hidden whole")
	}
}

func contains2(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// by_condition adds up to the total, so a hidden row never stands alone
// against it.
func TestByConditionSumsToTotal(t *testing.T) {
	r := flat(oct2026, 30)
	r.ByCondition = []ConditionRow[int64]{{"c1", CNE[int64]{20, 10, 10}}, {"c2", CNE[int64]{3, 1, 2}}}
	r.Total = CNE[int64]{23, 11, 12}
	r.Series[0] = Point[int64]{r.Series[0].Start, 23, 11}
	b, err := GuardResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	var c1, c2 ConditionRow[smallgroup.Guarded]
	for _, x := range b.ByCondition {
		if x.Condition == "c1" {
			c1 = x
		} else {
			c2 = x
		}
	}
	if c2.Cases.Shown() || (c1.Cases.Shown() && b.Total.Cases.Shown()) {
		t.Errorf("c2's 3 cases follow from the total: total %+v, c1 %+v, c2 %+v", b.Total.Cases, c1.Cases, c2.Cases)
	}
	// Rows that don't add up to the total: inconsistent, hidden whole.
	r.Total.Cases = 24
	r.Total.Existing = 13
	if b, err := GuardResponse(r, nil); !errors.Is(err, ErrInconsistent) || !b.Whole {
		t.Errorf("a total that isn't the rows' sum: %v", err)
	}
}

// The total is shared by the condition table and the series table: a
// total cell forced hidden in one is hidden in the other, and its row set
// hides a second member.
func TestSeriesSharedTotalForcedBothWays(t *testing.T) {
	r := flat(oct2026, 40)
	r.ByCondition = []ConditionRow[int64]{{"c1", CNE[int64]{20, 10, 10}}, {"c2", CNE[int64]{20, 9, 11}}}
	r.Total = CNE[int64]{40, 19, 21}
	r.Series[0] = Point[int64]{r.Series[0].Start, 25, 12}
	r.Series[1] = Point[int64]{r.Series[1].Start, 15, 7}
	sh, _, _, _ := layout(&r)
	var g string
	for _, c := range sh.cells {
		if c.path == "total.new" {
			g = c.group
		}
	}
	b, err := GuardResponse(r, Forced{g: true})
	if err != nil {
		t.Fatal(err)
	}
	if b.Total.New.Shown() || b.Total.Existing.Shown() {
		t.Fatalf("forced total.new: %+v", b.Total)
	}
	news, rows := 0, 0
	for _, s := range b.Series {
		if !s.New.Shown() {
			news++
		}
	}
	for _, c := range b.ByCondition {
		if !c.New.Shown() {
			rows++
		}
	}
	if news == 0 || rows == 0 {
		t.Errorf("a hidden total.new with every series (%d) or condition (%d) new shown gives it back", news, rows)
	}
}

func TestMedianHalfAndHidden(t *testing.T) {
	m := median([]int64{6, 1, 2, 5, 3, 4}, false)
	if m.Value == nil || *m.Value != 3.5 {
		t.Errorf("median of 1..6: %+v", m)
	}
	if m := median([]int64{1, 2, 3}, false); !m.Suppressed {
		t.Error("a median of 3 values is shown")
	}
	if m := median([]int64{1, 2, 3, 4, 5}, true); !m.Suppressed {
		t.Error("a median over a hidden count is shown")
	}
	if m := median(nil, false); m.Suppressed || m.Value != nil {
		t.Error("no values: nothing to measure")
	}
	j, _ := GuardedMinutes{Value: m.Value}.MarshalJSON()
	if string(j) != `{"value":3.5,"suppressed":false}` {
		t.Errorf("JSON: %s", j)
	}
}

func TestNoRepeatedKeys(t *testing.T) {
	r := flat(oct2026, 20)
	r.BySex = []Part[int64]{{Key: "female", Patients: 10}, {Key: "female", Patients: 10}}
	if b, err := GuardResponse(r, nil); !errors.Is(err, ErrInconsistent) || !b.Whole {
		t.Errorf("a repeated key: %v", err)
	}
	r = flat(oct2026, 20)
	r.Pairs = []Pair[int64, struct{}]{{A: "c1", B: "c2", APatients: 9, Patients: 6}, {A: "c2", B: "c1", APatients: 8, Patients: 6}}
	if _, err := GuardResponse(r, nil); !errors.Is(err, ErrInconsistent) {
		t.Errorf("a pair listed both ways round: %v", err)
	}
}

func TestInconsistentInputHidesAll(t *testing.T) {
	for name, edit := range map[string]func(*Raw){
		"partition":   func(r *Raw) { r.BySex[0].Patients = 19 },
		"negative":    func(r *Raw) { r.Alerts.Total, r.Alerts.Critical = -1, -1 },
		"pair over a": func(r *Raw) { r.Pairs = []Pair[int64, struct{}]{{A: "c1", B: "c2", APatients: 5, Patients: 6}} },
		"agreed": func(r *Raw) {
			r.Quality = []QualityRow[int64, struct{}]{{Condition: "c1", Consultations: 6, High: 6, Rated: 2, Agreed: 3}}
		},
		"series":        func(r *Raw) { r.Series = r.Series[:1] },
		"median values": func(r *Raw) { r.Alerts.MedianAck = []int64{1} },
		"filter":        func(r *Raw) { r.Context.Filters[DimSex] = "unknown" },
	} {
		r := flat(oct2026, 20)
		edit(&r)
		b, err := GuardResponse(r, nil)
		if err == nil || !b.Whole {
			t.Errorf("%s: %v, whole %v", name, err, b.Whole)
		}
	}
}

func TestDrillableOnlyShownFiveOrMore(t *testing.T) {
	r := flat(oct2026, 215)
	r.BySex = []Part[int64]{{Key: "female", Patients: 200}, {Key: "male", Patients: 9}, {Key: "other", Patients: 6}}
	b, _ := GuardResponse(r, nil)
	if !Drillable(b, Affected, "by_sex:female") || !Drillable(b, Affected, "by_age:18-39") {
		t.Error("a shown count of 5 or more isn't drillable")
	}
	r.BySex = []Part[int64]{{Key: "female", Patients: 212}, {Key: "male", Patients: 3}}
	b, _ = GuardResponse(r, nil)
	if Drillable(b, Affected, "by_sex:female") || Drillable(b, Affected, "by_sex:male") || Drillable(b, Prevalence, "nothing") {
		t.Error("a hidden count is drillable")
	}
	if Drillable(HiddenWhole(r.Context), Affected, "by_age:18-39") {
		t.Error("a body hidden whole is drillable")
	}
}

// long reports whether the full-size property runs are asked for.
func long() bool { return os.Getenv("ANALYSIS_LONG") == "1" }
