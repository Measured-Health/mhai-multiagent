package bus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// The DOCS stream and its first durable (documents design §8; §13 PR 4).
// Core creates them when CORE_DOCUMENTS_ENABLED is on (serve's
// ensureDocuments); the documents user may only pull and ack
// documents_scan. core_documents (core's consumer of scanned) comes with
// PR 5.
const (
	DocsStream = "DOCS"
	// DocsScanConsumer: the documents service's scan, on uploaded.
	DocsScanConsumer = "documents_scan"

	// DocsMaxPerSubject bounds what one hospital can queue per event kind;
	// past it a publish is refused (DiscardNewPerSubject), so one hospital
	// can't fill the stream for the others, and the rows' sweepers retry.
	DocsMaxPerSubject = 10000
	DocsMaxAge        = 7 * 24 * time.Hour
	DocsDuplicates    = 2 * time.Minute // Nats-Msg-Id = "<document_id>:<kind>"
	// DocsMaxMsgSize: ids and a code (docspb.Uploaded, docspb.Scanned),
	// plus headers.
	DocsMaxMsgSize = 4 << 10
	// DocsMaxBytes caps the stream: ids only, so it is small.
	DocsMaxBytes = 16 << 20

	// The scan's durable: a scan says it is in progress every 30 s, and a
	// blocked scan backs off up to 10 times before the documents service's
	// sweeper takes the row over; at most 4 messages in flight (two renders
	// at a time in docsandbox, design §12).
	DocsScanAckWait       = 2 * time.Minute
	DocsScanMaxDeliver    = 10
	DocsScanMaxAckPending = 4
)

// EnsureDocsStream creates DOCS and documents_scan, or updates them to
// this configuration: a work queue on documents.v1.*.uploaded and
// documents.v1.*.scanned, file storage, 7 days, bounded per subject with
// discard-new, duplicates dropped within 2 minutes.
func EnsureDocsStream(ctx context.Context, js jetstream.JetStream, replicas int) error {
	r, err := replicaCount(replicas)
	if err != nil {
		return err
	}
	if js == nil {
		return errors.New("bus: no JetStream")
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:                 DocsStream,
		Subjects:             []string{"documents.v1.*.uploaded", "documents.v1.*.scanned"},
		Storage:              jetstream.FileStorage,
		Retention:            jetstream.WorkQueuePolicy,
		Discard:              jetstream.DiscardNew,
		DiscardNewPerSubject: true,
		MaxMsgsPerSubject:    DocsMaxPerSubject,
		MaxMsgSize:           DocsMaxMsgSize,
		MaxBytes:             DocsMaxBytes,
		MaxAge:               DocsMaxAge,
		Duplicates:           DocsDuplicates,
		Replicas:             r,
	}); err != nil {
		return fmt.Errorf("bus: ensure stream DOCS: %w", err)
	}
	if _, err := js.CreateOrUpdateConsumer(ctx, DocsStream, jetstream.ConsumerConfig{
		Durable:       DocsScanConsumer,
		FilterSubject: "documents.v1.*.uploaded",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       DocsScanAckWait,
		MaxDeliver:    DocsScanMaxDeliver,
		MaxAckPending: DocsScanMaxAckPending,
	}); err != nil {
		return fmt.Errorf("bus: ensure consumer DOCS/%s: %w", DocsScanConsumer, err)
	}
	return nil
}
