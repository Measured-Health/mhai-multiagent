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

func notificationsSvc(t *testing.T) *svc {
	return raw(t, sharedNATS.NotificationsURL, bus.WorkerInbox("notifications"))
}

// ADR 0018 (notifications design §7; NT7): in PR 2 the notifications user
// answers core's control calls in queue group notifications and nothing
// else: no publish of its own (no due message, no request, job, API, KV,
// EMR or documents subject, no stream or consumer management), no inbox,
// no other service's subjects.
func TestNotificationsUserPermissions(t *testing.T) {
	hid := uuid.NewString()
	cannotPublish(t, notificationsSvc, "api.v1.clinical", "api.v1.platform", jobSubject("ingest_document"),
		"jobs.v1.global.x", "inference.v1.pipeline.health", "$KV.denylist.x", "$KV.health.x",
		"notify.v1.ctl.health", "notify.v1."+hid+".due", "notify.v1.platform.due", "documents.v1.ctl.health",
		"documents.v1."+hid+".uploaded", "emr.v1.ctl.health", "authz.v1.grant.notifications",
		"dead.v1."+hid+".notify_send", "$JS.API.STREAM.CREATE.NOTIFY", "$JS.API.CONSUMER.CREATE.NOTIFY.x",
		"$JS.API.STREAM.INFO.JOBS", bus.GatewayInboxPrefix+".x", "_INBOX_worker.notifications.x",
		"$JS.API.INFO") // last, so the secret scanner reads no value after "API"
	cannotSubscribe(t, notificationsSvc, "api.v1.>", "jobs.v1.>", "inference.v1.>", "$KV.>", bus.GatewayInboxPrefix+".>",
		"emr.v1.>", "documents.v1.>", "notify.v1.*.due", "_INBOX_worker.notifications.>", "_INBOX_bff.clinical.>", ">")
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

	// Core (gateway) calls the control subjects; nobody else can, nor
	// listen on them.
	canPublish(t, gateway, "notify.v1.ctl.health")
	for _, mk := range []func(*testing.T) *svc{pipeline, emrSvc, documentsSvc, bffClinical, bffPlatform, echo} {
		cannotPublish(t, mk, "notify.v1.ctl.health")
	}
	for _, mk := range []func(*testing.T) *svc{pipeline, emrSvc, documentsSvc, bffClinical, bffPlatform} {
		cannotSubscribe(t, mk, "notify.v1.ctl.>")
	}
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
