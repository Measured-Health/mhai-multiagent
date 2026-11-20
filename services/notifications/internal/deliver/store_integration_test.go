//go:build integration

package deliver

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/channel"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/testpg"
)

type world struct {
	super *pgxpool.Pool
	store PG
	h, h2 uuid.UUID
	u, u2 uuid.UUID
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	url, super := testpg.Migrated(t)
	app, err := pgxpool.New(ctx, testpg.Login(t, url, super, "notify_app", "mhai_notify_app"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	w := &world{super: super, store: PG{Pool: app}, h: testpg.Hospital(t, super), h2: testpg.Hospital(t, super)}
	w.u, w.u2 = w.user(t, w.h), w.user(t, w.h2)
	return w
}

func (w *world) user(t *testing.T, h uuid.UUID) uuid.UUID {
	t.Helper()
	var u uuid.UUID
	if err := w.super.QueryRow(context.Background(), `INSERT INTO users (hospital_id, email, display_name) VALUES ($1, $2, 'Synth U')
		RETURNING id`, h, "synth-"+uuid.NewString()[:8]+"@example.test").Scan(&u); err != nil {
		t.Fatal(err)
	}
	return u
}

func (w *world) message(t *testing.T, h, u uuid.UUID, kind, ch string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := w.super.QueryRow(context.Background(), `INSERT INTO message_outbox (hospital_id, recipient_id, kind, channel, created_by)
		VALUES ($1, $2, $3, $4, $2) RETURNING id`, h, u, kind, ch).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// countingChannel counts sends, safely across goroutines.
type countingChannel struct{ n atomic.Int64 }

func (c *countingChannel) Name() string { return "log" }
func (c *countingChannel) Send(context.Context, channel.Message) (channel.Result, error) {
	c.n.Add(1)
	return channel.Result{Outcome: channel.Accepted}, nil
}

func (w *world) sender(ch channel.Channel) *Sender {
	return &Sender{Store: w.store, Channels: map[string]channel.Channel{"log": ch}, Breaker: NewBreaker(), Metrics: &Metrics{}}
}

// NT9: a message of hospital A claimed under hospital B (a due subject
// naming B with A's id) is not_found, and nothing is sent or recorded.
func TestClaimOtherHospitalIsNotFound(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	id := w.message(t, w.h, w.u, "mfa_reminder", "log")
	c, err := w.store.Claim(ctx, w.h2, id, Lease)
	if err != nil || c.Outcome != NotFound || c.Kind != "" || c.Recipient != uuid.Nil {
		t.Fatalf("%+v %v", c, err)
	}
	ch := &countingChannel{}
	m := due(w.h2, id)
	w.sender(ch).HandleMsg(ctx, m)
	var n int
	if err := w.super.QueryRow(ctx, `SELECT count(*) FROM message_deliveries`).Scan(&n); err != nil || n != 0 ||
		ch.n.Load() != 0 || m.ended != "term" {
		t.Fatalf("deliveries %d, sends %d, %s, %v", n, ch.n.Load(), m.ended, err)
	}
	if _, err := w.store.Claim(ctx, w.h, uuid.New(), Lease); err != nil {
		t.Fatal(err)
	}
}

// NT18: the same due message handled five times, and twenty claims racing,
// send once: one delivery per message and channel, reserve then check.
func TestOneSendPerMessage(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	ch := &countingChannel{}
	s := w.sender(ch)
	id := w.message(t, w.h, w.u, "mfa_reminder", "log")
	for i := 0; i < 5; i++ {
		s.HandleMsg(ctx, due(w.h, id))
	}
	if ch.n.Load() != 1 {
		t.Fatalf("%d sends for one message", ch.n.Load())
	}
	var status, outcome string
	var attempts int
	var sent bool
	if err := w.super.QueryRow(ctx, `SELECT status, outcome_code, attempts, sent_at IS NOT NULL FROM message_deliveries
		WHERE message_id = $1`, id).Scan(&status, &outcome, &attempts, &sent); err != nil || status != "sent" ||
		outcome != "accepted" || attempts != 1 || !sent {
		t.Fatalf("%s %s %d %v %v", status, outcome, attempts, sent, err)
	}
	// Twenty claims of one fresh message at once: one holds the lease.
	race := w.message(t, w.h, w.u, "mfa_reminder", "log")
	var claimed, busy atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := w.store.Claim(ctx, w.h, race, Lease)
			switch {
			case err != nil:
				t.Error(err)
			case c.Outcome == Claimed:
				claimed.Add(1)
			case c.Outcome == Busy:
				busy.Add(1)
			}
		}()
	}
	wg.Wait()
	if claimed.Load() != 1 || busy.Load() != 19 {
		t.Fatalf("claimed %d, busy %d", claimed.Load(), busy.Load())
	}
	// Two senders racing over the same messages send each once.
	ids := []uuid.UUID{}
	for i := 0; i < 10; i++ {
		ids = append(ids, w.message(t, w.h, w.user(t, w.h), "mfa_reminder", "log"))
	}
	ch2 := &countingChannel{}
	a, b := w.sender(ch2), w.sender(ch2)
	wg.Add(2)
	for _, x := range []*Sender{a, b} {
		go func(x *Sender) {
			defer wg.Done()
			for _, id := range ids {
				x.HandleMsg(ctx, due(w.h, id))
			}
		}(x)
	}
	wg.Wait()
	if ch2.n.Load() != 10 {
		t.Fatalf("%d sends for 10 messages and two senders", ch2.n.Load())
	}
}

// NT17: a recipient deactivated (or removed) after the message was due is
// suppressed as inactive_user, and nothing is sent.
func TestDeactivatedRecipientGetsNothing(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	id := w.message(t, w.h, w.u, "mfa_reminder", "log")
	if _, err := w.super.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1`, w.u); err != nil {
		t.Fatal(err)
	}
	ch := &countingChannel{}
	m := due(w.h, id)
	w.sender(ch).HandleMsg(ctx, m)
	var status, outcome string
	if err := w.super.QueryRow(ctx, `SELECT status, outcome_code FROM message_deliveries WHERE message_id = $1`, id).
		Scan(&status, &outcome); err != nil || status != "suppressed" || outcome != "inactive_user" || ch.n.Load() != 0 ||
		m.ended != "ack" {
		t.Fatalf("%s %s sends %d %s %v", status, outcome, ch.n.Load(), m.ended, err)
	}
}

// The address reaches only the email channel, for an active recipient; the
// log channel never gets one.
func TestAddressOnlyForEmail(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	c, err := w.store.Claim(ctx, w.h, w.message(t, w.h, w.u, "mfa_reminder", "log"), Lease)
	if err != nil || c.Outcome != Claimed || c.Address != "" || c.Lease == uuid.Nil || c.Recipient != w.u {
		t.Fatalf("log: %+v %v", c, err)
	}
	c, err = w.store.Claim(ctx, w.h, w.message(t, w.h, w.u, "mfa_reminder", "email"), Lease)
	if err != nil || c.Outcome != Claimed || c.Address == "" || c.Channel != "email" {
		t.Fatalf("email: outcome %s channel %s address set %v %v", c.Outcome, c.Channel, c.Address != "", err)
	}
	// A held delivery waits; finishing needs this caller's lease.
	id := w.message(t, w.h, w.u, "mfa_reminder", "log")
	c, _ = w.store.Claim(ctx, w.h, id, Lease)
	if err := w.store.Finish(ctx, w.h, id, "log", uuid.New(), Finish{Status: "sent", Outcome: "accepted"}); err != ErrLeaseLost {
		t.Fatalf("finished with another lease: %v", err)
	}
	next := time.Now().Add(time.Hour)
	if err := w.store.Finish(ctx, w.h, id, "log", c.Lease, Finish{Status: "claimed", Outcome: "rate_limited", Next: next}); err != nil {
		t.Fatal(err)
	}
	if c, err := w.store.Claim(ctx, w.h, id, Lease); err != nil || c.Outcome != Held || c.NextAttempt.Before(next.Add(-time.Second)) {
		t.Fatalf("held: %+v %v", c, err)
	}
	// Under another hospital the store sees nothing of it.
	if err := w.store.Finish(ctx, w.h2, id, "log", c.Lease, Finish{Status: "sent", Outcome: "accepted"}); err != ErrLeaseLost {
		t.Fatalf("finished under another hospital: %v", err)
	}
}

// NT13: 1,000 reminders for one hospital in an hour reserve at most 200;
// the rest wait for the next hour; mandatory kinds still go (the
// recipient's own allowance aside). A spent recipient allowance takes
// nothing from the hospital's.
func TestHospitalHourlyCap(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	now := time.Now().UTC()
	ok := 0
	for i := 0; i < 1000; i++ {
		got, retry, err := w.store.Reserve(ctx, w.h, uuid.New(), false, now, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if got {
			ok++
		} else if !retry.Equal(now.Truncate(time.Hour).Add(time.Hour)) {
			t.Fatalf("retry at %v", retry)
		}
	}
	if ok != 200 {
		t.Fatalf("%d of 1000 reserved, want 200", ok)
	}
	if got, _, err := w.store.Reserve(ctx, w.h, uuid.New(), true, now, Limits{}); err != nil || !got {
		t.Fatalf("a mandatory message was held: %v", err)
	}
	// The other hospital is untouched; the next hour starts afresh.
	if got, _, err := w.store.Reserve(ctx, w.h2, uuid.New(), false, now, Limits{}); err != nil || !got {
		t.Fatalf("another hospital was capped: %v", err)
	}
	if got, _, err := w.store.Reserve(ctx, w.h, uuid.New(), false, now.Add(time.Hour), Limits{}); err != nil || !got {
		t.Fatalf("the next hour was capped: %v", err)
	}
	// Per recipient: 3 an hour here; the fourth waits and takes nothing
	// from the hospital's count.
	r := uuid.New()
	l := Limits{Recipient: 3, Hospital: 1000}
	later := now.Add(2 * time.Hour)
	for i := 0; i < 4; i++ {
		got, _, err := w.store.Reserve(ctx, w.h2, r, false, later, l)
		if err != nil || got != (i < 3) {
			t.Fatalf("recipient reserve %d: %v %v", i, got, err)
		}
	}
	var n int
	if err := w.super.QueryRow(ctx, `SELECT n FROM message_send_counts WHERE hospital_id = $1 AND scope = 'hospital'
		AND window_start = $2`, w.h2, later.Truncate(time.Hour)).Scan(&n); err != nil || n != 3 {
		t.Fatalf("the hospital's count is %d, %v", n, err)
	}
	if n, err := w.store.Retention(ctx); err != nil || n != 0 {
		t.Fatalf("retention: %d %v", n, err)
	}
	if _, err := w.super.Exec(ctx, `UPDATE message_send_counts SET window_start = window_start - interval '3 days'`); err != nil {
		t.Fatal(err)
	}
	if n, err := w.store.Retention(ctx); err != nil || n == 0 {
		t.Fatalf("old counters stay: %d %v", n, err)
	}
}
