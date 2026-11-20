package notifyctl

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/delivery"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/notifypb"
)

// Other kinds go by the outbox: Send does nothing for them. A link kind
// with no link, recipient or client is misuse, and nothing is sent.
func TestSenderShapes(t *testing.T) {
	s := Sender{}
	if s.Channel() != "email" {
		t.Fatal(s.Channel())
	}
	for _, k := range []delivery.Kind{delivery.MFAReset, delivery.MFAReminder, delivery.ReviewReminder, delivery.ReviewAssigned} {
		if err := s.Send(context.Background(), delivery.Message{Kind: k}); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
	for _, m := range []delivery.Message{{Kind: delivery.Invite, Recipient: uuid.New(), Link: "x"},
		{Kind: delivery.PasswordReset, Recipient: uuid.New()}} {
		if err := (Sender{Client: &Client{}}).Send(context.Background(), m); !errors.Is(err, ErrMisuse) && m.Link == "" {
			t.Errorf("%+v: %v", m, err)
		}
		if err := s.Send(context.Background(), m); !errors.Is(err, ErrMisuse) {
			t.Errorf("no client: %v", err)
		}
	}
}

// The client refuses misuse before anything is sent: an unknown op, the
// wrong messages, no hospital or request id, no connection.
func TestRequestMisuse(t *testing.T) {
	c := &Client{}
	ctx := context.Background()
	h := uuid.New()
	for name, err := range map[string]error{
		"unknown op":    c.Request(ctx, h, "rid", "nope", &notifypb.HealthRequest{}, &notifypb.HealthReply{}),
		"wrong request": c.Request(ctx, h, "rid", "send_link", &notifypb.HealthRequest{}, &notifypb.SendLinkReply{}),
		"wrong reply":   c.Request(ctx, h, "rid", "send_link", &notifypb.SendLinkRequest{}, &notifypb.HealthReply{}),
		"no hospital":   c.Request(ctx, uuid.Nil, "rid", "health", &notifypb.HealthRequest{}, &notifypb.HealthReply{}),
		"no rid":        c.Request(ctx, h, "", "health", &notifypb.HealthRequest{}, &notifypb.HealthReply{}),
		"no connection": c.Request(ctx, h, "rid", "health", &notifypb.HealthRequest{}, &notifypb.HealthReply{}),
		"nil request":   c.Request(ctx, h, "rid", "health", nil, &notifypb.HealthReply{}),
	} {
		if !errors.Is(err, ErrMisuse) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for in, want := range map[string]string{"failed": "failed", "no_recipient": "no_recipient", "invalid": "invalid",
		"synthetic free text": "other", "": "other"} {
		if closed(in) != want {
			t.Errorf("closed(%q) = %q", in, closed(in))
		}
	}
	if c.log() == nil {
		t.Fatal("log")
	}
}
