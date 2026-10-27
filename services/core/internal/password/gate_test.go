package password

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Past the gate's size, a caller waits up to Wait and then gets ErrBusy;
// a released slot lets the next one in.
func TestGateLimitsConcurrency(t *testing.T) {
	g := NewGate(2, 30*time.Millisecond)
	r1, err1 := g.Acquire(context.Background())
	r2, err2 := g.Acquire(context.Background())
	if err1 != nil || err2 != nil {
		t.Fatalf("first two: %v %v", err1, err2)
	}
	start := time.Now()
	if _, err := g.Acquire(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("third: want ErrBusy, got %v", err)
	}
	if time.Since(start) < 25*time.Millisecond {
		t.Fatal("third did not wait before giving up")
	}
	r1()
	r3, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	r2()
	r3()
	r3() // a second release is a no-op, never a second slot
	var wg sync.WaitGroup
	held := make(chan func(), 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, err := g.Acquire(context.Background()); err == nil {
				held <- r
			}
		}()
	}
	wg.Wait()
	close(held)
	n := 0
	for r := range held {
		n++
		r()
	}
	if n != 2 {
		t.Fatalf("double release opened extra slots: %d holders", n)
	}
}

// A waiting caller gives up when its request is cancelled.
func TestGateHonoursContext(t *testing.T) {
	g := NewGate(1, time.Minute)
	r, _ := g.Acquire(context.Background())
	defer r()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := g.Acquire(ctx); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
}

// The process-wide gate is sized from the CPUs.
func TestDefaultGateSize(t *testing.T) {
	if DefaultGate.Size() < 1 || DefaultGate.Wait <= 0 {
		t.Fatalf("default gate %d %v", DefaultGate.Size(), DefaultGate.Wait)
	}
}
