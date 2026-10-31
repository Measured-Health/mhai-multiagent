//go:build integration

package intel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
)

// workerJS is a JetStream handle as the pipeline worker's NATS user: the
// user that may publish dead.v1.>.
func workerJS(t *testing.T) jetstream.JetStream {
	t.Helper()
	nc, js, err := bus.Connect(context.Background(), sharedNATS.PipelineURL, "", bus.WorkerInbox("pipeline"), bus.TLS{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return js
}

// sendDead publishes a dead letter on subject, as a worker.
func sendDead(t *testing.T, js jetstream.JetStream, subject string, d *intelpb.DeadLetter) {
	t.Helper()
	b, err := proto.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish(context.Background(), subject, b); err != nil {
		t.Fatalf("publish dead letter: %v", err)
	}
}

func deadSubject(hid uuid.UUID) string { return "dead.v1." + hid.String() + ".run_query" }

// runConsumer runs one core copy's dead-letter consumer until the test
// ends, on its own gateway connection.
func runConsumer(t *testing.T, c *Client) {
	t.Helper()
	nc, js, err := bus.Connect(context.Background(), sharedNATS.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := RunDeadLetters(ctx, app, js, c); err != nil {
			t.Errorf("consumer: %v", err)
		}
	}()
	t.Cleanup(func() { cancel(); <-done; nc.Close() })
}

// waitFor polls cond for up to 15 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(15 * time.Second)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func lettersSeen(m *Metrics, n int64) func() bool {
	return func() bool { return m.DeadReceived.Load() >= n }
}

// F5: a dead letter for a known, queued job marks it failed with the
// letter's code and audits job.dead_lettered, query.failed and (a refusal)
// intelligence.refused, with no actor and the row's own hospital. A letter
// naming an unknown job, another hospital's job (whatever its
// subject_hospital says), or sent on another hospital's subject changes
// nothing and audits nothing.
func TestDeadLetterMarksFailed(t *testing.T) {
	c, _ := newClient(t)
	c.Metrics = &Metrics{}
	runConsumer(t, c)
	js := workerJS(t)
	hid := seedHospital(t, "active")
	other := seedHospital(t, "active")
	u := seedUser(t, hid, true, authz.RoleClinician)
	uo := seedUser(t, other, true, authz.RoleClinician)
	deadline := time.Now().Add(5 * time.Minute)
	q := seedQuery(t, hid, u, 0, &deadline, true)
	theirs := seedQuery(t, other, uo, 0, &deadline, true)

	sendDead(t, js, deadSubject(hid), &intelpb.DeadLetter{JobId: q.String(), Type: "run_query", SubjectHospital: hid.String(),
		Attempts: 1, Code: "forbidden", Reason: "forbidden", Kid: signer.KeyID})
	// Another hospital's job: on our subject, and on its own subject with
	// our hospital as subject_hospital; an unknown job.
	sendDead(t, js, deadSubject(hid), &intelpb.DeadLetter{JobId: theirs.String(), Type: "run_query", SubjectHospital: hid.String(),
		Code: "unauthorized", Reason: "binding"})
	sendDead(t, js, deadSubject(other), &intelpb.DeadLetter{JobId: theirs.String(), Type: "run_query", SubjectHospital: hid.String(),
		Code: "unauthorized", Reason: "binding"})
	sendDead(t, js, deadSubject(hid), &intelpb.DeadLetter{JobId: uuid.NewString(), Type: "run_query", SubjectHospital: hid.String(),
		Code: "not_found"})
	waitFor(t, "four letters", lettersSeen(c.Metrics, 4))
	waitFor(t, "the row marked", func() bool { return stateOf(t, q).status == "failed" })

	s := stateOf(t, q)
	if s.errCode == nil || *s.errCode != "forbidden" || !s.completed {
		t.Fatalf("row %+v", s)
	}
	for _, action := range []string{"job.dead_lettered", "query.failed", "intelligence.refused"} {
		rows := auditFor(t, action, q.String())
		if len(rows) != 1 || rows[0].actor != nil || *rows[0].hospital != hid.String() || *rows[0].requestID != "rid-"+q.String() {
			t.Fatalf("%s: %+v", action, rows)
		}
	}
	if r := auditFor(t, "intelligence.refused", q.String())[0]; r.payload["reason"] != "forbidden" || r.payload["kid"] != signer.KeyID ||
		r.payload["operation"] != "pipeline.run_query" {
		t.Fatalf("refused payload %v", r.payload)
	}
	if st := stateOf(t, theirs); st.status != "queued" {
		t.Fatalf("another hospital's row changed: %+v", st)
	}
	for _, action := range []string{"job.dead_lettered", "query.failed", "intelligence.refused"} {
		if n := len(auditFor(t, action, theirs.String())); n != 0 {
			t.Fatalf("%s audited for another hospital's job", action)
		}
	}
	if c.Metrics.DeadIgnored.Load() < 3 || c.Metrics.DeadRefused.Load() != 1 {
		t.Fatalf("metrics ignored %d refused %d", c.Metrics.DeadIgnored.Load(), c.Metrics.DeadRefused.Load())
	}
}

// F6: a job whose token expired in the queue is failed deadline_exceeded
// with query.failed, and raises no intelligence.refused and no alert. A
// letter never overwrites a finished row or touches a running one, and
// doesn't add a second query.failed to a row already failed.
func TestDeadLetterExpiredAndTerminal(t *testing.T) {
	c, logs := newClient(t)
	c.Metrics = &Metrics{}
	runConsumer(t, c)
	js := workerJS(t)
	hid := seedHospital(t, "active")
	u := seedUser(t, hid, true, authz.RoleClinician)
	deadline := time.Now().Add(5 * time.Minute)
	expired := seedQuery(t, hid, u, 0, &deadline, true)
	cancelled := seedQuery(t, hid, u, 0, &deadline, true)
	setRow(t, cancelled, "status = 'cancelled', cancel_reason = 'account_unusable'")
	running := seedQuery(t, hid, u, 0, &deadline, true)
	setRow(t, running, "status = 'running', attempts = 1")
	failed := seedQuery(t, hid, u, 0, &deadline, true)
	setRow(t, failed, "status = 'failed', error = 'internal'")
	for _, id := range []uuid.UUID{expired, cancelled} {
		sendDead(t, js, deadSubject(hid), &intelpb.DeadLetter{JobId: id.String(), Type: "run_query", SubjectHospital: hid.String(),
			Code: "deadline_exceeded", Reason: "expired"})
	}
	sendDead(t, js, deadSubject(hid), &intelpb.DeadLetter{JobId: running.String(), Type: "run_query", SubjectHospital: hid.String(),
		Code: "unavailable", Reason: "unavailable"})
	sendDead(t, js, deadSubject(hid), &intelpb.DeadLetter{JobId: failed.String(), Type: "run_query", SubjectHospital: hid.String(),
		Code: "internal", Reason: "internal"})
	waitFor(t, "four letters handled", func() bool {
		return c.Metrics.DeadReceived.Load() >= 4 && stateOf(t, expired).status == "failed"
	})

	if s := stateOf(t, expired); s.errCode == nil || *s.errCode != "deadline_exceeded" {
		t.Fatalf("expired row %+v", s)
	}
	if n := len(auditFor(t, "query.failed", expired.String())); n != 1 {
		t.Fatalf("expired query.failed %d", n)
	}
	if s := stateOf(t, cancelled); s.status != "cancelled" {
		t.Fatalf("a finished row was overwritten: %+v", s)
	}
	if s := stateOf(t, running); s.status != "running" {
		t.Fatalf("a running row was touched: %+v", s)
	}
	if s := stateOf(t, failed); s.status != "failed" || *s.errCode != "internal" {
		t.Fatalf("failed row %+v", s)
	}
	// A letter that changed nothing writes nothing to the chain.
	for _, id := range []uuid.UUID{cancelled, running, failed} {
		if n := auditCount(t, id); n != 0 {
			t.Fatalf("%d audit rows for a row the letter didn't change", n)
		}
	}
	for _, id := range []uuid.UUID{expired, cancelled, running, failed} {
		if n := len(auditFor(t, "intelligence.refused", id.String())); n != 0 {
			t.Fatalf("intelligence.refused for a non-refusal")
		}
	}
	if c.Metrics.DeadRefused.Load() != 0 || c.Metrics.DeadExpired.Load() != 1 || c.Metrics.DeadIgnored.Load() != 3 {
		t.Fatalf("metrics refused %d expired %d ignored %d", c.Metrics.DeadRefused.Load(), c.Metrics.DeadExpired.Load(),
			c.Metrics.DeadIgnored.Load())
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("an expired job raised an error-level log: %s", logs.String())
	}
}

// F24: two core copies share core_dead: every letter is handled once.
func TestTwoCoreCopiesShareDeadLetters(t *testing.T) {
	c1, _ := newClient(t)
	c2, _ := newClient(t)
	c1.Metrics, c2.Metrics = &Metrics{}, &Metrics{}
	runConsumer(t, c1)
	runConsumer(t, c2)
	js := workerJS(t)
	hid := seedHospital(t, "active")
	u := seedUser(t, hid, true, authz.RoleClinician)
	deadline := time.Now().Add(5 * time.Minute)
	var ids []uuid.UUID
	for i := 0; i < 40; i++ {
		q := seedQuery(t, hid, u, 0, &deadline, true)
		ids = append(ids, q)
		sendDead(t, js, deadSubject(hid), &intelpb.DeadLetter{JobId: q.String(), Type: "run_query", SubjectHospital: hid.String(),
			Code: "not_found"})
	}
	waitFor(t, "every row marked", func() bool {
		for _, q := range ids {
			if stateOf(t, q).status != "failed" {
				return false
			}
		}
		return true
	})
	time.Sleep(500 * time.Millisecond)
	for _, q := range ids {
		if n := len(auditFor(t, "job.dead_lettered", q.String())); n != 1 {
			t.Fatalf("job %s dead-lettered %d times", q, n)
		}
	}
	if c1.Metrics.DeadReceived.Load() == 0 || c2.Metrics.DeadReceived.Load() == 0 {
		t.Logf("one copy took every letter (%d / %d): allowed, but the split is the point",
			c1.Metrics.DeadReceived.Load(), c2.Metrics.DeadReceived.Load())
	}
}

// A24 (core side, with Go stand-ins for the worker): a job signed with K1
// waits in JOBS; K1 is removed from the trusted file (keys untrust
// --emergency) and core signs with K2; the worker refuses the job
// (signature) and dead-letters it with kid K1; core re-checks the creator,
// re-mints with K2 (Republish) under a new message id, and audits
// job.dead_lettered and job.retried; the second delivery verifies. A
// signature letter naming a kid that is still trusted is a plain refusal.
func TestDeadLetterReMintsAfterEmergencyRotation(t *testing.T) {
	ctx := context.Background()
	drainJobs(t, sharedNATS.PipelineURL)
	_, k2, _ := ed25519.GenerateKey(rand.Reader)
	signer2 := reqtoken.Signer{Key: k2, KeyID: reqtoken.KeyID(k2.Public().(ed25519.PublicKey))}
	old, _ := newClient(t) // signs with K1 (signer)
	c, _ := newClient(t)
	c.Signer = signer2
	c.Trusted = NewTrustedKeys(signer2.PublicKeys()) // K1 gone
	c.Metrics = &Metrics{}

	hid := seedHospital(t, "active")
	u := seedUser(t, hid, true, authz.RoleClinician)
	deadline := time.Now().Truncate(time.Second).Add(5 * time.Minute)
	q := seedQuery(t, hid, u, 0, &deadline, true)
	caller := Caller{Sub: u.String(), Hid: hid.String(), Rid: "rid-" + q.String(), Roles: []string{authz.RoleClinician}}
	if err := old.PublishJob(ctx, caller, runQuery, q, &intelpb.RunQueryJob{QueryId: q.String()}, deadline); err != nil {
		t.Fatal(err)
	}
	// The worker, trusting K2 only, refuses the K1 job.
	first := jobsFor(drainJobs(t, sharedNATS.PipelineURL), "jobs.v1."+hid.String()+".run_query")
	if len(first) != 1 {
		t.Fatalf("%d jobs", len(first))
	}
	if _, err := reqtoken.VerifyAt(first[0].header.Get("Mhai-Request-Token"), signer2.PublicKeys(),
		reqtoken.Expect{Aud: "pipeline", Subject: first[0].subject, Kind: reqtoken.KindJob}, first[0].data, time.Now()); err == nil {
		t.Fatal("a K1 token verified against K2")
	}
	runConsumer(t, c)
	sendDead(t, workerJS(t), deadSubject(hid), &intelpb.DeadLetter{JobId: q.String(), Type: "run_query",
		SubjectHospital: hid.String(), Attempts: 1, Code: "unauthorized", Reason: "signature", Kid: signer.KeyID})
	waitFor(t, "job.retried", func() bool { return len(auditFor(t, "job.retried", q.String())) == 1 })

	second := jobsFor(drainJobs(t, sharedNATS.PipelineURL), "jobs.v1."+hid.String()+".run_query")
	if len(second) != 1 || second[0].header.Get("Nats-Msg-Id") != RetryMsgID(q, 0) {
		t.Fatalf("re-minted jobs %d, msg id %q", len(second), func() string {
			if len(second) > 0 {
				return second[0].header.Get("Nats-Msg-Id")
			}
			return ""
		}())
	}
	cl, err := reqtoken.VerifyAt(second[0].header.Get("Mhai-Request-Token"), signer2.PublicKeys(),
		reqtoken.Expect{Aud: "pipeline", Subject: second[0].subject, Kind: reqtoken.KindJob}, second[0].data, time.Now())
	if err != nil || cl.KeyID != signer2.KeyID || cl.Sub != u.String() || cl.Hid != hid.String() {
		t.Fatalf("second delivery: %+v %v", cl, err)
	}
	if s := stateOf(t, q); s.status != "queued" || !s.published {
		t.Fatalf("row %+v", s)
	}
	if rows := auditFor(t, "job.dead_lettered", q.String()); len(rows) != 1 || rows[0].payload["kid"] != signer.KeyID {
		t.Fatalf("dead_lettered %+v", rows)
	}
	if rows := auditFor(t, "job.retried", q.String()); rows[0].payload["msg_id"] != RetryMsgID(q, 0) || rows[0].actor != nil {
		t.Fatalf("retried %+v", rows)
	}
	if n := len(auditFor(t, "intelligence.refused", q.String())); n != 0 {
		t.Fatalf("a rotation re-mint was audited as a refusal")
	}

	// Review fix 3: the same letter replayed (or forged again) doesn't
	// re-mint the row a second time for that kid.
	before := c.Metrics.DeadReceived.Load()
	for i := 0; i < 2; i++ {
		sendDead(t, workerJS(t), deadSubject(hid), &intelpb.DeadLetter{JobId: q.String(), Type: "run_query",
			SubjectHospital: hid.String(), Attempts: 2, Code: "unauthorized", Reason: "signature", Kid: signer.KeyID})
	}
	waitFor(t, "two replays", lettersSeen(c.Metrics, before+2))
	if n := len(auditFor(t, "job.retried", q.String())); n != 1 || c.Metrics.DeadRetried.Load() != 1 {
		t.Fatalf("re-minted again: %d job.retried, %d retried", n, c.Metrics.DeadRetried.Load())
	}
	if n := len(jobsFor(drainJobs(t, sharedNATS.PipelineURL), "jobs.v1."+hid.String()+".run_query")); n != 0 {
		t.Fatalf("a replay published %d jobs", n)
	}
	if s := stateOf(t, q); s.status != "queued" {
		t.Fatalf("a replay changed the row: %+v", s)
	}

	// A signature refusal with a kid core still trusts is not a rotation.
	q2 := seedQuery(t, hid, u, 0, &deadline, true)
	sendDead(t, workerJS(t), deadSubject(hid), &intelpb.DeadLetter{JobId: q2.String(), Type: "run_query",
		SubjectHospital: hid.String(), Code: "unauthorized", Reason: "signature", Kid: signer2.KeyID})
	waitFor(t, "q2 failed", func() bool { return stateOf(t, q2).status == "failed" })
	if n := len(auditFor(t, "job.retried", q2.String())); n != 0 {
		t.Fatal("re-minted for a trusted kid")
	}
	if n := len(auditFor(t, "intelligence.refused", q2.String())); n != 1 {
		t.Fatal("a real signature refusal wasn't audited")
	}

	// The creator deactivated before the re-mint: no new token. (The
	// deactivation trigger has already cancelled the row; the letter
	// leaves it so.)
	q3 := seedQuery(t, hid, u, 0, &deadline, true)
	if _, err := super.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1`, u); err != nil {
		t.Fatal(err)
	}
	seen := c.Metrics.DeadReceived.Load()
	sendDead(t, workerJS(t), deadSubject(hid), &intelpb.DeadLetter{JobId: q3.String(), Type: "run_query",
		SubjectHospital: hid.String(), Code: "unauthorized", Reason: "signature", Kid: signer.KeyID})
	waitFor(t, "q3 letter", lettersSeen(c.Metrics, seen+1))
	if n := len(auditFor(t, "job.retried", q3.String())); n != 0 {
		t.Fatal("re-minted for a deactivated creator")
	}
	if s := stateOf(t, q3); s.status != "cancelled" {
		t.Fatalf("q3 %+v", s)
	}
	if n := len(auditFor(t, "job.dead_lettered", q3.String())); n != 0 {
		t.Fatal("a letter for a cancelled row was audited")
	}
}

// A24 end to end needs the Python pipeline worker reloading its trusted
// file (SIGHUP) after core keys untrust --emergency. It runs on Task 15's
// compose stack and in the intelligence-boundary CI job.
func TestEmergencyRotationReMintsDeadLetteredJob(t *testing.T) {
	// Plan 3 T15 left this skipped: on the smoke stack it needs a key
	// rotation with SIGHUP to core and the workers mid-run, which the smoke
	// test doesn't do. Each half is tested: core's re-mint in
	// TestDeadLetterReMintsAfterEmergencyRotation, the worker's refusal of a
	// removed kid in test_reload_drops_removed_kid (Python, boundary).
	t.Skip("the whole-stack emergency rotation is not in the smoke test; " +
		"see TestDeadLetterReMintsAfterEmergencyRotation and test_reload_drops_removed_kid")
}

// Plan 3 T5 review, tied in 00009: mhai_jobs writes audit rows only about
// a job of the row's own hospital, with no actor.
func TestJobsAuditTiedToRowHospital(t *testing.T) {
	ctx := context.Background()
	hid := seedHospital(t, "active")
	other := seedHospital(t, "active")
	u := seedUser(t, hid, true, authz.RoleClinician)
	deadline := time.Now().Add(5 * time.Minute)
	q := seedQuery(t, hid, u, 0, &deadline, true)
	write := func(e audit.Event) error {
		return db.WithJobs(ctx, app, func(tx pgx.Tx) error { return audit.Write(ctx, tx, e) })
	}
	ok := audit.Event{HospitalID: hid.String(), Action: "job.dead_lettered", Entity: "query", EntityID: q.String()}
	if err := write(ok); err != nil {
		t.Fatalf("own job: %v", err)
	}
	for name, e := range map[string]audit.Event{
		"another hospital": {HospitalID: other.String(), Action: "job.dead_lettered", Entity: "query", EntityID: q.String()},
		"an actor":         {ActorID: u.String(), HospitalID: hid.String(), Action: "job.dead_lettered", Entity: "query", EntityID: q.String()},
		"unknown job":      {HospitalID: hid.String(), Action: "job.dead_lettered", Entity: "query", EntityID: uuid.NewString()},
		"not a job id":     {HospitalID: hid.String(), Action: "job.dead_lettered", Entity: "query", EntityID: "x"},
		"other entity":     {HospitalID: hid.String(), Action: "job.dead_lettered", Entity: "user", EntityID: q.String()},
		"no hospital":      {Action: "job.dead_lettered", Entity: "query", EntityID: q.String()},
	} {
		if err := write(e); err == nil {
			t.Errorf("%s: written", name)
		}
	}
}

// auditCount is every audit row about job id.
func auditCount(t *testing.T, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE entity_id = $1`, id.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Review fix 1: refusal letters replayed for a completed job, and one for
// another hospital's completed job, write nothing to the chain and raise
// no alert.
func TestDeadLetterReplayOnFinishedJobWritesNothing(t *testing.T) {
	c, _ := newClient(t)
	c.Metrics = &Metrics{}
	runConsumer(t, c)
	js := workerJS(t)
	hid := seedHospital(t, "active")
	other := seedHospital(t, "active")
	u := seedUser(t, hid, true, authz.RoleClinician)
	uo := seedUser(t, other, true, authz.RoleClinician)
	deadline := time.Now().Add(5 * time.Minute)
	done := seedQuery(t, hid, u, 0, &deadline, true)
	setRow(t, done, "status = 'completed', completed_at = now()")
	theirs := seedQuery(t, other, uo, 0, &deadline, true)
	setRow(t, theirs, "status = 'completed', completed_at = now()")
	letter := &intelpb.DeadLetter{JobId: done.String(), Type: "run_query", SubjectHospital: hid.String(), Code: "unauthorized", Reason: "binding"}
	sendDead(t, js, deadSubject(hid), letter)
	sendDead(t, js, deadSubject(hid), letter)
	sendDead(t, js, deadSubject(other), &intelpb.DeadLetter{JobId: theirs.String(), Type: "run_query", SubjectHospital: other.String(),
		Code: "forbidden", Reason: "forbidden"})
	waitFor(t, "three letters", lettersSeen(c.Metrics, 3))
	time.Sleep(200 * time.Millisecond)
	for _, id := range []uuid.UUID{done, theirs} {
		if n := auditCount(t, id); n != 0 {
			t.Fatalf("%d audit rows for a finished job", n)
		}
		if s := stateOf(t, id); s.status != "completed" {
			t.Fatalf("finished row changed: %+v", s)
		}
	}
	if c.Metrics.DeadRefused.Load() != 0 || c.Metrics.DeadIgnored.Load() != 3 {
		t.Fatalf("refused %d ignored %d", c.Metrics.DeadRefused.Load(), c.Metrics.DeadIgnored.Load())
	}
}
