package analytics

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// What changed (Plan 7c Task 8, spec §7.3): a change is flagged only on
// stored, shown cells, one period against one period. Fixed dates only.

// The week of 22–31 March 2026 (10 days) and February 2026 (28 days), the
// month that finished before it started.
var (
	chWeek  = calendar.Of(calendar.Week, day(2026, 3, 22))
	chMonth = calendar.Of(calendar.Month, day(2026, 2, 1))
)

// steady is a row whose every change measure is shown and above every
// minimum: q questions, c clinicians, 40 answers (needs review e of them),
// 40 reviews (agreed a), questions per clinician r.
func steady(q, c, e, a int64, r float64) Row {
	return Row{Questions: smallgroup.Count(q), Answers: smallgroup.Count(40), Pass: smallgroup.Count(40 - e),
		Regenerate: smallgroup.Count(0), Escalate: smallgroup.Count(e), Degraded: smallgroup.Count(0),
		PassShare: smallgroup.Share(40-e, 40), RegenerateShare: smallgroup.Share(0, 40), EscalateShare: smallgroup.Share(e, 40),
		Reviews: smallgroup.Count(40), AgreementRate: smallgroup.Share(a, 40), ActiveClinicians: smallgroup.Count(c),
		QuestionsPerClinician: RatioOf(r), MedianReviewHours: HoursOf(3.5)}
}

// pair is the week's and the month's bodies with the all row and hospital
// A as given.
func pair(weekA, monthA Row) (Body, Body) {
	w := bodyOf(chWeek, weekA, HospitalRow{Hospital: hospA, Staff: StaffFigure(9), Row: weekA})
	m := bodyOf(chMonth, monthA, HospitalRow{Hospital: hospA, Staff: StaffFigure(9), Row: monthA})
	return w, m
}

func flagsOf(fs []Flag, hospital bool, measure string) []Flag {
	var out []Flag
	for _, f := range fs {
		if (f.Hospital != nil) == hospital && f.Measure == measure {
			out = append(out, f)
		}
	}
	return out
}

// The Q3 table as decided (7 Nov 2026), and median review time not flagged
// (its minimum rests on a count no stored body holds).
func TestThresholdsAsDecided(t *testing.T) {
	want := map[string]struct {
		base      string
		min       int64
		change    float64
		perDay    bool
		up, down  string
		area      string
		shareRows bool
	}{
		"questions":          {"questions", 10, 0.5, true, "rising", "falling", "use", false},
		"needs_review_share": {"answers", 20, 0.15, false, "worse", "better", "quality", true},
		"agreement_rate":     {"reviews", 20, 0.15, false, "better", "worse", "quality", true},
	}
	if len(Thresholds) != len(want) {
		t.Fatalf("%d rows, want %d", len(Thresholds), len(want))
	}
	for _, th := range Thresholds {
		w, ok := want[th.Measure]
		if !ok || th.Base != w.base || th.MinBase != w.min || th.MinChange != w.change || th.PerDay != w.perDay ||
			th.Up != w.up || th.Down != w.down || th.Area != w.area || (th.Shape == ShapeShare) != w.shareRows {
			t.Errorf("%+v, want %+v", th, w)
		}
	}
	// Ruling C9: distinct clinicians over a week against a month's, and a
	// ratio over them, aren't like for like; not flagged.
	for _, m := range []string{"median_review_hours", "active_clinicians", "questions_per_clinician"} {
		if _, ok := ThresholdOf(m); ok {
			t.Errorf("%s is flagged", m)
		}
	}
}

// A12, F17: no change unless both sides, and every base of the minimum,
// are shown and at the minimum.
func TestChangeNeedsBothSidesShown(t *testing.T) {
	// Questions: 40 in the 10-day week against 56 in February's 28 days
	// (20.0 a week): up 100%.
	cur, prev := steady(40, 12, 10, 30, 3.33), steady(56, 12, 10, 30, 4.67)
	w, m := pair(cur, prev)
	if got := flagsOf(Changes(chWeek, w, chMonth, m), true, "questions"); len(got) != 1 || got[0].Direction != "rising" {
		t.Fatalf("the control flags nothing: %+v", got)
	}
	hide := func(r Row, f func(*Row)) Row { f(&r); return r }
	cases := map[string]struct {
		cur, prev Row
		measure   string
	}{
		"current hidden":       {hide(cur, func(r *Row) { r.Questions = hiddenCount }), prev, "questions"},
		"previous hidden":      {cur, hide(prev, func(r *Row) { r.Questions = hiddenCount }), "questions"},
		"previous not to show": {cur, hide(prev, func(r *Row) { r.Questions = smallgroup.Guarded{} }), "questions"},
		// 9 this week: under 10.
		"current under the minimum": {hide(cur, func(r *Row) { r.Questions = smallgroup.Count(9) }), hide(prev, func(r *Row) { r.Questions = smallgroup.Count(84) }), "questions"},
		// 20 in 28 days is 7.1 a 10-day week: under 10, though 20 is shown.
		"previous per day under the minimum": {hide(cur, func(r *Row) { r.Questions = smallgroup.Count(5) }), hide(prev, func(r *Row) { r.Questions = smallgroup.Count(20) }), "questions"},
		// The base of the shares: answers hidden on one side.
		"share base hidden":   {hide(cur, func(r *Row) { r.Answers = hiddenCount; r.EscalateShare = smallgroup.Share(30, 40) }), prev, "needs_review_share"},
		"share base under 20": {hide(cur, func(r *Row) { r.Answers = smallgroup.Count(19); r.EscalateShare = smallgroup.Share(15, 19) }), prev, "needs_review_share"},
		"share hidden":        {hide(cur, func(r *Row) { r.EscalateShare = hiddenShare }), prev, "needs_review_share"},
	}
	for name, c := range cases {
		w, m := pair(c.cur, c.prev)
		if got := flagsOf(Changes(chWeek, w, chMonth, m), true, c.measure); len(got) != 0 {
			t.Errorf("%s: flagged %+v", name, got)
		}
	}
	// A hospital the month doesn't list (onboarded since, or the first
	// month after the release): no change for it, the all row still.
	w, _ = pair(cur, prev)
	m = bodyOf(chMonth, prev)
	fs := Changes(chWeek, w, chMonth, m)
	if len(flagsOf(fs, true, "questions")) != 0 || len(flagsOf(fs, false, "questions")) != 1 {
		t.Errorf("no previous month: %+v", fs)
	}
	// Under the change thresholds: 29 against 20.0 a week is 45%.
	w, m = pair(hide(cur, func(r *Row) { r.Questions = smallgroup.Count(29) }), prev)
	if got := flagsOf(Changes(chWeek, w, chMonth, m), true, "questions"); len(got) != 0 {
		t.Errorf("a 45%% rise flagged: %+v", got)
	}
	// 14 points on a share: not flagged; 15: flagged worse.
	for _, tc := range []struct {
		e    int64
		want int
	}{{15, 0}, {16, 1}} {
		w, m = pair(hide(cur, func(r *Row) { r.EscalateShare = smallgroup.Share(tc.e, 40); r.Escalate = smallgroup.Count(tc.e) }), prev)
		got := flagsOf(Changes(chWeek, w, chMonth, m), true, "needs_review_share")
		if len(got) != tc.want || (tc.want == 1 && got[0].Direction != "worse") {
			t.Errorf("needs review %d/40 against 10/40: %+v", tc.e, got)
		}
	}
}

// A hidden cell's value never reaches the evaluator: a body holding one
// (a value under suppressed) flags nothing on it.
func TestChangeUsesStoredCellsOnly(t *testing.T) {
	big := int64(500)
	cur, prev := steady(40, 12, 10, 30, 3.33), steady(56, 12, 10, 30, 4.67)
	cur.Questions = smallgroup.Guarded{Value: &big, Suppressed: true}
	cur.ActiveClinicians = smallgroup.Guarded{Value: &big, Suppressed: true}
	share := 0.9
	cur.EscalateShare = smallgroup.GuardedShare{Share: &share, Suppressed: true}
	ratio := 99.0
	cur.QuestionsPerClinician = Ratio{Number{Value: &ratio, Suppressed: true}}
	w, m := pair(cur, prev)
	for _, f := range Changes(chWeek, w, chMonth, m) {
		switch f.Measure {
		case "questions", "needs_review_share":
			t.Errorf("flagged on a hidden value: %+v", f)
		}
	}
}

// previous is the month's shown cell per day, scaled to the week's length,
// one decimal: one period against one period.
func TestPreviousIsOnePeriod(t *testing.T) {
	cases := []struct {
		week  calendar.Period
		month calendar.Period
		prev  int64
		want  string
	}{
		// 22–31 March (10 days) against February 2026 (28 days): 56 → 20.0.
		{chWeek, chMonth, 56, "20.0"},
		// 1–7 April (7 days) against March (31 days): 100 → 22.6.
		{calendar.Of(calendar.Week, day(2026, 4, 1)), calendar.Of(calendar.Month, day(2026, 3, 1)), 100, "22.6"},
		// 22–30 April (9 days) against March: 31 → 9.0, under 10: none.
		{calendar.Of(calendar.Week, day(2026, 4, 22)), calendar.Of(calendar.Month, day(2026, 3, 1)), 31, ""},
	}
	for _, c := range cases {
		if !ComparedMonth(c.week).Equal(c.month) {
			t.Fatalf("%v compares %v, want %v", c.week, ComparedMonth(c.week), c.month)
		}
		cur, prev := steady(60, 12, 10, 30, 3), steady(c.prev, 12, 10, 30, 3)
		w := bodyOf(c.week, cur)
		m := bodyOf(c.month, prev)
		got := flagsOf(Changes(c.week, w, c.month, m), false, "questions")
		if c.want == "" {
			if len(got) != 0 {
				t.Errorf("%v: flagged %+v", c.week, got)
			}
			continue
		}
		if len(got) != 1 || got[0].Previous.Text() != c.want || got[0].Current.Text() != "60" {
			t.Errorf("%v: %+v, want previous %s", c.week, got, c.want)
		}
	}
	// Ruling C9: neither clinicians nor questions per clinician is
	// compared, whatever the figures.
	cur, prev := steady(40, 30, 10, 30, 9), steady(56, 12, 10, 30, 1)
	w, m := pair(cur, prev)
	for _, f := range Changes(chWeek, w, chMonth, m) {
		if f.Measure == "active_clinicians" || f.Measure == "questions_per_clinician" {
			t.Errorf("flagged %+v", f)
		}
	}
}

var numberIn = regexp.MustCompile(`[0-9]+(\.[0-9]+)?`)

// A12: the summary holds the two shown values and nothing else: no
// hospital name (even one with a digit in it), no other number.
func TestSummaryWording(t *testing.T) {
	named := Ref{ID: hospA.ID, Name: "Synthetic 7 Hospital"}
	cur, prev := steady(40, 30, 30, 10, 3.33), steady(56, 12, 10, 30, 4.67)
	w := bodyOf(chWeek, cur, HospitalRow{Hospital: named, Staff: StaffFigure(9), Row: cur})
	m := bodyOf(chMonth, prev, HospitalRow{Hospital: named, Staff: StaffFigure(9), Row: prev})
	fs := Changes(chWeek, w, chMonth, m)
	seen := map[string]bool{}
	for _, f := range fs {
		seen[f.Measure] = true
		if strings.Contains(f.Summary, "Synthetic") || strings.Contains(f.Summary, "7 Hospital") {
			t.Errorf("%s names the hospital", f.Summary)
		}
		nums := numberIn.FindAllString(f.Summary, -1)
		th, _ := ThresholdOf(f.Measure)
		want := []string{f.Previous.Text(), f.Current.Text()}
		if th.Shape == ShapeShare {
			want = []string{Shown{f.Previous.V, f.Previous.Digits - 2}.Text(), Shown{f.Current.V, f.Current.Digits - 2}.Text()}
		}
		if len(nums) != 2 || nums[0] != want[0] || nums[1] != want[1] {
			t.Errorf("%q: numbers %v, want %v", f.Summary, nums, want)
		}
		if len(f.Summary) > 200 {
			t.Errorf("%q is over 200", f.Summary)
		}
	}
	for _, m := range []string{"questions", "needs_review_share", "agreement_rate"} {
		if !seen[m] {
			t.Errorf("the fixture flags no %s: the wording isn't tested for it", m)
		}
	}
	if got := flagsOf(fs, false, "questions"); len(got) != 1 || got[0].Summary != "Questions up: 20.0 to 40 a week" {
		t.Errorf("questions summary: %+v", got)
	}
	if got := flagsOf(fs, false, "needs_review_share"); len(got) != 1 || got[0].Summary != "Needs-review share worse: 25.00% to 75.00%" {
		t.Errorf("share summary: %+v", got)
	}
}

func changeRow(id, measure, dir, cur, prev string, h *Ref) ChangeRow {
	th, _ := ThresholdOf(measure)
	return ChangeRow{ID: id, Hospital: h, Area: th.Area, Measure: measure, Direction: dir, Current: []byte(cur),
		Previous: []byte(prev), Summary: "Synthetic summary", Version: 1}
}

// The changes area: unavailable with no run; the window; items not
// followed up first, then by size; by_you per caller; a stored row that
// isn't a shown value left out.
func TestRenderChanges(t *testing.T) {
	if got, _ := RenderChanges(nil, nil, ""); marshal(t, got)["unavailable"] != true {
		t.Fatalf("no run: %v", got)
	}
	run := &ChangesRun{Week: chWeek, Month: chMonth, EvaluatedAt: day(2026, 4, 1).Add(20 * time.Minute)}
	author := "0b6c3d34-5b0e-4f55-9b7e-0000000000f1"
	note := "Synthetic note"
	small := changeRow("00000000-0000-4000-8000-000000000001", "questions", "rising", `{"value": 31, "suppressed": false}`, `{"value": 20.0, "suppressed": false}`, nil)
	large := changeRow("00000000-0000-4000-8000-000000000002", "questions", "falling", `{"value": 10, "suppressed": false}`, `{"value": 40.0, "suppressed": false}`, &hospA)
	followed := changeRow("00000000-0000-4000-8000-000000000003", "agreement_rate", "worse", `{"value": 0.1000, "suppressed": false}`, `{"value": 0.9000, "suppressed": false}`, &hospB)
	followed.FollowUp = &FollowUpRow{By: Ref{ID: author, Name: "Dr Synthetic Admin"}, At: day(2026, 4, 2), Note: &note, Version: 2}
	hidden := changeRow("00000000-0000-4000-8000-000000000004", "questions", "rising", `{"value": null, "suppressed": true}`, `{"value": 20.0, "suppressed": false}`, nil)
	extra := changeRow("00000000-0000-4000-8000-000000000005", "questions", "rising", `{"value": 31, "suppressed": false, "raw": 3}`, `{"value": 20.0, "suppressed": false}`, nil)
	badDir := changeRow("00000000-0000-4000-8000-000000000006", "questions", "worse", `{"value": 31, "suppressed": false}`, `{"value": 20.0, "suppressed": false}`, nil)
	got, degraded := RenderChanges(run, []ChangeRow{followed, small, hidden, large, extra, badDir}, author)
	if !degraded {
		t.Error("rows left out, not reported")
	}
	m := marshal(t, got)
	if keys(m) != "items,updated_at,window" || m["updated_at"] != "2026-04-01T00:20:00.000Z" {
		t.Fatalf("area %v", m)
	}
	if win := m["window"].(map[string]any); win["from"] != "2026-02-01T00:00:00.000Z" || win["to"] != "2026-04-01T00:00:00.000Z" {
		t.Errorf("window %v", win)
	}
	items := m["items"].([]any)
	var ids []string
	for _, x := range items {
		it := x.(map[string]any)
		ids = append(ids, it["id"].(string)[35:])
		if keys(it) != "area,current,direction,follow_up,hospital,id,measure,previous,serious,summary,version" {
			t.Errorf("item keys %s", keys(it))
		}
	}
	if strings.Join(ids, ",") != "2,1,3" {
		t.Errorf("order %v, want the larger change first and the followed-up last", ids)
	}
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), `"previous":{"value":20.0,"suppressed":false},"current":{"value":31,"suppressed":false}`) {
		t.Errorf("values not as stored: %s", raw)
	}
	fu := items[2].(map[string]any)["follow_up"].(map[string]any)
	if fu["by_you"] != true || fu["note"] != note || fu["version"] != float64(2) {
		t.Errorf("follow-up for its author: %v", fu)
	}
	other, _ := RenderChanges(run, []ChangeRow{followed}, "0b6c3d34-5b0e-4f55-9b7e-0000000000f2")
	if marshal(t, other)["items"].([]any)[0].(map[string]any)["follow_up"].(map[string]any)["by_you"] != false {
		t.Error("by_you for another caller")
	}
	empty, _ := RenderChanges(run, nil, author)
	if string(mustMarshal(t, marshal(t, empty)["items"])) != "[]" {
		t.Errorf("no changes: %v", marshal(t, empty)["items"])
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The change series: a hospital's cell with the others' median (a count
// median between 0 and 5 null), or the all cells with no median (F14).
func TestChangeSeries(t *testing.T) {
	rows := history(t, calendar.Month, day(2026, 6, 1), 3)
	all, err := ChangeSeries(calendar.Month, rows, "00000000-0000-4000-8000-000000000001", nil, "questions")
	if err != nil {
		t.Fatal(err)
	}
	m := marshal(t, all)
	if m["hospital"] != nil || m["measure"] != "questions" || m["change_id"] != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("all: %v", m)
	}
	for _, p := range m["points"].([]any) {
		pt := p.(map[string]any)
		if pt["median"] != nil || pt["value"].(map[string]any)["value"] != float64(48) {
			t.Errorf("all point %v", pt)
		}
	}
	a, err := ChangeSeries(calendar.Month, rows, "00000000-0000-4000-8000-000000000002", &hospA, "questions")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range marshal(t, a)["points"].([]any) {
		pt := p.(map[string]any)
		// A's 12; the others shown: B's 24 (C hidden) → 24.
		if pt["value"].(map[string]any)["value"] != float64(12) || pt["median"] != float64(24) {
			t.Errorf("A point %v", pt)
		}
	}
	// B against A's 6 and hidden C: 6. A count median of 2.5 is null.
	for _, p := range history(t, calendar.Month, day(2026, 6, 1), 1) {
		b := Body{}
		_ = json.Unmarshal(p.Body, &b)
		b.Hospitals[0].Row.Questions = smallgroup.Count(0)
		b.Hospitals[2].Row.Questions = smallgroup.Count(5)
		s := snap(t, b, p.Period)
		got, _ := ChangeSeries(calendar.Month, []Snapshot{s}, "00000000-0000-4000-8000-000000000003", &hospB, "questions")
		if pt := marshal(t, got)["points"].([]any)[0].(map[string]any); pt["median"] != nil {
			t.Errorf("a count median of 2.5 sent: %v", pt)
		}
	}
	for _, m := range []string{"median_review_hours", "active_clinicians", "questions_per_clinician"} {
		if _, err := ChangeSeries(calendar.Month, rows, "00000000-0000-4000-8000-000000000004", nil, m); err == nil {
			t.Errorf("%s: a measure no change has", m)
		}
	}
}
