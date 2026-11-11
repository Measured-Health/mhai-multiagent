package normalise

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// ErrUnreadable: a value or time that isn't exactly what the source format
// allows. The record is rejected as unreadable_data; nothing is half-parsed.
var ErrUnreadable = errors.New("normalise: unreadable value")

// ParseDecimal reads a source decimal strictly (EMR design §9.2; Review
// Focus 3). sep is the connection's decimal separator, '.' or ','; never
// inferred from the value. Surrounding spaces are allowed. A leading
// comparator (<, <=, >, >=) is returned apart: the caller keeps such a
// value as text, never as a number equal to its bound. Refused: the other
// separator, a thousands separator, a unit, an exponent, a sign other than
// a leading ASCII '-', NaN and infinities, and anything else.
func ParseDecimal(s string, sep rune) (float64, string, error) {
	if sep != '.' && sep != ',' {
		return 0, "", ErrUnreadable
	}
	s = strings.Trim(s, " ")
	comparator := ""
	for _, c := range []string{"<=", ">=", "<", ">"} {
		if strings.HasPrefix(s, c) {
			comparator, s = c, strings.TrimLeft(s[len(c):], " ")
			break
		}
	}
	body := strings.TrimPrefix(s, "-")
	intPart, frac, hasSep := strings.Cut(body, string(sep))
	if !digits(intPart) || (hasSep && !digits(frac)) {
		return 0, "", ErrUnreadable
	}
	v, err := strconv.ParseFloat(strings.Replace(s, string(sep), ".", 1), 64)
	if err != nil {
		return 0, "", ErrUnreadable
	}
	return v, comparator, nil
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// MaxFuture is how far ahead of now a source time may be (design §9.3).
const MaxFuture = 10 * time.Minute

// ParseFHIRTime reads a FHIR date or dateTime (Review Focus 1). A date
// (YYYY-MM-DD) is a date only: dateOnly is true and t is midnight of that
// date in hospitalTZ, and no hour may be read from it. A dateTime must
// carry seconds and an offset (Z or ±hh:mm), as FHIR requires; without one
// it is refused, never read in a guessed zone. t is in UTC. A partial date
// (a year, or a year and month) is refused.
func ParseFHIRTime(s string, hospitalTZ *time.Location) (time.Time, bool, error) {
	if hospitalTZ == nil {
		hospitalTZ = time.UTC
	}
	if len(s) == len("2006-01-02") {
		d, err := time.ParseInLocation("2006-01-02", s, hospitalTZ)
		if err != nil {
			return time.Time{}, false, ErrUnreadable
		}
		return d, true, nil
	}
	if !strings.Contains(s, "T") || len(s) < len("2006-01-02T15:04:05Z") {
		return time.Time{}, false, ErrUnreadable
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false, ErrUnreadable
	}
	return t.UTC(), false, nil
}

// CheckFuture refuses a time more than MaxFuture after now (A10).
func CheckFuture(t, now time.Time) error {
	if t.After(now.Add(MaxFuture)) {
		return ErrUnreadable
	}
	return nil
}
