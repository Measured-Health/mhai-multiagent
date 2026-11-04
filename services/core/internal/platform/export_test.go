package platform

import "time"

// SetLeaderSession replaces the leader's session settings for a test and
// returns the function that puts them back. Not safe while a sampler runs.
func SetLeaderSession(stmt string) (restore func()) {
	old := leaderSession
	leaderSession = stmt
	return func() { leaderSession = old }
}

// ResetGuardrailsCache empties the guardrail statistics' cache, so a test
// sees what it seeded.
func ResetGuardrailsCache() { guardrailsCache.reset() }

// SetGuardrailsClock sets the guardrail route's clock for a test and returns
// the function that puts it back.
func SetGuardrailsClock(now func() time.Time) (restore func()) {
	old := guardClock
	guardClock = now
	return func() { guardClock = old }
}
