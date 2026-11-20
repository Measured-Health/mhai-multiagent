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
