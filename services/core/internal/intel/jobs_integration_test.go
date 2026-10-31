//go:build integration

package intel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

func verifyJob(t *testing.T, m jobMsg) reqtoken.Claims {
	t.Helper()
	c, err := reqtoken.VerifyAt(m.header.Get("Mhai-Request-Token"), signer.PublicKeys(),
		reqtoken.Expect{Aud: "pipeline", Subject: m.subject, Kind: reqtoken.KindJob}, m.data, time.Now())
	if err != nil {
		t.Fatalf("job token: %v", err)
	}
	return c
}

// The job id is the Nats-Msg-Id, so a second publish of the same job is
// dropped by JetStream; the job goes on its hospital's subject.
func TestPublishJobSetsMsgID(t *testing.T) {
	c, _ := newClient(t)
	drainJobs(t, sharedNATS.PipelineURL)
	hid := uuid.New()
	caller := clinician(hid)
	job := uuid.New()
	deadline := time.Now().Add(5 * time.Minute)
	for i := 0; i < 2; i++ {
		if err := c.PublishJob(context.Background(), caller, "pipeline.run_query", job, &intelpb.RunQueryJob{QueryId: job.String()}, deadline); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	subject := "jobs.v1." + hid.String() + ".run_query"
	got := jobsFor(drainJobs(t, sharedNATS.PipelineURL), subject)
	if len(got) != 1 {
		t.Fatalf("%d messages on %s, want 1 (the duplicate dropped)", len(got), subject)
	}
	m := got[0]
	if m.header.Get("Nats-Msg-Id") != job.String() || m.header.Get("Mhai-Schema") != "mhai.intelligence.v1.RunQueryJob" ||
		m.header.Get("Mhai-Request-Id") != caller.Rid {
		t.Fatalf("headers %v", m.header)
	}
	cl := verifyJob(t, m)
	if cl.Hid != hid.String() || cl.Sub != caller.Sub || cl.Rid != caller.Rid || cl.Via != reqtoken.ViaCore || cl.ReplyPrefix != "" {
		t.Fatalf("claims %+v", cl)
	}
	var body intelpb.RunQueryJob
	if err := proto.Unmarshal(m.data, &body); err != nil || body.QueryId != job.String() {
		t.Fatalf("body %v %v", &body, err)
	}
}

// The token's exp is the job's deadline; a deadline already past, or past
// the operation's limit, is refused before anything is published.
func TestPublishJobDeadlineIsTokenExp(t *testing.T) {
	c, _ := newClient(t)
	drainJobs(t, sharedNATS.PipelineURL)
	hid := uuid.New()
	deadline := time.Now().Truncate(time.Second).Add(7 * time.Minute)
	job := uuid.New()
	if err := c.PublishJob(context.Background(), clinician(hid), "pipeline.run_query", job, &intelpb.RunQueryJob{QueryId: job.String()}, deadline); err != nil {
		t.Fatal(err)
	}
	got := jobsFor(drainJobs(t, sharedNATS.PipelineURL), "jobs.v1."+hid.String()+".run_query")
	if len(got) != 1 {
		t.Fatalf("%d messages", len(got))
	}
	if cl := verifyJob(t, got[0]); !cl.ExpiresAt.Equal(deadline) {
		t.Fatalf("exp %v, want the deadline %v", cl.ExpiresAt, deadline)
	}
	for name, d := range map[string]time.Time{
		"past":           time.Now().Add(-time.Second),
		"under a second": time.Now().Add(300 * time.Millisecond),
	} {
		j := uuid.New()
		if err := c.PublishJob(context.Background(), clinician(hid), "pipeline.run_query", j, &intelpb.RunQueryJob{QueryId: j.String()}, d); err == nil {
			t.Errorf("%s: published", name)
		}
	}
	if err := c.PublishJob(context.Background(), clinician(hid), "pipeline.run_query", uuid.New(), &intelpb.RunQueryJob{},
		time.Now().Add(-time.Minute)); !errors.Is(err, ErrDeadline) {
		t.Errorf("past deadline: %v", err)
	}
	if n := len(drainJobs(t, sharedNATS.PipelineURL)); n != 0 {
		t.Fatalf("%d refused jobs were published", n)
	}
	// A deadline past the operation's 10 minutes (clock skew, a long-lived
	// row) is cut to the limit, not refused.
	j := uuid.New()
	if err := c.PublishJob(context.Background(), clinician(hid), "pipeline.run_query", j, &intelpb.RunQueryJob{QueryId: j.String()},
		time.Now().Add(11*time.Minute)); err != nil {
		t.Fatalf("long deadline: %v", err)
	}
	got = jobsFor(drainJobs(t, sharedNATS.PipelineURL), "jobs.v1."+hid.String()+".run_query")
	if len(got) != 1 {
		t.Fatalf("%d messages", len(got))
	}
	if cl := verifyJob(t, got[0]); cl.ExpiresAt.Sub(cl.IssuedAt) != 10*time.Minute {
		t.Fatalf("lifetime %v, want the operation's 10 minutes", cl.ExpiresAt.Sub(cl.IssuedAt))
	}
}

// Fairness: core publishes a job only on jobs.v1.<the token's hospital>,
// and the hospital comes only from the caller. There is no way to name
// another subject; a caller without a hospital, or with one that isn't a
// canonical uuid, publishes nothing.
func TestPublishJobOnlyOnTokensHospital(t *testing.T) {
	c, _ := newClient(t)
	drainJobs(t, sharedNATS.PipelineURL)
	a, b := uuid.New(), uuid.New()
	for _, hid := range []uuid.UUID{a, b} {
		j := uuid.New()
		if err := c.PublishJob(context.Background(), clinician(hid), "pipeline.run_query", j, &intelpb.RunQueryJob{QueryId: j.String()},
			time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	msgs := drainJobs(t, sharedNATS.PipelineURL)
	if len(msgs) != 2 {
		t.Fatalf("%d messages", len(msgs))
	}
	for _, m := range msgs {
		cl := verifyJob(t, m)
		if m.subject != "jobs.v1."+cl.Hid+".run_query" {
			t.Errorf("subject %s carries a token for hospital %s", m.subject, cl.Hid)
		}
	}
	for name, caller := range map[string]Caller{
		"no hospital":       {Sub: uuid.NewString(), Rid: "r", Roles: []string{authz.RoleClinician}},
		"wildcard hospital": {Sub: uuid.NewString(), Hid: "*", Rid: "r", Roles: []string{authz.RoleClinician}},
		"upper-case uuid":   {Sub: uuid.NewString(), Hid: "A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11", Rid: "r", Roles: []string{authz.RoleClinician}},
		"global":            {Sub: uuid.NewString(), Hid: "global", Rid: "r", Roles: []string{authz.RoleClinician}},
		"svc:core":          System(a.String(), "r"), // run_query has svc_core = no
		"analyst":           {Sub: uuid.NewString(), Hid: a.String(), Rid: "r", Roles: []string{authz.RoleAnalyst}},
	} {
		j := uuid.New()
		if err := c.PublishJob(context.Background(), caller, "pipeline.run_query", j, &intelpb.RunQueryJob{QueryId: j.String()},
			time.Now().Add(time.Minute)); err == nil {
			t.Errorf("%s: published", name)
		}
	}
	// Misuse: a request/reply operation, the wrong message, no job id.
	if err := c.PublishJob(context.Background(), clinician(a), "echo.ping", uuid.New(), &intelpb.PingRequest{}, time.Now().Add(time.Minute)); err == nil {
		t.Error("published a request/reply operation as a job")
	}
	if err := c.PublishJob(context.Background(), clinician(a), "pipeline.run_query", uuid.New(), &intelpb.PingRequest{}, time.Now().Add(time.Minute)); err == nil {
		t.Error("published the wrong message type")
	}
	if err := c.PublishJob(context.Background(), clinician(a), "pipeline.run_query", uuid.Nil, &intelpb.RunQueryJob{}, time.Now().Add(time.Minute)); err == nil {
		t.Error("published with no job id")
	}
	if n := len(drainJobs(t, sharedNATS.PipelineURL)); n != 0 {
		t.Fatalf("%d refused jobs were published", n)
	}
}

// A full per-hospital subject (DiscardNewPerSubject, F3) refuses the
// publish: ErrBackpressure, 503 unavailable. Another hospital still
// publishes. A fresh server, so the 200 waiting jobs bother no other test.
func TestPublishJobBackpressure(t *testing.T) {
	ctx := context.Background()
	n, stop, err := testutil.NewNATS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	nc, js, err := bus.Connect(ctx, n.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{})
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if err := bus.EnsureStreams(ctx, js, bus.StreamConfig{}); err != nil {
		t.Fatal(err)
	}
	c := &Client{NC: nc, JS: js, Signer: signer, Log: logx.New(&logBuf{})}
	full := clinician(uuid.New())
	for i := 0; i < bus.JobsMaxPerSubject; i++ {
		j := uuid.New()
		if err := c.PublishJob(ctx, full, "pipeline.run_query", j, &intelpb.RunQueryJob{QueryId: j.String()}, time.Now().Add(time.Minute)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	j := uuid.New()
	err = c.PublishJob(ctx, full, "pipeline.run_query", j, &intelpb.RunQueryJob{QueryId: j.String()}, time.Now().Add(time.Minute))
	if !errors.Is(err, ErrBackpressure) {
		t.Fatalf("over the per-subject limit: %v", err)
	}
	if code, _ := Code(err); code != "unavailable" {
		t.Fatalf("code %s", code)
	}
	other := clinician(uuid.New())
	if err := c.PublishJob(ctx, other, "pipeline.run_query", j, &intelpb.RunQueryJob{QueryId: j.String()}, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("another hospital: %v", err)
	}
	s, err := js.Stream(ctx, bus.JobsStream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx, jetstream.WithSubjectFilter("jobs.v1.>"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.State.Subjects["jobs.v1."+full.Hid+".run_query"]; got != bus.JobsMaxPerSubject {
		t.Fatalf("full subject holds %d", got)
	}
}
