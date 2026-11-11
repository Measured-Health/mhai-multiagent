// Package replay is a bounded, per-process set of request-token ids seen
// (plan departure 3). A verifier checks it after the token verifies: a jti
// seen before is a replay. An id is remembered until its token's expiry
// plus a window (for clock skew), then forgotten. At the bound the set
// refuses (ErrFull) rather than forget a live id: the caller refuses the
// message, failing closed.
//
// One set per process: a replay delivered to another copy of the service
// isn't caught there (accepted residual risk; every emr control op is
// idempotent or bound and short-lived).
package replay

import (
	"errors"
	"sync"
	"time"
)

var (
	// ErrFull: the set holds max live ids; refuse the message.
	ErrFull = errors.New("replay: set full")
	// ErrEmpty: no id to check; refuse the message.
	ErrEmpty = errors.New("replay: empty id")
)

// Set remembers ids until their expiry plus the window.
type Set struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	m      map[string]time.Time // id -> when it may be forgotten
}

// New returns a set that keeps an id for window after its token's expiry
// and holds at most max ids.
func New(window time.Duration, max int) *Set {
	return &Set{window: window, max: max, m: map[string]time.Time{}}
}

// SeenOrAdd is SeenOrAddAt at the current time.
func (s *Set) SeenOrAdd(jti string, exp time.Time) (bool, error) {
	return s.SeenOrAddAt(jti, exp, time.Now())
}

// SeenOrAddAt reports whether jti was seen and is still remembered at now;
// if not, it records jti until exp plus the window. A repeat never extends
// the first record. Check and record are one step under the lock, so of
// concurrent presentations of one id exactly one is new.
func (s *Set) SeenOrAddAt(jti string, exp, now time.Time) (bool, error) {
	if jti == "" {
		return false, ErrEmpty
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if until, ok := s.m[jti]; ok {
		if now.Before(until) {
			return true, nil
		}
		delete(s.m, jti)
	}
	if len(s.m) >= s.max {
		for k, until := range s.m {
			if !now.Before(until) {
				delete(s.m, k)
			}
		}
		if len(s.m) >= s.max {
			return false, ErrFull
		}
	}
	s.m[jti] = exp.Add(s.window)
	return false, nil
}
