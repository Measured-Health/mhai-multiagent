package grant

import (
	"sync"
	"time"
)

// Default limits (spec §17.3). They are per copy of core, in memory.
const (
	DefaultPerSession  = 20  // grants per second per session
	DefaultPerHospital = 200 // grants per second per hospital
	DefaultInFlight    = 64  // grant requests handled at once

	// refusalAuditRate bounds the refusals audited per second per service
	// before a principal is known (a junk token is free to send): past it,
	// a refusal is counted, not written, so a flood can't serialise the
	// audit chain (spec §10.2, decision 4; F7).
	refusalAuditRate = 20

	// Table bounds. A full table fails closed: the request is refused
	// rate_limited.
	maxLimiterKeys = 100_000
	maxReplayIDs   = 200_000
)

// Limits bounds the grant endpoint. Zero fields take the defaults.
type Limits struct {
	PerSession  int // per second
	PerHospital int // per second
	InFlight    int
	// Now is the limits' clock (rates and the replay window); defaults to
	// time.Now. Tests freeze it.
	Now func() time.Time
}

func (l Limits) withDefaults() Limits {
	if l.PerSession <= 0 {
		l.PerSession = DefaultPerSession
	}
	if l.PerHospital <= 0 {
		l.PerHospital = DefaultPerHospital
	}
	if l.InFlight <= 0 {
		l.InFlight = DefaultInFlight
	}
	if l.Now == nil {
		l.Now = time.Now
	}
	return l
}

// rateLimiter is a token bucket per key: rate tokens a second, holding at
// most rate (one second's burst). The table holds at most max keys; idle
// (full) buckets are swept to make room, and a table still full refuses.
type rateLimiter struct {
	mu   sync.Mutex
	rate float64
	max  int
	m    map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newRateLimiter(perSecond, max int) *rateLimiter {
	return &rateLimiter{rate: float64(perSecond), max: max, m: map[string]*bucket{}}
}

// refill brings b up to now. A clock that went backwards adds nothing.
func (l *rateLimiter) refill(b *bucket, now time.Time) {
	if now.After(b.at) {
		b.tokens = min(l.rate, b.tokens+now.Sub(b.at).Seconds()*l.rate)
		b.at = now
	}
}

// allow takes one token from key's bucket, reporting whether there was one.
func (l *rateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.m[key]
	if !ok {
		if len(l.m) >= l.max {
			for k, old := range l.m {
				l.refill(old, now)
				if old.tokens >= l.rate {
					delete(l.m, k)
				}
			}
			if len(l.m) >= l.max {
				return false
			}
		}
		b = &bucket{tokens: l.rate, at: now}
		l.m[key] = b
	}
	l.refill(b, now)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// replaySet remembers request ids for a window (departure 15: per copy of
// core). It holds at most max ids; expired ids are swept to make room, and
// a set still full refuses.
type replaySet struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	m      map[string]time.Time // id → when it expires
}

type replayResult int

const (
	replayNew replayResult = iota
	replaySeen
	replayFull
)

func newReplaySet(window time.Duration, max int) *replaySet {
	return &replaySet{window: window, max: max, m: map[string]time.Time{}}
}

// add records id at now. A repeat inside the window doesn't extend it.
func (s *replaySet) add(id string, now time.Time) replayResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if exp, ok := s.m[id]; ok && now.Before(exp) {
		return replaySeen
	}
	if len(s.m) >= s.max {
		for k, exp := range s.m {
			if !now.Before(exp) {
				delete(s.m, k)
			}
		}
		if len(s.m) >= s.max {
			return replayFull
		}
	}
	s.m[id] = now.Add(s.window)
	return replayNew
}

// remove forgets id, for a request that was refused after its id was
// recorded and was granted nothing.
func (s *replaySet) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

// floodCounter counts events per key in a fixed window that starts at the
// key's first event. It holds at most max keys: expired windows are swept
// to make room, and a table still full counts nothing (add returns 0), so
// it can't grow with attacker-chosen keys.
type floodCounter struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	m      map[string]*flood
}

type flood struct {
	n     int
	until time.Time
}

func newFloodCounter(window time.Duration, max int) *floodCounter {
	return &floodCounter{window: window, max: max, m: map[string]*flood{}}
}

// add counts one event for key at now and returns the count in its window
// (0 when the table is full and key is new).
func (f *floodCounter) add(key string, now time.Time) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[key]
	if ok && !now.Before(e.until) {
		delete(f.m, key)
		ok = false
	}
	if !ok {
		if len(f.m) >= f.max {
			for k, old := range f.m {
				if !now.Before(old.until) {
					delete(f.m, k)
				}
			}
			if len(f.m) >= f.max {
				return 0
			}
		}
		e = &flood{until: now.Add(f.window)}
		f.m[key] = e
	}
	e.n++
	return e.n
}
