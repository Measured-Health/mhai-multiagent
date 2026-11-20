package channel

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The log channel accepts and logs kind and ids, never the address
// (NT19), and works without a logger.
func TestLogChannel(t *testing.T) {
	var buf bytes.Buffer
	l := Log{Logger: slog.New(slog.NewJSONHandler(&buf, nil))}
	if l.Name() != "log" {
		t.Fatal(l.Name())
	}
	m := Message{ID: uuid.New(), Hospital: uuid.New(), Recipient: uuid.New(), Subject: uuid.New(), Kind: "review_reminder",
		Address: "synthetic-recipient@example.test"}
	r, err := l.Send(context.Background(), m)
	if err != nil || r.Outcome != Accepted || r.ProviderID == "" {
		t.Fatalf("%+v %v", r, err)
	}
	out := buf.String()
	if strings.Contains(out, "synthetic-recipient") || strings.Contains(out, "example.test") {
		t.Fatalf("the log carries the address: %s", out)
	}
	for _, want := range []string{"review_reminder", m.ID.String(), m.Hospital.String(), m.Recipient.String(), m.Subject.String()} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %s", want)
		}
	}
	if _, err := (Log{}).Send(context.Background(), Message{Kind: "mfa_reminder"}); err != nil {
		t.Fatal(err)
	}
}
