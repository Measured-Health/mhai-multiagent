//go:build integration

package bus_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

func documentsSvc(t *testing.T) *svc {
	return raw(t, sharedNATS.DocumentsURL, bus.WorkerInbox("documents"))
}

// ADR 0016 (documents design §8, §17 departure 3): in PR 1 the documents
// user answers core's control calls in queue group documents and does
// nothing else: it publishes nothing (not even the subjects later PRs give
// it), reaches no request, job, API, KV, EMR or stream subject, and can't
// read another service's inbox.
func TestDocumentsUserPermissions(t *testing.T) {
	hid := uuid.NewString()
	cannotPublish(t, documentsSvc, "api.v1.clinical", "api.v1.platform", jobSubject("ingest_document"),
		"jobs.v1.global.x", "inference.v1.pipeline.health", "$KV.denylist.x", "$KV.health.x", "$JS.API.INFO",
		"documents.v1.ctl.health", "documents.v1."+hid+".scanned", "documents.v1."+hid+".uploaded",
		"documents.v1."+hid+".page", "documents.v1."+hid+".intake", "emr.v1."+hid+".fetched", "emr.v1.ctl.health",
		"records.v1."+hid+".created", "authz.v1.grant.documents", "dead.v1."+hid+".ingest_document",
		"$JS.API.STREAM.CREATE.DOCS", "$JS.API.STREAM.INFO.JOBS", "$JS.API.CONSUMER.MSG.NEXT.JOBS.pipeline",
		"$JS.API.CONSUMER.MSG.NEXT.EMR.emr_normalise", bus.GatewayInboxPrefix+".x", "_INBOX_worker.emr.x")
	cannotSubscribe(t, documentsSvc, "api.v1.>", "jobs.v1.>", "inference.v1.>", "$KV.>", bus.GatewayInboxPrefix+".>",
		"emr.v1.>", "records.v1.>", "documents.v1.*.uploaded", "documents.v1.*.page", "documents.v1.*.intake",
		"_INBOX_worker.emr.>", "_INBOX_bff.clinical.>", ">",
		// No inbox of its own in PR 1: it asks nothing.
		"_INBOX_worker.documents.>")
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
