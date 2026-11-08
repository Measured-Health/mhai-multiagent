// Package calendar is the nested reporting calendar of platform analytics
// (Plan 7c spec §4; rulings R41a and R41a-2). All in UTC.
//
// A period is (kind, [From, To)):
//
//   - week: a month-aligned block, days 1–7, 8–14, 15–21 and 22 to the
//     month's end (7 to 10 days). No other week exists anywhere in core.
//   - month: a calendar month.
//   - quarter: Jan–Mar, Apr–Jun, Jul–Sep, Oct–Dec.
//
// Weeks nest in months and months in quarters, so no period crosses a
// quarter boundary: the block of a period is its quarter. A period is
// finished Grace after its end. The same rules are written once more in
// SQL, mhai_reporting_period_ok (migration 00019); a test runs both over
// every day from 2024 to 2030.
package calendar

import (
	"errors"
	"time"
)

// Kind is a period kind.
type Kind string

// The three kinds.
const (
	Week    Kind = "week"
	Month   Kind = "month"
	Quarter Kind = "quarter"
)

// Grace is how long after a period ends its figures wait (R43): longer than
// any write transaction, so a commit dated inside the period is counted.
// platform.GuardrailsGrace is this constant.
const Grace = 15 * time.Minute

// Period is one reporting period, [From, To) in UTC.
type Period struct {
	Kind     Kind
	From, To time.Time
}

// Equal reports whether p and q are the same period.
func (p Period) Equal(q Period) bool {
	return p.Kind == q.Kind && p.From.Equal(q.From) && p.To.Equal(q.To)
}

// Contains reports whether q lies inside p (p contains itself).
func (p Period) Contains(q Period) bool {
	return !q.From.Before(p.From) && !q.To.After(p.To)
}

// Finished reports whether p ended at least Grace before now.
func (p Period) Finished(now time.Time) bool { return !p.To.Add(Grace).After(now) }

// String is "kind from" (dates only), for tests and errors.
func (p Period) String() string {
	return string(p.Kind) + " " + p.From.Format(time.DateOnly) + "–" + p.To.Format(time.DateOnly)
}

var weekStarts = [4]int{1, 8, 15, 22}

func dayOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func monthOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// Of is the period of kind k that contains t. An unknown kind gives the
// zero Period.
func Of(k Kind, t time.Time) Period {
	m := monthOf(t)
	switch k {
	case Week:
		d := t.UTC().Day()
		i := 3
		for i > 0 && d < weekStarts[i] {
			i--
		}
		from := m.AddDate(0, 0, weekStarts[i]-1)
		to := from.AddDate(0, 0, 7)
		if i == 3 {
			to = m.AddDate(0, 1, 0)
		}
		return Period{Week, from, to}
	case Month:
		return Period{Month, m, m.AddDate(0, 1, 0)}
	case Quarter:
		q := time.Date(m.Year(), time.Month((int(m.Month())-1)/3*3+1), 1, 0, 0, 0, 0, time.UTC)
		return Period{Quarter, q, q.AddDate(0, 3, 0)}
	}
	return Period{}
}

// Next is the period of the same kind after p.
func Next(p Period) Period { return Of(p.Kind, p.To) }

// Prev is the period of the same kind before p.
func Prev(p Period) Period { return Of(p.Kind, p.From.Add(-time.Hour)) }

// Latest is the newest period of kind k finished at now: the one whose
// To + Grace ≤ now.
func Latest(k Kind, now time.Time) Period {
	return Prev(Of(k, now.UTC().Add(-Grace)))
}

// Previous is the n periods of p's kind ending with p, oldest first.
func Previous(p Period, n int) []Period {
	if n <= 0 {
		return nil
	}
	out := make([]Period, n)
	for i := n - 1; i >= 0; i-- {
		out[i] = p
		p = Prev(p)
	}
	return out
}

// Parent is a week's month or a month's quarter; a quarter has none.
func Parent(p Period) (Period, bool) {
	switch p.Kind {
	case Week:
		return Of(Month, p.From), true
	case Month:
		return Of(Quarter, p.From), true
	}
	return Period{}, false
}

// Children is a month's four weeks or a quarter's three months, in order;
// a week has none.
func Children(p Period) []Period {
	var k Kind
	switch p.Kind {
	case Month:
		k = Week
	case Quarter:
		k = Month
	default:
		return nil
	}
	var out []Period
	for c := Of(k, p.From); c.From.Before(p.To); c = Next(c) {
		out = append(out, c)
	}
	return out
}

// BlockOf is the quarter that contains p.
func BlockOf(p Period) Period { return Of(Quarter, p.From) }

// Valid reports whether [from, to) is a period of kind k: the shape alone,
// the Go twin of SQL's mhai_reporting_period_ok.
func Valid(k Kind, from, to time.Time) bool {
	if _, err := ParseKind(string(k)); err != nil {
		return false
	}
	p := Of(k, from)
	return dayOf(from).Equal(from) && p.From.Equal(from) && p.To.Equal(to)
}

var errPeriod = errors.New("calendar: not a finished reporting period")

// ParseKind reads "week", "month" or "quarter".
func ParseKind(s string) (Kind, error) {
	switch Kind(s) {
	case Week, Month, Quarter:
		return Kind(s), nil
	}
	return "", errPeriod
}

// Parse reads a period from its kind and its first day ("2006-01-02") and
// refuses anything that isn't a period of that kind, finished at now.
func Parse(kind, from string, now time.Time) (Period, error) {
	k, err := ParseKind(kind)
	if err != nil {
		return Period{}, err
	}
	t, err := time.Parse(time.DateOnly, from)
	if err != nil || t.Format(time.DateOnly) != from {
		return Period{}, errPeriod
	}
	p := Of(k, t)
	if !p.From.Equal(t) || !p.Finished(now) {
		return Period{}, errPeriod
	}
	return p, nil
}
