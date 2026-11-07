// Package analytics is the stored shape of platform analytics (Plan 7c spec
// §7, invariant I2): Body, one finished period's guarded figures, as the
// scheduler stores it in analytics_snapshots and the routes read it back.
// Everything in a Body is a smallgroup guarded value: the render code
// takes a Body as its only input and has no access to raw counts. The
// builder and the scheduler, which do, live in internal/analytics/scheduler;
// no route package may import it (a test fails if one does).
package analytics

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// MaxBodyBytes bounds a stored body (the table's CHECK is the same).
const MaxBodyBytes = 4 << 20

// MinReviewsForMedian is the decided corrections a median review time needs
// (spec §5): fewer and it is hidden.
const MinReviewsForMedian = 5

// Body is one period's stored figures.
type Body struct {
	Kind      string        `json:"kind"`
	From      string        `json:"from"` // first day, 2006-01-02
	To        string        `json:"to"`   // the day after the last
	All       Row           `json:"all"`
	Hospitals []HospitalRow `json:"hospitals"`
}

// Ref names a hospital.
type Ref struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// HospitalRow is one hospital's figures and its staff count.
type HospitalRow struct {
	Hospital Ref                `json:"hospital"`
	Staff    smallgroup.Guarded `json:"staff"`
	Row
}

// HideSmallStaff puts the staff count under the small-group rule (a count
// of 1 to 4 hidden) until the privacy lead signs off Q4 (spec §16: the
// staff count as a plain number). Switching it off is this code change:
// set it false and make HospitalRow.Staff a plain number again (a guarded
// value refuses a shown 1 to 4 when read back).
const HideSmallStaff = true

// StaffFigure is a hospital's staff count as stored.
func StaffFigure(n int64) smallgroup.Guarded {
	if HideSmallStaff {
		return smallgroup.Count(n)
	}
	return smallgroup.Guarded{Value: &n}
}

// Row is one row's guarded figures: the QG family (questions, answers and
// the decision mix, degraded, the three shares of answers), the R family
// (reviews, agreement rate), and the non-linear figures.
type Row struct {
	Questions             smallgroup.Guarded      `json:"questions"`
	Answers               smallgroup.Guarded      `json:"answers"`
	Pass                  smallgroup.Guarded      `json:"pass"`
	Regenerate            smallgroup.Guarded      `json:"regenerate"`
	Escalate              smallgroup.Guarded      `json:"escalate"`
	Degraded              smallgroup.Guarded      `json:"degraded"`
	PassShare             smallgroup.GuardedShare `json:"pass_share"`
	RegenerateShare       smallgroup.GuardedShare `json:"regenerate_share"`
	EscalateShare         smallgroup.GuardedShare `json:"escalate_share"`
	Reviews               smallgroup.Guarded      `json:"reviews"`
	AgreementRate         smallgroup.GuardedShare `json:"agreement_rate"`
	ActiveClinicians      smallgroup.Guarded      `json:"active_clinicians"`
	QuestionsPerClinician Ratio                   `json:"questions_per_clinician"`
	MedianReviewHours     Hours                   `json:"median_review_hours"`
}

// Ratio is a Number written with two decimals (questions per clinician),
// Hours one with one (a median review time): a figure is never written as
// a bare 1 to 4, which would read as a small count.
type (
	Ratio struct{ Number }
	Hours struct{ Number }
)

// RatioOf and HoursOf round v and show it.
func RatioOf(v float64) Ratio { return Ratio{NumberOf(v, 2)} }
func HoursOf(v float64) Hours { return Hours{NumberOf(v, 1)} }

func (r Ratio) MarshalJSON() ([]byte, error) { return r.Number.marshal(2) }
func (h Hours) MarshalJSON() ([]byte, error) { return h.Number.marshal(1) }

// Number is a guarded non-count figure (a ratio or a median), encoded as
// {value, suppressed}: shown, hidden ({null, true}) or nothing to show
// ({null, false}).
type Number struct {
	Value      *float64
	Suppressed bool
}

// Shown reports whether the value is sent.
func (n Number) Shown() bool { return !n.Suppressed && n.Value != nil }

// NumberOf rounds v to digits places, shown.
func NumberOf(v float64, digits int) Number {
	p := math.Pow10(digits)
	r := math.Round(v*p) / p
	return Number{Value: &r}
}

// marshal writes {"value": n | null, "suppressed": bool}, n with digits
// decimals.
func (n Number) marshal(digits int) ([]byte, error) {
	if !n.Shown() || math.IsNaN(*n.Value) || math.IsInf(*n.Value, 0) || *n.Value < 0 {
		return json.Marshal(struct {
			Value      *float64 `json:"value"`
			Suppressed bool     `json:"suppressed"`
		}{nil, n.Suppressed || n.Value != nil})
	}
	return []byte(`{"value":` + strconv.FormatFloat(*n.Value, 'f', digits, 64) + `,"suppressed":false}`), nil
}

// UnmarshalJSON reads {value, suppressed} strictly.
func (n *Number) UnmarshalJSON(b []byte) error {
	var v struct {
		Value      *float64 `json:"value"`
		Suppressed *bool    `json:"suppressed"`
	}
	if err := strict(b, &v); err != nil {
		return err
	}
	switch {
	case v.Suppressed == nil:
		return errBody
	case v.Value == nil:
		*n = Number{Suppressed: *v.Suppressed}
	case !*v.Suppressed && *v.Value >= 0 && !math.IsInf(*v.Value, 0):
		x := *v.Value
		*n = Number{Value: &x}
	default:
		return errBody
	}
	return nil
}

var errBody = errors.New("analytics: not a stored body")

func strict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		return errBody
	}
	return nil
}

// Dates are a period's first day and the day after its last.
func Dates(p calendar.Period) (from, to string) {
	return p.From.Format(time.DateOnly), p.To.Format(time.DateOnly)
}

// DecodeBody reads a stored body strictly (A16): no unknown field; every
// value a guarded one (a shown count of 1 to 4 is refused by smallgroup's
// decoders); the kind and dates of p; canonical hospital ids, each once,
// with names that can be shown (or, for a hospital whose name can't be,
// an empty name, no staff count and every figure hidden: renderers leave
// such a row out).
func DecodeBody(b []byte, p calendar.Period) (Body, error) {
	var out Body
	if len(b) == 0 || len(b) > MaxBodyBytes {
		return Body{}, errBody
	}
	if err := strict(b, &out); err != nil {
		return Body{}, err
	}
	from, to := Dates(p)
	if out.Kind != string(p.Kind) || out.From != from || out.To != to || out.Hospitals == nil {
		return Body{}, errBody
	}
	seen := map[string]bool{}
	for _, h := range out.Hospitals {
		id, err := uuid.Parse(h.Hospital.ID)
		named := audit.ShowableName(h.Hospital.Name) || (h.Hospital.Name == "" && !h.Staff.Shown() && h.Row.AllHidden())
		if err != nil || id.String() != h.Hospital.ID || seen[h.Hospital.ID] || !named {
			return Body{}, errBody
		}
		seen[h.Hospital.ID] = true
	}
	return out, nil
}

// AllHidden reports whether no figure of the row is shown.
func (r Row) AllHidden() bool {
	for _, g := range []smallgroup.Guarded{r.Questions, r.Answers, r.Pass, r.Regenerate, r.Escalate, r.Degraded, r.Reviews, r.ActiveClinicians} {
		if g.Shown() {
			return false
		}
	}
	for _, g := range []smallgroup.GuardedShare{r.PassShare, r.RegenerateShare, r.EscalateShare, r.AgreementRate} {
		if g.Shown() {
			return false
		}
	}
	return !r.QuestionsPerClinician.Shown() && !r.MedianReviewHours.Shown()
}
