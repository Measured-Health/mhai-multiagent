package disclose

import (
	"errors"
	"strconv"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// View is one of the contract's five views.
type View string

// The five views.
const (
	Prevalence  View = "prevalence"
	Affected    View = "affected"
	CoOccurring View = "co-occurring"
	Findings    View = "findings"
	Quality     View = "quality"
)

// Views is every view, in the contract's order.
var Views = []View{Prevalence, Affected, CoOccurring, Findings, Quality}

// The filter dimensions, in canonical order (plan Task 5).
const (
	DimCondition = iota
	DimAgeBand
	DimSex
	DimLocation
	DimSetting
	DimSpecialty
	DimSource
	nDims
)

// DimNames are the filters' query names, by dimension.
var DimNames = [nDims]string{"condition", "age_band", "sex", "location", "setting", "specialty", "source"}

// The contract's closed code lists.
var (
	AgeBands       = []string{"0-17", "18-39", "40-64", "65-79", "80+"}
	Sexes          = []string{"female", "male", "other", "not_recorded"}
	Settings       = []string{"admitted", "outpatient", "emergency"}
	Confidences    = []string{"high", "caution", "review", "failed"}
	Severities     = []string{"critical", "urgent", "info"}
	DismissReasons = []string{"not_relevant", "already_known", "wrong_patient", "other"}
	Flags          = []string{"low", "high", "abnormal"}
)

// Filters are a request's filters by dimension; "" is unset. At most
// FilterCap may be set (CheckFilters).
type Filters [nDims]string

// Count is the number of filters set.
func (f Filters) Count() int {
	n := 0
	for _, v := range f {
		if v != "" {
			n++
		}
	}
	return n
}

// Without is f with dimension d unset.
func (f Filters) Without(d int) Filters { f[d] = ""; return f }

// Context is what one body answers: a frozen period and its filters.
type Context struct {
	Period  calendar.Period
	Filters Filters
	// First: nothing was recorded before the period's first day (the
	// hospital's first period at its level), so every case in it is new.
	// It is public: the attacker knows where the hospital starts.
	First bool
}

// CNE is cases, and their new and existing halves (existing = cases − new).
type CNE[C any] struct{ Cases, New, Existing C }

// ConditionRow is one row of prevalence's by_condition.
type ConditionRow[C any] struct {
	Condition string
	CNE[C]
}

// Point is one series point: a child period (or, for a week, the week).
// Its existing half is never sent.
type Point[C any] struct {
	Start      string
	Cases, New C
}

// Part is one member of a partition of the patients. Key "" is the null
// row. Setting is a location row's setting ("" none).
type Part[C any] struct {
	Key, Setting string
	Patients     C
}

// Pair is a co-occurring pair: A's patients, those with both, and the
// share of A's with both.
type Pair[C, S any] struct {
	A, B      string
	APatients C
	Patients  C
	ShareOfA  S
}

// FindingRow is one finding (name and flag; "" is null).
type FindingRow[C any] struct {
	Name, Flag        string
	Patients, Results C
}

// ReasonRow is one dismissal reason's count.
type ReasonRow[C any] struct {
	Reason string
	Count  C
}

// Alerts is the findings view's alert block. The medians are over the
// acknowledged alerts' minutes (all, and critical).
type Alerts[C, M any] struct {
	Total, Critical, Urgent, Info C
	MedianAck, CriticalMedianAck  M
	Dismissed                     C
	DismissReasons                []ReasonRow[C]
}

// Corrections are a condition's corrections by status.
type Corrections[C any] struct{ Total, Approved, Rejected, Waiting C }

// QualityRow is one condition's consultations. Agreed is never sent: it is
// published through AgreementRate (agreed of rated), which a shown rated
// turns back into agreed.
type QualityRow[C, S any] struct {
	Condition                                    string
	Consultations, High, Caution, Review, Failed C
	Rated, Agreed                                C
	AgreementRate                                S
	Corrections                                  Corrections[C]
}

// Report is the five views of one context, with counts C, shares S and
// medians M: Raw holds raw counts (memory only, one computation), Body the
// guarded values. Meta's patients is Patients, shared by every view.
type Report[C, S, M any] struct {
	Context Context
	// Whole: hidden whole (A7, or D5's T5): every number hidden, every
	// list empty. Set on a Body only.
	Whole bool
	// Sources are the period's sources (ids; "" the patients with none):
	// a partition of the patients that no view lists, given to the guard so
	// the unfiltered context is the sum of its sources (PR 3 review,
	// condition 2). Never sent; empty in a body hidden whole.
	Sources     []string
	Patients    C
	Total       CNE[C]
	ByCondition []ConditionRow[C]
	Series      []Point[C]
	ByAge       []Part[C]
	BySex       []Part[C]
	BySetting   []Part[C]
	ByLocation  []Part[C]
	BySpecialty []Part[C]
	Pairs       []Pair[C, S]
	Findings    []FindingRow[C]
	Alerts      Alerts[C, M]
	Quality     []QualityRow[C, S]
}

// Raw is one context's raw values, as the definer functions return them:
// counts, and the acknowledged minutes the medians need. Every list holds
// every row with a non-zero count (a row it leaves out is 0), except that
// pairs and quality rows may also be listed at 0.
type Raw = Report[int64, struct{}, []int64]

// Body is one context's guarded response: never a raw value (a test pins
// it). It is what is stored and served; render (PR 4) adds names, drill
// keys and updated_at.
type Body struct {
	Report[smallgroup.Guarded, smallgroup.GuardedShare, GuardedMinutes]
}

// GuardedMinutes is a median in minutes (a whole number or a half), or
// hidden; {null, false} is nothing to measure.
type GuardedMinutes struct {
	Value      *float64
	Suppressed bool
}

// MarshalJSON writes {"value": m | null, "suppressed": bool}.
func (g GuardedMinutes) MarshalJSON() ([]byte, error) {
	if g.Suppressed || g.Value == nil {
		if g.Suppressed {
			return []byte(`{"value":null,"suppressed":true}`), nil
		}
		return []byte(`{"value":null,"suppressed":false}`), nil
	}
	return []byte(`{"value":` + strconv.FormatFloat(*g.Value, 'f', -1, 64) + `,"suppressed":false}`), nil
}

// Forced is groups to hide whatever the rules say, by group key: from D3
// (ParentForced) and D5 (FamilyForced).
type Forced map[string]bool

// Union adds g's groups to f (f may be nil) and returns it.
func (f Forced) Union(g Forced) Forced {
	if f == nil {
		f = Forced{}
	}
	for k, v := range g {
		if v {
			f[k] = true
		}
	}
	return f
}

// ChildBodies is a child period's raw values and its stored body, for D3.
type ChildBodies struct {
	Raw  Raw
	Body Body
}

// ParentContext is a stored filter parent (P, F \ {f}), for D5.
type ParentContext struct {
	Body Body
}

// HistoryPoint is one earlier period of a history, copied from its stored
// body (D3).
type HistoryPoint struct {
	Start                string
	Cases, New, Patients smallgroup.Guarded
}

// The errors.
var (
	// ErrInconsistent: the raw values don't add up, or a key repeats. The
	// context is hidden whole, and the caller raises the alert (§12).
	ErrInconsistent = errors.New("disclose: inconsistent raw values")
	// ErrTooManyFilters: more than FilterCap filters (422 too_many_filters).
	ErrTooManyFilters = errors.New("disclose: too many filters")
	// ErrInvalid: a filter or period that isn't one (422 validation_failed).
	ErrInvalid = errors.New("disclose: invalid filter or period")
)

// PeriodOf reads a period from its level and first day (YYYY-MM-DD, a
// hospital day), on 7c's calendar: anything that isn't a period's first
// day is ErrInvalid. Whether it is frozen is the database's (D2).
func PeriodOf(level, start string) (calendar.Period, error) {
	k, err := calendar.ParseKind(level)
	if err != nil {
		return calendar.Period{}, ErrInvalid
	}
	t, err := time.Parse(time.DateOnly, start)
	if err != nil || t.Format(time.DateOnly) != start {
		return calendar.Period{}, ErrInvalid
	}
	p := calendar.Of(k, t)
	if !p.From.Equal(t) {
		return calendar.Period{}, ErrInvalid
	}
	return p, nil
}
