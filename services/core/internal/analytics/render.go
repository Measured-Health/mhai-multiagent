package analytics

// The analytics reads (Plan 7c spec §7.1, §7.2; invariant I2): GET
// /platform/analytics and the hospital series, rendered from stored
// snapshots and nothing else. Every cell sent is a stored guarded cell as
// stored; every median and trend is computed from shown cells only; nothing
// spans several periods (R41a-2). The render takes the stored rows as its
// only input: no clock, no database, no raw count.

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// StateOK is a stored snapshot with a body.
const StateOK = "ok"

// Snapshot is one stored row of analytics_snapshots as a route reads it.
type Snapshot struct {
	Period     calendar.Period
	State      string
	Body       []byte
	ComputedAt time.Time
}

// Stamp is an instant as the routes send it.
func Stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// Out is GET /platform/analytics.
type Out struct {
	Period    string  `json:"period"`
	From      *string `json:"from"`
	To        *string `json:"to"`
	Changes   any     `json:"changes"`
	Use       any     `json:"use"`
	Alerts    any     `json:"alerts"`
	Quality   any     `json:"quality"`
	Hospitals any     `json:"hospitals"`
}

// Unavailable is an area that can't be shown.
type Unavailable struct {
	Unavailable bool `json:"unavailable"`
}

// NotYet is an area before the first published period of its kind (F19).
type NotYet struct {
	NotYet    bool   `json:"not_yet"`
	FirstFrom string `json:"first_from"`
}

// CountMedian is the median of shown counts: hidden strictly between 0 and
// 5, written as it is (an integer or a half).
type CountMedian struct{ Number }

func (c CountMedian) MarshalJSON() ([]byte, error) { return c.Number.marshal(-1) }

// UseArea is the Use area.
type UseArea struct {
	UpdatedAt string       `json:"updated_at"`
	Rows      []UseRow     `json:"rows"`
	Median    UseMedian    `json:"median"`
	Trend     []CountPoint `json:"trend"`
}

// UseRow is one hospital's use.
type UseRow struct {
	Hospital              Ref                `json:"hospital"`
	Questions             smallgroup.Guarded `json:"questions"`
	ActiveClinicians      smallgroup.Guarded `json:"active_clinicians"`
	QuestionsPerClinician Ratio              `json:"questions_per_clinician"`
}

// UseMedian is the median of each use figure over the hospitals showing it.
type UseMedian struct {
	Questions             CountMedian `json:"questions"`
	ActiveClinicians      CountMedian `json:"active_clinicians"`
	QuestionsPerClinician Ratio       `json:"questions_per_clinician"`
}

// Confidence is the HGA decision mix: high pass, medium regenerate, low
// escalate.
type Confidence struct {
	High   smallgroup.Guarded `json:"high"`
	Medium smallgroup.Guarded `json:"medium"`
	Low    smallgroup.Guarded `json:"low"`
}

// ConfidenceMedian is Confidence's medians.
type ConfidenceMedian struct {
	High   CountMedian `json:"high"`
	Medium CountMedian `json:"medium"`
	Low    CountMedian `json:"low"`
}

// QualityArea is the Answer quality area.
type QualityArea struct {
	UpdatedAt string        `json:"updated_at"`
	Rows      []QualityRow  `json:"rows"`
	Median    QualityMedian `json:"median"`
	Trend     []SharePoint  `json:"trend"`
}

// QualityRow is one hospital's answer quality.
type QualityRow struct {
	Hospital          Ref                     `json:"hospital"`
	Answers           smallgroup.Guarded      `json:"answers"`
	NeedsReviewShare  smallgroup.GuardedShare `json:"needs_review_share"`
	Confidence        Confidence              `json:"confidence"`
	Reviews           smallgroup.Guarded      `json:"reviews"`
	AgreementRate     smallgroup.GuardedShare `json:"agreement_rate"`
	MedianReviewHours Hours                   `json:"median_review_hours"`
}

// QualityMedian is the medians of the quality figures.
type QualityMedian struct {
	Answers           CountMedian             `json:"answers"`
	NeedsReviewShare  smallgroup.GuardedShare `json:"needs_review_share"`
	Confidence        ConfidenceMedian        `json:"confidence"`
	Reviews           CountMedian             `json:"reviews"`
	AgreementRate     smallgroup.GuardedShare `json:"agreement_rate"`
	MedianReviewHours Hours                   `json:"median_review_hours"`
}

// HospitalsArea is the Hospitals area.
type HospitalsArea struct {
	UpdatedAt string    `json:"updated_at"`
	Rows      []SizeRow `json:"rows"`
}

// SizeRow is one hospital's size.
type SizeRow struct {
	Hospital Ref  `json:"hospital"`
	Size     Size `json:"size"`
}

// Size is staff, active clinicians and questions.
type Size struct {
	Staff            smallgroup.Guarded `json:"staff"`
	ActiveClinicians smallgroup.Guarded `json:"active_clinicians"`
	Questions        smallgroup.Guarded `json:"questions"`
}

// CountPoint and SharePoint are trend points: one period's all cell.
type (
	CountPoint struct {
		Start string             `json:"start"`
		Value smallgroup.Guarded `json:"value"`
	}
	SharePoint struct {
		Start string                  `json:"start"`
		Value smallgroup.GuardedShare `json:"value"`
	}
)

// SeriesOut is a series: {measure, hospital, points}.
type SeriesOut struct {
	Measure  string        `json:"measure"`
	Hospital *Ref          `json:"hospital"`
	Points   []SeriesPoint `json:"points"`
}

// SeriesPoint is one period's stored cell and the other hospitals' median.
type SeriesPoint struct {
	Start  string `json:"start"`
	Value  any    `json:"value"`
	Median *Fixed `json:"median"`
}

// Fixed is a plain number with a fixed number of decimals.
type Fixed struct {
	V      float64
	Digits int
}

func (f Fixed) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatFloat(f.V, 'f', f.Digits, 64)), nil
}

// SeriesMeasures are the hospital series measures served (F10): the two
// Critical ones wait for alerts.
var SeriesMeasures = []string{"questions_per_clinician", "needs_review_share"}

// The series refusals.
var (
	ErrMeasure    = errors.New("analytics: not a series measure")
	ErrNoHospital = errors.New("analytics: no such hospital in a stored period")
)

// TrendLength is the periods a trend or series covers: 12 weeks, 12
// months, 8 quarters.
func TrendLength(k calendar.Kind) int {
	if k == calendar.Quarter {
		return 8
	}
	return 12
}

// FirstEligible is the first period of kind k that starts at or after the
// release epoch (R41a-3): a month or quarter is eligible only when its
// first week is.
func FirstEligible(k calendar.Kind, epoch time.Time) calendar.Period {
	p := calendar.Of(k, epoch)
	if p.From.Before(epoch) {
		p = calendar.Next(p)
	}
	return p
}

// stored is one period of the window: its body, or nil when the stored row
// is unavailable or unreadable (hidden whole).
type stored struct {
	period   calendar.Period
	body     *Body
	computed time.Time
}

// window picks the newest stored period of kind k and the stored periods
// of the TrendLength ending with it, oldest first. degraded reports a row
// stored ok whose body couldn't be read.
func window(k calendar.Kind, rows []Snapshot) (win []stored, degraded bool) {
	var latest calendar.Period
	found := false
	byFrom := map[time.Time]Snapshot{}
	for _, r := range rows {
		if r.Period.Kind != k || !calendar.Valid(k, r.Period.From, r.Period.To) {
			continue
		}
		byFrom[r.Period.From] = r
		if !found || r.Period.From.After(latest.From) {
			latest, found = r.Period, true
		}
	}
	if !found {
		return nil, false
	}
	for _, p := range calendar.Previous(latest, TrendLength(k)) {
		r, ok := byFrom[p.From]
		if !ok {
			continue // never published: no point
		}
		s := stored{period: p, computed: r.ComputedAt}
		if r.State == StateOK {
			if b, err := DecodeBody(r.Body, p); err == nil {
				s.body = &b
			} else {
				degraded = true
			}
		}
		win = append(win, s)
	}
	return win, degraded
}

// Render is GET /platform/analytics for kind k from its stored rows (any
// order; rows of other kinds are ignored). degraded reports a stored body
// that couldn't be read (served unavailable or hidden), for the log.
func Render(k calendar.Kind, rows []Snapshot, epoch time.Time) (Out, bool) {
	out := Out{Period: string(k), Changes: Unavailable{true}, Alerts: nil}
	win, degraded := window(k, rows)
	if len(win) == 0 {
		ny := NotYet{NotYet: true, FirstFrom: Stamp(FirstEligible(k, epoch).From)}
		out.Use, out.Quality, out.Hospitals = ny, ny, ny
		return out, degraded
	}
	last := win[len(win)-1]
	from, to := Stamp(last.period.From), Stamp(last.period.To)
	out.From, out.To = &from, &to
	if last.body == nil {
		out.Use, out.Quality, out.Hospitals = Unavailable{true}, Unavailable{true}, Unavailable{true}
		return out, degraded
	}
	useTrend := make([]CountPoint, 0, len(win))
	qualityTrend := make([]SharePoint, 0, len(win))
	for _, s := range win {
		c, sh := smallgroup.Guarded{Suppressed: true}, smallgroup.GuardedShare{Suppressed: true}
		if s.body != nil {
			c, sh = s.body.All.Questions, s.body.All.EscalateShare
		}
		useTrend = append(useTrend, CountPoint{Start: Stamp(s.period.From), Value: c})
		qualityTrend = append(qualityTrend, SharePoint{Start: Stamp(s.period.From), Value: sh})
	}
	out.Use = useArea(*last.body, last.computed, useTrend)
	out.Quality = qualityArea(*last.body, last.computed, qualityTrend)
	out.Hospitals = hospitalsArea(*last.body, last.computed)
	return out, degraded
}

// named is the body's hospitals whose names can be shown (a row stored
// with an empty name has every figure hidden and is left out).
func named(b Body) []HospitalRow {
	out := make([]HospitalRow, 0, len(b.Hospitals))
	for _, h := range b.Hospitals {
		if h.Hospital.Name != "" {
			out = append(out, h)
		}
	}
	return out
}

// shown collects the shown values of a column.
type shownVals []float64

func (s *shownVals) count(g smallgroup.Guarded) {
	if g.Shown() {
		*s = append(*s, float64(*g.Value))
	}
}

func (s *shownVals) share(g smallgroup.GuardedShare) {
	if g.Shown() {
		*s = append(*s, *g.Share)
	}
}

func (s *shownVals) number(n Number) {
	if n.Shown() {
		*s = append(*s, *n.Value)
	}
}

// countMedian is the median of shown counts: nothing to show with none,
// hidden strictly between 0 and 5.
func countMedian(v shownVals) CountMedian {
	m, ok := median(v)
	switch {
	case !ok:
		return CountMedian{}
	case m > 0 && m < smallgroup.Min:
		return CountMedian{Number{Suppressed: true}}
	}
	return CountMedian{Number{Value: &m}}
}

func shareMedian(v shownVals) smallgroup.GuardedShare {
	m, ok := median(v)
	if !ok {
		return smallgroup.GuardedShare{}
	}
	r := math.Round(m*math.Pow10(smallgroup.ShareDigits)) / math.Pow10(smallgroup.ShareDigits)
	return smallgroup.GuardedShare{Share: &r}
}

func ratioMedian(v shownVals) Ratio {
	if m, ok := median(v); ok {
		return RatioOf(m)
	}
	return Ratio{}
}

func hoursMedian(v shownVals) Hours {
	if m, ok := median(v); ok {
		return HoursOf(m)
	}
	return Hours{}
}

func useArea(b Body, computed time.Time, trend []CountPoint) UseArea {
	a := UseArea{UpdatedAt: Stamp(computed), Rows: []UseRow{}, Trend: trend}
	if a.Trend == nil {
		a.Trend = []CountPoint{}
	}
	var q, c, r shownVals
	for _, h := range named(b) {
		a.Rows = append(a.Rows, UseRow{Hospital: h.Hospital, Questions: h.Questions, ActiveClinicians: h.ActiveClinicians,
			QuestionsPerClinician: h.QuestionsPerClinician})
		q.count(h.Questions)
		c.count(h.ActiveClinicians)
		r.number(h.QuestionsPerClinician.Number)
	}
	a.Median = UseMedian{Questions: countMedian(q), ActiveClinicians: countMedian(c), QuestionsPerClinician: ratioMedian(r)}
	return a
}

func qualityArea(b Body, computed time.Time, trend []SharePoint) QualityArea {
	a := QualityArea{UpdatedAt: Stamp(computed), Rows: []QualityRow{}, Trend: trend}
	if a.Trend == nil {
		a.Trend = []SharePoint{}
	}
	var ans, nrs, hi, me, lo, rev, agr, hrs shownVals
	for _, h := range named(b) {
		a.Rows = append(a.Rows, QualityRow{Hospital: h.Hospital, Answers: h.Answers, NeedsReviewShare: h.EscalateShare,
			Confidence: Confidence{High: h.Pass, Medium: h.Regenerate, Low: h.Escalate}, Reviews: h.Reviews,
			AgreementRate: h.AgreementRate, MedianReviewHours: h.MedianReviewHours})
		ans.count(h.Answers)
		nrs.share(h.EscalateShare)
		hi.count(h.Pass)
		me.count(h.Regenerate)
		lo.count(h.Escalate)
		rev.count(h.Reviews)
		agr.share(h.AgreementRate)
		hrs.number(h.MedianReviewHours.Number)
	}
	a.Median = QualityMedian{Answers: countMedian(ans), NeedsReviewShare: shareMedian(nrs),
		Confidence: ConfidenceMedian{High: countMedian(hi), Medium: countMedian(me), Low: countMedian(lo)},
		Reviews:    countMedian(rev), AgreementRate: shareMedian(agr), MedianReviewHours: hoursMedian(hrs)}
	return a
}

func hospitalsArea(b Body, computed time.Time) HospitalsArea {
	a := HospitalsArea{UpdatedAt: Stamp(computed), Rows: []SizeRow{}}
	for _, h := range named(b) {
		a.Rows = append(a.Rows, SizeRow{Hospital: h.Hospital, Size: Size{Staff: h.Staff, ActiveClinicians: h.ActiveClinicians, Questions: h.Questions}})
	}
	return a
}

// median is the median of v (the mean of the middle two when even).
func median(v []float64) (float64, bool) {
	if len(v) == 0 {
		return 0, false
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2], true
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2, true
}

// seriesCell is one row's cell of a measure, and its decimals.
func seriesCell(r Row, measure string) (any, shownVals) {
	var v shownVals
	if measure == "questions_per_clinician" {
		v.number(r.QuestionsPerClinician.Number)
		return r.QuestionsPerClinician, v
	}
	v.share(r.EscalateShare)
	return r.EscalateShare, v
}

// HospitalSeries is GET /platform/analytics/hospitals/{id}/series: the
// hospital's stored cell of measure for each stored period of the latest
// TrendLength of kind k, oldest first, with the median of the other named
// hospitals' shown values (null with none). A period stored unavailable or
// unreadable is hidden whole; one that doesn't list the hospital has
// nothing to show. An id not named in any readable stored period of the
// window is ErrNoHospital.
func HospitalSeries(k calendar.Kind, rows []Snapshot, id, measure string) (SeriesOut, error) {
	digits := smallgroup.ShareDigits
	var hidden, nothing any = smallgroup.GuardedShare{Suppressed: true}, smallgroup.GuardedShare{}
	switch measure {
	case "needs_review_share":
	case "questions_per_clinician":
		digits = 2
		hidden, nothing = Ratio{Number{Suppressed: true}}, Ratio{}
	default:
		return SeriesOut{}, ErrMeasure
	}
	win, _ := window(k, rows)
	var ref *Ref
	for i := len(win) - 1; i >= 0 && ref == nil; i-- {
		if win[i].body == nil {
			continue
		}
		for _, h := range win[i].body.Hospitals {
			if h.Hospital.ID == id && h.Hospital.Name != "" {
				r := h.Hospital
				ref = &r
				break
			}
		}
	}
	if ref == nil {
		return SeriesOut{}, ErrNoHospital
	}
	out := SeriesOut{Measure: measure, Hospital: ref, Points: make([]SeriesPoint, 0, len(win))}
	for _, s := range win {
		pt := SeriesPoint{Start: Stamp(s.period.From), Value: hidden}
		if s.body != nil {
			pt.Value = nothing
			var others shownVals
			for _, h := range s.body.Hospitals {
				cell, v := seriesCell(h.Row, measure)
				switch {
				case h.Hospital.ID == id:
					pt.Value = cell
				case h.Hospital.Name != "":
					others = append(others, v...)
				}
			}
			if m, ok := median(others); ok {
				p := math.Pow10(digits)
				pt.Median = &Fixed{V: math.Round(m*p) / p, Digits: digits}
			}
		}
		out.Points = append(out.Points, pt)
	}
	return out, nil
}
