// Package delivery is how core hands a message it owes a person (an
// invite, a password reset link, a two-step notice or reminder, a review
// reminder or assignment) to whatever delivers it.
//
// No email provider is chosen yet (TODO.md, "Mail provider"), so the only
// sender is Log: it records in the outbox (message_outbox, migrations
// 00049 and 00053) and in the service log that a message is due, by kind
// and ids, and never the link, the token, an email address or any text. Log is
// allowed only in development (config refuses CORE_DELIVERY=log
// elsewhere); with no sender configured, every action that must send
// something fails closed with 503 no_delivery before it changes anything.
//
// A real sender (email) implements Sender: Record writes the outbox row in
// the action's transaction, and Send runs after commit with the message's
// link, which it alone may see. With CORE_NOTIFY_ENABLED, core's drainer
// (internal/notify) publishes each due row's id to the notifications
// service after commit (notifications design §9.1).
package delivery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Kind is what a message is.
type Kind string

// The kinds core sends (message_outbox.kind).
const (
	Invite         Kind = "invite"
	PasswordReset  Kind = "password_reset"
	MFAReset       Kind = "mfa_reset"
	MFAReminder    Kind = "mfa_reminder"
	ReviewReminder Kind = "review_reminder"
	ReviewAssigned Kind = "review_assigned"
)

// Message is one message to one person of one hospital.
type Message struct {
	Kind       Kind
	HospitalID uuid.UUID
	Recipient  uuid.UUID
	// Subject is the correction a review message is about (uuid.Nil
	// otherwise).
	Subject uuid.UUID
	// By is the hospital admin who caused it.
	By uuid.UUID
	// Link is the secret link an invite or reset carries ("" otherwise).
	// Only a sender's Send sees it; it is never recorded or logged.
	Link      string
	ExpiresAt time.Time
}

// Sender delivers messages.
type Sender interface {
	// Channel is message_outbox.channel.
	Channel() string
	// Send delivers m after the action's commit. An error means the
	// person may not get it; the caller tells the admin.
	Send(ctx context.Context, m Message) error
}

// ErrNoSender: no sender is configured.
var ErrNoSender = errors.New("delivery: no sender configured")

// Record writes m's outbox row in tx (RLS: the caller's hospital). It
// records only that a message is due.
func Record(ctx context.Context, tx pgx.Tx, s Sender, m Message) error {
	if s == nil {
		return ErrNoSender
	}
	var subject *uuid.UUID
	if m.Subject != uuid.Nil {
		subject = &m.Subject
	}
	_, err := tx.Exec(ctx, `INSERT INTO message_outbox (hospital_id, recipient_id, kind, subject_id, channel, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)`, m.HospitalID, m.Recipient, string(m.Kind), subject, s.Channel(), m.By)
	return err
}

// Log is the development sender: it logs that a message is due, by kind
// and ids only.
type Log struct{ Logger *slog.Logger }

// Channel is "log".
func (Log) Channel() string { return "log" }

// Send logs m without its link.
func (l Log) Send(ctx context.Context, m Message) error {
	lg := l.Logger
	if lg == nil {
		lg = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	attrs := []any{"kind", string(m.Kind), "hospital_id", m.HospitalID.String(), "recipient_id", m.Recipient.String(),
		"by", m.By.String()}
	if m.Subject != uuid.Nil {
		attrs = append(attrs, "subject_id", m.Subject.String())
	}
	lg.InfoContext(ctx, "delivery: message due (log sender; nothing is sent)", attrs...)
	return nil
}
