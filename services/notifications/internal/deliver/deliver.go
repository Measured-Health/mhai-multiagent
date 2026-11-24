// Package deliver is the notifications service's sender (notifications
// design §9; §15 PR 3): the notify_send durable on NOTIFY. Each message is
// notify.v1.<hospital>.due carrying one outbox id and nothing else. For
// each:
//
//  1. the subject and body are checked (a uuid hospital, one canonical id,
//     no unknown field); anything else ends with nothing done;
//  2. notify_claim, under the subject's hospital, reserves the delivery
//     (one per message and channel) and then checks it: another
//     hospital's message is not_found (NT9), a delivery someone else holds
//     or one already finished is left alone (NT18), a deactivated or
//     removed recipient is suppressed (NT17);
//  3. the channel must be enabled (NOTIFY_CHANNELS), the circuit breaker
//     closed (10 failures in a row pause sending for 5 minutes), and the
//     hourly allowances not spent: per recipient, and per hospital for all
//     but the mandatory kinds (NT13); a message that can't go yet waits,
//     nothing is dropped;
//  4. the global pace (NOTIFY_RATE_PER_SECOND) is kept, then the channel
//     sends;
//  5. accepted: sent; refused for good: failed (rejected_address); any
//     other failure retries after 1 min, 5 min, 30 min, 2 h, then every
//     6 h, and after 24 hours of trying is failed (provider_down), counted
//     and dead-lettered as dead.v1.<hospital>.notify_send (NT14).
//
// Logs carry the kind, ids and a closed outcome code; never the address,
// a name or a provider's text (NT19).
package deliver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/notifypb"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/channel"
)

// The NOTIFY stream and the sender's durable (core creates both when
// CORE_NOTIFY_ENABLED is on; the notifications user may only pull and ack
// notify_send).
const (
	Stream  = "NOTIFY"
	Durable = "notify_send"
	// Lease is how long a claim holds a delivery: the durable's AckWait,
	// so a message redelivered after a crash finds its lease expired.
	Lease = 2 * time.Minute
	// GiveUpAfter is how long a delivery is tried before it is failed.
	GiveUpAfter = 24 * time.Hour
	// sendTimeout bounds one channel send.
	sendTimeout = 30 * time.Second
	fetchBatch  = 4
	fetchWait   = 5 * time.Second
	// maxBody bounds a due message's body (one id).
	maxBody = 256
)

// mandatory kinds are always sent (design §10): the hospital's hourly cap
// doesn't hold them, though the recipient's does.
var mandatory = map[string]bool{"invite": true, "password_reset": true, "mfa_reset": true,
	"security_password_changed": true, "security_password_reset": true, "security_mfa_enabled": true,
	"security_mfa_replaced": true, "security_backup_code_used": true, "security_backup_codes_made": true}

// Mandatory reports whether kind is a mandatory (security) message.
func Mandatory(kind string) bool { return mandatory[kind] }

// Limits are the hourly allowances (design §9.4). Zero is the default.
type Limits struct {
	Recipient int // per recipient per hour (default 10)
	Hospital  int // per hospital per hour, mandatory kinds aside (default 200)
}

func (l Limits) recipient() int {
	if l.Recipient <= 0 {
		return 10
	}
	return l.Recipient
}

func (l Limits) hospital() int {
	if l.Hospital <= 0 {
		return 200
	}
	return l.Hospital
}

// backoff is the wait before retry n (1-based).
var backoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}

// Backoff is the wait after the attempts-th failed send.
func Backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > len(backoff) {
		return backoff[len(backoff)-1]
	}
	return backoff[attempts-1]
}

// Msg is what HandleMsg needs of a JetStream message.
type Msg interface {
	Subject() string
	Data() []byte
	Ack() error
	NakWithDelay(time.Duration) error
	Term() error
}

// DeadLetters records a delivery the service gave up on.
type DeadLetters interface {
	Dead(ctx context.Context, hospital, message uuid.UUID, outcome string, attempts int) error
}

// Metrics count deliveries by how they ended or waited. Counts only: no
// address, user id or hospital name (NT19).
type Metrics struct {
	Sent, Failed, Suppressed, Retried, Held, NotFound, Refused, DeadLettered, Errors atomic.Int64
}

// Sender handles notify_send's messages.
type Sender struct {
	Store    Store
	Channels map[string]channel.Channel // the enabled channels, by name
	Limits   Limits
	Pacer    *Pacer
	Breaker  *Breaker
	Dead     DeadLetters
	Log      *slog.Logger
	Metrics  *Metrics
	Now      func() time.Time
}

func (s *Sender) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Sender) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (s *Sender) metrics() *Metrics {
	if s.Metrics == nil {
		s.Metrics = &Metrics{}
	}
	return s.Metrics
}

// ParseDue reads notify.v1.<hospital>.due and its body: canonical uuids
// only. The platform subject has no kind yet (PR 6), so it is refused.
func ParseDue(subject string, data []byte) (uuid.UUID, uuid.UUID, bool) {
	seg := strings.Split(subject, ".")
	if len(seg) != 4 || seg[0] != "notify" || seg[1] != "v1" || seg[3] != "due" {
		return uuid.Nil, uuid.Nil, false
	}
	h, err := uuid.Parse(seg[2])
	if err != nil || h == uuid.Nil || h.String() != seg[2] {
		return uuid.Nil, uuid.Nil, false
	}
	var d notifypb.Due
	if len(data) > maxBody || proto.Unmarshal(data, &d) != nil || len(d.ProtoReflect().GetUnknown()) > 0 {
		return uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(d.GetMessageId())
	if err != nil || id == uuid.Nil || id.String() != d.GetMessageId() {
		return uuid.Nil, uuid.Nil, false
	}
	return h, id, true
}

// wait is the nak delay until t (at least a second).
func (s *Sender) wait(t time.Time) time.Duration {
	return max(t.Sub(s.now()), time.Second)
}

func (s *Sender) record(kind string, h, id uuid.UUID, outcome string) {
	s.log().Info("notifications: delivery", "kind", kind, "message_id", id.String(), "hospital_id", h.String(),
		"outcome", outcome)
}

// finish records f, retrying a failed write a few times: a delivery sent
// but not recorded could go twice after its lease ends.
func (s *Sender) finish(ctx context.Context, h, id uuid.UUID, c Claim, f Finish) error {
	var err error
	for i := 0; i < 3; i++ {
		if err = s.Store.Finish(ctx, h, id, c.Channel, c.Lease, f); err == nil || errors.Is(err, ErrLeaseLost) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(i+1) * 200 * time.Millisecond):
		}
	}
	return err
}

// HandleMsg settles one due message.
func (s *Sender) HandleMsg(ctx context.Context, m Msg) {
	h, id, ok := ParseDue(m.Subject(), m.Data())
	if !ok {
		s.metrics().Refused.Add(1)
		s.log().Warn("notifications: due message refused", "reason", "malformed")
		_ = m.Term()
		return
	}
	c, err := s.Store.Claim(ctx, h, id, Lease)
	if err != nil {
		s.metrics().Errors.Add(1)
		s.log().Warn("notifications: claim failed; retried", "message_id", id.String(), "reason", "internal_error")
		_ = m.NakWithDelay(30 * time.Second)
		return
	}
	switch c.Outcome {
	case NotFound:
		// Another hospital's id on this subject, or a row gone: nothing
		// is sent (NT9).
		s.metrics().NotFound.Add(1)
		s.record("", h, id, "not_found")
		_ = m.Term()
		return
	case Done:
		_ = m.Ack()
		return
	case Busy, Held:
		_ = m.NakWithDelay(s.wait(c.NextAttempt))
		return
	case Inactive:
		s.suppress(ctx, m, h, id, c)
		return
	case Claimed:
	default:
		s.metrics().Errors.Add(1)
		_ = m.NakWithDelay(30 * time.Second)
		return
	}
	s.send(ctx, m, h, id, c)
}

func (s *Sender) suppress(ctx context.Context, m Msg, h, id uuid.UUID, c Claim) {
	if err := s.finish(ctx, h, id, c, Finish{Status: "suppressed", Outcome: "inactive_user", Attempts: c.Attempts}); err != nil {
		s.metrics().Errors.Add(1)
		_ = m.NakWithDelay(30 * time.Second)
		return
	}
	s.metrics().Suppressed.Add(1)
	s.record(c.Kind, h, id, "inactive_user")
	_ = m.Ack()
}

// hold releases the claim until next, without counting an attempt.
func (s *Sender) hold(ctx context.Context, m Msg, h, id uuid.UUID, c Claim, outcome string, next time.Time) {
	if err := s.finish(ctx, h, id, c, Finish{Status: "claimed", Outcome: outcome, Attempts: c.Attempts, Next: next}); err != nil {
		s.metrics().Errors.Add(1)
	}
	s.metrics().Held.Add(1)
	s.record(c.Kind, h, id, outcome)
	_ = m.NakWithDelay(s.wait(next))
}

func (s *Sender) send(ctx context.Context, m Msg, h, id uuid.UUID, c Claim) {
	now := s.now()
	ch, ok := s.Channels[c.Channel]
	if !ok {
		// The outbox names a channel this service doesn't send on: a
		// configuration fault. It is retried like a failure and given up
		// on after a day, never dropped silently.
		s.failed(ctx, m, h, id, c, now, "channel_off")
		return
	}
	if s.Breaker != nil {
		if open, until := s.Breaker.Open(now); open {
			s.hold(ctx, m, h, id, c, "breaker_open", until)
			return
		}
	}
	reserved, retry, err := s.Store.Reserve(ctx, h, c.Recipient, Mandatory(c.Kind), now, s.Limits)
	if err != nil {
		// A counter that can't be written stops the send (fail closed).
		s.metrics().Errors.Add(1)
		s.hold(ctx, m, h, id, c, "rate_limited", now.Add(time.Minute))
		return
	}
	if !reserved {
		s.hold(ctx, m, h, id, c, "rate_limited", retry)
		return
	}
	if s.Pacer != nil {
		if err := s.Pacer.Wait(ctx); err != nil {
			s.hold(ctx, m, h, id, c, "rate_limited", now.Add(time.Minute))
			return
		}
	}
	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	res, err := ch.Send(sctx, channel.Message{ID: id, Hospital: h, Recipient: c.Recipient, Subject: c.Subject, Kind: c.Kind,
		Address: c.Address, When: c.Created})
	cancel()
	tried := s.now()
	switch {
	case err == nil && res.Outcome == channel.Accepted:
		if s.Breaker != nil {
			s.Breaker.Success()
		}
		if err := s.finish(ctx, h, id, c, Finish{Status: "sent", Outcome: "accepted", Attempts: c.Attempts + 1, Tried: tried,
			ProviderID: res.ProviderID}); err != nil {
			s.metrics().Errors.Add(1)
			s.log().Error("notifications: sent but not recorded", "message_id", id.String(), "reason", "internal_error")
		}
		s.metrics().Sent.Add(1)
		s.record(c.Kind, h, id, "accepted")
		_ = m.Ack()
	case err == nil && res.Outcome == channel.Permanent:
		if s.Breaker != nil {
			s.Breaker.Success() // the provider answered
		}
		if err := s.finish(ctx, h, id, c, Finish{Status: "failed", Outcome: "rejected_address", Attempts: c.Attempts + 1,
			Tried: tried}); err != nil {
			s.metrics().Errors.Add(1)
			_ = m.NakWithDelay(30 * time.Second)
			return
		}
		s.metrics().Failed.Add(1)
		s.record(c.Kind, h, id, "rejected_address")
		_ = m.Ack()
	default:
		if s.Breaker != nil {
			s.Breaker.Failure(tried)
		}
		s.failed(ctx, m, h, id, c, tried, "provider_down")
	}
}

// failed counts a failed attempt: retried with a back-off, or after
// GiveUpAfter of trying, failed and dead-lettered.
func (s *Sender) failed(ctx context.Context, m Msg, h, id uuid.UUID, c Claim, tried time.Time, outcome string) {
	attempts := c.Attempts + 1
	first := c.FirstAttempt
	if first.IsZero() {
		first = tried
	}
	if tried.Sub(first) >= GiveUpAfter {
		if err := s.finish(ctx, h, id, c, Finish{Status: "failed", Outcome: outcome, Attempts: attempts, Tried: tried}); err != nil {
			s.metrics().Errors.Add(1)
			_ = m.NakWithDelay(30 * time.Second)
			return
		}
		s.metrics().Failed.Add(1)
		s.record(c.Kind, h, id, outcome)
		if s.Dead != nil {
			if err := s.Dead.Dead(ctx, h, id, outcome, attempts); err != nil {
				s.log().Warn("notifications: dead letter not published", "message_id", id.String(), "reason", "publish_failed")
			} else {
				s.metrics().DeadLettered.Add(1)
			}
		}
		_ = m.Ack()
		return
	}
	next := tried.Add(Backoff(attempts))
	if err := s.finish(ctx, h, id, c, Finish{Status: "claimed", Outcome: "retrying", Attempts: attempts, Next: next,
		Tried: tried}); err != nil {
		s.metrics().Errors.Add(1)
	}
	s.metrics().Retried.Add(1)
	s.record(c.Kind, h, id, "retrying")
	_ = m.NakWithDelay(s.wait(next))
}

// Pacer spaces sends to a global rate (design §9.4).
type Pacer struct {
	mu    sync.Mutex
	every time.Duration
	next  time.Time
}

// NewPacer paces to perSecond sends a second (at least 1).
func NewPacer(perSecond int) *Pacer {
	return &Pacer{every: time.Second / time.Duration(max(perSecond, 1))}
}

// Wait returns when the next send may go, or ctx's error.
func (p *Pacer) Wait(ctx context.Context) error {
	p.mu.Lock()
	now := time.Now()
	at := p.next
	if at.Before(now) {
		at = now
	}
	p.next = at.Add(p.every)
	p.mu.Unlock()
	d := time.Until(at)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Breaker pauses sending after Threshold failures in a row, for Cooldown
// (design §9.3: 10 and 5 minutes). Messages wait; nothing is dropped.
type Breaker struct {
	Threshold int
	Cooldown  time.Duration
	mu        sync.Mutex
	fails     int
	until     time.Time
}

// NewBreaker is the design's breaker.
func NewBreaker() *Breaker { return &Breaker{Threshold: 10, Cooldown: 5 * time.Minute} }

// Open reports whether sending is paused at now, and until when.
func (b *Breaker) Open(now time.Time) (bool, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return now.Before(b.until), b.until
}

// Success resets the count.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails = 0
}

// Failure counts one and opens the breaker at the threshold.
func (b *Breaker) Failure(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails++
	if b.fails >= b.Threshold {
		b.until = now.Add(b.Cooldown)
		b.fails = 0
	}
}

// JSDead publishes dead.v1.<hospital>.notify_send onto DEAD, ids and a
// code only, de-duplicated by the message id.
type JSDead struct{ JS jetstream.JetStream }

// Dead publishes the letter.
func (d JSDead) Dead(ctx context.Context, hospital, message uuid.UUID, outcome string, attempts int) error {
	b, err := proto.Marshal(&notifypb.DeadLetter{MessageId: message.String(), Outcome: outcome, Attempts: uint32(max(attempts, 0))})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = d.JS.PublishMsg(ctx, &nats.Msg{Subject: "dead.v1." + hospital.String() + ".notify_send", Data: b},
		jetstream.WithMsgID(message.String()+":notify_send"))
	return err
}

// Run pulls notify_send until ctx ends. Until core has made the stream
// (CORE_NOTIFY_ENABLED), it waits.
func (s *Sender) Run(ctx context.Context, js jetstream.JetStream) {
	var cons jetstream.Consumer
	for ctx.Err() == nil {
		k, err := js.Consumer(ctx, Stream, Durable)
		if err == nil {
			cons = k
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
	for ctx.Err() == nil {
		batch, err := cons.Fetch(fetchBatch, jetstream.FetchMaxWait(fetchWait))
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		for m := range batch.Messages() {
			s.HandleMsg(ctx, m)
		}
	}
}

// RunRetention runs the store's retention now and then every day.
func RunRetention(ctx context.Context, st Store, log *slog.Logger) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		if n, err := st.Retention(ctx); err != nil {
			if ctx.Err() == nil {
				log.Warn("notifications: retention failed", "reason", "internal_error")
			}
		} else if n > 0 {
			log.Info("notifications: retention", "removed", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
