package scheduler

import (
	"math"
	"time"
)

// MaxBackoff caps the wait after a failure or a 429 (design §7.1).
const MaxBackoff = 15 * time.Minute

// Stale reports whether a source counts as stale (design §15.4): no
// successful poll for 3 poll intervals; for file_export also no new file
// for longer than the declared interval plus 50% (required for that type).
// A source we can't judge (an unknown type, no interval, never polled, a
// last success in the future) is stale: doubt never reads as fresh.
func Stale(connType string, lastOK time.Time, pollSeconds int, expectedEveryMinutes *int, lastNewFile *time.Time, now time.Time) bool {
	if pollSeconds <= 0 || lastOK.IsZero() || lastOK.After(now) {
		return true
	}
	if now.Sub(lastOK) > 3*time.Duration(pollSeconds)*time.Second {
		return true
	}
	switch connType {
	case "fhir_r4", "document_archive":
		return false
	case "file_export":
		if expectedEveryMinutes == nil || *expectedEveryMinutes <= 0 || lastNewFile == nil || lastNewFile.After(now) {
			return true
		}
		limit := time.Duration(*expectedEveryMinutes) * time.Minute * 3 / 2
		return now.Sub(*lastNewFile) > limit
	}
	return true
}

// NextDue is when a connection is polled next after a run with outcome
// code (design §7.1). ok: one interval with jitter (jitter in [-1, 1] is
// ±10%). A 429 waits what the source named, capped at MaxBackoff and never
// less than one interval; any other failure (and a 429 naming nothing)
// doubles from the interval per consecutive failure, capped at MaxBackoff
// or the interval when that is longer.
func NextDue(now time.Time, poll time.Duration, code string, failures int, retryAfter time.Duration, jitter float64) time.Time {
	if poll <= 0 {
		poll = 60 * time.Second
	}
	jitter = math.Max(-1, math.Min(1, jitter))
	if code == "ok" {
		return now.Add(poll + time.Duration(float64(poll)*0.1*jitter))
	}
	limit := max(MaxBackoff, poll)
	if code == "too_many_requests" && retryAfter > 0 {
		return now.Add(max(poll, min(retryAfter, MaxBackoff)))
	}
	d := poll
	for i := 0; i < failures && d < limit; i++ {
		d *= 2
	}
	return now.Add(min(d, limit))
}
