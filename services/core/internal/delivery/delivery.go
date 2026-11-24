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
//
// Security notices (a password changed or reset, two-step turned on or
// moved to a new app, a backup code used or new ones made) are recorded by
// Notice in the change's own transaction, through a definer (migration
// 00058). Unlike the admin actions, a change never waits on them: with no
// sender, or if the notice can't be recorded, the change goes ahead and
// the failure is logged.
package delivery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
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

	// The security notices (migration 00058): told to the account's own
	// address when its security changes. Recorded by Notice, never by
	// Record.
	NoticePasswordChanged Kind = "security_password_changed"
	NoticePasswordReset   Kind = "security_password_reset"
	NoticeMFAEnabled      Kind = "security_mfa_enabled"
	NoticeMFAReplaced     Kind = "security_mfa_replaced"
	NoticeBackupCodeUsed  Kind = "security_backup_code_used"
	NoticeBackupCodesMade Kind = "security_backup_codes_made"
)

// SecurityNotices are the kinds Notice records.
var SecurityNotices = []Kind{NoticePasswordChanged, NoticePasswordReset, NoticeMFAEnabled, NoticeMFAReplaced,
	NoticeBackupCodeUsed, NoticeBackupCodesMade}

// IsSecurityNotice reports whether k is one of SecurityNotices.
func IsSecurityNotice(k Kind) bool { return slices.Contains(SecurityNotices, k) }

// ErrNotNotice: Notice was given a kind that isn't a security notice.
var ErrNotNotice = errors.New("delivery: not a security notice kind")

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

// Notice records a security notice of kind for user in tx, through
// mhai_security_notice (migration 00058; tx runs as mhai_auth): the
// recipient's hospital is the database's, never the caller's. It runs in a
// savepoint, so a failure here never aborts the change it reports: the
// caller logs it and goes on. It returns the recipient's hospital when a row
// was written, and uuid.Nil when none was (a platform account, an inactive
// one, or one already told of this kind in this minute).
func Notice(ctx context.Context, tx pgx.Tx, s Sender, user uuid.UUID, kind Kind) (uuid.UUID, error) {
	if s == nil {
		return uuid.Nil, ErrNoSender
	}
	if !IsSecurityNotice(kind) || user == uuid.Nil {
		return uuid.Nil, ErrNotNotice
	}
	// A named savepoint in the caller's transaction (not a new one).
	if _, err := tx.Exec(ctx, `SAVEPOINT mhai_security_notice`); err != nil {
		return uuid.Nil, err
	}
	var h *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT mhai_security_notice($1, $2, $3)`, user, string(kind), s.Channel()).Scan(&h); err != nil {
		_, _ = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT mhai_security_notice`)
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT mhai_security_notice`); err != nil {
		return uuid.Nil, err
	}
	if h == nil {
		return uuid.Nil, nil
	}
	return *h, nil
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
