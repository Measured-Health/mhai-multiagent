//go:build integration

package intel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
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

// seedCorrection adds a correction to a fresh query of hid: submitted by
// submitter, approved by decider (nil: still waiting).
func seedCorrection(t *testing.T, hid, submitter uuid.UUID, decider uuid.UUID) uuid.UUID {
	t.Helper()
	q := seedQuery(t, hid, submitter, time.Minute, nil, true)
	id := uuid.New()
	if _, err := super.Exec(context.Background(), `INSERT INTO feedback_events (id, hospital_id, query_id, submitted_by, action, status, reason, corrected_diagnosis)
		VALUES ($1, $2, $3, $4, 'correct', 'pending_review', 'synthetic reason', 'synthetic diagnosis')`, id, hid, q, submitter); err != nil {
		t.Fatal(err)
	}
	if decider != uuid.Nil {
		decide(t, hid, decider, id, `UPDATE feedback_events SET status = 'approved', decided_by = $2, decided_at = now() WHERE id = $1`)
	}
	return id
}

// decide runs q ($1 the correction, $2 the user) as the gateway for hid with
// mhai.user_id set to user, as the decision endpoint does.
func decide(t *testing.T, hid, user, id uuid.UUID, q string) {
	t.Helper()
	err := db.WithHospitalUser(context.Background(), app, hid, user, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), q, id, user)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func approveJob(correction uuid.UUID) (uuid.UUID, *intelpb.ApproveCorrectionJob) {
	job := uuid.New()
	return job, &intelpb.ApproveCorrectionJob{JobId: job.String(), CorrectionId: correction.String()}
}

func callerOf(hid, sub uuid.UUID) Caller {
	return Caller{Sub: sub.String(), Hid: hid.String(), Rid: "rid-" + uuid.NewString(), Roles: []string{authz.RoleClinician}}
}

// C28: PublishJob checks the two-person rule against the database before it
// mints anything. The job goes out only for the clinician who decided the
// correction and is not its submitter.
func TestPublishJobRefusesSecondPersonMismatch(t *testing.T) {
	c, _ := newClient(t)
	c.Pool = app
	drainJobs(t, sharedNATS.PipelineURL)
	hid := seedHospital(t, "active")
	submitter := seedUser(t, hid, true, authz.RoleClinician)
	decider := seedUser(t, hid, true, authz.RoleClinician)
	bystander := seedUser(t, hid, true, authz.RoleClinician)
	approved := seedCorrection(t, hid, submitter, decider)
	waiting := seedCorrection(t, hid, submitter, uuid.Nil)
	deadline := time.Now().Add(5 * time.Minute)
	other := seedHospital(t, "active")
	foreign := seedUser(t, other, true, authz.RoleClinician)

	for name, tc := range map[string]struct {
		caller     Caller
		correction uuid.UUID
	}{
		"the submitter":                      {callerOf(hid, submitter), approved},
		"the submitter of a waiting one":     {callerOf(hid, submitter), waiting},
		"a clinician who didn't decide":      {callerOf(hid, bystander), approved},
		"decided_by is null":                 {callerOf(hid, decider), waiting},
		"a correction that doesn't exist":    {callerOf(hid, decider), uuid.New()},
		"another hospital's clinician":       {callerOf(other, foreign), approved},
		"another hospital's clinician, mine": {callerOf(other, decider), approved},
	} {
		job, msg := approveJob(tc.correction)
		if err := c.PublishJob(context.Background(), tc.caller, "pipeline.approve_correction", job, msg, deadline); !errors.Is(err, ErrSecondPerson) {
			t.Errorf("%s: %v, want ErrSecondPerson", name, err)
		}
	}
	subject := "jobs.v1." + hid.String() + ".approve_correction"
	if n := waitingOn(t, subject); n != 0 {
		t.Fatalf("%d refused jobs were published", n)
	}
	if code, _ := Code(ErrSecondPerson); code != "forbidden" {
		t.Errorf("ErrSecondPerson maps to %q", code)
	}

	// The decider, who isn't the submitter, publishes.
	job, msg := approveJob(approved)
	if err := c.PublishJob(context.Background(), callerOf(hid, decider), "pipeline.approve_correction", job, msg, deadline); err != nil {
		t.Fatalf("the decider: %v", err)
	}
	if n := waitingOn(t, subject); n != 1 {
		t.Fatalf("%d jobs on %s, want 1", n, subject)
	}
}

// waitingOn is how many jobs wait on subject in JOBS. (The pipeline
// consumer's filter is run_query only, so these are not drained.)
func waitingOn(t *testing.T, subject string) uint64 {
	t.Helper()
	ctx := context.Background()
	st, err := gwJS.Stream(ctx, bus.JobsStream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := st.Info(ctx, jetstream.WithSubjectFilter(subject))
	if err != nil {
		t.Fatal(err)
	}
	return info.State.Subjects[subject]
}

// A rejected correction has a decider too; it is not an approval.
func TestPublishJobSecondPersonNeedsApproved(t *testing.T) {
	c, _ := newClient(t)
	c.Pool = app
	hid := seedHospital(t, "active")
	submitter := seedUser(t, hid, true, authz.RoleClinician)
	decider := seedUser(t, hid, true, authz.RoleClinician)
	id := seedCorrection(t, hid, submitter, uuid.Nil)
	decide(t, hid, decider, id, `UPDATE feedback_events SET status = 'rejected', decided_by = $2, decided_at = now(), rejection_code = 'unsafe' WHERE id = $1`)
	job, msg := approveJob(id)
	if err := c.PublishJob(context.Background(), callerOf(hid, decider), "pipeline.approve_correction", job, msg, time.Now().Add(time.Minute)); !errors.Is(err, ErrSecondPerson) {
		t.Fatalf("a rejected correction: %v", err)
	}
}

// With no pool the check can't run, and the job is refused.
func TestPublishJobSecondPersonNoPool(t *testing.T) {
	c, _ := newClient(t)
	hid := seedHospital(t, "active")
	job, msg := approveJob(uuid.New())
	if err := c.PublishJob(context.Background(), callerOf(hid, uuid.New()), "pipeline.approve_correction", job, msg, time.Now().Add(time.Minute)); !errors.Is(err, ErrMisuse) {
		t.Fatalf("got %v", err)
	}
}
