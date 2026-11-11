package analytics

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// Plan 7c Tasks 6 and 7 (spec §7.1, §7.2): the analytics reads render from
// stored bodies only. Every date here is fixed: nothing reads the clock.

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

var (
	hospA = Ref{ID: "0b6c3d34-5b0e-4f55-9b7e-0000000000a1", Name: "Synthetic North"}
	hospB = Ref{ID: "0b6c3d34-5b0e-4f55-9b7e-0000000000b2", Name: "Synthetic South"}
	hospC = Ref{ID: "0b6c3d34-5b0e-4f55-9b7e-0000000000c3", Name: "Synthetic East"}
)

var hiddenCount = smallgroup.Guarded{Suppressed: true}
var hiddenShare = smallgroup.GuardedShare{Suppressed: true}

// fullRow is a row with every figure shown, scaled by n.
func fullRow(n int64) Row {
	return Row{Questions: smallgroup.Count(12 * n), Answers: smallgroup.Count(10 * n), Pass: smallgroup.Count(5 * n),
		Regenerate: smallgroup.Count(0), Escalate: smallgroup.Count(5 * n), Degraded: smallgroup.Count(0),
		PassShare: smallgroup.Share(5*n, 10*n), RegenerateShare: smallgroup.Share(0, 10*n), EscalateShare: smallgroup.Share(5*n, 10*n),
		Reviews: smallgroup.Count(5 * n), AgreementRate: smallgroup.Share(5*n, 5*n), ActiveClinicians: smallgroup.Count(6 * n),
		QuestionsPerClinician: RatioOf(2), MedianReviewHours: HoursOf(float64(n) + 0.5)}
}

// hiddenRow hides the use figures and the escalations.
func hiddenRow() Row {
	r := fullRow(1)
	r.Questions, r.ActiveClinicians, r.Escalate, r.EscalateShare, r.PassShare = hiddenCount, hiddenCount, hiddenCount, hiddenShare, hiddenShare
	r.QuestionsPerClinician = Ratio{Number{Suppressed: true}}
	r.MedianReviewHours = Hours{Number{Suppressed: true}}
	return r
}

func bodyOf(p calendar.Period, all Row, rows ...HospitalRow) Body {
	from, to := Dates(p)
	return Body{Kind: string(p.Kind), From: from, To: to, All: all, Hospitals: rows}
}

func snap(t *testing.T, b Body, p calendar.Period) Snapshot {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return Snapshot{Period: p, State: StateOK, Body: raw, ComputedAt: p.To.Add(20 * time.Minute)}
}

// history is n stored periods of kind k ending with last, each with hospitals
// A (scaled 1), B (scaled 2) and C (hidden figures).
func history(t *testing.T, k calendar.Kind, last time.Time, n int) []Snapshot {
	t.Helper()
	var out []Snapshot
	for _, p := range calendar.Previous(calendar.Of(k, last), n) {
		b := bodyOf(p, fullRow(4), HospitalRow{Hospital: hospA, Staff: StaffFigure(9), Row: fullRow(1)},
			HospitalRow{Hospital: hospB, Staff: StaffFigure(3), Row: fullRow(2)},
			HospitalRow{Hospital: hospC, Staff: StaffFigure(12), Row: hiddenRow()})
		out = append(out, snap(t, b, p))
	}
	return out
}

func marshal(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func keys(m map[string]any) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return strings.Join(out, ",")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// The shape (spec §7.1): every area's keys as the contract, alerts null,
// changes (and, before the route fills it, what_changed) unavailable, the
// latest stored period's bounds.
func TestRenderShape(t *testing.T) {
	rows := history(t, calendar.Month, day(2026, 10, 1), 3)
	out, degraded := Render(calendar.Month, rows, day(2026, 7, 1))
	if degraded {
		t.Error("degraded")
	}
	m := marshal(t, out)
	if keys(m) != "alerts,changes,from,hospitals,period,quality,to,use,what_changed" {
		t.Fatalf("keys %s", keys(m))
	}
	if m["period"] != "month" || m["from"] != "2026-10-01T00:00:00.000Z" || m["to"] != "2026-11-01T00:00:00.000Z" {
		t.Errorf("bounds %v %v %v", m["period"], m["from"], m["to"])
	}
	if m["alerts"] != nil {
		t.Errorf("alerts %v, want null", m["alerts"])
	}
	// changes is {unavailable: true} always; What changed is the route's
	// to fill (what_changed).
	for _, a := range []string{"changes", "what_changed"} {
		if c, _ := m[a].(map[string]any); keys(c) != "unavailable" || c["unavailable"] != true {
			t.Errorf("%s %v", a, m[a])
		}
	}
	use := m["use"].(map[string]any)
	if keys(use) != "median,rows,trend,updated_at" || use["updated_at"] != "2026-11-01T00:20:00.000Z" {
		t.Errorf("use %v", keys(use))
	}
	if r := use["rows"].([]any)[0].(map[string]any); keys(r) != "active_clinicians,hospital,questions,questions_per_clinician" {
		t.Errorf("use row %s", keys(r))
	}
	if keys(use["median"].(map[string]any)) != "active_clinicians,questions,questions_per_clinician" {
		t.Errorf("use median %s", keys(use["median"].(map[string]any)))
	}
	q := m["quality"].(map[string]any)
	if keys(q) != "median,rows,trend,updated_at" {
		t.Errorf("quality %s", keys(q))
	}
	qr := q["rows"].([]any)[0].(map[string]any)
	if keys(qr) != "agreement_rate,answers,confidence,hospital,median_review_hours,needs_review_share,reviews" {
		t.Errorf("quality row %s", keys(qr))
	}
	if keys(qr["confidence"].(map[string]any)) != "high,low,medium" {
		t.Errorf("confidence %v", qr["confidence"])
	}
	if keys(q["median"].(map[string]any)) != "agreement_rate,answers,confidence,median_review_hours,needs_review_share,reviews" {
		t.Errorf("quality median %s", keys(q["median"].(map[string]any)))
	}
	h := m["hospitals"].(map[string]any)
	if keys(h) != "rows,updated_at" {
		t.Errorf("hospitals %s", keys(h))
	}
	hr := h["rows"].([]any)[1].(map[string]any)
	if keys(hr) != "hospital,size" || keys(hr["size"].(map[string]any)) != "active_clinicians,questions,staff" {
		t.Errorf("hospitals row %v", hr)
	}
	// Staff 3 stays hidden (Q4 open).
	if s := hr["size"].(map[string]any)["staff"].(map[string]any); s["suppressed"] != true || s["value"] != nil {
		t.Errorf("staff of 3: %v", s)
	}
}

// Every cell of a row is the stored cell, as stored (I2): the quality
// mix is pass, regenerate, escalate; needs review is the escalate share.
func TestRenderCellsAreStored(t *testing.T) {
	rows := history(t, calendar.Week, day(2026, 10, 22), 1)
	out, _ := Render(calendar.Week, rows, day(2026, 7, 1))
	b, err := DecodeBody(rows[0].Body, rows[0].Period)
	if err != nil {
		t.Fatal(err)
	}
	use := out.Use.(UseArea)
	q := out.Quality.(QualityArea)
	hs := out.Hospitals.(HospitalsArea)
	if len(use.Rows) != 3 || len(q.Rows) != 3 || len(hs.Rows) != 3 {
		t.Fatalf("rows %d %d %d", len(use.Rows), len(q.Rows), len(hs.Rows))
	}
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	for i, h := range b.Hospitals {
		u, qr, sr := use.Rows[i], q.Rows[i], hs.Rows[i]
		if u.Hospital != h.Hospital || j(u.Questions) != j(h.Questions) || j(u.ActiveClinicians) != j(h.ActiveClinicians) ||
			!sameNumber(u.QuestionsPerClinician.Number, h.QuestionsPerClinician.Number) {
			t.Errorf("use row %d differs", i)
		}
		if j(qr.Answers) != j(h.Answers) || j(qr.NeedsReviewShare) != j(h.EscalateShare) || j(qr.Confidence.High) != j(h.Pass) ||
			j(qr.Confidence.Medium) != j(h.Regenerate) || j(qr.Confidence.Low) != j(h.Escalate) || j(qr.Reviews) != j(h.Reviews) ||
			j(qr.AgreementRate) != j(h.AgreementRate) || !sameNumber(qr.MedianReviewHours.Number, h.MedianReviewHours.Number) {
			t.Errorf("quality row %d differs", i)
		}
		if j(sr.Size.Staff) != j(h.Staff) || j(sr.Size.Questions) != j(h.Questions) || j(sr.Size.ActiveClinicians) != j(h.ActiveClinicians) {
			t.Errorf("size row %d differs", i)
		}
	}
}

func sameNumber(a, b Number) bool {
	if a.Suppressed != b.Suppressed || (a.Value == nil) != (b.Value == nil) {
		return false
	}
	return a.Value == nil || *a.Value == *b.Value
}

// A hospital whose name can't be shown (stored with an empty name, every
// figure hidden) is left out of every area.
func TestRenderLeavesOutUnnamed(t *testing.T) {
	p := calendar.Of(calendar.Month, day(2026, 9, 1))
	b := bodyOf(p, fullRow(2), HospitalRow{Hospital: hospA, Staff: StaffFigure(9), Row: fullRow(1)},
		HospitalRow{Hospital: Ref{ID: uuid.NewString()}, Staff: hiddenCount, Row: allHidden()})
	out, _ := Render(calendar.Month, []Snapshot{snap(t, b, p)}, day(2026, 7, 1))
	if n := len(out.Use.(UseArea).Rows); n != 1 {
		t.Errorf("%d use rows", n)
	}
	if n := len(out.Hospitals.(HospitalsArea).Rows); n != 1 {
		t.Errorf("%d size rows", n)
	}
}

func allHidden() Row {
	h, s := hiddenCount, hiddenShare
	n := Number{Suppressed: true}
	return Row{Questions: h, Answers: h, Pass: h, Regenerate: h, Escalate: h, Degraded: h, PassShare: s, RegenerateShare: s,
		EscalateShare: s, Reviews: h, AgreementRate: s, ActiveClinicians: h, QuestionsPerClinician: Ratio{n}, MedianReviewHours: Hours{n}}
}

// The trend: the 12 latest weeks, 12 months or 8 quarters, oldest first,
// each the stored all cell of its own period (no total across periods);
// an unavailable or unreadable period's point is hidden; an absent one
// (never published) has no point.
func TestRenderTrend(t *testing.T) {
	for _, c := range []struct {
		k    calendar.Kind
		last time.Time
		n    int
	}{{calendar.Week, day(2026, 10, 22), 12}, {calendar.Month, day(2026, 10, 1), 12}, {calendar.Quarter, day(2026, 7, 1), 8}} {
		rows := history(t, c.k, c.last, c.n+3) // older ones fall outside
		// Each period's all cell is its own: questions 100+i.
		for i := range rows {
			b, _ := DecodeBody(rows[i].Body, rows[i].Period)
			b.All.Questions = smallgroup.Count(int64(100 + i))
			rows[i] = snap(t, b, rows[i].Period)
		}
		out, _ := Render(c.k, rows, day(2020, 1, 1))
		tr := out.Use.(UseArea).Trend
		if len(tr) != c.n {
			t.Fatalf("%s: %d points, want %d", c.k, len(tr), c.n)
		}
		want := calendar.Previous(calendar.Of(c.k, c.last), c.n)
		for i, pt := range tr {
			if pt.Start != Stamp(want[i].From) {
				t.Errorf("%s point %d starts %s, want %v", c.k, i, pt.Start, want[i])
			}
			if v := pt.Value; !v.Shown() || *v.Value != int64(100+3+i) {
				t.Errorf("%s point %d: %+v, want the period's own all cell %d", c.k, i, v, 100+3+i)
			}
		}
		if qt := out.Quality.(QualityArea).Trend; len(qt) != c.n || !qt[0].Value.Shown() || *qt[0].Value.Share != 0.5 {
			t.Errorf("%s: quality trend %+v", c.k, qt)
		}
	}
	// One period unavailable, one unreadable, one absent.
	rows := history(t, calendar.Month, day(2026, 10, 1), 6)
	rows[1] = Snapshot{Period: rows[1].Period, State: "unavailable", ComputedAt: rows[1].ComputedAt}
	rows[2].Body = bytes.Replace(rows[2].Body, []byte(`"questions":{"value":48,`), []byte(`"questions":{"value":3,`), 1)
	rows = append(rows[:3], rows[4:]...)
	out, degraded := Render(calendar.Month, rows, day(2020, 1, 1))
	if !degraded {
		t.Error("an unreadable stored body isn't reported")
	}
	tr := out.Use.(UseArea).Trend
	if len(tr) != 5 {
		t.Fatalf("%d points, want 5 (one absent)", len(tr))
	}
	for i, pt := range tr {
		hidden := i == 1 || i == 2
		if hidden != (pt.Value.Suppressed && pt.Value.Value == nil) {
			t.Errorf("point %d: %+v", i, pt.Value)
		}
	}
	if tr[3].Start != Stamp(day(2026, 9, 1)) {
		t.Errorf("after the absent August: %s", tr[3].Start)
	}
	if qt := out.Quality.(QualityArea).Trend; qt[1].Value != hiddenShare || qt[2].Value != hiddenShare {
		t.Errorf("quality trend %+v", qt)
	}
}

// A13: medians use shown values only. Hidden cells that still hold a raw
// value in memory (they never reach a stored body) change nothing; a count
// median strictly between 0 and 5 is hidden; no shown value is {null,
// false}.
func TestRenderMedianFromShownOnly(t *testing.T) {
	p := calendar.Of(calendar.Month, day(2026, 9, 1))
	raw := func(n int64) smallgroup.Guarded { return smallgroup.Guarded{Value: &n, Suppressed: true} }
	a, b, c := fullRow(1), fullRow(2), fullRow(1)
	c.Questions, c.Answers = raw(1000), raw(1000) // hidden, with a value that would move the median
	b.Answers = smallgroup.Count(0)
	body := bodyOf(p, fullRow(4), HospitalRow{Hospital: hospA, Row: a, Staff: StaffFigure(9)},
		HospitalRow{Hospital: hospB, Row: b, Staff: StaffFigure(9)}, HospitalRow{Hospital: hospC, Row: c, Staff: StaffFigure(9)})
	use := useArea(body, time.Time{}, nil)
	if v := use.Median.Questions; !v.Shown() || *v.Value != 18 {
		t.Errorf("questions median %+v, want (12+24)/2 = 18", v)
	}
	q := qualityArea(body, time.Time{}, nil)
	// answers shown: 10 and 0 → 5 (not between 0 and 5: shown).
	if v := q.Median.Answers; !v.Shown() || *v.Value != 5 {
		t.Errorf("answers median %+v, want 5", v)
	}
	// A count median of 2.5 is hidden.
	b.Answers = smallgroup.Count(0)
	a.Answers = smallgroup.Count(5)
	body.Hospitals[0].Row, body.Hospitals[1].Row = a, b
	if v := qualityArea(body, time.Time{}, nil).Median.Answers; v.Shown() || !v.Suppressed {
		t.Errorf("answers median of 0 and 5: %+v, want hidden", v)
	}
	// Nothing shown: {null, false}.
	body.Hospitals = body.Hospitals[2:]
	if v := useArea(body, time.Time{}, nil).Median.Questions; v.Shown() || v.Suppressed {
		t.Errorf("no shown value: %+v", v)
	}
	raw2, _ := json.Marshal(CountMedian{Number{Value: ptr(5.5)}})
	if string(raw2) != `{"value":5.5,"suppressed":false}` {
		t.Errorf("a count median %s", raw2)
	}
}

func ptr(f float64) *float64 { return &f }

// F19: before the first published period of a kind, from and to are null
// and every area is {not_yet, first_from}: the start of the first period
// that starts at or after the release.
func TestRenderNotYet(t *testing.T) {
	epoch := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	for k, want := range map[calendar.Kind]string{calendar.Week: "2026-11-15T00:00:00.000Z", calendar.Month: "2026-12-01T00:00:00.000Z",
		calendar.Quarter: "2027-01-01T00:00:00.000Z"} {
		out, _ := Render(k, nil, epoch)
		m := marshal(t, out)
		if m["from"] != nil || m["to"] != nil || m["alerts"] != nil {
			t.Errorf("%s: %v", k, m)
		}
		for _, a := range []string{"use", "quality", "hospitals"} {
			v := m[a].(map[string]any)
			if keys(v) != "first_from,not_yet" || v["not_yet"] != true || v["first_from"] != want {
				t.Errorf("%s %s: %v, want first_from %s", k, a, v, want)
			}
		}
	}
	// An epoch exactly on a period start: that period is the first.
	if p := FirstEligible(calendar.Week, day(2026, 11, 8)); !p.From.Equal(day(2026, 11, 8)) {
		t.Errorf("first week %v", p)
	}
}

// A16, F13: the latest stored period unavailable, or unreadable, makes
// use, quality and hospitals {unavailable: true}; from and to still name
// it; changes and alerts are as always.
func TestRenderAreaUnavailable(t *testing.T) {
	rows := history(t, calendar.Week, day(2026, 10, 22), 3)
	last := len(rows) - 1
	for name, mutate := range map[string]func(s *Snapshot){
		"unavailable": func(s *Snapshot) { s.State, s.Body = "unavailable", nil },
		"a shown 3": func(s *Snapshot) {
			s.Body = bytes.Replace(s.Body, []byte(`"answers":{"value":40,`), []byte(`"answers":{"value":3,`), 1)
		},
		"an unknown key": func(s *Snapshot) { s.Body = bytes.Replace(s.Body, []byte(`{"kind"`), []byte(`{"extra":1,"kind"`), 1) },
		"another period": func(s *Snapshot) {
			s.Body = bytes.Replace(s.Body, []byte(`"from":"2026-10-22"`), []byte(`"from":"2026-10-15"`), 1)
		},
	} {
		rs := append([]Snapshot{}, rows...)
		mutate(&rs[last])
		out, degraded := Render(calendar.Week, rs, day(2026, 1, 1))
		m := marshal(t, out)
		if m["from"] != "2026-10-22T00:00:00.000Z" || m["to"] != "2026-11-01T00:00:00.000Z" {
			t.Errorf("%s: bounds %v %v", name, m["from"], m["to"])
		}
		for _, a := range []string{"use", "quality", "hospitals", "changes"} {
			if v, _ := m[a].(map[string]any); keys(v) != "unavailable" || v["unavailable"] != true {
				t.Errorf("%s: %s %v", name, a, m[a])
			}
		}
		if m["alerts"] != nil {
			t.Errorf("%s: alerts %v", name, m["alerts"])
		}
		if degraded != (name != "unavailable") {
			t.Errorf("%s: degraded %v", name, degraded)
		}
	}
}

// A1 (unit): the render depends on the stored rows alone: the same rows in
// any order give the same bytes.
func TestRenderStable(t *testing.T) {
	rows := history(t, calendar.Month, day(2026, 10, 1), 12)
	a, _ := Render(calendar.Month, rows, day(2025, 1, 1))
	rev := make([]Snapshot, len(rows))
	for i := range rows {
		rev[len(rows)-1-i] = rows[i]
	}
	b, _ := Render(calendar.Month, rev, day(2025, 1, 1))
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	if !bytes.Equal(ra, rb) {
		t.Errorf("order changes the bytes:\n%s\n%s", ra, rb)
	}
}

// A11 (unit): nothing rendered shows a count of 1 to 4.
func TestRenderNoRawSmallCount(t *testing.T) {
	rows := history(t, calendar.Week, day(2026, 10, 22), 12)
	out, _ := Render(calendar.Week, rows, day(2025, 1, 1))
	raw, _ := json.Marshal(out)
	if m := regexp.MustCompile(`"value":\s*[1-4]\s*[,}]`).Find(raw); m != nil {
		t.Errorf("a small count: %s in %s", m, raw)
	}
	for _, h := range []Ref{hospA, hospB} {
		for _, measure := range SeriesMeasures {
			s, err := HospitalSeries(calendar.Week, rows, h.ID, measure)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(s)
			if m := regexp.MustCompile(`"value":\s*[1-4]\s*[,}]`).Find(raw); m != nil {
				t.Errorf("series: a small count %s in %s", m, raw)
			}
		}
	}
}

// The series (spec §7.2): the hospital's stored cell for each of the latest
// periods, oldest first; median of the other hospitals' shown values or
// null; the hospital as named in the newest body.
func TestHospitalSeriesPoints(t *testing.T) {
	for _, c := range []struct {
		k    calendar.Kind
		last time.Time
		n    int
	}{{calendar.Week, day(2026, 10, 22), 12}, {calendar.Month, day(2026, 10, 1), 12}, {calendar.Quarter, day(2026, 7, 1), 8}} {
		rows := history(t, c.k, c.last, c.n+2)
		s, err := HospitalSeries(c.k, rows, hospA.ID, "needs_review_share")
		if err != nil {
			t.Fatal(err)
		}
		m := marshal(t, s)
		if keys(m) != "hospital,measure,points" || m["measure"] != "needs_review_share" {
			t.Errorf("%s: %v", c.k, keys(m))
		}
		if h := m["hospital"].(map[string]any); h["id"] != hospA.ID || h["name"] != hospA.Name {
			t.Errorf("%s: hospital %v", c.k, h)
		}
		pts := m["points"].([]any)
		if len(pts) != c.n {
			t.Fatalf("%s: %d points", c.k, len(pts))
		}
		want := calendar.Previous(calendar.Of(c.k, c.last), c.n)
		for i, x := range pts {
			pt := x.(map[string]any)
			if keys(pt) != "median,start,value" || pt["start"] != Stamp(want[i].From) {
				t.Errorf("%s point %d: %v", c.k, i, pt)
			}
			// A's share 0.5; the others: B 0.5 shown, C hidden → median 0.5.
			if v := pt["value"].(map[string]any); v["value"] != 0.5 || v["suppressed"] != false {
				t.Errorf("%s point %d value %v", c.k, i, v)
			}
			if pt["median"] != 0.5 {
				t.Errorf("%s point %d median %v", c.k, i, pt["median"])
			}
		}
		// C's figures are hidden: every point hidden; the median of A and B.
		sc, err := HospitalSeries(c.k, rows, hospC.ID, "questions_per_clinician")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(sc)
		if strings.Contains(string(raw), `"suppressed":false`) || !strings.Contains(string(raw), `"median":2.00`) {
			t.Errorf("%s: C's series %s", c.k, raw)
		}
	}
}

// A13 for the series, and the period edges: a hidden cell's raw value
// can't reach the median; a period whose body is unavailable is hidden
// whole (value hidden, median null); a period not listing the hospital
// has nothing to show.
func TestSeriesMedianFromShownOnly(t *testing.T) {
	rows := history(t, calendar.Month, day(2026, 10, 1), 4)
	// The newest month: B's share hidden, so A's median is C's... hidden too:
	// nothing shown, null.
	b, _ := DecodeBody(rows[3].Body, rows[3].Period)
	b.Hospitals[1].EscalateShare = hiddenShare
	rows[3] = snap(t, b, rows[3].Period)
	// The month before: unavailable.
	rows[2] = Snapshot{Period: rows[2].Period, State: "unavailable"}
	// The first month doesn't list A.
	b0, _ := DecodeBody(rows[0].Body, rows[0].Period)
	b0.Hospitals = b0.Hospitals[1:]
	rows[0] = snap(t, b0, rows[0].Period)
	s, err := HospitalSeries(calendar.Month, rows, hospA.ID, "needs_review_share")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(s.Points)
	want := `[{"start":"2026-07-01T00:00:00.000Z","value":{"value":null,"suppressed":false},"median":0.5000},` +
		`{"start":"2026-08-01T00:00:00.000Z","value":{"value":0.5000,"suppressed":false},"median":0.5000},` +
		`{"start":"2026-09-01T00:00:00.000Z","value":{"value":null,"suppressed":true},"median":null},` +
		`{"start":"2026-10-01T00:00:00.000Z","value":{"value":0.5000,"suppressed":false},"median":null}]`
	if string(raw) != want {
		t.Errorf("points\n%s\nwant\n%s", raw, want)
	}
	// The median over shown values, even ones: (2 + 3) / 2 per clinician.
	vals := []float64{2, 3}
	if m, ok := median(vals); !ok || m != 2.5 {
		t.Errorf("median %v %v", m, ok)
	}
}

// F10, A22: the two Critical measures and anything else are refused; an
// unknown or non-canonical hospital id, or one in no stored body, is not
// found.
func TestHospitalSeriesRefusals(t *testing.T) {
	rows := history(t, calendar.Week, day(2026, 10, 22), 3)
	for _, m := range []string{"critical_median_ack_minutes", "critical_within_target", "questions", "", "NEEDS_REVIEW_SHARE"} {
		if _, err := HospitalSeries(calendar.Week, rows, hospA.ID, m); !errors.Is(err, ErrMeasure) {
			t.Errorf("measure %q: %v", m, err)
		}
	}
	for _, id := range []string{uuid.NewString(), strings.ToUpper(hospA.ID), "x", ""} {
		if _, err := HospitalSeries(calendar.Week, rows, id, "needs_review_share"); !errors.Is(err, ErrNoHospital) {
			t.Errorf("id %q: %v", id, err)
		}
	}
	if _, err := HospitalSeries(calendar.Week, nil, hospA.ID, "needs_review_share"); !errors.Is(err, ErrNoHospital) {
		t.Errorf("nothing stored: %v", err)
	}
	// Only in unreadable or unnamed rows: not found.
	p := rows[2].Period
	b := bodyOf(p, fullRow(1), HospitalRow{Hospital: Ref{ID: hospA.ID}, Staff: hiddenCount, Row: allHidden()})
	if _, err := HospitalSeries(calendar.Week, []Snapshot{snap(t, b, p)}, hospA.ID, "needs_review_share"); !errors.Is(err, ErrNoHospital) {
		t.Errorf("an unnamed row: %v", err)
	}
}

// I2: the render code (package analytics) imports neither the scheduler
// (the builder and its row types) nor the database.
func TestRenderImportsNoBuilder(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	if len(files) == 0 {
		t.Fatal("no files")
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if strings.Contains(p, "/internal/analytics/scheduler") || strings.Contains(p, "/internal/db") ||
				strings.Contains(p, "jackc/pgx") {
				t.Errorf("%s imports %s", f, p)
			}
		}
		if bytes.Contains(src, []byte("FrozenRow")) {
			t.Errorf("%s names the frozen rows", f)
		}
	}
}
