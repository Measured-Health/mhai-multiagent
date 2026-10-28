package password

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"
)

// ErrBusy is returned by Gate.Acquire when no slot frees up in time. The
// caller answers 503 not_ready.
var ErrBusy = errors.New("password: too many concurrent hash checks")

// Gate bounds how many argon2 computations run at once in the process.
// Each takes 64 MiB and a CPU for tens of milliseconds, so an unbounded
// burst of sign-ins could exhaust memory. Past the limit, a caller waits up
// to Wait for a slot and is then refused.
type Gate struct {
	slots chan struct{}
	// Wait is how long Acquire waits for a slot.
	Wait time.Duration
}

// DefaultWait is how long a caller of DefaultGate waits for a slot.
const DefaultWait = 2 * time.Second

// DefaultGate is the process-wide gate: one slot per CPU.
var DefaultGate = NewGate(runtime.NumCPU(), DefaultWait)

// NewGate returns a gate with n slots (at least 1).
func NewGate(n int, wait time.Duration) *Gate {
	if n < 1 {
		n = 1
	}
	return &Gate{slots: make(chan struct{}, n), Wait: wait}
}

// Size is the number of slots.
func (g *Gate) Size() int { return cap(g.slots) }

// Acquire takes a slot, waiting up to Wait (or until ctx ends). It returns
// a release function, safe to call more than once, or ErrBusy.
func (g *Gate) Acquire(ctx context.Context) (release func(), err error) {
	t := time.NewTimer(g.Wait)
	defer t.Stop()
	select {
	case g.slots <- struct{}{}:
	case <-t.C:
		return nil, ErrBusy
	case <-ctx.Done():
		return nil, ErrBusy
	}
	var once sync.Once
	return func() { once.Do(func() { <-g.slots }) }, nil
}
