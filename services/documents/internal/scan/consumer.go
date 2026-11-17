package scan

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
)

// The DOCS stream and the scan's durable (core creates both when
// CORE_DOCUMENTS_ENABLED is on; the documents user may only pull and ack
// documents_scan).
const (
	Stream  = "DOCS"
	Durable = "documents_scan"
	// MaxDeliver is the durable's: past it the message ends and the
	// sweeper picks the row up (it stays scanning or uploaded).
	MaxDeliver = 10
	// AckWait is the durable's; a scan in progress says so every
	// progressEvery, so a long render isn't redelivered.
	AckWait       = 2 * time.Minute
	progressEvery = 30 * time.Second
	// ScanTimeout bounds one scan: inside the 10-minute lease, so a scan
	// that runs long stops before another can take the row over.
	ScanTimeout = 8 * time.Minute
	fetchBatch  = 2
	fetchWait   = 5 * time.Second
)

// Msg is what HandleMsg needs of a JetStream message.
type Msg interface {
	Subject() string
	Data() []byte
	Ack() error
	NakWithDelay(time.Duration) error
	InProgress() error
	Term() error
	Deliveries() uint64
}

// ConsumerMetrics count messages by how they ended.
type ConsumerMetrics struct {
	Refused, Done, Retried, Ended atomic.Int64
}

// Consumer is the documents_scan durable's handler.
type Consumer struct {
	Scanner *Scanner
	Log     *slog.Logger
	Metrics *ConsumerMetrics
}

func (c *Consumer) metrics() *ConsumerMetrics {
	if c.Metrics == nil {
		c.Metrics = &ConsumerMetrics{}
	}
	return c.Metrics
}

// parseUploaded reads documents.v1.<hid>.uploaded and its body: canonical
// uuids only.
func parseUploaded(subject string, data []byte) (uuid.UUID, uuid.UUID, bool) {
	seg := strings.Split(subject, ".")
	if len(seg) != 4 || seg[0] != "documents" || seg[1] != "v1" || seg[3] != "uploaded" {
		return uuid.Nil, uuid.Nil, false
	}
	h, err := uuid.Parse(seg[2])
	if err != nil || h == uuid.Nil || h.String() != seg[2] {
		return uuid.Nil, uuid.Nil, false
	}
	var u docspb.Uploaded
	if len(data) > 256 || proto.Unmarshal(data, &u) != nil {
		return uuid.Nil, uuid.Nil, false
	}
	d, err := uuid.Parse(u.GetDocumentId())
	if err != nil || d == uuid.Nil || d.String() != u.GetDocumentId() {
		return uuid.Nil, uuid.Nil, false
	}
	return h, d, true
}

// Backoff is the delay before the n-th retry: 30 s doubling, at most 10
// minutes.
func Backoff(deliveries uint64) time.Duration {
	d := 30 * time.Second
	for i := uint64(1); i < deliveries && d < 10*time.Minute; i++ {
		d *= 2
	}
	return min(d, 10*time.Minute)
}

// HandleMsg settles one documents.v1.<hid>.uploaded message. A malformed
// subject or body is ended with nothing done (counted). The document is
// read under the subject's hospital, so another hospital's id is nothing
// to scan (DS1). A blocked scan is retried with a back-off; past
// MaxDeliver the message ends and the sweeper takes over.
func (c *Consumer) HandleMsg(ctx context.Context, m Msg) {
	h, d, ok := parseUploaded(m.Subject(), m.Data())
	if !ok {
		c.metrics().Refused.Add(1)
		_ = m.Term()
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ScanTimeout)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(progressEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				_ = m.InProgress()
			}
		}
	}()
	if _, err := c.Scanner.Scan(ctx, h, d); err != nil {
		if m.Deliveries() >= MaxDeliver {
			c.metrics().Ended.Add(1)
			_ = m.Term()
			return
		}
		c.metrics().Retried.Add(1)
		_ = m.NakWithDelay(Backoff(m.Deliveries()))
		return
	}
	c.metrics().Done.Add(1)
	_ = m.Ack()
}

type jsMsg struct{ jetstream.Msg }

func (m jsMsg) Deliveries() uint64 {
	md, err := m.Metadata()
	if err != nil {
		return MaxDeliver
	}
	return md.NumDelivered
}

// Run pulls documents_scan until ctx ends. Until core has made the stream
// (CORE_DOCUMENTS_ENABLED), it waits.
func (c *Consumer) Run(ctx context.Context, js jetstream.JetStream) {
	var cons jetstream.Consumer
	for ctx.Err() == nil {
		k, err := js.Consumer(ctx, Stream, Durable)
		if err == nil {
			cons = k
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
	for ctx.Err() == nil {
		batch, err := cons.Fetch(fetchBatch, jetstream.FetchMaxWait(fetchWait))
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		for m := range batch.Messages() {
			c.HandleMsg(ctx, jsMsg{m})
		}
	}
}

// JSAnnouncer publishes documents.v1.<hid>.scanned on DOCS, de-duplicated
// by "<document_id>:scanned".
type JSAnnouncer struct{ JS jetstream.JetStream }

// Scanned publishes the outcome.
func (a JSAnnouncer) Scanned(ctx context.Context, hospital, document uuid.UUID, outcome, reason string, pages int) error {
	b, err := proto.Marshal(&docspb.Scanned{DocumentId: document.String(), Outcome: outcome, Reason: reason,
		Pages: uint32(max(pages, 0))})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = a.JS.PublishMsg(ctx, &nats.Msg{Subject: "documents.v1." + hospital.String() + ".scanned", Data: b},
		jetstream.WithMsgID(document.String()+":scanned"))
	return err
}

// Sweeper scans rows a lost message left behind (§8), every Every.
type Sweeper struct {
	Due     func(ctx context.Context, limit int) ([]Due, error)
	Scanner *Scanner
	Log     *slog.Logger
	// Every is a minute when zero; Limit is 50 rows a run when zero.
	Every time.Duration
	Limit int
}

// RunOnce scans each due row once; it returns how many it tried.
func (s *Sweeper) RunOnce(ctx context.Context) (int, error) {
	limit := s.Limit
	if limit == 0 {
		limit = 50
	}
	due, err := s.Due(ctx, limit)
	if err != nil {
		return 0, err
	}
	for _, d := range due {
		if ctx.Err() != nil {
			break
		}
		sctx, cancel := context.WithTimeout(ctx, ScanTimeout)
		_, _ = s.Scanner.Scan(sctx, d.Hospital, d.Document)
		cancel()
	}
	return len(due), nil
}

// Run sweeps until ctx ends.
func (s *Sweeper) Run(ctx context.Context) {
	every := s.Every
	if every == 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.RunOnce(ctx); err != nil && ctx.Err() == nil && s.Log != nil {
				s.Log.Warn("documents: the scan sweep failed; it runs again in a minute")
			}
		}
	}
}
