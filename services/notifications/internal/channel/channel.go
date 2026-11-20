// Package channel is how the notifications service hands one message to
// one outbound channel (notifications design §12.1). PR 3 builds the log
// channel (development only: it logs that a message would go, by kind and
// ids, and sends nothing); the email channel over SMTP submission comes
// with PR 4, SMS and WhatsApp later behind their own design.
//
// A Message carries ids, the kind and, for a channel that needs one, the
// recipient's address read at send time by notify_claim. It never carries
// patient data, a link, a token or free text: there is no field for any.
package channel

import (
	"context"
	"io"
	"log/slog"

	"github.com/google/uuid"
)

// Message is one message to one person, ready for a channel.
type Message struct {
	ID        uuid.UUID // the message_outbox row
	Hospital  uuid.UUID
	Recipient uuid.UUID
	// Subject is the correction a review message is about (uuid.Nil
	// otherwise).
	Subject uuid.UUID
	Kind    string
	// Address is the recipient's address for a channel that needs one
	// (email); "" for the log channel. Never logged.
	Address string
}

// Outcome is what a channel did with a message.
type Outcome int

const (
	// Accepted: the channel took it.
	Accepted Outcome = iota
	// Retryable: try again later (a timeout, a temporary refusal, the
	// provider down).
	Retryable
	// Permanent: never retry (the address refused).
	Permanent
)

// Result is a send's outcome and, when accepted, the provider's id for
// the message (only its SHA-256 is stored).
type Result struct {
	Outcome    Outcome
	ProviderID string
}

// Channel sends messages. Send must not log the address or any provider
// text; an error is treated as Retryable.
type Channel interface {
	Name() string
	Send(ctx context.Context, m Message) (Result, error)
}

// Log is the development channel: it logs that a message would go, by
// kind and ids, never the address, and sends nothing. Config refuses it
// outside development.
type Log struct{ Logger *slog.Logger }

// Name is "log".
func (Log) Name() string { return "log" }

// Send logs m's kind and ids.
func (l Log) Send(ctx context.Context, m Message) (Result, error) {
	lg := l.Logger
	if lg == nil {
		lg = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	attrs := []any{"kind", m.Kind, "message_id", m.ID.String(), "hospital_id", m.Hospital.String(),
		"recipient_id", m.Recipient.String()}
	if m.Subject != uuid.Nil {
		attrs = append(attrs, "subject_id", m.Subject.String())
	}
	lg.InfoContext(ctx, "notifications: message delivered to the log channel (nothing is sent)", attrs...)
	return Result{Outcome: Accepted, ProviderID: "log:" + m.ID.String()}, nil
}
