package routes

import (
	"sync"
	"time"
)

// The rates of design §5.6 and D7 (the other constants live in
// disclose/constants.go).
const (
	// AggregateReads is the most aggregate reads (views and filters) one
	// session may make in AggregateWindow; past it, 429 rate_limited.
	AggregateReads  = 30
	AggregateWindow = time.Minute
	// ListsPerMinute and ListsPerDay are the most de-identified lists one
	// person may open in a minute and in a hospital day (design §5.6);
	// past either, 429 rate_limited and one flagged entry a day. Counted
	// in the database, across core copies (mhai_analysis_list_admit).
	ListsPerMinute = 10
	ListsPerDay    = 50
	// NamedPerDay is the most named lists, replays included, one person
	// may open in a hospital day (design §5.6, M7); past it, 429
	// rate_limited and one flagged entry a day. Reserved in the database
	// (mhai_analysis_named_admit, which refuses a limit above 5).
	NamedPerDay = 5
	// ClearanceMePerMinute is GET /analysis/clearance/me's limit per
	// session (design §8.4: the app polls every 30 seconds).
	ClearanceMePerMinute = 6
)

// limiter is a sliding window per session, in this core copy (a shared
// limiter across copies is on the "Before scale" list). It forgets a
// session once its window is empty.
type limiter struct {
	mu   sync.Mutex
	max  int
	win  time.Duration
	now  func() time.Time
	hits map[string][]time.Time
}

func newLimiter(max int, win time.Duration, now func() time.Time) *limiter {
	if now == nil {
		now = time.Now
	}
	return &limiter{max: max, win: win, now: now, hits: map[string][]time.Time{}}
}

// allow records a hit for key and reports whether it is within the limit.
// A refused hit isn't recorded.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	keep := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.win {
			keep = append(keep, t)
		}
	}
	if len(keep) >= l.max {
		l.hits[key] = keep
		return false
	}
	l.hits[key] = append(keep, now)
	// Drop windows that emptied, so the map doesn't grow with sessions.
	if len(l.hits) > 10000 {
		for k, ts := range l.hits {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= l.win {
				delete(l.hits, k)
			}
		}
	}
	return true
}

// NewContextsMax is the most contexts one request may make the engine
// newly compute and store (its own family: child periods and filter
// parents; then its history points'), whatever the person's cold budget
// (security review). A request whose own family is larger is 429; a
// history point past it is sent hidden whole, computing nothing.
const NewContextsMax = 32

// ColdContexts is the most distinct contexts one person may have computed
// cold in one period (security review): past it, 429 rate_limited.
const ColdContexts = 100
