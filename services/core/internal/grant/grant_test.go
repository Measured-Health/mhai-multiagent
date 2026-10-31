package grant

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// TestCodesAreCoreCodes: every grant refusal code is one of core's own
// problem codes (the worker passes it to the BFF as grant_refused:<code>,
// and the BFF shows that code and its status), and the list covers every
// code the principal check can return.
func TestCodesAreCoreCodes(t *testing.T) {
	codes := Codes()
	if len(codes) == 0 {
		t.Fatal("no codes")
	}
	for _, c := range codes {
		if _, ok := httpx.Codes[c]; !ok {
			t.Errorf("grant code %q is not in httpx.Codes", c)
		}
	}
	for _, c := range []string{"unauthenticated", "token_expired", "mfa_required", "forbidden", "not_found",
		"rate_limited", "not_ready", CodeOverloaded} {
		if !slices.Contains(codes, c) {
			t.Errorf("Codes() is missing %q", c)
		}
	}
	codes[0] = "changed"
	if Codes()[0] == "changed" {
		t.Error("Codes() returned the table itself")
	}
}

// TestRefusalCodeCoversEveryCode (error-code ruling): the mapping is the
// identity on every grant code, and anything else (not_platform, a typo,
// "") fails closed to forbidden.
func TestRefusalCodeCoversEveryCode(t *testing.T) {
	for _, c := range Codes() {
		if got := refusalCode(c); got != c {
			t.Errorf("refusalCode(%q) = %q", c, got)
		}
	}
	for _, c := range []string{"", "not_platform", "internal_error", "ok", "FORBIDDEN"} {
		if got := refusalCode(c); got != "forbidden" {
			t.Errorf("refusalCode(%q) = %q, want forbidden", c, got)
		}
	}
}

func TestServiceOf(t *testing.T) {
	for subject, want := range map[string]string{
		"authz.v1.grant.echo":     "echo",
		"authz.v1.grant.pipeline": "pipeline",
		"authz.v1.grant.":         "",
		"authz.v1.grant.echo.x":   "",
		"authz.v1.grant.Echo":     "",
		"authz.v1.grant.*":        "",
		"authz.v1.grantecho":      "",
		"api.v1.clinical":         "",
	} {
		got, ok := serviceOf(subject)
		if ok != (want != "") || got != want {
			t.Errorf("serviceOf(%q) = %q, %v", subject, got, ok)
		}
	}
}

// TestReplyOK: only the asking service's own inbox, with literal tokens.
func TestReplyOK(t *testing.T) {
	good := []string{"_INBOX_worker.echo.abc", "_INBOX_worker.echo.abc.1", "_INBOX_worker.echo.Zx9-_"}
	bad := []string{
		"", "_INBOX_worker.echo", "_INBOX_worker.echo.", "_INBOX_worker.echo..x", "_INBOX_worker.echo.*",
		"_INBOX_worker.echo.>", "_INBOX_worker.echo.a*", "_INBOX_worker.echo.a b", "_INBOX_worker.echo.a\tb",
		"_INBOX_worker.echox.y", "_INBOX_worker.pipeline.x", "_INBOX_gateway.x", "_INBOX_bff.clinical.x",
		"jobs.v1.x.run_query", "$KV.denylist.x", "$JS.API.STREAM.DELETE.JOBS",
		"_INBOX_worker.echo." + strings.Repeat("a", MaxReply),
	}
	for _, r := range good {
		if !replyOK("echo", r) {
			t.Errorf("refused %q", r)
		}
	}
	for _, r := range bad {
		if replyOK("echo", r) {
			t.Errorf("accepted %q", r)
		}
	}
}

func TestValidRequestID(t *testing.T) {
	for id, want := range map[string]bool{
		"abcdefgh": true, "A1_b2-C3d4": true, strings.Repeat("a", 64): true,
		"abcdefg": false, strings.Repeat("a", 65): false, "abcdefg.h": false, "abcdefg h": false,
		"abcdefg*": false, "abcdéfgh": false, "": false,
	} {
		if validRequestID(id) != want {
			t.Errorf("validRequestID(%q) != %v", id, want)
		}
	}
}

func TestGrantTTL(t *testing.T) {
	cases := []struct {
		timeout time.Duration
		ms      uint32
		want    time.Duration
		ok      bool
	}{
		{5 * time.Second, 3000, 3 * time.Second, true},
		{5 * time.Second, 50_000, 5 * time.Second, true},
		{5 * time.Second, 3500, 3500 * time.Millisecond, true},
		{5 * time.Second, 400, time.Second, true}, // Mint's whole-second floor
		{5 * time.Second, 0, 0, false},
		{0, 3000, 0, false},
	}
	for _, c := range cases {
		got, ok := grantTTL(c.timeout, c.ms)
		if got != c.want || ok != c.ok {
			t.Errorf("grantTTL(%v, %d) = %v, %v; want %v, %v", c.timeout, c.ms, got, ok, c.want, c.ok)
		}
	}
}

// TestRateLimiter: a burst of exactly the per-second rate on a still
// clock, a refill with time, separate keys, and a full table fails closed.
func TestRateLimiter(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newRateLimiter(20, 3)
	n := 0
	for i := 0; i < 200; i++ {
		if l.allow("s1", now) {
			n++
		}
	}
	if n != 20 {
		t.Fatalf("burst allowed %d, want 20", n)
	}
	if !l.allow("s2", now) {
		t.Fatal("another key limited")
	}
	if l.allow("s1", now.Add(10*time.Millisecond)) {
		t.Fatal("refilled too fast")
	}
	if !l.allow("s1", now.Add(100*time.Millisecond)) {
		t.Fatal("no refill after 100 ms (2 tokens)")
	}
	if !l.allow("s3", now) {
		t.Fatal("third key")
	}
	if l.allow("s4", now) {
		t.Fatal("a full table must fail closed")
	}
	// Idle buckets (full again) are swept to make room.
	if !l.allow("s4", now.Add(2*time.Second)) {
		t.Fatal("idle buckets not swept")
	}
	// A clock going backwards refills nothing.
	l2 := newRateLimiter(1, 10)
	if !l2.allow("k", now) || l2.allow("k", now.Add(-time.Hour)) {
		t.Fatal("backwards clock")
	}
}

// TestReplaySet: a request id is new once per window; the set is bounded
// and fails closed when full.
func TestReplaySet(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := newReplaySet(2*time.Minute, 2)
	if r := s.add("a", now); r != replayNew {
		t.Fatal("first a")
	}
	if r := s.add("a", now.Add(119*time.Second)); r != replaySeen {
		t.Fatal("a within 2 minutes")
	}
	if r := s.add("b", now); r != replayNew {
		t.Fatal("b")
	}
	if r := s.add("c", now); r != replayFull {
		t.Fatal("full set must fail closed")
	}
	if r := s.add("c", now.Add(121*time.Second)); r != replayNew {
		t.Fatal("expired entries not swept")
	}
	if r := s.add("a", now.Add(121*time.Second)); r != replayNew {
		t.Fatal("a after the window")
	}
}

func TestReplaySetRemove(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := newReplaySet(2*time.Minute, 10)
	s.add("a", now)
	s.remove("a")
	if s.add("a", now) != replayNew {
		t.Fatal("removed id still seen")
	}
}

// TestSampler: the first event logs, the rest of the interval are counted
// into the next line.
func TestSampler(t *testing.T) {
	l := newSampler(time.Hour)
	if n, ok := l.take(); !ok || n != 1 {
		t.Fatalf("first: %d %v", n, ok)
	}
	for i := 0; i < 5; i++ {
		if _, ok := l.take(); ok {
			t.Fatal("logged inside the interval")
		}
	}
	l.last = l.last.Add(-2 * time.Hour)
	if n, ok := l.take(); !ok || n != 6 {
		t.Fatalf("next interval: %d %v", n, ok)
	}
}

// A flood of refusals of verified tokens is counted, and one session's
// refusals raise a sampled signal at each multiple of FloodEvery a minute:
// a counter and one log line, never a label.
func TestSessionRefusalFloodIsSampled(t *testing.T) {
	var logs strings.Builder
	now := time.Unix(1_000_000, 0)
	s := &Server{Metrics: &Metrics{}, Log: slog.New(slog.NewTextHandler(&logs, nil)),
		flood: newFloodCounter(time.Minute, 10), lim: Limits{Now: func() time.Time { return now }}.withDefaults()}
	c := call{service: "echo"}
	for i := 0; i < 2*FloodEvery+5; i++ {
		s.noteVerifiedRefusal(c)
	}
	if got := s.Metrics.VerifiedRefused.Load(); got != 2*FloodEvery+5 {
		t.Fatalf("verified refusals %d", got)
	}
	if got := s.Metrics.RefusalFlood.Load(); got != 2 {
		t.Fatalf("flood signals %d, want 2", got)
	}
	if n := strings.Count(logs.String(), "session refusal flood"); n != 2 {
		t.Fatalf("flood log lines %d: %s", n, logs.String())
	}
	// A new minute starts a new window.
	now = now.Add(time.Minute)
	for i := 0; i < FloodEvery-1; i++ {
		s.noteVerifiedRefusal(c)
	}
	if got := s.Metrics.RefusalFlood.Load(); got != 2 {
		t.Fatalf("flood signals %d after a new window", got)
	}
}

// The flood table is bounded: a full table counts no new key, and expired
// windows make room.
func TestFloodCounterIsBounded(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	f := newFloodCounter(time.Minute, 2)
	if f.add("a", now) != 1 || f.add("b", now) != 1 || f.add("a", now) != 2 {
		t.Fatal("counts")
	}
	if f.add("c", now) != 0 {
		t.Fatal("a third key must not be tracked while the table is full")
	}
	if f.add("c", now.Add(2*time.Minute)) != 1 {
		t.Fatal("expired windows should make room")
	}
}
