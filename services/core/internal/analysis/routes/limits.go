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
