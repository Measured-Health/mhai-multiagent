package deliver

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/notifypb"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/channel"
)

var t0 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

// fakeMsg records how a message ended.
type fakeMsg struct {
	subject string
	data    []byte
	ended   string
	delay   time.Duration
}

func (m *fakeMsg) Subject() string { return m.subject }
func (m *fakeMsg) Data() []byte    { return m.data }
func (m *fakeMsg) Ack() error      { m.ended = "ack"; return nil }
func (m *fakeMsg) Term() error     { m.ended = "term"; return nil }
func (m *fakeMsg) NakWithDelay(d time.Duration) error {
	m.ended, m.delay = "nak", d
	return nil
}

func due(h, id uuid.UUID) *fakeMsg {
	b, _ := proto.Marshal(&notifypb.Due{MessageId: id.String()})
	return &fakeMsg{subject: "notify.v1." + h.String() + ".due", data: b}
}

// fakeStore answers one claim and records finishes.
type fakeStore struct {
	mu         sync.Mutex
	claim      Claim
	claimErr   error
	finishes   []Finish
	finishErr  error
	reserveOK  bool
	retry      time.Time
	reserveErr error
	mandatory  []bool
}

func (f *fakeStore) Claim(context.Context, uuid.UUID, uuid.UUID, time.Duration) (Claim, error) {
	return f.claim, f.claimErr
}

func (f *fakeStore) Finish(_ context.Context, _, _ uuid.UUID, _ string, _ uuid.UUID, fin Finish) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishes = append(f.finishes, fin)
	return f.finishErr
}

func (f *fakeStore) Reserve(_ context.Context, _, _ uuid.UUID, mandatory bool, _ time.Time, _ Limits) (bool, time.Time, error) {
	f.mandatory = append(f.mandatory, mandatory)
	return f.reserveOK, f.retry, f.reserveErr
}

func (f *fakeStore) Retention(context.Context) (int, error) { return 3, nil }

func (f *fakeStore) last() Finish {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.finishes) == 0 {
		return Finish{}
	}
	return f.finishes[len(f.finishes)-1]
}

// fakeChannel answers each send with res and err.
type fakeChannel struct {
	res   channel.Result
	err   error
	sends int
}

func (c *fakeChannel) Name() string { return "log" }
func (c *fakeChannel) Send(context.Context, channel.Message) (channel.Result, error) {
	c.sends++
	return c.res, c.err
}

type fakeDead struct{ n int }

func (d *fakeDead) Dead(context.Context, uuid.UUID, uuid.UUID, string, int) error { d.n++; return nil }

type rig struct {
	s    *Sender
	st   *fakeStore
	ch   *fakeChannel
	dead *fakeDead
	logs *bytes.Buffer
	h    uuid.UUID
	id   uuid.UUID
}

func newRig(outcome string) *rig {
	r := &rig{st: &fakeStore{reserveOK: true, claim: Claim{Outcome: outcome, Lease: uuid.New(), Kind: "mfa_reminder",
		Recipient: uuid.New(), Channel: "log", Address: "synthetic-addr@example.test"}},
		ch:   &fakeChannel{res: channel.Result{Outcome: channel.Accepted, ProviderID: "synthetic-provider-id"}},
		dead: &fakeDead{}, logs: &bytes.Buffer{}, h: uuid.New(), id: uuid.New()}
	r.s = &Sender{Store: r.st, Channels: map[string]channel.Channel{"log": r.ch}, Breaker: NewBreaker(), Dead: r.dead, Metrics: &Metrics{},
		Log: slog.New(slog.NewJSONHandler(r.logs, nil)), Now: func() time.Time { return t0 }}
	return r
}

func (r *rig) run() *fakeMsg {
	m := due(r.h, r.id)
	r.s.HandleMsg(context.Background(), m)
	return m
}

func TestParseDue(t *testing.T) {
	h, id := uuid.New(), uuid.New()
	m := due(h, id)
	if gh, gid, ok := ParseDue(m.subject, m.data); !ok || gh != h || gid != id {
		t.Fatal("a good due message refused")
	}
	unknown := protowire.AppendString(protowire.AppendTag(append([]byte(nil), m.data...), 9, protowire.BytesType), "smuggled")
	upper, _ := proto.Marshal(&notifypb.Due{MessageId: strings.ToUpper(id.String())})
	for _, tc := range []struct {
		subject string
		data    []byte
	}{
		{"notify.v1.platform.due", m.data}, {"notify.v1." + strings.ToUpper(h.String()) + ".due", m.data},
		{"notify.v1.00000000-0000-0000-0000-000000000000.due", m.data}, {"notify.v2." + h.String() + ".due", m.data},
		{"notify.v1." + h.String() + ".sent", m.data}, {"notify.v1." + h.String() + ".due.x", m.data},
		{m.subject, unknown}, {m.subject, upper}, {m.subject, []byte{0xff}}, {m.subject, make([]byte, 300)},
		{m.subject, nil},
	} {
		if _, _, ok := ParseDue(tc.subject, tc.data); ok {
			t.Errorf("accepted %s %x", tc.subject, tc.data)
		}
	}
}

// A malformed message ends with nothing claimed or sent.
func TestMalformedEnds(t *testing.T) {
	r := newRig(Claimed)
	m := &fakeMsg{subject: "notify.v1.platform.due"}
	r.s.HandleMsg(context.Background(), m)
	if m.ended != "term" || r.ch.sends != 0 || r.s.metrics().Refused.Load() != 1 {
		t.Fatalf("%s %d", m.ended, r.ch.sends)
	}
}

// The happy path: sent once, recorded with its provider id, acked; the
// log carries ids and the outcome, never the address (NT19).
func TestSentOnce(t *testing.T) {
	r := newRig(Claimed)
	m := r.run()
	f := r.st.last()
	if m.ended != "ack" || r.ch.sends != 1 || f.Status != "sent" || f.Outcome != "accepted" || f.Attempts != 1 ||
		f.ProviderID != "synthetic-provider-id" || !f.Tried.Equal(t0) {
		t.Fatalf("%s %d %+v", m.ended, r.ch.sends, f)
	}
	out := r.logs.String()
	if strings.Contains(out, "synthetic-addr") || strings.Contains(out, "example.test") || strings.Contains(out, "synthetic-provider-id") {
		t.Fatalf("the log carries the address or the provider id: %s", out)
	}
	if !strings.Contains(out, r.id.String()) || !strings.Contains(out, `"outcome":"accepted"`) {
		t.Fatalf("log: %s", out)
	}
	if r.s.metrics().Sent.Load() != 1 || r.st.mandatory[0] {
		t.Fatal("metrics or mandatory")
	}
}

// NT9, NT17, NT18: another hospital's id sends nothing; a deactivated
// recipient is suppressed; a delivery someone else holds, one waiting, or
// one finished is left alone.
func TestClaimOutcomes(t *testing.T) {
	for _, tc := range []struct {
		outcome, ended, status string
	}{
		{NotFound, "term", ""}, {Done, "ack", ""}, {Busy, "nak", ""}, {Held, "nak", ""}, {Inactive, "ack", "suppressed"},
		{"surprise", "nak", ""},
	} {
		r := newRig(tc.outcome)
		r.st.claim.NextAttempt = t0.Add(time.Hour)
		m := r.run()
		if m.ended != tc.ended || r.ch.sends != 0 || r.st.last().Status != tc.status {
			t.Errorf("%s: %s, %d sends, %+v", tc.outcome, m.ended, r.ch.sends, r.st.last())
		}
		if tc.outcome == Held && m.delay != time.Hour {
			t.Errorf("held for %v", m.delay)
		}
		if tc.outcome == Inactive && r.st.last().Outcome != "inactive_user" {
			t.Errorf("suppressed as %s", r.st.last().Outcome)
		}
	}
	r := newRig(Claimed)
	r.st.claimErr = errors.New("synthetic: database down")
	if m := r.run(); m.ended != "nak" || r.ch.sends != 0 {
		t.Fatal("a claim error sent")
	}
	r = newRig(Inactive)
	r.st.finishErr = errors.New("synthetic: database down")
	if m := r.run(); m.ended != "nak" {
		t.Fatal("an unrecorded suppression acked")
	}
}

// NT13: past a cap the message waits for the next hour and nothing is
// sent; a counter that can't be written holds it too (fail closed);
// mandatory kinds are reserved as mandatory.
func TestRateLimitsHold(t *testing.T) {
	r := newRig(Claimed)
	r.st.reserveOK, r.st.retry = false, t0.Add(time.Hour)
	m := r.run()
	if m.ended != "nak" || m.delay != time.Hour || r.ch.sends != 0 || r.st.last().Outcome != "rate_limited" ||
		r.st.last().Status != "claimed" || r.st.last().Attempts != 0 {
		t.Fatalf("%s %v %+v", m.ended, m.delay, r.st.last())
	}
	r = newRig(Claimed)
	r.st.reserveErr = errors.New("synthetic: counter down")
	if m := r.run(); m.ended != "nak" || r.ch.sends != 0 {
		t.Fatal("sent without a counter")
	}
	r = newRig(Claimed)
	r.st.claim.Kind = "mfa_reset"
	r.run()
	if !r.st.mandatory[0] {
		t.Fatal("mfa_reset not mandatory")
	}
	for k, want := range map[string]bool{"invite": true, "password_reset": true, "mfa_reset": true, "mfa_reminder": false,
		"review_reminder": false, "review_assigned": false} {
		if Mandatory(k) != want {
			t.Errorf("%s mandatory %v", k, !want)
		}
	}
}

// NT14: a failing channel retries on the schedule without the address in
// the log, and after 24 hours of trying the delivery is failed and
// dead-lettered; a permanent refusal fails at once.
func TestRetriesThenDeadLetter(t *testing.T) {
	r := newRig(Claimed)
	r.ch.err = errors.New("synthetic: provider down for synthetic-addr@example.test")
	m := r.run()
	f := r.st.last()
	if m.ended != "nak" || m.delay != time.Minute || f.Status != "claimed" || f.Outcome != "retrying" || f.Attempts != 1 ||
		!f.Next.Equal(t0.Add(time.Minute)) {
		t.Fatalf("%s %v %+v", m.ended, m.delay, f)
	}
	if strings.Contains(r.logs.String(), "synthetic-addr") {
		t.Fatal("the provider's error text reached the log")
	}
	// The fifth attempt waits 6 hours.
	r.st.claim.Attempts, r.st.claim.FirstAttempt = 4, t0.Add(-10*time.Hour)
	if m := r.run(); m.delay != 6*time.Hour {
		t.Fatalf("fifth retry after %v", m.delay)
	}
	// 24 hours after the first attempt: failed, dead-lettered, acked.
	r.st.claim.Attempts, r.st.claim.FirstAttempt = 8, t0.Add(-24*time.Hour)
	m = r.run()
	if f := r.st.last(); m.ended != "ack" || f.Status != "failed" || f.Outcome != "provider_down" || r.dead.n != 1 ||
		r.s.metrics().DeadLettered.Load() != 1 {
		t.Fatalf("give up: %s %+v %d", m.ended, f, r.dead.n)
	}
	r = newRig(Claimed)
	r.ch.res = channel.Result{Outcome: channel.Permanent}
	if m := r.run(); m.ended != "ack" || r.st.last().Status != "failed" || r.st.last().Outcome != "rejected_address" {
		t.Fatalf("permanent: %s %+v", m.ended, r.st.last())
	}
	r = newRig(Claimed)
	r.ch.res = channel.Result{Outcome: channel.Retryable}
	if m := r.run(); m.ended != "nak" || r.st.last().Outcome != "retrying" {
		t.Fatalf("retryable: %s %+v", m.ended, r.st.last())
	}
}

// A channel this service doesn't send on is retried and finally failed
// as channel_off, never dropped silently.
func TestChannelOff(t *testing.T) {
	r := newRig(Claimed)
	r.st.claim.Channel = "email"
	m := r.run()
	if m.ended != "nak" || r.ch.sends != 0 || r.st.last().Outcome != "retrying" {
		t.Fatalf("%s %+v", m.ended, r.st.last())
	}
	r.st.claim.FirstAttempt, r.st.claim.Attempts = t0.Add(-25*time.Hour), 9
	if m := r.run(); m.ended != "ack" || r.st.last().Outcome != "channel_off" || r.dead.n != 1 {
		t.Fatalf("%s %+v", m.ended, r.st.last())
	}
}

// Ten failures in a row open the breaker: the next message waits five
// minutes without a send or an attempt; a success closes the count.
func TestBreaker(t *testing.T) {
	r := newRig(Claimed)
	r.ch.err = errors.New("synthetic: down")
	for i := 0; i < 10; i++ {
		r.run()
	}
	sends := r.ch.sends
	m := r.run()
	if m.ended != "nak" || m.delay != 5*time.Minute || r.ch.sends != sends || r.st.last().Outcome != "breaker_open" {
		t.Fatalf("%s %v %+v", m.ended, m.delay, r.st.last())
	}
	b := NewBreaker()
	for i := 0; i < 9; i++ {
		b.Failure(t0)
	}
	b.Success()
	b.Failure(t0)
	if open, _ := b.Open(t0); open {
		t.Fatal("a success didn't reset the count")
	}
}

func TestBackoffAndLimits(t *testing.T) {
	for n, want := range map[int]time.Duration{0: time.Minute, 1: time.Minute, 2: 5 * time.Minute, 3: 30 * time.Minute,
		4: 2 * time.Hour, 5: 6 * time.Hour, 9: 6 * time.Hour} {
		if Backoff(n) != want {
			t.Errorf("Backoff(%d) = %v", n, Backoff(n))
		}
	}
	if (Limits{}).recipient() != 10 || (Limits{}).hospital() != 200 || (Limits{Recipient: 3, Hospital: 7}).hospital() != 7 {
		t.Fatal("limits")
	}
}

// The pacer spaces sends and gives way to a cancelled context.
func TestPacer(t *testing.T) {
	p := NewPacer(20)
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := p.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if el := time.Since(start); el < 90*time.Millisecond {
		t.Fatalf("3 sends at 20/s took %v", el)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	slow := NewPacer(1)
	_ = slow.Wait(context.Background())
	if err := slow.Wait(ctx); err == nil {
		t.Fatal("waited past a cancelled context")
	}
	r := newRig(Claimed)
	r.s.Pacer = slow
	m := due(r.h, r.id)
	r.s.HandleMsg(ctx, m)
	if m.ended != "nak" || r.ch.sends != 0 {
		t.Fatalf("sent while stopping: %s", m.ended)
	}
}

// A sent message whose record fails is still acked (it went), and the
// error is counted; finish retries a failing write.
func TestSentButNotRecorded(t *testing.T) {
	r := newRig(Claimed)
	r.st.finishErr = errors.New("synthetic: database down")
	m := r.run()
	if m.ended != "ack" || r.s.metrics().Errors.Load() != 1 || len(r.st.finishes) != 3 {
		t.Fatalf("%s errors %d finishes %d", m.ended, r.s.metrics().Errors.Load(), len(r.st.finishes))
	}
	r = newRig(Claimed)
	r.st.finishErr = ErrLeaseLost
	r.run()
	if len(r.st.finishes) != 1 {
		t.Fatal("a lost lease was retried")
	}
}

func TestRetentionLoopStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() { RunRetention(ctx, &fakeStore{}, slog.New(slog.NewJSONHandler(&buf, nil))); close(done) }()
	cancel()
	<-done
	s := &Sender{}
	if s.log() == nil || s.metrics() == nil || s.now().IsZero() {
		t.Fatal("defaults")
	}
}
