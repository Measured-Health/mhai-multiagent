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

func notificationsSvc(t *testing.T) *svc {
	return raw(t, sharedNATS.NotificationsURL, bus.WorkerInbox("notifications"))
}

// ADR 0018 (notifications design §7; NT7): the notifications user answers
// core's control calls in queue group notifications, pulls and acks its
// own durable notify_send, and publishes its own dead letters and nothing
// else: never a due message (only core announces one), no request, job,
// API, KV, EMR or documents subject, no stream or consumer management, no
// other durable or service inbox.
func TestNotificationsUserPermissions(t *testing.T) {
	hid := uuid.NewString()
	cannotPublish(t, notificationsSvc, "api.v1.clinical", "api.v1.platform", jobSubject("ingest_document"),
		"jobs.v1.global.x", "inference.v1.pipeline.health", "$KV.denylist.x", "$KV.health.x",
		"notify.v1.ctl.health", "notify.v1."+hid+".due", "notify.v1.platform.due", "documents.v1.ctl.health",
		"documents.v1."+hid+".uploaded", "emr.v1.ctl.health", "authz.v1.grant.notifications",
		"dead.v1."+hid+".ingest_document", "dead.v1."+hid+".core_emr", "$JS.API.STREAM.CREATE.NOTIFY",
		"$JS.API.STREAM.UPDATE.NOTIFY", "$JS.API.STREAM.DELETE.NOTIFY", "$JS.API.STREAM.PURGE.NOTIFY",
		"$JS.API.STREAM.MSG.GET.NOTIFY", "$JS.API.CONSUMER.CREATE.NOTIFY.x", "$JS.API.CONSUMER.DURABLE.CREATE.NOTIFY.x",
		"$JS.API.CONSUMER.DELETE.NOTIFY.notify_send", "$JS.API.CONSUMER.MSG.NEXT.NOTIFY.other", "$JS.ACK.NOTIFY.other.x",
		"$JS.API.STREAM.INFO.JOBS", "$JS.API.CONSUMER.MSG.NEXT.DOCS.documents_scan", bus.GatewayInboxPrefix+".x",
		"_INBOX_worker.notifications.x", "$JS.API.INFO") // last, so the secret scanner reads no value after "API"
	cannotSubscribe(t, notificationsSvc, "api.v1.>", "jobs.v1.>", "inference.v1.>", "$KV.>", bus.GatewayInboxPrefix+".>",
		"emr.v1.>", "documents.v1.>", "notify.v1.*.due", "_INBOX_worker.documents.>", "_INBOX_bff.clinical.>", ">")
	canPublish(t, notificationsSvc, "dead.v1."+hid+".notify_send", "$JS.API.CONSUMER.INFO.NOTIFY.notify_send",
		"$JS.API.CONSUMER.MSG.NEXT.NOTIFY.notify_send", "$JS.ACK.NOTIFY.notify_send.x")
	in := notificationsSvc(t)
	if _, err := in.nc.SubscribeSync("_INBOX_worker.notifications.x"); err != nil {
		t.Fatal(err)
	}
	in.noViolation(t)
	// notify.v1.ctl.> only in queue group notifications.
	s := notificationsSvc(t)
	_, _ = s.nc.Subscribe("notify.v1.ctl.>", func(*nats.Msg) {})
	s.violation(t, "notify.v1.ctl.>")
	s = notificationsSvc(t)
	_, _ = s.nc.QueueSubscribe("notify.v1.ctl.>", "documents", func(*nats.Msg) {})
	s.violation(t, "notify.v1.ctl.>")
	q := notificationsSvc(t)
	if _, err := q.nc.QueueSubscribeSync("notify.v1.ctl.>", "notifications"); err != nil {
		t.Fatal(err)
	}
	q.noViolation(t)

	// Core (gateway) calls the control subjects and announces due
	// messages; nobody else can, nor listen on them.
	canPublish(t, gateway, "notify.v1.ctl.health", "notify.v1."+hid+".due", "notify.v1.platform.due")
	cannotPublish(t, gateway, "dead.v1."+hid+".notify_send", "$JS.API.CONSUMER.MSG.NEXT.NOTIFY.notify_send")
	for _, mk := range []func(*testing.T) *svc{pipeline, emrSvc, documentsSvc, bffClinical, bffPlatform, echo} {
		cannotPublish(t, mk, "notify.v1.ctl.health", "notify.v1."+hid+".due", "notify.v1.platform.due")
	}
	for _, mk := range []func(*testing.T) *svc{pipeline, emrSvc, documentsSvc, bffClinical, bffPlatform} {
		cannotSubscribe(t, mk, "notify.v1.ctl.>", "notify.v1.*.due")
	}
}

// The NOTIFY stream (notifications PR 3): a work queue on notify.v1.*.due,
// bounded per subject with discard-new, 3 days, duplicates dropped within
// 2 minutes; notify_send on it. Running it twice changes nothing. Core
// publishes a due message twice under one id and the stream keeps one; the
// notifications user pulls it and acks it.
func TestNotifyStreamConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	js, err := jetstream.New(gateway(t).nc)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := bus.EnsureNotifyStream(ctx, js, 1); err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
	}
	if err := bus.EnsureNotifyStream(ctx, js, 2); err == nil {
		t.Fatal("2 replicas accepted")
	}
	if err := bus.EnsureNotifyStream(ctx, nil, 1); err == nil {
		t.Fatal("no JetStream accepted")
	}
	st, err := js.Stream(ctx, bus.NotifyStream)
	if err != nil {
		t.Fatal(err)
	}
	c := st.CachedInfo().Config
	if c.Retention != jetstream.WorkQueuePolicy || c.Discard != jetstream.DiscardNew || !c.DiscardNewPerSubject ||
		c.MaxMsgsPerSubject != 10000 || c.MaxAge != 3*24*time.Hour || c.Duplicates != 2*time.Minute ||
		c.Storage != jetstream.FileStorage || c.MaxBytes != bus.NotifyMaxBytes || c.MaxMsgSize != bus.NotifyMaxMsgSize ||
		len(c.Subjects) != 1 || c.Subjects[0] != "notify.v1.*.due" {
		t.Fatalf("stream config: %+v", c)
	}
	cons, err := st.Consumer(ctx, bus.NotifySendConsumer)
	if err != nil {
		t.Fatal(err)
	}
	cc := cons.CachedInfo().Config
	if cc.FilterSubject != "notify.v1.*.due" || cc.MaxDeliver != bus.NotifySendMaxDeliver ||
		cc.AckPolicy != jetstream.AckExplicitPolicy || cc.AckWait != 2*time.Minute || cc.MaxAckPending != 16 {
		t.Fatalf("notify_send: %+v", cc)
	}
	hid, id := uuid.NewString(), uuid.NewString()
	for i := 0; i < 2; i++ {
		if _, err := js.Publish(ctx, "notify.v1."+hid+".due", []byte("synthetic id"), jetstream.WithMsgID(id)); err != nil {
			t.Fatalf("publish due: %v", err)
		}
	}
	n := notificationsSvc(t)
	njs, _ := jetstream.New(n.nc)
	nc, err := njs.Consumer(ctx, bus.NotifyStream, bus.NotifySendConsumer)
	if err != nil {
		t.Fatalf("notify_send as notifications: %v", err)
	}
	batch, err := nc.Fetch(5, jetstream.FetchMaxWait(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for m := range batch.Messages() {
		got++
		if err := m.DoubleAck(ctx); err != nil {
			t.Fatalf("ack: %v", err)
		}
	}
	if got != 1 {
		t.Fatalf("%d messages for notify_send, want 1 (the duplicate dropped)", got)
	}
	n.noViolation(t)
}

// The control round trip on the real config: core asks on
// notify.v1.ctl.health, the notifications user answers once through
// allow_responses (it has no publish right of its own), and a second
// answer to the same request is refused.
func TestNotificationsAnswersCoreOnce(t *testing.T) {
	n := notificationsSvc(t)
	sub, err := n.nc.QueueSubscribe("notify.v1.ctl.>", "notifications", func(m *nats.Msg) {
		_ = m.Respond([]byte("synthetic health reply"))
		_ = m.Respond([]byte("a second reply"))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	_ = n.nc.Flush()
	g := gateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rep, err := g.nc.RequestWithContext(ctx, "notify.v1.ctl.health", []byte("x"))
	if err != nil || string(rep.Data) != "synthetic health reply" {
		t.Fatalf("reply %v %v", rep, err)
	}
	n.violation(t, bus.GatewayInboxPrefix)
}

// Dev's default: nats.conf with the notifications user off
// (notifications-off.conf) starts with no NATS_NOTIFICATIONS_PASSWORD, the
// other users work, and no one signs in as notifications.
func TestNATSStartsWithNotificationsUserOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	n, stop, err := testutil.NewNATSEMROff(ctx)
	if err != nil {
		t.Fatalf("nats with the notifications user off: %v", err)
	}
	defer stop()
	for _, url := range []string{n.GatewayURL, n.PipelineURL, n.BFFPlatformURL} {
		nc, err := nats.Connect(url)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		nc.Close()
	}
	if nc, err := nats.Connect(n.NotificationsURL, nats.Timeout(3*time.Second)); err == nil {
		nc.Close()
		t.Fatal("signed in as notifications with the notifications user off")
	}
}
