//go:build integration

package documents

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
)

// Over the real nats.conf: core announces uploaded onto DOCS (deduplicated
// by Nats-Msg-Id), and core_documents (Run) pulls a scanned message the
// documents user published and writes its entry once.
func TestAnnounceAndConsumeOverNATS(t *testing.T) {
	w := newWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	n, stop, err := testutil.NewNATS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	core, err := nats.Connect(n.GatewayURL, nats.CustomInboxPrefix(bus.GatewayInboxPrefix))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	js, _ := jetstream.New(core)
	if err := bus.EnsureDocsStream(ctx, js, 1); err != nil {
		t.Fatal(err)
	}
	doc := w.startUploaded(t, w.hA, w.clin)
	a := JSAnnouncer{JS: js}
	for i := 0; i < 2; i++ {
		if err := a.Uploaded(ctx, w.hA, doc); err != nil {
			t.Fatalf("announce: %v", err)
		}
	}
	st, err := js.Stream(ctx, bus.DocsStream)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := st.Info(ctx)
	if info.State.Msgs != 1 {
		t.Fatalf("%d uploaded messages, want 1 (deduplicated)", info.State.Msgs)
	}

	exec(t, w.super, `UPDATE documents SET status = 'ready', pages = 2, sha256 = $2, scanned_at = now(), scan_started_at = now()
		WHERE id = $1`, doc, bytes.Repeat([]byte{4}, 32))
	docs, err := nats.Connect(n.DocumentsURL, nats.CustomInboxPrefix("_INBOX_worker.documents"))
	if err != nil {
		t.Fatal(err)
	}
	defer docs.Close()
	djs, _ := jetstream.New(docs)
	body, _ := proto.Marshal(&docspb.Scanned{DocumentId: doc.String(), Outcome: "ready", Pages: 2})
	if _, err := djs.Publish(ctx, "documents.v1."+w.hA.String()+".scanned", body,
		jetstream.WithMsgID(doc.String()+":scanned")); err != nil {
		t.Fatal(err)
	}
	if _, err := djs.Publish(ctx, "documents.v1."+uuid.NewString()+".scanned", []byte("junk"),
		jetstream.WithMsgID("junk:scanned")); err != nil {
		t.Fatal(err)
	}
	c := &Consumer{Pool: w.app, Metrics: &ConsumerMetrics{}}
	rctx, rcancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(rctx, js) }()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && (c.metrics().Handled.Load() < 1 || c.metrics().Refused.Load() < 1) {
		time.Sleep(200 * time.Millisecond)
	}
	rcancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if c.metrics().Handled.Load() != 1 || c.metrics().Refused.Load() != 1 ||
		w.count(t, `SELECT count(*) FROM audit_log WHERE action = 'document.ready' AND entity_id = $1`, doc.String()) != 1 {
		t.Fatalf("handled %d refused %d", c.metrics().Handled.Load(), c.metrics().Refused.Load())
	}
}
