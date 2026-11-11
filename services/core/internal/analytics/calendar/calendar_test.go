package calendar

import (
	"testing"
	"time"
)

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// every month from 2024 to 2030.
func months() []Period {
	var out []Period
	for t := day(2024, 1, 1); t.Before(day(2031, 1, 1)); t = t.AddDate(0, 1, 0) {
		out = append(out, Of(Month, t))
	}
	return out
}

// R41a-2: month-aligned weeks (1–7, 8–14, 15–21, 22–end) tile every month,
// with no gap or overlap, 7 to 10 days each.
func TestWeeksTileEveryMonth(t *testing.T) {
	ms := months()
	if len(ms) != 84 {
		t.Fatalf("%d months", len(ms))
	}
	for _, m := range ms {
		if m.Kind != Month || m.From.Day() != 1 || !m.To.Equal(m.From.AddDate(0, 1, 0)) {
			t.Fatalf("month %v", m)
		}
		ws := Children(m)
		if len(ws) != 4 {
			t.Fatalf("%v: %d weeks", m, len(ws))
		}
		at := m.From
		for i, w := range ws {
			days := int(w.To.Sub(w.From).Hours() / 24)
			if w.Kind != Week || !w.From.Equal(at) || days < 7 || days > 10 || (i < 3 && days != 7) {
				t.Fatalf("%v: week %d is %v (%d days)", m, i, w, days)
			}
			if w.From.Day() != []int{1, 8, 15, 22}[i] {
				t.Fatalf("%v: week %d starts on %d", m, i, w.From.Day())
			}
			at = w.To
		}
		if !at.Equal(m.To) {
			t.Fatalf("%v: weeks end at %v", m, at)
		}
	}
	// Every day of 2024–2030 lies in exactly one week, and Of finds it.
	for d := day(2024, 1, 1); d.Before(day(2031, 1, 1)); d = d.AddDate(0, 0, 1) {
		w := Of(Week, d.Add(13*time.Hour))
		if d.Before(w.From) || !d.Before(w.To) {
			t.Fatalf("%v not in its week %v", d, w)
		}
		if !Valid(Week, w.From, w.To) {
			t.Fatalf("%v not valid", w)
		}
	}
}

// Every week lies in exactly one month, every month in one quarter; the
// children of a period's parent include it; a quarter is its own block.
func TestEveryPeriodNestsInItsParent(t *testing.T) {
	for _, m := range months() {
		q, ok := Parent(m)
		if !ok || q.Kind != Quarter || !q.Contains(m) || !BlockOf(m).Equal(q) {
			t.Fatalf("month %v: parent %v %v", m, q, ok)
		}
		found := 0
		for _, c := range Children(q) {
			if c.Equal(m) {
				found++
			}
		}
		if found != 1 || len(Children(q)) != 3 {
			t.Fatalf("month %v appears %d times in %v", m, found, Children(q))
		}
		for _, w := range Children(m) {
			p, ok := Parent(w)
			if !ok || !p.Equal(m) || !BlockOf(w).Equal(q) || !q.Contains(w) {
				t.Fatalf("week %v: parent %v block %v", w, p, BlockOf(w))
			}
		}
		if !BlockOf(q).Equal(q) {
			t.Fatalf("quarter %v: block %v", q, BlockOf(q))
		}
		if _, ok := Parent(q); ok {
			t.Fatalf("a quarter has a parent")
		}
		if q.From.Month()%3 != 1 || !q.To.Equal(q.From.AddDate(0, 3, 0)) {
			t.Fatalf("quarter %v", q)
		}
	}
	// No period crosses a quarter boundary: every week of a quarter is one
	// of its 12.
	q := Of(Quarter, day(2026, 11, 7))
	var weeks []Period
	for _, m := range Children(q) {
		weeks = append(weeks, Children(m)...)
	}
	if len(weeks) != 12 || !weeks[0].From.Equal(day(2026, 10, 1)) || !weeks[11].To.Equal(day(2027, 1, 1)) {
		t.Fatalf("Q4 2026 weeks %v", weeks)
	}
}

// R43: a period is finished 15 minutes after its end.
func TestLatestWaitsForGrace(t *testing.T) {
	nov := day(2026, 11, 1)
	if got := Latest(Month, nov.Add(14*time.Minute)); !got.From.Equal(day(2026, 9, 1)) {
		t.Errorf("at 00:14 on 1 Nov: %v", got)
	}
	if got := Latest(Month, nov.Add(14*time.Minute+59*time.Second)); !got.From.Equal(day(2026, 9, 1)) {
		t.Errorf("at 00:14:59 on 1 Nov: %v", got)
	}
	if got := Latest(Month, nov.Add(15*time.Minute)); !got.From.Equal(day(2026, 10, 1)) {
		t.Errorf("at 00:15 on 1 Nov: %v", got)
	}
	if got := Latest(Week, nov.Add(15*time.Minute)); !got.From.Equal(day(2026, 10, 22)) || !got.To.Equal(nov) {
		t.Errorf("week at 00:15 on 1 Nov: %v", got)
	}
	if got := Latest(Week, day(2026, 11, 8).Add(10*time.Minute)); !got.From.Equal(day(2026, 10, 22)) {
		t.Errorf("week at 00:10 on 8 Nov: %v", got)
	}
	if got := Latest(Quarter, day(2026, 10, 1).Add(15*time.Minute)); !got.From.Equal(day(2026, 7, 1)) {
		t.Errorf("quarter at 00:15 on 1 Oct: %v", got)
	}
	if Grace != 15*time.Minute {
		t.Errorf("grace %v", Grace)
	}
}

// R41a: the latest month is the same for every minute from 1 Nov 00:15 to
// 1 Dec 00:14.
func TestLatestIsStableWithinPeriod(t *testing.T) {
	want := Of(Month, day(2026, 10, 15))
	for at := day(2026, 11, 1).Add(15 * time.Minute); at.Before(day(2026, 12, 1).Add(15 * time.Minute)); at = at.Add(time.Minute) {
		if got := Latest(Month, at); !got.Equal(want) {
			t.Fatalf("at %v: %v", at, got)
		}
	}
	// And the next minute moves on.
	if got := Latest(Month, day(2026, 12, 1).Add(15*time.Minute)); !got.From.Equal(day(2026, 11, 1)) {
		t.Fatalf("on 1 Dec 00:15: %v", got)
	}
}

// A3 (unit): Parse refuses anything that isn't a finished nested period.
func TestParseRefusesNonPeriods(t *testing.T) {
	now := day(2026, 11, 7).Add(9 * time.Hour)
	for _, c := range []struct{ kind, from string }{
		{"week", "2026-10-02"},  // not a week start
		{"week", "2026-10-05"},  // a Monday-to-Sunday week
		{"week", "2026-10-29"},  // not a week start
		{"month", "2026-10-02"}, // a rolling 30 days
		{"month", "2026-09-07"}, // a rolling month
		{"quarter", "2026-08-01"},
		{"quarter", "2026-10-01"}, // not finished
		{"month", "2026-11-01"},   // not finished
		{"week", "2026-11-01"},    // ends 8 Nov: not finished
		{"week", "2027-01-01"},    // future
		{"30d", "2026-10-01"},
		{"7d", "2026-10-01"},
		{"", "2026-10-01"},
		{"week", ""},
		{"week", "2026-10-01T00:00:00Z"},
		{"week", "01-10-2026"},
		{"Week", "2026-10-01"},
	} {
		if p, err := Parse(c.kind, c.from, now); err == nil {
			t.Errorf("Parse(%q, %q) = %v", c.kind, c.from, p)
		}
	}
	for _, c := range []struct{ kind, from, to string }{
		{"week", "2026-10-22", "2026-11-01"},
		{"week", "2026-10-01", "2026-10-08"},
		{"month", "2026-10-01", "2026-11-01"},
		{"quarter", "2026-07-01", "2026-10-01"},
	} {
		p, err := Parse(c.kind, c.from, now)
		if err != nil || p.From.Format(time.DateOnly) != c.from || p.To.Format(time.DateOnly) != c.to {
			t.Errorf("Parse(%q, %q) = %v, %v", c.kind, c.from, p, err)
		}
	}
	for _, k := range []string{"7d", "30d", "90d", "", "WEEK", "year"} {
		if _, err := ParseKind(k); err == nil {
			t.Errorf("ParseKind(%q) accepted", k)
		}
	}
	for _, k := range []string{"week", "month", "quarter"} {
		if got, err := ParseKind(k); err != nil || string(got) != k {
			t.Errorf("ParseKind(%q) = %v %v", k, got, err)
		}
	}
	// Valid: the shape alone.
	if Valid(Week, day(2026, 10, 5), day(2026, 10, 12)) || Valid(Month, day(2026, 10, 1), day(2026, 10, 31)) ||
		Valid(Week, day(2026, 10, 22), day(2026, 10, 29)) || Valid(Kind("7d"), day(2026, 10, 1), day(2026, 10, 8)) ||
		Valid(Week, day(2026, 10, 1).Add(time.Hour), day(2026, 10, 8).Add(time.Hour)) {
		t.Error("Valid accepted a non-period")
	}
}

// Previous lists n periods ending with p, oldest first; Next and Prev step.
func TestPreviousNextPrev(t *testing.T) {
	w := Of(Week, day(2026, 11, 3))
	ps := Previous(w, 12)
	if len(ps) != 12 || !ps[11].Equal(w) || !ps[0].From.Equal(day(2026, 8, 8)) {
		t.Fatalf("12 weeks to %v: %v", w, ps)
	}
	for i := 1; i < len(ps); i++ {
		if !ps[i-1].To.Equal(ps[i].From) || !Next(ps[i-1]).Equal(ps[i]) || !Prev(ps[i]).Equal(ps[i-1]) {
			t.Fatalf("not successive: %v %v", ps[i-1], ps[i])
		}
	}
	q := Of(Quarter, day(2026, 11, 3))
	qs := Previous(q, 8)
	if len(qs) != 8 || !qs[0].From.Equal(day(2025, 1, 1)) {
		t.Fatalf("8 quarters: %v", qs)
	}
	if !Next(Of(Month, day(2026, 12, 9))).From.Equal(day(2027, 1, 1)) {
		t.Fatal("next month across a year")
	}
}
