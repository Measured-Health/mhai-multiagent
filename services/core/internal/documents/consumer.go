package documents

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
)

// JSAnnouncer publishes documents.v1.<hid>.uploaded {document_id} with
// Nats-Msg-Id "<document_id>:uploaded" (DOCS drops a duplicate within its
// window).
type JSAnnouncer struct{ JS jetstream.JetStream }

// Uploaded announces document's file for its scan.
func (a JSAnnouncer) Uploaded(ctx context.Context, hospital, document uuid.UUID) error {
	if a.JS == nil {
		return errors.New("documents: no JetStream")
	}
	body, err := proto.Marshal(&docspb.Uploaded{DocumentId: document.String()})
	if err != nil {
		return err
	}
	m := nats.NewMsg("documents.v1." + hospital.String() + ".uploaded")
	m.Header.Set(jetstream.MsgIDHeader, document.String()+":uploaded")
	m.Data = body
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = a.JS.PublishMsg(ctx, m)
	return err
}

// The consumer's limits.
const (
	consumerDeadline   = 30 * time.Second
	consumerFetchBatch = 16
	consumerFetchWait  = 5 * time.Second
	retryDelay         = 10 * time.Second
)

// Msg is a delivered message (a jetstream.Msg; a fake in tests).
type Msg interface {
	Subject() string
	Data() []byte
	Ack() error
	NakWithDelay(time.Duration) error
	Term() error
}

// ConsumerMetrics are the consumer's counters.
type ConsumerMetrics struct {
	Handled     atomic.Int64 // entries written
	Duplicates  atomic.Int64 // a row already audited (a redelivery)
	Refused     atomic.Int64 // a malformed subject or body, dropped
	Mismatches  atomic.Int64 // the row disagrees with the message, or isn't scanned
	Retried     atomic.Int64 // a database failure, naked for later
	Unavailable atomic.Int64 // the durable isn't there yet
}

// Consumer is core_documents: it reads documents.v1.<hid>.scanned and, from
// the row (never the message), writes the scan's audit entry once
// (document.ready, document.quarantined or document.scan_failed; design §11,
// §17 departures 24 and 39) and, for a live clearance letter, moves the
// letter from checking to waiting or refused with its clearance entry
// (mhai_clearance_scanned). It runs as the gateway under the subject's
// hospital, with no user: another hospital's document is not found.
type Consumer struct {
	Pool *pgxpool.Pool
	// Letter moves a live clearance letter on from its document's row, in
	// the same transaction (letters.Scanned). Nil leaves letters checking.
	Letter  func(ctx context.Context, tx pgx.Tx, document uuid.UUID) error
	Log     *slog.Logger
	Metrics *ConsumerMetrics
}

func (c *Consumer) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (c *Consumer) metrics() *ConsumerMetrics {
	if c.Metrics == nil {
		c.Metrics = &ConsumerMetrics{}
	}
	return c.Metrics
}

// The scan's outcomes and their reasons (docspb.Scanned).
var scanReasons = map[string]map[string]bool{
	"ready": {"": true},
	"quarantined": {"unsupported_type": true, "too_large": true, "too_many_pages": true, "too_many_pixels": true,
		"password_protected": true, "malware": true, "unreadable": true, "checksum_mismatch": true},
	"failed": {"scan_unavailable": true},
}

// Parse checks a scanned message's subject and body: the hospital from the
// subject, the document and outcome from a body of exactly docspb.Scanned.
func Parse(subject string, data []byte) (hospital uuid.UUID, msg *docspb.Scanned, ok bool) {
	seg := strings.Split(subject, ".")
	if len(seg) != 4 || seg[0] != "documents" || seg[1] != "v1" || seg[3] != "scanned" {
		return uuid.Nil, nil, false
	}
	h, okH := CanonicalID(seg[2])
	var s docspb.Scanned
	if !okH || len(data) > bus.DocsMaxMsgSize || proto.Unmarshal(data, &s) != nil ||
		len(s.ProtoReflect().GetUnknown()) > 0 {
		return uuid.Nil, nil, false
	}
	if _, ok := CanonicalID(s.GetDocumentId()); !ok || !scanReasons[s.GetOutcome()][s.GetReason()] ||
		(s.GetOutcome() == "ready") != (s.GetPages() > 0) || s.GetPages() > 50 {
		return uuid.Nil, nil, false
	}
	return h, &s, true
}

// HandleMsg handles one message: a malformed one is dropped (Term), a
// database failure is naked for a retry, anything else is acked.
func (c *Consumer) HandleMsg(ctx context.Context, m Msg) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), consumerDeadline)
	defer cancel()
	hospital, s, ok := Parse(m.Subject(), m.Data())
	if !ok {
		c.metrics().Refused.Add(1)
		c.log().Warn("documents: a scanned message was refused", "reason", "malformed")
		_ = m.Term()
		return
	}
	doc, _ := uuid.Parse(s.GetDocumentId())
	if err := c.Handle(ctx, hospital, doc, s.GetOutcome()); err != nil {
		c.metrics().Retried.Add(1)
		c.log().Error("documents: a scanned message failed; it will be retried", "hospital_id", hospital.String(),
			"document_id", doc.String(), "reason", "internal_error")
		_ = m.NakWithDelay(retryDelay)
		return
	}
	_ = m.Ack()
}

// Handle records document's scan in hospital, from its row, once.
func (c *Consumer) Handle(ctx context.Context, hospital, document uuid.UUID, said string) error {
	if c.Pool == nil {
		return errors.New("documents: no database")
	}
	return db.WithHospital(ctx, c.Pool, hospital, func(tx pgx.Tx) error {
		var (
			marked, scanned bool
			purpose, status *string
			reason          *string
			pages           *int
		)
		err := tx.QueryRow(ctx, `SELECT marked, purpose, status, reason, pages, scanned FROM documents_scan_audited($1)`,
			document).Scan(&marked, &purpose, &status, &reason, &pages, &scanned)
		if errors.Is(err, pgx.ErrNoRows) {
			// Not this hospital's, or gone: nothing to record.
			c.metrics().Mismatches.Add(1)
			c.log().Warn("documents: a scanned message names no document of its hospital", "hospital_id", hospital.String(),
				"document_id", document.String())
			return nil
		}
		if err != nil {
			return err
		}
		if !scanned {
			c.metrics().Mismatches.Add(1)
			c.log().Warn("documents: a scanned message for a row whose scan hasn't finished", "document_id", document.String())
			return nil
		}
		if marked {
			action, payload := scanEntry(document, *status, reason, pages)
			if outcome := scanOutcome(*status, pages); outcome != said {
				c.metrics().Mismatches.Add(1)
				c.log().Warn("documents: a scanned message disagrees with its row; the row is recorded",
					"document_id", document.String(), "said", said, "row", outcome)
			}
			if err := audit.Write(ctx, tx, audit.Event{HospitalID: hospital.String(), Action: action, Entity: "document",
				EntityID: document.String(), Payload: payload}); err != nil {
				return err
			}
			c.metrics().Handled.Add(1)
		} else {
			c.metrics().Duplicates.Add(1)
		}
		if purpose != nil && *purpose == "clearance" && c.Letter != nil {
			return c.Letter(ctx, tx, document)
		}
		return nil
	})
}

// scanOutcome is the outcome a row records: quarantined and failed are
// their status; anything with pages was ready (a later status or a cancel
// keeps them).
func scanOutcome(status string, pages *int) string {
	switch {
	case status == "quarantined" || status == "failed":
		return status
	case pages != nil:
		return "ready"
	}
	return status
}

// scanEntry is the row's audit entry: ids and codes only.
func scanEntry(document uuid.UUID, status string, reason *string, pages *int) (string, map[string]any) {
	p := map[string]any{"document_id": document.String()}
	r := ""
	if reason != nil {
		r = *reason
	}
	switch scanOutcome(status, pages) {
	case "quarantined":
		p["reason"] = r
		return "document.quarantined", p
	case "failed":
		p["reason"] = r
		return "document.scan_failed", p
	}
	if pages != nil {
		p["pages"] = *pages
	}
	return "document.ready", p
}

// Run pulls core_documents until ctx ends (every core copy shares the
// durable; each message goes to one). The durable is looked up until it is
// found (CORE_DOCUMENTS_ENABLED creates it at start-up).
func (c *Consumer) Run(ctx context.Context, js jetstream.JetStream) error {
	if js == nil || c.Pool == nil {
		return errors.New("documents: consumer unavailable")
	}
	var cons jetstream.Consumer
	for ctx.Err() == nil {
		x, err := js.Consumer(ctx, bus.DocsStream, bus.CoreDocsConsumer)
		if err == nil {
			cons = x
			break
		}
		c.metrics().Unavailable.Add(1)
		c.log().Error("documents: the core_documents durable isn't available; retrying")
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
	}
	for ctx.Err() == nil {
		batch, err := cons.Fetch(consumerFetchBatch, jetstream.FetchMaxWait(consumerFetchWait))
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		for m := range batch.Messages() {
			c.HandleMsg(ctx, m)
		}
	}
	return nil
}
