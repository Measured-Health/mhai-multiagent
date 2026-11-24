//go:build integration

package deliver

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/channel"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/email"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/templates"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/testsmtp"
)

// PR 4 end to end on the real claim: a due email message goes once over
// STARTTLS to the recipient's address as it is at send time (NT6: changed
// after the message was due, the new one), with the fixed notice and no
// patient data; the delivery records sent and only the hash of the
// message id. A refused address fails at once.
func TestEmailDeliveryEndToEnd(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	srv, err := testsmtp.Start(testsmtp.StartTLS, "mhai-sender", "synthetic-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	app, err := templates.ParseApp("https://app.example.test", false)
	if err != nil {
		t.Fatal(err)
	}
	ec := &email.Channel{App: app, Config: email.Config{Host: "localhost", Port: 587, Username: srv.User, Password: srv.Password,
		From: "no-reply@mail.example.test", ReplyTo: "support@example.test", RootCAs: srv.CAs, Timeout: 5 * time.Second,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, srv.Addr)
		}}}
	s := &Sender{Store: w.store, Channels: map[string]channel.Channel{"email": ec}, Breaker: NewBreaker(), Metrics: &Metrics{}}

	id := w.message(t, w.h, w.u, "mfa_reminder", "email")
	newAddr := "synthetic-moved-" + uuid.NewString()[:8] + "@example.test"
	if _, err := w.super.Exec(ctx, `UPDATE users SET email = $1 WHERE id = $2`, newAddr, w.u); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		s.HandleMsg(ctx, due(w.h, id))
	}
	msgs := srv.Messages()
	if len(msgs) != 1 || len(msgs[0].To) != 1 || msgs[0].To[0] != newAddr ||
		!strings.Contains(msgs[0].Data, "Subject: Set up two-step sign-in for MHAI") {
		t.Fatalf("%d messages: %+v", len(msgs), msgs)
	}
	var status string
	var hashLen int
	if err := w.super.QueryRow(ctx, `SELECT status, length(provider_ref_hash) FROM message_deliveries WHERE message_id = $1`, id).
		Scan(&status, &hashLen); err != nil || status != "sent" || hashLen != 32 {
		t.Fatalf("%s %d %v", status, hashLen, err)
	}
	// A refused address: failed at once, never retried.
	r := w.user(t, w.h)
	if _, err := w.super.Exec(ctx, `UPDATE users SET email = 'reject-synthetic@example.test' WHERE id = $1`, r); err != nil {
		t.Fatal(err)
	}
	bad := w.message(t, w.h, r, "mfa_reminder", "email")
	m := due(w.h, bad)
	s.HandleMsg(ctx, m)
	var outcome string
	if err := w.super.QueryRow(ctx, `SELECT status, outcome_code FROM message_deliveries WHERE message_id = $1`, bad).
		Scan(&status, &outcome); err != nil || status != "failed" || outcome != "rejected_address" || m.ended != "ack" {
		t.Fatalf("%s %s %s %v", status, outcome, m.ended, err)
	}
}

// Security notices (migration 00058) end to end on the real claim: the
// claim answers when the change happened (the outbox row's created_at), and
// the email goes once to the account's address over the test SMTP double,
// with the fixed subject, that time in UTC, the contact line and no link.
func TestSecurityNoticeEmailEndToEnd(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	srv, err := testsmtp.Start(testsmtp.StartTLS, "mhai-sender", "synthetic-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	app, err := templates.ParseApp("https://app.example.test", false)
	if err != nil {
		t.Fatal(err)
	}
	ec := &email.Channel{App: app, Config: email.Config{Host: "localhost", Port: 587, Username: srv.User, Password: srv.Password,
		From: "no-reply@mail.example.test", ReplyTo: "support@example.test", RootCAs: srv.CAs, Timeout: 5 * time.Second,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, srv.Addr)
		}}}
	s := &Sender{Store: w.store, Channels: map[string]channel.Channel{"email": ec}, Breaker: NewBreaker(), Metrics: &Metrics{}}

	var addr string
	if err := w.super.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, w.u).Scan(&addr); err != nil {
		t.Fatal(err)
	}
	id := w.message(t, w.h, w.u, "security_backup_code_used", "email")
	at := time.Date(2026, 11, 24, 14, 5, 0, 0, time.UTC)
	if _, err := w.super.Exec(ctx, `UPDATE message_outbox SET created_at = $1 WHERE id = $2`, at, id); err != nil {
		t.Fatal(err)
	}
	c, err := w.store.Claim(ctx, w.h, id, Lease)
	if err != nil || c.Outcome != Claimed || !c.Created.Equal(at) || c.Kind != "security_backup_code_used" {
		t.Fatalf("%+v %v", c, err)
	}
	if err := w.store.Finish(ctx, w.h, id, c.Channel, c.Lease, Finish{Status: "claimed", Outcome: "retrying"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s.HandleMsg(ctx, due(w.h, id))
	}
	msgs := srv.Messages()
	if len(msgs) != 1 || len(msgs[0].To) != 1 || msgs[0].To[0] != addr {
		t.Fatalf("%d messages: %+v", len(msgs), msgs)
	}
	// Undo the quoted-printable soft line breaks (the words checked hold
	// no '=' and no other escape).
	text := strings.ReplaceAll(msgs[0].Data, "=\r\n", "")
	for _, want := range []string{"Subject: A backup code was used on your MHAI account", "24 November 2026 at 14:05 UTC",
		"info@measured-health.org straight away", "Reply-To: support@example.test"} {
		if !strings.Contains(text, want) {
			t.Errorf("the email lacks %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"https://", "href", "app.example.test/"} {
		if strings.Contains(text, bad) {
			t.Errorf("the notice carries %q", bad)
		}
	}
}
