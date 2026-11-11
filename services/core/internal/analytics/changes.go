package analytics

// What changed (Plan 7c spec §7.3; Q3 decided 7 Nov 2026). Once a week,
// right after the week's snapshot is stored, the scheduler compares the
// latest week with the month that finished before the week started: one
// period against one period, never a total over several (R41a-2). Changes
// takes the two stored bodies as its only input, so it sees shown cells and
// nothing else: a change is flagged only when every value it rests on
// (both sides, and the minimums' bases) is shown, so its existence says
// nothing about a hidden value (A12), and nothing it sends adds an equation
// to what the bodies already publish.

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// The value shapes of a change measure.
const (
	ShapeCount = "count" // a guarded count, sent as an integer
	ShapeRatio = "ratio" // questions per clinician, two decimals
	ShapeShare = "share" // a share of 0 to 1, four decimals
)

// Threshold is one row of the §7.3 table: when a measure's change is
// flagged. Each side's Base (a stored figure of the same row) must be shown
// and at least MinBase; the change must be at least MinChange: a share of
// previous for counts and ratios, points (a fraction of 1) for shares.
// PerDay: previous is the month's value per day, scaled to the week's
// length. Up and Down are the directions of a rise and of a fall.
type Threshold struct {
	Measure   string
	Label     string // the summary's words for the measure
	Area      string
	Shape     string
	Base      string
	MinBase   int64
	MinChange float64
	PerDay    bool
	Up, Down  string
}

// Thresholds is the §7.3 table (Q3, decided 7 Nov 2026: counts flag at a
// change of at least 50% with at least 10 on both sides; the rate rows keep
// 20 and 15 points). The product owner may change it: this table is the
// one place.
//
// Ruling C9 (the 7c-3 review): active clinicians and questions per
// clinician have no row. A week's distinct clinicians can't be compared
// fairly with a whole month's, and the ratio mixes the two windows: both
// would flag nearly every week where clinicians rotate shifts (anti-goal
// 1). They come back as like-for-like comparisons (TODO.md).
// median_review_hours has no row: its minimum (10 decided on both sides)
// rests on a count no stored body holds, and a change is computed from
// stored, shown cells only.
var Thresholds = []Threshold{
	{Measure: "questions", Label: "Questions", Area: "use", Shape: ShapeCount, Base: "questions", MinBase: 10, MinChange: 0.5,
		PerDay: true, Up: "rising", Down: "falling"},
	{Measure: "needs_review_share", Label: "Needs-review share", Area: "quality", Shape: ShapeShare, Base: "answers", MinBase: 20,
		MinChange: 0.15, Up: "worse", Down: "better"},
	{Measure: "agreement_rate", Label: "Agreement rate", Area: "quality", Shape: ShapeShare, Base: "reviews", MinBase: 20,
		MinChange: 0.15, Up: "better", Down: "worse"},
}

// ThresholdOf is the row of measure m.
func ThresholdOf(m string) (Threshold, bool) {
	for _, t := range Thresholds {
		if t.Measure == m {
			return t, true
		}
	}
	return Threshold{}, false
}

// Shown is a shown value as an integer number of units of 10^-Digits:
// written {"value": V/10^Digits with Digits decimals, "suppressed": false}.
type Shown struct {
	V      int64
	Digits int
}

// Float is the value.
func (s Shown) Float() float64 { return float64(s.V) / math.Pow10(s.Digits) }

// Text is the value with its decimals.
func (s Shown) Text() string { return strconv.FormatFloat(s.Float(), 'f', s.Digits, 64) }

func (s Shown) MarshalJSON() ([]byte, error) {
	return []byte(`{"value":` + s.Text() + `,"suppressed":false}`), nil
}

// Flag is one flagged change: a row (Hospital nil for all hospitals), the
// measure and its direction, the two shown values and the summary.
type Flag struct {
	Hospital  *Ref
	Measure   string
	Area      string
	Direction string
	Current   Shown
	Previous  Shown
	Summary   string
}

// Key is the change's key while it is open: hospital, measure, direction.
func (f Flag) Key() string {
	h := ""
	if f.Hospital != nil {
		h = f.Hospital.ID
	}
	return h + "/" + f.Measure + "/" + f.Direction
}

// ComparedMonth is the month a week is compared with: the month that
// finished before the week started (the previous calendar month).
func ComparedMonth(w calendar.Period) calendar.Period {
	return calendar.Prev(calendar.Of(calendar.Month, w.From))
}

// days is a period's length in whole days.
func days(p calendar.Period) int64 { return int64(p.To.Sub(p.From) / (24 * time.Hour)) }

// side is one row's figures of one period as fixed-point integers: shown
// or not.
type side map[string]*int64

// sideOf reads the figures a change may rest on from a stored row: counts
// in units, ratios in hundredths, shares in units of 10^-4. A hidden or
// empty figure is absent.
func sideOf(r Row) side {
	s := side{}
	count := func(k string, g smallgroup.Guarded) {
		if g.Shown() {
			v := *g.Value
			s[k] = &v
		}
	}
	fixed := func(k string, v *float64, ok bool, digits int) {
		if ok {
			x := int64(math.Round(*v * math.Pow10(digits)))
			s[k] = &x
		}
	}
	count("questions", r.Questions)
	count("active_clinicians", r.ActiveClinicians)
	count("answers", r.Answers)
	count("reviews", r.Reviews)
	fixed("questions_per_clinician", r.QuestionsPerClinician.Value, r.QuestionsPerClinician.Shown(), 2)
	fixed("needs_review_share", r.EscalateShare.Share, r.EscalateShare.Shown(), smallgroup.ShareDigits)
	fixed("agreement_rate", r.AgreementRate.Share, r.AgreementRate.Shown(), smallgroup.ShareDigits)
	return s
}

// digitsOf is a shape's decimals as sent: current, and previous (a count
// per day has one decimal).
func digitsOf(t Threshold) (cur, prev int) {
	switch t.Shape {
	case ShapeRatio:
		return 2, 2
	case ShapeShare:
		return smallgroup.ShareDigits, smallgroup.ShareDigits
	}
	if t.PerDay {
		return 0, 1
	}
	return 0, 0
}

// scale is v (in 10^-from units) per day of month, times the week's days,
// in 10^-to units, rounded half up; or v moved to to's units.
func scale(v int64, from, to int, num, den int64) int64 {
	p := int64(math.Pow10(to - from))
	if den == 0 {
		return v * p
	}
	return (2*v*p*num + den) / (2 * den)
}

// evaluate flags measure t for one row: cur and prev are the week's and
// the month's sides. ok is false when nothing is flagged.
func evaluate(t Threshold, cur, prev side, wDays, mDays int64) (c, p Shown, dir string, ok bool) {
	cv, pv := cur[t.Measure], prev[t.Measure]
	cb, pb := cur[t.Base], prev[t.Base]
	// Every value the change rests on is shown (A12).
	if cv == nil || pv == nil || cb == nil || pb == nil || *cb < t.MinBase || *pb < t.MinBase {
		return Shown{}, Shown{}, "", false
	}
	cd, pd := digitsOf(t)
	srcDigits := cd // a stored month value has the current's decimals
	num, den := int64(1), int64(0)
	if t.PerDay {
		num, den = wDays, mDays
	}
	c = Shown{V: *cv, Digits: cd}
	p = Shown{V: scale(*pv, srcDigits, pd, num, den), Digits: pd}
	// Both sides at the minimum on the values sent, when the base is the
	// measure itself (a count per day is compared as sent).
	if t.Base == t.Measure && (c.Float() < float64(t.MinBase) || p.Float() < float64(t.MinBase)) {
		return Shown{}, Shown{}, "", false
	}
	// Compare in one unit: the finer of the two.
	d := max(cd, pd)
	ci := c.V * int64(math.Pow10(d-cd))
	pi := p.V * int64(math.Pow10(d-pd))
	diff := ci - pi
	if diff == 0 {
		return Shown{}, Shown{}, "", false
	}
	abs := diff
	if abs < 0 {
		abs = -abs
	}
	switch t.Shape {
	case ShapeShare:
		// Points: MinChange in units of 10^-d.
		if abs < int64(math.Round(t.MinChange*math.Pow10(d))) {
			return Shown{}, Shown{}, "", false
		}
	default:
		// A share of previous: abs / pi ≥ MinChange, in integers.
		if pi <= 0 || float64(abs) < t.MinChange*float64(pi) {
			return Shown{}, Shown{}, "", false
		}
	}
	dir = t.Down
	if diff > 0 {
		dir = t.Up
	}
	return c, p, dir, true
}

// Verbs are the summary's word for each direction.
var verbs = map[string]string{"rising": "up", "falling": "down", "worse": "worse", "better": "better"}

// Summary is a change's plain words: the measure, the direction and the two
// shown values, and nothing else (no hospital name; no other number, A12).
// Shares are written as percentages with two decimals (the share's four).
func Summary(t Threshold, dir string, cur, prev Shown) string {
	text := func(s Shown) string {
		if t.Shape == ShapeShare {
			return Shown{V: s.V, Digits: s.Digits - 2}.Text() + "%"
		}
		return s.Text()
	}
	out := t.Label + " " + verbs[dir] + ": " + text(prev) + " to " + text(cur)
	if t.Shape != ShapeShare && t.PerDay {
		out += " a week"
	}
	return out
}

// Changes evaluates week w against month m (ComparedMonth(w)) from their
// stored bodies: the all row and every named hospital the week lists that
// the month lists too (a hospital with no previous month gets no change,
// F17). The order is the rows' and Thresholds'.
func Changes(w calendar.Period, week Body, m calendar.Period, month Body) []Flag {
	wd, md := days(w), days(m)
	var out []Flag
	row := func(ref *Ref, cur, prev Row) {
		cs, ps := sideOf(cur), sideOf(prev)
		for _, t := range Thresholds {
			c, p, dir, ok := evaluate(t, cs, ps, wd, md)
			if !ok {
				continue
			}
			out = append(out, Flag{Hospital: ref, Measure: t.Measure, Area: t.Area, Direction: dir, Current: c, Previous: p,
				Summary: Summary(t, dir, c, p)})
		}
	}
	row(nil, week.All, month.All)
	prev := map[string]Row{}
	for _, h := range month.Hospitals {
		if h.Hospital.Name != "" {
			prev[h.Hospital.ID] = h.Row
		}
	}
	for _, h := range week.Hospitals {
		p, ok := prev[h.Hospital.ID]
		if !ok || h.Hospital.Name == "" {
			continue
		}
		ref := h.Hospital
		row(&ref, h.Row, p)
	}
	return out
}

// --- the changes area (GET /platform/analytics) ---------------------------

// ChangesRun is the newest evaluation: the week and the month it compared,
// and when.
type ChangesRun struct {
	Week, Month calendar.Period
	EvaluatedAt time.Time
}

// ChangeRow is one open change as stored (current and previous as the
// stored JSON), with its follow-up.
type ChangeRow struct {
	ID        string
	Hospital  *Ref
	Area      string
	Measure   string
	Direction string
	Serious   bool
	Current   []byte
	Previous  []byte
	Summary   string
	Version   int
	FollowUp  *FollowUpRow
}

// FollowUpRow is a change's follow-up as stored.
type FollowUpRow struct {
	By      Ref
	At      time.Time
	Note    *string
	Version int
}

// ChangesArea is the changes area: {updated_at, window, items}.
type ChangesArea struct {
	UpdatedAt string      `json:"updated_at"`
	Window    Window      `json:"window"`
	Items     []ChangeOut `json:"items"`
}

// Window is the compared span: the month's start to the week's end.
type Window struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ChangeOut is one change as the routes send it.
type ChangeOut struct {
	ID        string          `json:"id"`
	Hospital  *Ref            `json:"hospital"`
	Area      string          `json:"area"`
	Measure   string          `json:"measure"`
	Direction string          `json:"direction"`
	Serious   bool            `json:"serious"`
	Previous  json.RawMessage `json:"previous"`
	Current   json.RawMessage `json:"current"`
	Summary   string          `json:"summary"`
	FollowUp  *FollowUpOut    `json:"follow_up"`
	Version   int             `json:"version"`
}

// FollowUpOut is a follow-up as sent: by_you set per caller.
type FollowUpOut struct {
	By      Ref     `json:"by"`
	At      string  `json:"at"`
	Note    *string `json:"note"`
	ByYou   bool    `json:"by_you"`
	Version int     `json:"version"`
}

// ErrChange is a stored change that can't be sent.
var ErrChange = errors.New("analytics: not a stored change")

// shownJSON reads a stored shown value strictly and writes it compactly,
// its number as stored: {"value": n >= 0, "suppressed": false}.
func shownJSON(b []byte) (json.RawMessage, float64, error) {
	var v struct {
		Value      *json.Number `json:"value"`
		Suppressed *bool        `json:"suppressed"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil || dec.More() || v.Value == nil || v.Suppressed == nil || *v.Suppressed {
		return nil, 0, ErrChange
	}
	f, err := v.Value.Float64()
	if err != nil || f < 0 || math.IsInf(f, 0) || strings.ContainsAny(v.Value.String(), "eE+") {
		return nil, 0, ErrChange
	}
	return json.RawMessage(`{"value":` + v.Value.String() + `,"suppressed":false}`), f, nil
}

// ChangeOutOf is row as caller sees it. A row outside the vocabulary, with
// a value that isn't shown, or naming a hospital whose name can't be shown,
// is ErrChange.
func ChangeOutOf(row ChangeRow, caller string) (ChangeOut, error) {
	t, ok := ThresholdOf(row.Measure)
	if !ok || t.Area != row.Area || (row.Direction != t.Up && row.Direction != t.Down) || row.Serious || row.Version < 1 {
		return ChangeOut{}, ErrChange
	}
	if row.Hospital != nil && !audit.ShowableName(row.Hospital.Name) {
		return ChangeOut{}, ErrChange
	}
	cur, _, err1 := shownJSON(row.Current)
	prev, _, err2 := shownJSON(row.Previous)
	if err1 != nil || err2 != nil || row.Summary == "" {
		return ChangeOut{}, ErrChange
	}
	out := ChangeOut{ID: row.ID, Hospital: row.Hospital, Area: row.Area, Measure: row.Measure, Direction: row.Direction,
		Previous: prev, Current: cur, Summary: row.Summary, Version: row.Version}
	if f := row.FollowUp; f != nil {
		if !audit.ShowableName(f.By.Name) || f.Version < 1 {
			return ChangeOut{}, ErrChange
		}
		out.FollowUp = &FollowUpOut{By: f.By, At: Stamp(f.At), Note: f.Note, ByYou: caller != "" && f.By.ID == caller, Version: f.Version}
	}
	return out, nil
}

// size is how far past its threshold a change is: the change over
// MinChange (a share of previous, or points).
func size(row ChangeRow) float64 {
	t, _ := ThresholdOf(row.Measure)
	_, c, _ := shownJSON(row.Current)
	_, p, _ := shownJSON(row.Previous)
	d := math.Abs(c - p)
	if t.Shape != ShapeShare {
		if p <= 0 {
			return 0
		}
		d /= p
	}
	return d / t.MinChange
}

// RenderChanges is the changes area from the newest run and its open
// changes, for caller (a user id; by_you is set per caller). No run yet is
// {unavailable: true}. Items not followed up come first, each group by size
// of change, largest first. degraded reports a stored change left out.
func RenderChanges(run *ChangesRun, rows []ChangeRow, caller string) (any, bool) {
	if run == nil {
		return Unavailable{true}, false
	}
	type item struct {
		out  ChangeOut
		size float64
	}
	var items []item
	degraded := false
	for _, r := range rows {
		o, err := ChangeOutOf(r, caller)
		if err != nil {
			degraded = true
			continue
		}
		items = append(items, item{o, size(r)})
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if (a.out.FollowUp == nil) != (b.out.FollowUp == nil) {
			return a.out.FollowUp == nil
		}
		if a.size != b.size {
			return a.size > b.size
		}
		return a.out.ID < b.out.ID
	})
	area := ChangesArea{UpdatedAt: Stamp(run.EvaluatedAt), Window: Window{From: Stamp(run.Month.From), To: Stamp(run.Week.To)},
		Items: make([]ChangeOut, 0, len(items))}
	for _, it := range items {
		area.Items = append(area.Items, it.out)
	}
	return area, degraded
}

// --- the change series -------------------------------------------------------

// measureCell is a row's stored cell of a change measure, its shown value
// (nil when not shown), the hidden and nothing-to-show forms, and how its
// median is written (digits; count: a count median, hidden strictly between
// 0 and 5).
func measureCell(r Row, measure string) (cell any, shown *float64, hidden, nothing any, digits int, count bool) {
	countOf := func(g smallgroup.Guarded) (any, *float64, any, any, int, bool) {
		var v *float64
		if g.Shown() {
			f := float64(*g.Value)
			v = &f
		}
		return g, v, smallgroup.Guarded{Suppressed: true}, smallgroup.Guarded{}, 1, true
	}
	shareOf := func(g smallgroup.GuardedShare) (any, *float64, any, any, int, bool) {
		var v *float64
		if g.Shown() {
			v = g.Share
		}
		return g, v, smallgroup.GuardedShare{Suppressed: true}, smallgroup.GuardedShare{}, smallgroup.ShareDigits, false
	}
	switch measure {
	case "questions":
		return countOf(r.Questions)
	case "active_clinicians":
		return countOf(r.ActiveClinicians)
	case "questions_per_clinician":
		var v *float64
		if r.QuestionsPerClinician.Shown() {
			v = r.QuestionsPerClinician.Value
		}
		return r.QuestionsPerClinician, v, Ratio{Number{Suppressed: true}}, Ratio{}, 2, false
	case "needs_review_share":
		return shareOf(r.EscalateShare)
	default:
		return shareOf(r.AgreementRate)
	}
}

// ChangeSeries is GET /platform/analytics/changes/{id}/series for change id
// of measure: for a hospital's change (ref set), the hospital's stored cell
// for each stored period of the latest TrendLength of kind k, oldest first,
// with the median of the other named hospitals' shown values; for a change
// across all hospitals (ref nil), the all cell and no median (F14). A period
// stored unavailable or unreadable is hidden whole; one that doesn't list
// the hospital has nothing to show. A count median strictly between 0 and 5
// is sent null, as a count median is hidden.
func ChangeSeries(k calendar.Kind, rows []Snapshot, id string, ref *Ref, measure string) (SeriesOut, error) {
	if _, ok := ThresholdOf(measure); !ok {
		return SeriesOut{}, ErrMeasure
	}
	_, _, hidden, nothing, digits, isCount := measureCell(Row{}, measure)
	win, _ := window(k, rows)
	out := SeriesOut{ChangeID: &id, Measure: measure, Hospital: ref, Points: make([]SeriesPoint, 0, len(win))}
	for _, s := range win {
		pt := SeriesPoint{Start: Stamp(s.period.From), Value: hidden}
		if s.body != nil {
			if ref == nil {
				pt.Value, _, _, _, _, _ = measureCell(s.body.All, measure)
			} else {
				pt.Value = nothing
				var others []float64
				for _, h := range s.body.Hospitals {
					cell, v, _, _, _, _ := measureCell(h.Row, measure)
					switch {
					case h.Hospital.ID == ref.ID:
						pt.Value = cell
					case h.Hospital.Name != "" && v != nil:
						others = append(others, *v)
					}
				}
				if m, ok := median(others); ok && !(isCount && m > 0 && m < smallgroup.Min) {
					d := digits
					if isCount {
						d = 0
						if m != math.Trunc(m) {
							d = 1
						}
					}
					p := math.Pow10(d)
					pt.Median = &Fixed{V: math.Round(m*p) / p, Digits: d}
				}
			}
		}
		out.Points = append(out.Points, pt)
	}
	return out, nil
}
