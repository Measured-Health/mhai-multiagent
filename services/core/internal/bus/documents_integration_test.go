//go:build integration

package bus_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

func documentsSvc(t *testing.T) *svc {
	return raw(t, sharedNATS.DocumentsURL, bus.WorkerInbox("documents"))
}

// ADR 0016 (documents design §8, §17 departure 3): the documents user
// answers core's control calls in queue group documents, and from PR 4
// publishes scanned, pulls and acks documents_scan and reads its own
// inbox. Nothing else: no uploaded (core's and the emr service's), no
// request, job, API, KV or EMR subject, no stream or consumer management,
// no other durable, no other service's inbox.
func TestDocumentsUserPermissions(t *testing.T) {
	hid := uuid.NewString()
	cannotPublish(t, documentsSvc, "api.v1.clinical", "api.v1.platform", jobSubject("ingest_document"),
		"jobs.v1.global.x", "inference.v1.pipeline.health", "$KV.denylist.x", "$KV.health.x", "$JS.API.INFO",
		"documents.v1.ctl.health", "documents.v1."+hid+".uploaded",
		"documents.v1."+hid+".page", "documents.v1."+hid+".intake", "emr.v1."+hid+".fetched", "emr.v1.ctl.health",
		"records.v1."+hid+".created", "authz.v1.grant.documents", "dead.v1."+hid+".ingest_document",
		"$JS.API.STREAM.CREATE.DOCS", "$JS.API.STREAM.UPDATE.DOCS", "$JS.API.STREAM.DELETE.DOCS",
		"$JS.API.STREAM.PURGE.DOCS", "$JS.API.STREAM.MSG.GET.DOCS", "$JS.API.CONSUMER.CREATE.DOCS.x",
		"$JS.API.CONSUMER.DURABLE.CREATE.DOCS.x", "$JS.API.CONSUMER.DELETE.DOCS.documents_scan",
		"$JS.API.CONSUMER.MSG.NEXT.DOCS.core_documents", "$JS.ACK.DOCS.core_documents.x",
		"$JS.API.STREAM.INFO.JOBS", "$JS.API.CONSUMER.MSG.NEXT.JOBS.pipeline",
		"$JS.API.CONSUMER.MSG.NEXT.EMR.emr_normalise", bus.GatewayInboxPrefix+".x", "_INBOX_worker.emr.x")
	cannotSubscribe(t, documentsSvc, "api.v1.>", "jobs.v1.>", "inference.v1.>", "$KV.>", bus.GatewayInboxPrefix+".>",
		"emr.v1.>", "records.v1.>", "documents.v1.*.uploaded", "documents.v1.*.scanned", "documents.v1.*.page",
		"documents.v1.*.intake", "_INBOX_worker.emr.>", "_INBOX_bff.clinical.>", ">")
	canPublish(t, documentsSvc, "documents.v1."+hid+".scanned", "$JS.API.CONSUMER.INFO.DOCS.documents_scan",
		"$JS.API.CONSUMER.MSG.NEXT.DOCS.documents_scan", "$JS.ACK.DOCS.documents_scan.x")
	in := documentsSvc(t)
	if _, err := in.nc.SubscribeSync("_INBOX_worker.documents.x"); err != nil {
		t.Fatal(err)
	}
	in.noViolation(t)
	// documents.v1.ctl.> only in queue group documents.
	s := documentsSvc(t)
	_, _ = s.nc.Subscribe("documents.v1.ctl.>", func(*nats.Msg) {})
	s.violation(t, "documents.v1.ctl.>")
	s = documentsSvc(t)
	_, _ = s.nc.QueueSubscribe("documents.v1.ctl.>", "emr", func(*nats.Msg) {})
	s.violation(t, "documents.v1.ctl.>")
	q := documentsSvc(t)
	if _, err := q.nc.QueueSubscribeSync("documents.v1.ctl.>", "documents"); err != nil {
		t.Fatal(err)
	}
	q.noViolation(t)

	// Core (gateway) calls the control subjects; nobody else can.
	canPublish(t, gateway, "documents.v1.ctl.health")
	cannotPublish(t, pipeline, "documents.v1.ctl.health")
	cannotPublish(t, emrSvc, "documents.v1.ctl.health")
	cannotPublish(t, bffClinical, "documents.v1.ctl.health")
	cannotPublish(t, bffPlatform, "documents.v1.ctl.health")
	cannotPublish(t, echo, "documents.v1.ctl.health")
	// Only the documents user announces a scan; core announces an upload.
	cannotPublish(t, gateway, "documents.v1."+hid+".scanned")
	canPublish(t, gateway, "documents.v1."+hid+".uploaded")
	cannotPublish(t, pipeline, "documents.v1."+hid+".scanned", "documents.v1."+hid+".uploaded")
	cannotPublish(t, emrSvc, "documents.v1."+hid+".scanned")
	// Nor may anyone else listen on them.
	for _, mk := range []func(*testing.T) *svc{pipeline, emrSvc, bffClinical, bffPlatform} {
		cannotSubscribe(t, mk, "documents.v1.ctl.>")
	}
}

// The control round trip on the real config: core asks on
// documents.v1.ctl.health, the documents user answers once through
// allow_responses (it has no publish right of its own), and a second
// answer to the same request is refused.
func TestDocumentsAnswersCoreOnce(t *testing.T) {
	d := documentsSvc(t)
	sub, err := d.nc.QueueSubscribe("documents.v1.ctl.>", "documents", func(m *nats.Msg) {
		_ = m.Respond([]byte("synthetic health reply"))
		_ = m.Respond([]byte("a second reply"))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	_ = d.nc.Flush()
	g := gateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rep, err := g.nc.RequestWithContext(ctx, "documents.v1.ctl.health", []byte("x"))
	if err != nil || string(rep.Data) != "synthetic health reply" {
		t.Fatalf("reply %v %v", rep, err)
	}
	d.violation(t, bus.GatewayInboxPrefix)
}

// Dev's default: nats.conf with the documents user off
// (documents-off.conf) starts with no NATS_DOCUMENTS_PASSWORD, the other
// users work, and no one signs in as documents.
func TestNATSStartsWithDocumentsUserOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	n, stop, err := testutil.NewNATSEMROff(ctx)
	if err != nil {
		t.Fatalf("nats with the documents user off: %v", err)
	}
	defer stop()
	for _, url := range []string{n.GatewayURL, n.PipelineURL, n.BFFPlatformURL} {
		nc, err := nats.Connect(url)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		nc.Close()
	}
	if nc, err := nats.Connect(n.DocumentsURL, nats.Timeout(3*time.Second)); err == nil {
		nc.Close()
		t.Fatal("signed in as documents with the documents user off")
	}
}

// The DOCS stream (documents PR 4): a work queue on uploaded and scanned,
// bounded per subject with discard-new, 7 days, duplicates dropped within
// 2 minutes; documents_scan on uploaded only. Running it twice changes
// nothing. The documents user pulls an uploaded message, acks it, and
// publishes scanned onto the stream.
func TestDocsStreamConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	js, err := jetstream.New(gateway(t).nc)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := bus.EnsureDocsStream(ctx, js, 1); err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
	}
	if err := bus.EnsureDocsStream(ctx, js, 2); err == nil {
		t.Fatal("2 replicas accepted")
	}
	st, err := js.Stream(ctx, bus.DocsStream)
	if err != nil {
		t.Fatal(err)
	}
	c := st.CachedInfo().Config
	if c.Retention != jetstream.WorkQueuePolicy || c.Discard != jetstream.DiscardNew || !c.DiscardNewPerSubject ||
		c.MaxMsgsPerSubject != 10000 || c.MaxAge != 7*24*time.Hour || c.Duplicates != 2*time.Minute ||
		c.Storage != jetstream.FileStorage || c.MaxBytes != bus.DocsMaxBytes || c.MaxMsgSize != bus.DocsMaxMsgSize ||
		len(c.Subjects) != 2 || c.Subjects[0] != "documents.v1.*.uploaded" || c.Subjects[1] != "documents.v1.*.scanned" {
		t.Fatalf("stream config: %+v", c)
	}
	cons, err := st.Consumer(ctx, bus.DocsScanConsumer)
	if err != nil {
		t.Fatal(err)
	}
	cc := cons.CachedInfo().Config
	if cc.FilterSubject != "documents.v1.*.uploaded" || cc.MaxDeliver != 10 || cc.AckPolicy != jetstream.AckExplicitPolicy ||
		cc.AckWait != 2*time.Minute || cc.MaxAckPending != 4 {
		t.Fatalf("documents_scan: %+v", cc)
	}

	// Core announces an upload (as it will from PR 5); the documents user
	// pulls it from documents_scan, acks, and announces the scan.
	hid := uuid.NewString()
	if _, err := js.Publish(ctx, "documents.v1."+hid+".uploaded", []byte("synthetic ids"),
		jetstream.WithMsgID("synthetic:uploaded")); err != nil {
		t.Fatalf("publish uploaded: %v", err)
	}
	d := documentsSvc(t)
	djs, _ := jetstream.New(d.nc)
	dc, err := djs.Consumer(ctx, bus.DocsStream, bus.DocsScanConsumer)
	if err != nil {
		t.Fatalf("documents_scan as documents: %v", err)
	}
	batch, err := dc.Fetch(1, jetstream.FetchMaxWait(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for m := range batch.Messages() {
		n++
		if err := m.DoubleAck(ctx); err != nil {
			t.Fatalf("ack: %v", err)
		}
	}
	if n != 1 {
		t.Fatalf("%d messages for documents_scan", n)
	}
	if _, err := djs.Publish(ctx, "documents.v1."+hid+".scanned", []byte("synthetic ids"),
		jetstream.WithMsgID("synthetic:scanned")); err != nil {
		t.Fatalf("publish scanned: %v", err)
	}
	d.noViolation(t)
}
