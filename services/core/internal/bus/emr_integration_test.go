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

func emrSvc(t *testing.T) *svc { return raw(t, sharedNATS.EMRURL, bus.WorkerInbox("emr")) }

// EMR design §3.2: the emr user publishes only its ids-only events and dead
// letters, pulls only its own two durables, and reaches nothing else: no
// requests, jobs, API, KV, or stream and consumer management.
func TestEMRUserPermissions(t *testing.T) {
	hid := uuid.NewString()
	cannotPublish(t, emrSvc, "api.v1.clinical", "api.v1.platform", jobSubject("run_query"), "jobs.v1.global.x",
		"inference.v1.pipeline.health", "inference.v1.echo.ping", "$KV.denylist.x", "$KV.health.x", "$JS.API.INFO",
		"emr.v1.ctl.health", "authz.v1.grant.emr", "dead.v1."+hid+".run_query",
		"$JS.API.STREAM.CREATE.EMR", "$JS.API.STREAM.UPDATE.EMR", "$JS.API.STREAM.DELETE.EMR", "$JS.API.STREAM.PURGE.EMR",
		"$JS.API.STREAM.INFO.EMR", "$JS.API.STREAM.MSG.GET.EMR",
		"$JS.API.CONSUMER.CREATE.EMR.x", "$JS.API.CONSUMER.DURABLE.CREATE.EMR.x", "$JS.API.CONSUMER.DELETE.EMR.emr_rules",
		"$JS.API.CONSUMER.MSG.NEXT.EMR.core_emr", "$JS.ACK.EMR.core_emr.x", "$JS.API.CONSUMER.INFO.EMR.core_emr",
		"$JS.API.CONSUMER.MSG.NEXT.JOBS.pipeline", "$JS.API.CONSUMER.MSG.NEXT.DEAD.core_dead")
	cannotSubscribe(t, emrSvc, "api.v1.>", "jobs.v1.>", "inference.v1.>", "$KV.>", bus.GatewayInboxPrefix+".>",
		"emr.v1.>", "records.v1.>")
	// emr.v1.ctl.> only in queue group emr.
	s := emrSvc(t)
	_, _ = s.nc.Subscribe("emr.v1.ctl.>", func(*nats.Msg) {})
	s.violation(t, "emr.v1.ctl.>")
	s = emrSvc(t)
	_, _ = s.nc.QueueSubscribe("emr.v1.ctl.>", "pipeline", func(*nats.Msg) {})
	s.violation(t, "emr.v1.ctl.>")
	q := emrSvc(t)
	if _, err := q.nc.QueueSubscribeSync("emr.v1.ctl.>", "emr"); err != nil {
		t.Fatal(err)
	}
	q.noViolation(t)
	canPublish(t, emrSvc, "emr.v1."+hid+".fetched", "emr.v1."+hid+".finding", "emr.v1."+hid+".signal",
		"records.v1."+hid+".created", "dead.v1."+hid+".emr_normalise", "dead.v1."+hid+".emr_rules")

	// Core (gateway) calls the control subjects; the workers can't.
	canPublish(t, gateway, "emr.v1.ctl.test", "emr.v1.ctl.health")
	cannotPublish(t, pipeline, "emr.v1.ctl.test")
	cannotPublish(t, bffPlatform, "emr.v1.ctl.test")
	// Nobody but the emr user publishes the event subjects.
	cannotPublish(t, gateway, "records.v1."+hid+".created", "emr.v1."+hid+".finding")
	cannotPublish(t, pipeline, "emr.v1."+hid+".fetched", "records.v1."+hid+".created")
}

// The EMR stream (plan Task 5): bounded per subject with discard-new, 7
// days, duplicates dropped within 2 minutes; three durables, each with
// MaxDeliver 5. Running it twice changes nothing.
func TestEMRStreamConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	js, err := jetstream.New(gateway(t).nc)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := bus.EnsureEMRStream(ctx, js, 1); err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
	}
	st, err := js.Stream(ctx, bus.EMRStream)
	if err != nil {
		t.Fatal(err)
	}
	c := st.CachedInfo().Config
	if c.Retention != jetstream.WorkQueuePolicy || c.Discard != jetstream.DiscardNew || !c.DiscardNewPerSubject ||
		c.MaxMsgsPerSubject != 50000 || c.MaxAge != 7*24*time.Hour || c.Duplicates != 2*time.Minute ||
		c.Storage != jetstream.FileStorage || c.MaxBytes != bus.EMRMaxBytes || c.MaxMsgSize != bus.EMRMaxMsgSize {
		t.Fatalf("stream config: %+v", c)
	}
	want := map[string][]string{
		bus.EMRNormaliseConsumer: {"emr.v1.*.fetched"},
		bus.EMRRulesConsumer:     {"records.v1.*.created"},
		bus.CoreEMRConsumer:      {"emr.v1.*.finding", "emr.v1.*.signal"},
	}
	for name, filters := range want {
		cons, err := st.Consumer(ctx, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		cc := cons.CachedInfo().Config
		got := cc.FilterSubjects
		if cc.FilterSubject != "" {
			got = []string{cc.FilterSubject}
		}
		if len(got) != len(filters) || cc.MaxDeliver != 5 || cc.AckPolicy != jetstream.AckExplicitPolicy {
			t.Errorf("%s: %+v", name, cc)
			continue
		}
		for i := range filters {
			if got[i] != filters[i] {
				t.Errorf("%s filters %v, want %v", name, got, filters)
			}
		}
	}
	if err := bus.EnsureEMRStream(ctx, js, 2); err == nil {
		t.Fatal("2 replicas accepted")
	}

	// The emr user pulls its own durable: a published record reaches
	// emr_rules, and acks.
	e := emrSvc(t)
	ejs, _ := jetstream.New(e.nc)
	subj := "records.v1." + uuid.NewString() + ".created"
	if _, err := ejs.Publish(ctx, subj, []byte("synthetic ids")); err != nil {
		t.Fatalf("emr publish: %v", err)
	}
	cons, err := ejs.Consumer(ctx, bus.EMRStream, bus.EMRRulesConsumer)
	if err != nil {
		t.Fatalf("emr_rules as emr: %v", err)
	}
	found := false
	for !found {
		batch, err := cons.Fetch(1, jetstream.FetchMaxWait(3*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for m := range batch.Messages() {
			n++
			found = found || m.Subject() == subj
			if err := m.DoubleAck(ctx); err != nil {
				t.Fatalf("ack: %v", err)
			}
		}
		if n == 0 {
			t.Fatal("no message for emr_rules")
		}
	}
	e.noViolation(t)
}

// Dev's default: nats.conf with the emr user off (emr-off.conf) starts with
// no NATS_EMR_PASSWORD, every other user works, and no one signs in as emr.
func TestNATSStartsWithEMRUserOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	n, stop, err := testutil.NewNATSEMROff(ctx)
	if err != nil {
		t.Fatalf("nats with the emr user off: %v", err)
	}
	defer stop()
	for _, url := range []string{n.GatewayURL, n.PipelineURL, n.BFFPlatformURL} {
		nc, err := nats.Connect(url)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		nc.Close()
	}
	if nc, err := nats.Connect(n.EMRURL, nats.Timeout(3*time.Second)); err == nil {
		nc.Close()
		t.Fatal("signed in as emr with the emr user off")
	}
}
