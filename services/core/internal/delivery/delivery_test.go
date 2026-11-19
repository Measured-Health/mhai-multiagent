package delivery_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/delivery"
)

// The log sender records that a message is due by kind and ids, and never
// the link (the secret a reset or invite carries).
func TestLogSenderNeverLogsTheLink(t *testing.T) {
	var buf bytes.Buffer
	s := delivery.Log{Logger: slog.New(slog.NewJSONHandler(&buf, nil))}
	if s.Channel() != "log" {
		t.Fatal(s.Channel())
	}
	h, u, by, c := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	link := "https://app.example.test/reset-password?token=SYNTH-SECRET-TOKEN-z8"
	if err := s.Send(context.Background(), delivery.Message{Kind: delivery.ReviewReminder, HospitalID: h, Recipient: u,
		Subject: c, By: by, Link: link}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "SYNTH-SECRET-TOKEN") || strings.Contains(out, "reset-password") {
		t.Fatalf("the log carries the link: %s", out)
	}
	for _, want := range []string{"review_reminder", h.String(), u.String(), by.String(), c.String()} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %s: %s", want, out)
		}
	}
	// A nil logger is fine; no subject, no subject id.
	if err := (delivery.Log{}).Send(context.Background(), delivery.Message{Kind: delivery.MFAReminder}); err != nil {
		t.Fatal(err)
	}
}

// Record refuses without a sender (the callers' no_delivery).
func TestRecordNeedsASender(t *testing.T) {
	if err := delivery.Record(context.Background(), nil, nil, delivery.Message{}); !errors.Is(err, delivery.ErrNoSender) {
		t.Fatal(err)
	}
}
