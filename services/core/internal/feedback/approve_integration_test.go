//go:build integration

package feedback_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/feedback"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intel"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/server"
)

// Plan 4 Task 6: an approval publishes pipeline.approve_correction (spec
// §4.5; abuse cases C3, C13, C23, C32). Every name and id is synthetic.

// recJS is core's JetStream handle with a record of every publish that
// JetStream accepted (the message after the publish, so its Nats-Msg-Id
// header is set) and whether JetStream called it a duplicate.
type recJS struct {
	jetstream.JetStream
	mu   sync.Mutex
	recs []pubRec
}

type pubRec struct {
	m   *nats.Msg
	dup bool
}

func (r *recJS) PublishMsg(ctx context.Context, m *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	ack, err := r.JetStream.PublishMsg(ctx, m, opts...)
	if err == nil {
		r.mu.Lock()
		r.recs = append(r.recs, pubRec{m: m, dup: ack.Duplicate})
		r.mu.Unlock()
	}
	return ack, err
}

// on is the publishes on subject.
func (r *recJS) on(subject string) []pubRec {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []pubRec
	for _, p := range r.recs {
		if p.m.Subject == subject {
			out = append(out, p)
		}
	}
	return out
}

func approveSubject(hid uuid.UUID) string { return "jobs.v1." + hid.String() + ".approve_correction" }

// stored is how many messages JOBS holds on subject (no consumer reads
// approve_correction in these tests, so every stored message stays).
func stored(t *testing.T, subject string) uint64 {
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

type jobRow struct {
	id, hospital, createdBy uuid.UUID
	typ, status             string
	requestID               *string
	published               bool
	deadline                time.Time
}

func jobOf(t *testing.T, correction uuid.UUID) jobRow {
	t.Helper()
	var j jobRow
	if err := super.QueryRow(context.Background(), `SELECT id, hospital_id, created_by, type, status, request_id, published_at IS NOT NULL, deadline_at
		FROM jobs WHERE subject_id = $1`, correction).
		Scan(&j.id, &j.hospital, &j.createdBy, &j.typ, &j.status, &j.requestID, &j.published, &j.deadline); err != nil {
		t.Fatal(err)
	}
	return j
}

func approve(t *testing.T, e *env, u user, cid uuid.UUID, hdr map[string]string) resp {
	t.Helper()
	return e.do(t, "POST", "/corrections/"+cid.String()+"/approve", mint(t, u), "", hdr)
}

// C3 (publish side): one approval publishes one approve_correction job on
// the hospital's subject, with the job id as Nats-Msg-Id, a job token for
// the reviewer and the hospital, and a body of ids only.
func TestApprovePublishesOneJob(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	sub := clinician(t, hid)
	rev := clinician(t, hid)
	cid := submitCorrection(t, e, sub)
	if r := approve(t, e, rev, cid, nil); r.code != 204 {
		t.Fatalf("approve: %d %s", r.code, r.raw)
	}
	j := jobOf(t, cid)
	subject := approveSubject(hid)
	recs := e.js.on(subject)
	if len(recs) != 1 || recs[0].dup {
		t.Fatalf("%d publishes on %s (%+v)", len(recs), subject, recs)
	}
	m := recs[0].m
	if got := m.Header.Get("Nats-Msg-Id"); got != j.id.String() {
		t.Fatalf("Nats-Msg-Id %q, want the job id %s", got, j.id)
	}
	cl, err := reqtoken.VerifyAt(m.Header.Get("Mhai-Request-Token"), signer.PublicKeys(),
		reqtoken.Expect{Aud: "pipeline", Subject: subject, Kind: reqtoken.KindJob}, m.Data, time.Now())
	if err != nil {
		t.Fatalf("job token: %v", err)
	}
	if cl.Sub != rev.id.String() || cl.Hid != hid.String() {
		t.Fatalf("token sub %s hid %s, want the reviewer %s and %s", cl.Sub, cl.Hid, rev.id, hid)
	}
	var body intelpb.ApproveCorrectionJob
	if err := proto.Unmarshal(m.Data, &body); err != nil || body.JobId != j.id.String() || body.CorrectionId != cid.String() {
		t.Fatalf("body %+v %v", &body, err)
	}
	if strings.Contains(string(m.Data), "Synthetic") {
		t.Fatal("the job body carries the correction's text")
	}
	if !j.published || j.status != "queued" {
		t.Fatalf("job row %+v", j)
	}
	if n := stored(t, subject); n != 1 {
		t.Fatalf("JOBS holds %d on %s", n, subject)
	}
}

// The jobs row is the one correction.approved names, inserted in the same
// transaction: type, subject, creator, hospital, request id, a 5-minute
// deadline.
func TestApprovedJobIDMatchesJobsRow(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	sub := clinician(t, hid)
	rev := clinician(t, hid)
	cid := submitCorrection(t, e, sub)
	before := time.Now()
	if r := approve(t, e, rev, cid, nil); r.code != 204 {
		t.Fatalf("approve: %d %s", r.code, r.raw)
	}
	j := jobOf(t, cid)
	var rid string
	var payloadJob string
	if err := super.QueryRow(context.Background(), `SELECT request_id, payload->>'job_id' FROM audit_log
		WHERE action = 'correction.approved' AND entity_id = $1`, cid.String()).Scan(&rid, &payloadJob); err != nil {
		t.Fatal(err)
	}
	if payloadJob != j.id.String() {
		t.Fatalf("correction.approved names job %s, the jobs row is %s", payloadJob, j.id)
	}
	if j.typ != "approve_correction" || j.hospital != hid || j.createdBy != rev.id || j.requestID == nil || *j.requestID != rid {
		t.Fatalf("job row %+v (audit request id %s)", j, rid)
	}
	if d := j.deadline.Sub(before); d < 4*time.Minute || d > 6*time.Minute {
		t.Fatalf("deadline %v after the request", d)
	}
}

// C13: the same Idempotency-Key replays 204 and publishes nothing more; a
// new key is 409 already_decided. One message, one job.
func TestApproveIdempotent(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	sub := clinician(t, hid)
	rev := clinician(t, hid)
	cid := submitCorrection(t, e, sub)
	tok := mint(t, rev)
	key := map[string]string{"Idempotency-Key": "approve-" + uniq()}
	for i := 0; i < 2; i++ {
		if r := e.do(t, "POST", "/corrections/"+cid.String()+"/approve", tok, "", key); r.code != 204 {
			t.Fatalf("approve %d: %d %s", i, r.code, r.raw)
		}
	}
	problem(t, e.do(t, "POST", "/corrections/"+cid.String()+"/approve", tok, "", map[string]string{"Idempotency-Key": "approve-" + uniq()}),
		409, "already_decided")
	if n := len(e.js.on(approveSubject(hid))); n != 1 {
		t.Fatalf("%d publishes", n)
	}
	if n := stored(t, approveSubject(hid)); n != 1 {
		t.Fatalf("JOBS holds %d", n)
	}
	if n := jobsFor(t, cid); n != 1 {
		t.Fatalf("%d jobs", n)
	}
}

// C23: the approval, the jobs row and correction.approved commit together:
// a failure after audit.Write rolls all three back, and nothing is
// published.
func TestApprovalAuditInSameTransaction(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	sub := clinician(t, hid)
	rev := clinician(t, hid)
	cid := submitCorrection(t, e, sub)
	restore := feedback.SetAfterApproveAudit(func() error { return errors.New("forced failure after the audit write") })
	r := approve(t, e, rev, cid, nil)
	restore()
	problem(t, r, 500, "internal_error")
	if row := rowOf(t, cid); row.status != "pending_review" || row.decidedBy != nil {
		t.Fatalf("approval survived: %+v", row)
	}
	if n := jobsFor(t, cid); n != 0 {
		t.Fatalf("%d jobs survived", n)
	}
	if n := len(auditFor(t, hid, "correction.approved")); n != 0 {
		t.Fatalf("%d correction.approved rows survived", n)
	}
	if n := len(e.js.on(approveSubject(hid))); n != 0 {
		t.Fatalf("%d publishes", n)
	}
	// Without the fault it goes through.
	if r := approve(t, e, rev, cid, nil); r.code != 204 {
		t.Fatalf("approve: %d %s", r.code, r.raw)
	}
}

// C32: 20 approve_correction jobs in flight per hospital. The 21st
// approval still answers 204 and commits, but its job waits unpublished;
// the sweeper publishes it once a slot frees, and not before.
func TestApprovalBudget(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	hid := seedHospital(t)
	sub := clinician(t, hid)
	rev := clinician(t, hid)
	var cids []uuid.UUID
	for i := 0; i < 21; i++ {
		cid := submitCorrection(t, e, sub)
		if r := approve(t, e, rev, cid, nil); r.code != 204 {
			t.Fatalf("approval %d: %d %s", i+1, r.code, r.raw)
		}
		cids = append(cids, cid)
	}
	if n := len(e.js.on(approveSubject(hid))); n != 20 {
		t.Fatalf("%d published, want 20", n)
	}
	last := cids[20]
	if j := jobOf(t, last); j.published || j.status != "queued" {
		t.Fatalf("21st job %+v", j)
	}
	if r := rowOf(t, last); r.status != "approved" {
		t.Fatalf("21st approval not committed: %+v", r)
	}
	// Old enough for the sweeper, the order kept; still no free slot.
	if _, err := super.Exec(ctx, `UPDATE jobs SET created_at = created_at - interval '30 seconds' WHERE hospital_id = $1`, hid); err != nil {
		t.Fatal(err)
	}
	_, _ = intel.SweepOnce(ctx, app, e.client)
	if j := jobOf(t, last); j.published {
		t.Fatal("the sweeper published past the budget")
	}
	if n := len(e.js.on(approveSubject(hid))); n != 20 {
		t.Fatalf("%d published after a full sweep", n)
	}
	// One completes: the sweeper publishes the waiting job.
	if _, err := super.Exec(ctx, `UPDATE jobs SET status = 'completed', completed_at = now() WHERE subject_id = $1`, cids[0]); err != nil {
		t.Fatal(err)
	}
	_, _ = intel.SweepOnce(ctx, app, e.client)
	if j := jobOf(t, last); !j.published {
		t.Fatal("the sweeper didn't publish the waiting job")
	}
	recs := e.js.on(approveSubject(hid))
	if len(recs) != 21 || recs[20].m.Header.Get("Nats-Msg-Id") != jobOf(t, last).id.String() {
		t.Fatalf("%d published", len(recs))
	}
}

// C3 with two eligible reviewers and the submitter at once: one approval,
// one already_decided, one self_approval; one correction.approved and one
// job.
func TestParallelApprovalsTwoReviewers(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	sub := clinician(t, hid)
	rev1 := clinician(t, hid)
	rev2 := clinician(t, hid)
	for round := 0; round < 5; round++ {
		cid := submitCorrection(t, e, sub)
		toks := []string{mint(t, sub), mint(t, rev1), mint(t, rev2)}
		out := make([]resp, len(toks))
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, tok := range toks {
			wg.Add(1)
			go func(i int, tok string) {
				defer wg.Done()
				<-start
				out[i] = e.do(t, "POST", "/corrections/"+cid.String()+"/approve", tok, "", nil)
			}(i, tok)
		}
		close(start)
		wg.Wait()
		if out[0].code != 403 || out[0].body["code"] != "self_approval" {
			t.Fatalf("round %d: submitter got %d %s", round, out[0].code, out[0].raw)
		}
		won, lost := out[1], out[2]
		winner := rev1
		if won.code != 204 {
			won, lost, winner = out[2], out[1], rev2
		}
		if won.code != 204 || lost.code != 409 || lost.body["code"] != "already_decided" {
			t.Fatalf("round %d: reviewers got %d and %d (%s %s)", round, out[1].code, out[2].code, out[1].raw, out[2].raw)
		}
		if r := rowOf(t, cid); r.status != "approved" || r.decidedBy == nil || *r.decidedBy != winner.id {
			t.Fatalf("round %d: row %+v", round, r)
		}
		n := 0
		for _, a := range auditFor(t, hid, "correction.approved") {
			if a.entityID == cid.String() {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("round %d: %d correction.approved rows", round, n)
		}
		if j := jobsFor(t, cid); j != 1 {
			t.Fatalf("round %d: %d jobs", round, j)
		}
		if j := jobOf(t, cid); j.createdBy != winner.id {
			t.Fatalf("round %d: job created by %s, the approver is %s", round, j.createdBy, winner.id)
		}
	}
}

// N parallel submits for one question: one is recorded, the rest are 409.
func TestParallelSubmitsOneRow(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	u := clinician(t, hid)
	q := seedQuery(t, u, true)
	const n = 8
	toks := make([]string, n)
	for i := range toks {
		toks[i] = mint(t, u)
	}
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			body := correctBody(q, "Synthetic flutter", "Synthetic reason for review")
			if i%2 == 1 {
				body = validateBody(q)
			}
			r := e.do(t, "POST", "/feedback", toks[i], body, nil)
			codes[i] = r.code
			if r.code == 409 && r.body["code"] != "conflict" {
				t.Errorf("409 with %s", r.raw)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case 200:
			ok++
		case 409:
			conflict++
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Fatalf("codes %v", codes)
	}
	var rows int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM feedback_events WHERE query_id = $1`, q).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("%d feedback rows", rows)
	}
}

// The feedback routes log through the service's logger even with no
// intelligence client (the log must not depend on Intel being set).
func TestFeedbackLogsWithoutIntel(t *testing.T) {
	logs := &syncBuf{}
	log := logx.New(logs)
	svc := &auth.Service{Pool: app, Buckets: buckets, Issuer: issuer, Keys: issuer.PublicKeys(), Log: log,
		MFAKey: []byte(testMFAKey), Argon: password.NewGate(64, time.Minute)}
	guard := &authz.Guard{Keys: issuer.PublicKeys(), Denylist: buckets.Denylist, Pool: app, Log: log,
		Idem: httpx.IdempotentLog(app, log), IdemAnon: httpx.IdempotentAnonLog(app, log)}
	m := authz.NewMux(guard)
	server.Register(m, server.Deps{Auth: svc, Pool: app})
	e := &env{h: m.Handler([]string{serviceKey}), logs: logs, js: &recJS{JetStream: gwJS}}
	hid := seedHospital(t)
	sub := clinician(t, hid)
	cid := submitCorrection(t, e, sub)
	problem(t, approve(t, e, sub, cid, nil), http.StatusForbidden, "self_approval")
	if !strings.Contains(logs.String(), "feedback: approve refused") {
		t.Fatalf("no feedback log line: %s", logs.String())
	}
}
