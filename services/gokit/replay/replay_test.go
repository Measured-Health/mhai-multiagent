package replay

import (
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// Plan departure 3: a jti is seen until its exp plus the window, then
// forgotten; at the bound a new jti is refused (fail closed), unless an
// expired one can be swept to make room.
func TestReplaySetSeenFullAndExpiry(t *testing.T) {
	s := New(2*time.Minute, 2)
	exp := t0.Add(60 * time.Second)
	if seen, err := s.SeenOrAddAt("a", exp, t0); seen || err != nil {
		t.Fatalf("first a: %v %v", seen, err)
	}
	if seen, err := s.SeenOrAddAt("a", exp, t0.Add(time.Second)); !seen || err != nil {
		t.Fatalf("second a: %v %v", seen, err)
	}
	// Still seen just before exp + window.
	if seen, _ := s.SeenOrAddAt("a", exp, exp.Add(2*time.Minute-time.Second)); !seen {
		t.Fatal("a forgotten before exp + window")
	}
	if seen, err := s.SeenOrAddAt("b", exp, t0); seen || err != nil {
		t.Fatalf("b: %v %v", seen, err)
	}
	// Full: a third jti is refused while a and b are live.
	if _, err := s.SeenOrAddAt("c", exp, t0); !errors.Is(err, ErrFull) {
		t.Fatalf("c at the bound: %v, want ErrFull", err)
	}
	// Past exp + window, a and b are swept: c fits, and a is new again.
	later := exp.Add(2 * time.Minute)
	if seen, err := s.SeenOrAddAt("c", later.Add(time.Minute), later); seen || err != nil {
		t.Fatalf("c after expiry: %v %v", seen, err)
	}
	if seen, err := s.SeenOrAddAt("a", later.Add(time.Minute), later); seen || err != nil {
		t.Fatalf("a after expiry: %v %v", seen, err)
	}
	// A repeat never extends the first entry.
	if seen, _ := s.SeenOrAddAt("a", later.Add(time.Hour), later); !seen {
		t.Fatal("a not seen")
	}
}

func TestReplaySetRefusesEmptyJti(t *testing.T) {
	s := New(time.Minute, 10)
	if _, err := s.SeenOrAdd("", time.Now().Add(time.Minute)); !errors.Is(err, ErrEmpty) {
		t.Fatalf("empty jti: %v", err)
	}
}

// Consume once: of 50 concurrent presentations of one jti, exactly one is
// new.
func TestReplaySetConcurrentOneWins(t *testing.T) {
	s := New(time.Minute, 1000)
	var fresh atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if seen, err := s.SeenOrAdd("same", time.Now().Add(time.Minute)); err == nil && !seen {
				fresh.Add(1)
			}
		}()
	}
	wg.Wait()
	if fresh.Load() != 1 {
		t.Fatalf("%d presentations were new, want 1", fresh.Load())
	}
	for i := 0; i < 999; i++ {
		if _, err := s.SeenOrAdd(strconv.Itoa(i), time.Now().Add(time.Minute)); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	if _, err := s.SeenOrAdd("one-too-many", time.Now().Add(time.Minute)); !errors.Is(err, ErrFull) {
		t.Fatalf("over the bound: %v", err)
	}
}
