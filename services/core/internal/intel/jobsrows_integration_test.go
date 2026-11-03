//go:build integration

package intel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
)

// Plan 4 Task 6: Plan 3's loops (sweeper, deadline expiry, dead letters,
// drainer) widened to the generic jobs table. Every id is synthetic.

// recJS records what JetStream accepted (the message after the publish,
// with its Nats-Msg-Id header).
type recJS struct {
	jetstream.JetStream
	mu   sync.Mutex
	msgs []*nats.Msg
}

func (r *recJS) PublishMsg(ctx context.Context, m *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	ack, err := r.JetStream.PublishMsg(ctx, m, opts...)
	if err == nil {
		r.mu.Lock()
		r.msgs = append(r.msgs, m)
		r.mu.Unlock()
	}
	return ack, err
}

func (r *recJS) on(subject string) []jobMsg {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []jobMsg
	for _, m := range r.msgs {
		if m.Subject == subject {
			out = append(out, jobMsg{subject: m.Subject, header: m.Header, data: m.Data})
		}
	}
	return out
}

func approveSubjectOf(hid uuid.UUID) string { return "jobs.v1." + hid.String() + ".approve_correction" }

// seedJob adds a queued approve_correction row for correction as the
// superuser: created age ago by createdBy, with deadline, published or not.
func seedJob(t *testing.T, hid, correction, createdBy uuid.UUID, age time.Duration, deadline time.Time, published bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var pub any
	if published {
		pub = time.Now()
	}
	if _, err := super.Exec(context.Background(), `INSERT INTO jobs
		(id, hospital_id, type, subject_id, created_by, request_id, created_at, deadline_at, published_at)
		VALUES ($1, $2, 'approve_correction', $3, $4, $5, now() - make_interval(secs => $6), $7, $8)`,
		id, hid, correction, createdBy, "rid-"+id.String(), age.Seconds(), deadline, pub); err != nil {
		t.Fatal(err)
	}
	return id
}

type jobState struct {
	status    string
	errCode   *string
	published bool
	failedAt  bool
}

func jobStateOf(t *testing.T, id uuid.UUID) jobState {
	t.Helper()
	var s jobState
	if err := super.QueryRow(context.Background(), `SELECT status, error, published_at IS NOT NULL, failed_at IS NOT NULL FROM jobs WHERE id = $1`, id).
		Scan(&s.status, &s.errCode, &s.published, &s.failedAt); err != nil {
		t.Fatal(err)
	}
	return s
}

func setJob(t *testing.T, id uuid.UUID, set string) {
	t.Helper()
	if _, err := super.Exec(context.Background(), `UPDATE jobs SET `+set+` WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

// addJobFact puts a correction.* fact for job in the outbox, as a worker
// would.
func addJobFact(t *testing.T, hid, job, correction uuid.UUID, action string, payload map[string]any) {
	t.Helper()
	addJobFactAs(t, hid, job, "correction", correction.String(), action, payload)
}

// addJobFactAs is addJobFact with the fact's own entity and entity id.
func addJobFactAs(t *testing.T, hid, job uuid.UUID, entity, entityID, action string, payload map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	if _, err := super.Exec(context.Background(), `INSERT INTO intelligence_audit_outbox (hospital_id, job_id, action, entity, entity_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)`, hid, job, action, entity, entityID, string(raw)); err != nil {
		t.Fatal(err)
	}
}

func jobOutboxCount(t *testing.T, job uuid.UUID) int {
	t.Helper()
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM intelligence_audit_outbox WHERE job_id = $1`, job).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The message id names the job, and so does the body: an
// ApproveCorrectionJob whose job_id isn't the Nats-Msg-Id is refused
// before anything is minted.
func TestPublishJobRequiresJobIDMatch(t *testing.T) {
	c, _ := newClient(t)
	c.Pool = app
	hid := seedHospital(t, "active")
	submitter := seedUser(t, hid, true, authz.RoleClinician)
	decider := seedUser(t, hid, true, authz.RoleClinician)
	cor := seedCorrection(t, hid, submitter, decider)
	deadline := time.Now().Add(5 * time.Minute)
	job := uuid.New()
	for name, body := range map[string]*intelpb.ApproveCorrectionJob{
		"another job id": {JobId: uuid.NewString(), CorrectionId: cor.String()},
		"no job id":      {CorrectionId: cor.String()},
		"upper case":     {JobId: strings.ToUpper(job.String()), CorrectionId: cor.String()},
	} {
		if err := c.PublishJob(context.Background(), callerOf(hid, decider), "pipeline.approve_correction", job, body, deadline); !errors.Is(err, ErrMisuse) {
			t.Errorf("%s: %v, want ErrMisuse", name, err)
		}
	}
	if n := waitingOn(t, approveSubjectOf(hid)); n != 0 {
		t.Fatalf("%d refused jobs published", n)
	}
	if err := c.PublishJob(context.Background(), callerOf(hid, decider), "pipeline.approve_correction", job,
		&intelpb.ApproveCorrectionJob{JobId: job.String(), CorrectionId: cor.String()}, deadline); err != nil {
		t.Fatalf("matching job id: %v", err)
	}
	// A retry's message id (<job>.r<n>) still names the job.
	if err := c.publishJob(context.Background(), callerOf(hid, decider), "pipeline.approve_correction", RetryMsgID(job, 1),
		&intelpb.ApproveCorrectionJob{JobId: job.String(), CorrectionId: cor.String()}, deadline); err != nil {
		t.Fatalf("retry id: %v", err)
	}
}

// The sweeper publishes an unpublished, stale jobs row as its creator (the
// approver), with the job id as Nats-Msg-Id and ids in the body, then sets
// published_at. A row whose creator isn't the approver is skipped (the
// two-person rule refuses it; never retried as transient, never
// published); a young row waits; a row past its deadline is failed
// deadline_exceeded and audited job.dead_lettered against the job.
func TestSweeperRepublishesJobsRows(t *testing.T) {
	c, _ := newClient(t)
	c.Pool = app
	rec := &recJS{JetStream: gwJS}
	c.JS = rec
	ctx := context.Background()
	hid := seedHospital(t, "active")
	submitter := seedUser(t, hid, true, authz.RoleClinician)
	decider := seedUser(t, hid, true, authz.RoleClinician)
	bystander := seedUser(t, hid, true, authz.RoleClinician)
	deadline := time.Now().Truncate(time.Second).Add(5 * time.Minute)

	stale := seedJob(t, hid, seedCorrection(t, hid, submitter, decider), decider, 20*time.Second, deadline, false)
	staleCor := uuid.Nil
	if err := super.QueryRow(ctx, `SELECT subject_id FROM jobs WHERE id = $1`, stale).Scan(&staleCor); err != nil {
		t.Fatal(err)
	}
	wrongPerson := seedJob(t, hid, seedCorrection(t, hid, submitter, decider), bystander, 20*time.Second, deadline, false)
	young := seedJob(t, hid, seedCorrection(t, hid, submitter, decider), decider, 0, deadline, false)
	expired := seedJob(t, hid, seedCorrection(t, hid, submitter, decider), decider, 20*time.Second, time.Now().Add(-time.Second), false)
	// An approver who has since lost the clinician role: the row is
	// cancelled, not left.
	demoted := seedUser(t, hid, true, authz.RoleClinician)
	lostRole := seedJob(t, hid, seedCorrection(t, hid, submitter, demoted), demoted, 20*time.Second, deadline, false)
	if _, err := super.Exec(ctx, `DELETE FROM user_roles WHERE user_id = $1`, demoted); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, demoted, authz.RoleAnalyst); err != nil {
		t.Fatal(err)
	}
	// The sweep is global: the rows left queued and unpublished here must
	// not reach other tests' sweeps (their clients have no pool, so the
	// two-person check can't run there).
	t.Cleanup(func() {
		_, _ = super.Exec(context.Background(), `UPDATE jobs SET status = 'cancelled', cancel_reason = 'account_unusable'
			WHERE hospital_id = $1 AND status = 'queued'`, hid)
	})

	_, _ = SweepOnce(ctx, app, c)

	msgs := rec.on(approveSubjectOf(hid))
	if len(msgs) != 1 {
		t.Fatalf("%d published, want 1", len(msgs))
	}
	m := msgs[0]
	if m.header.Get("Nats-Msg-Id") != stale.String() {
		t.Fatalf("published %s, want %s", m.header.Get("Nats-Msg-Id"), stale)
	}
	cl := verifyJob(t, m)
	if cl.Sub != decider.String() || cl.Hid != hid.String() || cl.Rid != "rid-"+stale.String() || !cl.ExpiresAt.Equal(deadline) {
		t.Fatalf("claims %+v", cl)
	}
	var body intelpb.ApproveCorrectionJob
	if err := proto.Unmarshal(m.data, &body); err != nil || body.JobId != stale.String() || body.CorrectionId != staleCor.String() {
		t.Fatalf("body %+v %v", &body, err)
	}
	if s := jobStateOf(t, stale); s.status != "queued" || !s.published {
		t.Fatalf("stale row %+v", s)
	}
	if s := jobStateOf(t, wrongPerson); s.status != "queued" || s.published {
		t.Fatalf("wrong-person row %+v", s)
	}
	if s := jobStateOf(t, young); s.status != "queued" || s.published {
		t.Fatalf("young row %+v", s)
	}
	s := jobStateOf(t, expired)
	if s.status != "failed" || s.errCode == nil || *s.errCode != "deadline_exceeded" || !s.failedAt || s.published {
		t.Fatalf("expired row %+v", s)
	}
	a := auditFor(t, "job.dead_lettered", expired.String())
	if len(a) != 1 || a[0].actor != nil || *a[0].hospital != hid.String() || *a[0].entity != "job" ||
		*a[0].requestID != "rid-"+expired.String() || a[0].payload["type"] != "approve_correction" ||
		a[0].payload["error"] != "deadline_exceeded" || a[0].payload["job_id"] != expired.String() {
		t.Fatalf("expiry audit %+v", a)
	}
	if s := jobStateOf(t, lostRole); s.status != "cancelled" || s.published {
		t.Fatalf("lost-role row %+v", s)
	}
	var reason *string
	if err := super.QueryRow(ctx, `SELECT cancel_reason FROM jobs WHERE id = $1`, lostRole).Scan(&reason); err != nil ||
		reason == nil || *reason != "account_unusable" {
		t.Fatalf("cancel reason %v %v", reason, err)
	}
	if c := auditFor(t, "job.cancelled", lostRole.String()); len(c) != 1 || c[0].actor != nil || *c[0].entity != "job" ||
		*c[0].hospital != hid.String() || c[0].payload["job_id"] != lostRole.String() ||
		c[0].payload["type"] != "approve_correction" || c[0].payload["reason"] != "account_unusable" {
		t.Fatalf("job.cancelled %+v", c)
	}
	// The skipped row is never published, sweep after sweep.
	_, _ = SweepOnce(ctx, app, c)
	if n := len(rec.on(approveSubjectOf(hid))); n != 1 {
		t.Fatalf("%d published after a second sweep", n)
	}
	if s := jobStateOf(t, wrongPerson); s.published {
		t.Fatal("the wrong-person row was published")
	}
	if n := len(auditFor(t, "job.cancelled", lostRole.String())); n != 1 {
		t.Fatalf("%d job.cancelled rows after a second sweep", n)
	}
}

// F5 for jobs rows: a dead letter for a queued approve_correction job of
// the subject's hospital marks it failed and audits job.dead_lettered and
// (a refusal) intelligence.refused against the job, with no actor and the
// row's hospital; no query.failed. Another hospital's job, a letter whose
// type doesn't match its subject, and a run_query letter naming a jobs row
// change nothing.
func TestDeadLetterApproveCorrectionMarksFailed(t *testing.T) {
	c, _ := newClient(t)
	c.Metrics = &Metrics{}
	runConsumer(t, c)
	js := workerJS(t)
	hid := seedHospital(t, "active")
	other := seedHospital(t, "active")
	sub, dec := seedUser(t, hid, true, authz.RoleClinician), seedUser(t, hid, true, authz.RoleClinician)
	osub, odec := seedUser(t, other, true, authz.RoleClinician), seedUser(t, other, true, authz.RoleClinician)
	deadline := time.Now().Add(5 * time.Minute)
	job := seedJob(t, hid, seedCorrection(t, hid, sub, dec), dec, 0, deadline, true)
	theirs := seedJob(t, other, seedCorrection(t, other, osub, odec), odec, 0, deadline, true)
	subj := func(h uuid.UUID, typ string) string { return "dead.v1." + h.String() + "." + typ }

	sendDead(t, js, subj(hid, "approve_correction"), &intelpb.DeadLetter{JobId: job.String(), Type: "approve_correction",
		SubjectHospital: hid.String(), Attempts: 1, Code: "forbidden", Reason: "forbidden", Kid: signer.KeyID})
	sendDead(t, js, subj(hid, "approve_correction"), &intelpb.DeadLetter{JobId: theirs.String(), Type: "approve_correction",
		SubjectHospital: hid.String(), Code: "unauthorized", Reason: "binding"})
	sendDead(t, js, subj(other, "approve_correction"), &intelpb.DeadLetter{JobId: theirs.String(), Type: "approve_correction",
		SubjectHospital: hid.String(), Code: "unauthorized", Reason: "binding"})
	sendDead(t, js, subj(other, "approve_correction"), &intelpb.DeadLetter{JobId: theirs.String(), Type: "run_query",
		SubjectHospital: other.String(), Code: "unauthorized", Reason: "binding"})
	sendDead(t, js, subj(other, "run_query"), &intelpb.DeadLetter{JobId: theirs.String(), Type: "run_query",
		SubjectHospital: other.String(), Code: "unauthorized", Reason: "binding"})
	waitFor(t, "five letters", lettersSeen(c.Metrics, 5))
	waitFor(t, "the row marked", func() bool { return jobStateOf(t, job).status == "failed" })

	s := jobStateOf(t, job)
	if s.errCode == nil || *s.errCode != "forbidden" || !s.failedAt {
		t.Fatalf("row %+v", s)
	}
	for _, action := range []string{"job.dead_lettered", "intelligence.refused"} {
		rows := auditFor(t, action, job.String())
		if len(rows) != 1 || rows[0].actor != nil || *rows[0].hospital != hid.String() || *rows[0].entity != "job" ||
			*rows[0].requestID != "rid-"+job.String() {
			t.Fatalf("%s: %+v", action, rows)
		}
	}
	if p := auditFor(t, "job.dead_lettered", job.String())[0].payload; p["type"] != "approve_correction" || p["error"] != "forbidden" {
		t.Fatalf("dead_lettered payload %v", p)
	}
	if p := auditFor(t, "intelligence.refused", job.String())[0].payload; p["operation"] != "pipeline.approve_correction" || p["reason"] != "forbidden" {
		t.Fatalf("refused payload %v", p)
	}
	if n := len(auditFor(t, "query.failed", job.String())); n != 0 {
		t.Fatalf("%d query.failed rows for a jobs row", n)
	}
	waitFor(t, "the others ignored", func() bool { return c.Metrics.DeadIgnored.Load() >= 4 })
	if st := jobStateOf(t, theirs); st.status != "queued" {
		t.Fatalf("another hospital's job changed: %+v", st)
	}
	if n := auditCount(t, theirs); n != 0 {
		t.Fatalf("%d audit rows for another hospital's job", n)
	}
	if c.Metrics.DeadRefused.Load() != 1 {
		t.Fatalf("refused %d", c.Metrics.DeadRefused.Load())
	}
}

// The A12 twin for job facts: a correction.applied that names another
// actor, hospital or request id is recorded with the jobs row's creator,
// hospital and request id, against the job.
func TestDrainerJobFactUsesJobsIdentity(t *testing.T) {
	hid := seedHospital(t, "active")
	other := seedHospital(t, "active")
	sub, dec := seedUser(t, hid, true, authz.RoleClinician), seedUser(t, hid, true, authz.RoleClinician)
	intruder := seedUser(t, other, true, authz.RoleClinician)
	cor := seedCorrection(t, hid, sub, dec)
	job := seedJob(t, hid, cor, dec, 0, time.Now().Add(5*time.Minute), true)
	setJob(t, job, "status = 'completed', attempts = 1, completed_at = now()")
	addJobFact(t, hid, job, cor, "correction.applied", map[string]any{
		"correction_id": cor.String(), "attempts": 1, "persona_update": "skipped",
		"model_versions": map[string]any{"nli": "synthetic-1.0"},
		"actor_id":       intruder.String(), "hospital_id": other.String(), "request_id": "forged", "sub": intruder.String(),
	})
	drainAll(t)
	rows := auditFor(t, "correction.applied", cor.String())
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	r := rows[0]
	if r.actor == nil || *r.actor != dec.String() || *r.hospital != hid.String() || *r.requestID != "rid-"+job.String() ||
		*r.entity != "correction" || *r.entityID != cor.String() {
		t.Fatalf("identity from the fact: %+v", r)
	}
	for _, k := range identityKeys {
		if _, ok := r.payload[k]; ok {
			t.Errorf("payload kept %q: %v", k, r.payload)
		}
	}
	if r.payload["correction_id"] != cor.String() || r.payload["persona_update"] != "skipped" || r.payload["job_id"] != job.String() ||
		r.payload["fact_entity"] != nil || r.payload["fact_entity_id"] != nil {
		t.Fatalf("payload %v", r.payload)
	}
	if jobOutboxCount(t, job) != 0 {
		t.Fatal("fact left in the outbox")
	}
	// A payload outside the schema (free text under a known key) is
	// recorded as refused, with no text.
	cor2 := seedCorrection(t, hid, sub, dec)
	job2 := seedJob(t, hid, cor2, dec, 0, time.Now().Add(5*time.Minute), true)
	setJob(t, job2, "status = 'failed', attempts = 1, error = 'internal', failed_at = now()")
	addJobFact(t, hid, job2, cor2, "correction.apply_failed", map[string]any{"correction_id": cor2.String(), "error": "a synthetic note", "attempts": 1})
	drainAll(t)
	rows = auditFor(t, "correction.apply_failed", cor2.String())
	if len(rows) != 1 || rows[0].payload["payload_refused"] != true || rows[0].payload["error"] != nil ||
		rows[0].payload["job_id"] != job2.String() || *rows[0].entity != "correction" {
		t.Fatalf("refused payload %+v", rows)
	}
	ok, broken, err := audit.Verify(context.Background(), app)
	if err != nil || !ok {
		t.Fatalf("chain verify: ok %v at %d, %v", ok, broken, err)
	}
}

// A job fact whose action doesn't match the job's status now is taken off
// the outbox and never appended.
func TestDrainerRefusesMismatchedAction(t *testing.T) {
	hid := seedHospital(t, "active")
	sub, dec := seedUser(t, hid, true, authz.RoleClinician), seedUser(t, hid, true, authz.RoleClinician)
	applied := map[string]any{"attempts": 1, "persona_update": "skipped"}
	failed := map[string]any{"error": "internal", "attempts": 1}
	for _, c := range []struct {
		action, set string
		payload     map[string]any
	}{
		{"correction.applied", "status = 'queued'", applied},
		{"correction.applied", "status = 'running', attempts = 1", applied},
		{"correction.applied", "status = 'failed', error = 'internal'", applied},
		{"correction.apply_failed", "status = 'completed'", failed},
		{"correction.apply_failed", "status = 'running', attempts = 1", failed},
	} {
		cor := seedCorrection(t, hid, sub, dec)
		job := seedJob(t, hid, cor, dec, 0, time.Now().Add(5*time.Minute), true)
		setJob(t, job, c.set)
		p := map[string]any{"correction_id": cor.String()}
		for k, v := range c.payload {
			p[k] = v
		}
		addJobFact(t, hid, job, cor, c.action, p)
		drainAll(t)
		if rows := auditFor(t, c.action, cor.String()); len(rows) != 0 {
			t.Errorf("%s with %s appended: %+v", c.action, c.set, rows)
		}
		if jobOutboxCount(t, job) != 0 {
			t.Errorf("%s with %s left in the outbox", c.action, c.set)
		}
	}
}

// Regression: Plan 3's drainer picked the oldest 100 facts and deleted
// only those joined to clinical_queries, so 100 job facts at the head
// stalled every query fact behind them for good.
func TestDrainerJobFactsDoNotStallQueryFacts(t *testing.T) {
	hid := seedHospital(t, "active")
	sub, dec := seedUser(t, hid, true, authz.RoleClinician), seedUser(t, hid, true, authz.RoleClinician)
	cor := seedCorrection(t, hid, sub, dec)
	job := seedJob(t, hid, cor, dec, 0, time.Now().Add(5*time.Minute), true)
	setJob(t, job, "status = 'completed', attempts = 1")
	for i := 0; i < drainBatch+1; i++ {
		addJobFact(t, hid, job, cor, "correction.applied", map[string]any{"correction_id": cor.String(), "attempts": 1, "persona_update": "skipped"})
	}
	deadline := time.Now().Add(5 * time.Minute)
	q := seedQuery(t, hid, sub, 0, &deadline, true)
	setRow(t, q, "status = 'completed'")
	addFact(t, hid, q, "query.completed", map[string]any{"query_id": q.String(), "decision": "PASS", "attempts": 1})
	drainAll(t)
	if n := len(auditFor(t, "query.completed", q.String())); n != 1 {
		t.Fatalf("query fact appended %d times", n)
	}
	if n := len(auditFor(t, "correction.applied", cor.String())); n != drainBatch+1 {
		t.Fatalf("%d job facts appended", n)
	}
	if outboxCount(t, q) != 0 || jobOutboxCount(t, job) != 0 {
		t.Fatal("facts left in the outbox")
	}
}

// A job fact names the job's own correction: a correction_id, or an entity
// id, of another correction (or another job, or an entity that isn't the
// correction) is recorded as {payload_refused}, never as said. The job's
// own correction goes through.
func TestDrainerJobFactCorrectionMustMatchSubject(t *testing.T) {
	hid := seedHospital(t, "active")
	sub, dec := seedUser(t, hid, true, authz.RoleClinician), seedUser(t, hid, true, authz.RoleClinician)
	other := seedCorrection(t, hid, sub, dec)
	otherJob := seedJob(t, hid, other, dec, 0, time.Now().Add(5*time.Minute), true)
	for name, c := range map[string]struct {
		entity     string
		entityID   func(cor uuid.UUID) string
		correction func(cor uuid.UUID) string
		ok         bool
	}{
		"its own correction":           {"correction", func(cor uuid.UUID) string { return cor.String() }, func(cor uuid.UUID) string { return cor.String() }, true},
		"no entity id":                 {"correction", func(uuid.UUID) string { return "" }, func(cor uuid.UUID) string { return cor.String() }, true},
		"another correction_id":        {"correction", func(cor uuid.UUID) string { return cor.String() }, func(uuid.UUID) string { return other.String() }, false},
		"another correction as entity": {"correction", func(uuid.UUID) string { return other.String() }, func(cor uuid.UUID) string { return cor.String() }, false},
		"both another correction":      {"correction", func(uuid.UUID) string { return other.String() }, func(uuid.UUID) string { return other.String() }, false},
		"another job as entity":        {"job", func(uuid.UUID) string { return otherJob.String() }, func(cor uuid.UUID) string { return cor.String() }, false},
		"the correction as a query":    {"query", func(cor uuid.UUID) string { return cor.String() }, func(cor uuid.UUID) string { return cor.String() }, false},
		"the job itself as entity":     {"job", func(uuid.UUID) string { return "" }, func(cor uuid.UUID) string { return cor.String() }, true},
	} {
		cor := seedCorrection(t, hid, sub, dec)
		job := seedJob(t, hid, cor, dec, 0, time.Now().Add(5*time.Minute), true)
		setJob(t, job, "status = 'completed', attempts = 1")
		addJobFactAs(t, hid, job, c.entity, c.entityID(cor), "correction.applied",
			map[string]any{"correction_id": c.correction(cor), "attempts": 1, "persona_update": "skipped"})
		drainAll(t)
		rows := auditFor(t, "correction.applied", cor.String())
		if len(rows) != 1 || *rows[0].entity != "correction" {
			t.Fatalf("%s: rows %+v", name, rows)
		}
		p := rows[0].payload
		switch {
		case c.ok && (p["payload_refused"] != nil || p["correction_id"] != cor.String() || p["job_id"] != job.String()):
			t.Errorf("%s: refused: %v", name, p)
		case !c.ok && (p["payload_refused"] != true || len(p) != 2 || p["job_id"] != job.String()):
			t.Errorf("%s: recorded: %v", name, p)
		}
		if jobOutboxCount(t, job) != 0 {
			t.Errorf("%s: left in the outbox", name)
		}
	}
}

// M-2: an error in the jobs table's steps doesn't stop the questions'
// sweep: the stale question is still published, and the sweep reports
// the error.
func TestSweeperJobsErrorDoesNotStopQueries(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()
	drainJobs(t, sharedNATS.PipelineURL)
	hid := seedHospital(t, "active")
	creator := seedUser(t, hid, true, authz.RoleClinician)
	deadline := time.Now().Add(5 * time.Minute)
	q := seedQuery(t, hid, creator, 20*time.Second, &deadline, false)
	jobsTableFault = func() error { return errors.New("synthetic jobs-table fault") }
	_, err := SweepOnce(ctx, app, c)
	jobsTableFault = nil
	if err == nil {
		t.Fatal("the jobs-table fault wasn't reported")
	}
	if s := stateOf(t, q); !s.published {
		t.Fatalf("question not published: %+v", s)
	}
	if n := len(jobsFor(drainJobs(t, sharedNATS.PipelineURL), "jobs.v1."+hid.String()+".run_query")); n != 1 {
		t.Fatalf("%d run_query jobs published", n)
	}
}
