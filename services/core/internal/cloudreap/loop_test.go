package cloudreap

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestValidAge(t *testing.T) {
	for d, want := range map[time.Duration]bool{
		0: false, time.Minute: false, MinAge - time.Second: false, MinAge: true, DefaultAge: true,
		MaxAge: true, MaxAge + time.Second: false, -time.Hour: false,
	} {
		if got := ValidAge(d); got != want {
			t.Errorf("ValidAge(%v) = %v, want %v", d, got, want)
		}
	}
	if MinAge != 30*time.Minute || DefaultAge != time.Hour || MaxAge != 7*24*time.Hour {
		t.Error("the bounds moved; keep them with config's CORE_CLOUD_RESERVATION_REAP_AFTER and migration 00047")
	}
}

// A loop without a pool, or with an age under the floor, never starts: it
// refuses before any tick (fail closed at start-up, nothing reaped).
func TestRunRefusesNoPoolAndBadAge(t *testing.T) {
	ctx := context.Background()
	if err := (&Loop{}).Run(ctx); !errors.Is(err, errNoPool) {
		t.Errorf("no pool: %v", err)
	}
	if err := (&Loop{}).RunOnce(ctx); !errors.Is(err, errNoPool) {
		t.Errorf("RunOnce, no pool: %v", err)
	}
	pool := &pgxpool.Pool{} // never used: the age is refused first
	for _, age := range []time.Duration{time.Minute, MinAge - time.Nanosecond, MaxAge + time.Hour, -time.Hour} {
		l := &Loop{Pool: pool, Age: age}
		if err := l.Run(ctx); !errors.Is(err, errAge) {
			t.Errorf("age %v: Run %v", age, err)
		}
		if err := l.RunOnce(ctx); !errors.Is(err, errAge) {
			t.Errorf("age %v: RunOnce %v", age, err)
		}
	}
}

func TestDefaults(t *testing.T) {
	l := &Loop{}
	if l.age() != DefaultAge {
		t.Errorf("age %v", l.age())
	}
	if got := l.interval(); !got.Valid || got.Microseconds != DefaultAge.Microseconds() {
		t.Errorf("interval %+v", got)
	}
	if l.metrics() == nil || l.Metrics == nil {
		t.Error("no metrics")
	}
	l.log().Info("discarded")
}

// Another copy holds the lock: Tick does nothing; a lock error is returned.
func TestTickFollowsTheLock(t *testing.T) {
	ctx := context.Background()
	l := &Loop{Pool: &pgxpool.Pool{}, Lock: func(context.Context) (func(), bool, error) { return nil, false, nil }}
	if ran, err := l.Tick(ctx); ran || err != nil {
		t.Errorf("not the leader: ran %v err %v", ran, err)
	}
	boom := errors.New("lock failed")
	l.Lock = func(context.Context) (func(), bool, error) { return nil, false, boom }
	if ran, err := l.Tick(ctx); ran || !errors.Is(err, boom) {
		t.Errorf("lock error: ran %v err %v", ran, err)
	}
}

// Run counts and logs a failed tick, without the error's text, and stops
// when ctx ends.
func TestRunCountsAFailedTickAndStops(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	m := &Metrics{}
	calls := 0
	l := &Loop{Pool: &pgxpool.Pool{}, Log: slog.New(slog.NewTextHandler(&buf, nil)), Metrics: m, Every: time.Millisecond,
		Lock: func(context.Context) (func(), bool, error) {
			calls++
			if calls == 2 {
				cancel()
			}
			return nil, false, &pgconn.PgError{Code: "57P01", Message: "synthetic secret text"}
		}}
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
	if m.Failures.Load() < 1 {
		t.Error("the failed tick was not counted")
	}
	out := buf.String()
	if !strings.Contains(out, "sqlstate=57P01") || strings.Contains(out, "synthetic secret text") {
		t.Errorf("log: %s", out)
	}
}

func TestFailLogsHospitalAndCode(t *testing.T) {
	var buf bytes.Buffer
	l := &Loop{Log: slog.New(slog.NewTextHandler(&buf, nil))}
	h := uuid.New()
	l.fail("reap", h, errors.New("synthetic text that must not be logged"))
	if l.Metrics.Failures.Load() != 1 {
		t.Error("not counted")
	}
	out := buf.String()
	if !strings.Contains(out, h.String()) || !strings.Contains(out, "step=reap") || strings.Contains(out, "synthetic text") ||
		strings.Contains(out, "sqlstate") {
		t.Errorf("log: %s", out)
	}
}
