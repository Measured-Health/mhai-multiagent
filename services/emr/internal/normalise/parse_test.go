package normalise

import (
	"testing"
	"time"
)

// Review Focus 3: the strict parser accepts only the configured separator;
// comparators come apart; units, exponents, NaN, Unicode minus and
// thousands separators are refused.
func TestStrictDecimal(t *testing.T) {
	type want struct {
		v   float64
		cmp string
		ok  bool
	}
	for _, c := range []struct {
		in  string
		sep rune
		want
	}{
		{"6.5", '.', want{6.5, "", true}},
		{" 6.5 ", '.', want{6.5, "", true}},
		{"5,5", ',', want{5.5, "", true}},
		{"-3", '.', want{-3, "", true}},
		{"0", '.', want{0, "", true}},
		{"<0.5", '.', want{0.5, "<", true}},
		{">100", '.', want{100, ">", true}},
		{">= 7", '.', want{7, ">=", true}},
		{"5,5", '.', want{}},
		{"5.5", ',', want{}},
		{"1,234.5", '.', want{}},
		{"1.234,5", ',', want{}},
		{"6.5 mmol/l", '.', want{}},
		{"\u22123", '.', want{}},
		{"6.5e1", '.', want{}},
		{"1E3", '.', want{}},
		{"NaN", '.', want{}},
		{"Inf", '.', want{}},
		{"+3", '.', want{}},
		{".5", '.', want{}},
		{"5.", '.', want{}},
		{"", '.', want{}},
		{"--3", '.', want{}},
		{"5.5.5", '.', want{}},
		{"0x10", '.', want{}},
		{"5\u00a05", '.', want{}},
		{"6.5", ';', want{}},
	} {
		v, cmp, err := ParseDecimal(c.in, c.sep)
		if (err == nil) != c.ok || (c.ok && (v != c.v || cmp != c.cmp)) {
			t.Errorf("ParseDecimal(%q, %q) = %v %q %v, want %v %q ok=%v", c.in, c.sep, v, cmp, err, c.v, c.cmp, c.ok)
		}
	}
}

// Review Focus 1, A10: a date is a date; a dateTime without an offset is
// refused; an offset is applied; 9 minutes ahead passes, 11 doesn't.
func TestTimesDateOnlyNoOffsetOffsetAndFuture(t *testing.T) {
	lagos, err := time.LoadLocation("Africa/Lagos")
	if err != nil {
		lagos = time.FixedZone("WAT", 3600)
	}
	d, dateOnly, err := ParseFHIRTime("2026-11-07", lagos)
	if err != nil || !dateOnly || d.Hour() != 0 || d.Format("2006-01-02") != "2026-11-07" {
		t.Fatalf("date: %v %v %v", d, dateOnly, err)
	}
	for _, bad := range []string{"2026-11-07T08:00:00", "2026-11-07T08:00", "2026-11", "2026", "07/11/2026",
		"2026-11-07 08:00:00Z", "2026-11-07T08:00:00+0100", "2026-13-01", "2026-11-07T25:00:00Z", ""} {
		if _, _, err := ParseFHIRTime(bad, lagos); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	got, dateOnly, err := ParseFHIRTime("2026-11-07T00:30:00+01:00", lagos)
	if err != nil || dateOnly || !got.Equal(time.Date(2026, 11, 6, 23, 30, 0, 0, time.UTC)) || got.Location() != time.UTC {
		t.Fatalf("offset across midnight UTC: %v %v %v", got, dateOnly, err)
	}
	if got, _, err := ParseFHIRTime("2026-11-07T08:00:00.123Z", nil); err != nil || got.Nanosecond() != 123000000 {
		t.Fatalf("fractional seconds: %v %v", got, err)
	}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	if err := CheckFuture(now.Add(9*time.Minute), now); err != nil {
		t.Errorf("9 minutes ahead refused: %v", err)
	}
	if err := CheckFuture(now.Add(11*time.Minute), now); err == nil {
		t.Error("11 minutes ahead accepted")
	}
	if err := CheckFuture(now.Add(time.Hour), now); err == nil {
		t.Error("1 hour ahead accepted")
	}
}
