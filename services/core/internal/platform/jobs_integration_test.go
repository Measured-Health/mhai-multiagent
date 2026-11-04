//go:build integration

package platform_test

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intel"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/platform"
)

// Plan 4 Task 12: the failed jobs list, retry and cancel through core's
// whole chain (spec §6.2; contract §14.8.3; C16, C18, C26-C29, C32, C33).
// Every name, id and string here is synthetic.

const (
	testQueryCap = 10
	cancelMarker = "SYNTHETIC-CANCEL-NOTE-5d2a"
)

// recJS records what JetStream accepted (with its Nats-Msg-Id header).
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

// msgIDsFor are the Nats-Msg-Ids published for job id, in order.
func (r *recJS) msgIDsFor(id uuid.UUID) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, m := range r.msgs {
		if v := m.Header.Get("Nats-Msg-Id"); strings.HasPrefix(v, id.String()) {
			out = append(out, m.Subject+" "+v)
		}
	}
	return out
}

// syncBuffer is a log sink the handlers may write from many goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// --- seeds ---------------------------------------------------------------

// seedJobQuestion adds a question of hid by creator, as the superuser, in
// status with error code errCode ("" for none), created an hour ago.
func seedJobQuestion(t *testing.T, hid, creator uuid.UUID, status, errCode string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var e any
	if errCode != "" {
		e = errCode
	}
	var done any
	if status == "failed" || status == "completed" || status == "cancelled" {
		done = time.Now().Add(-time.Minute)
	}
	if _, err := super.Exec(context.Background(), `INSERT INTO clinical_queries
		(id, hospital_id, clinical_question, urgency, status, error, attempts, created_by, request_id, created_at, completed_at, deadline_at)
		VALUES ($1, $2, $7, 'routine', $3, $4, 5, $5, $8, now() - interval '1 hour', $6,
		        now() - interval '50 minutes')`, id, hid, status, e, creator, done, "Synthetic question "+id.String(), "rid-q-"+id.String()); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedCorrection adds an approved correction of hid (submitted by
// submitter, approved by decider as the gateway does) on a completed
// question.
func seedCorrection(t *testing.T, hid, submitter, decider uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	q := seedJobQuestion(t, hid, submitter, "completed", "")
	id := uuid.New()
	if _, err := super.Exec(ctx, `INSERT INTO feedback_events (id, hospital_id, query_id, submitted_by, action, status, reason, corrected_diagnosis)
		VALUES ($1, $2, $3, $4, 'correct', 'pending_review', 'synthetic reason', 'synthetic diagnosis')`, id, hid, q, submitter); err != nil {
		t.Fatal(err)
	}
	if err := db.WithHospitalUser(ctx, app, hid, decider, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE feedback_events SET status = 'approved', decided_by = $2, decided_at = now() WHERE id = $1`, id, decider)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedApproveJob adds an approve_correction job of hid for correction by
// creator (the approver), in status with errCode, created an hour ago.
func seedApproveJob(t *testing.T, hid, correction, creator uuid.UUID, status, errCode string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var e, failed any
	if errCode != "" {
		e = errCode
	}
	if status == "failed" {
		failed = time.Now().Add(-time.Minute)
	}
	deadline := time.Now().Add(-50 * time.Minute)
	if status == "queued" || status == "running" {
		deadline = time.Now().Add(5 * time.Minute)
	}
	if _, err := super.Exec(context.Background(), `INSERT INTO jobs
		(id, hospital_id, type, subject_id, created_by, request_id, status, error, attempts, created_at, deadline_at, failed_at, published_at)
		VALUES ($1, $2, 'approve_correction', $3, $4, $9, $5, $6, 5, now() - interval '1 hour', $7, $8, now() - interval '1 hour')`,
		id, hid, correction, creator, status, e, deadline, failed, "rid-j-"+id.String()); err != nil {
		t.Fatal(err)
	}
	return id
}

// clinicalSite is an active hospital with a submitter and an approver.
type clinicalSite struct {
	hid                 uuid.UUID
	submitter, approver user
}

func newSite(t *testing.T) clinicalSite {
	t.Helper()
	h := seedHospital(t, "active")
	return clinicalSite{hid: h, submitter: seedUser(t, h, "", authz.RoleClinician), approver: seedUser(t, h, "", authz.RoleClinician)}
}

// failedApproval is a failed approve_correction job of s, and its correction.
func (s clinicalSite) failedApproval(t *testing.T, errCode string) (job, correction uuid.UUID) {
	t.Helper()
	c := seedCorrection(t, s.hid, s.submitter.id, s.approver.id)
	return seedApproveJob(t, s.hid, c, s.approver.id, "failed", errCode), c
}

func qid(id uuid.UUID) string { return "q_" + id.String() }
func jid(id uuid.UUID) string { return "j_" + id.String() }

func idsBody(ids ...string) string {
	b, _ := json.Marshal(map[string]any{"ids": ids})
	return string(b)
}

// strs is a JSON array of strings from a reply.
func strs(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func retry(t *testing.T, tok string, ids ...string) resp {
	return post(t, "/platform/jobs/retry", tok, idsBody(ids...))
}

func queryState(t *testing.T, id uuid.UUID) (status string, retries int, published bool) {
	t.Helper()
	if err := super.QueryRow(context.Background(), `SELECT status, retry_count, published_at IS NOT NULL FROM clinical_queries WHERE id = $1`, id).
		Scan(&status, &retries, &published); err != nil {
		t.Fatal(err)
	}
	return
}

func jobState(t *testing.T, id uuid.UUID) (status string, retries int, published bool) {
	t.Helper()
	if err := super.QueryRow(context.Background(), `SELECT status, retry_count, published_at IS NOT NULL FROM jobs WHERE id = $1`, id).
		Scan(&status, &retries, &published); err != nil {
		t.Fatal(err)
	}
	return
}

// jobAudits are the job.retried and job.cancelled rows u wrote.
func jobAudits(t *testing.T, u user) []auditRow {
	t.Helper()
	rows, err := super.Query(context.Background(), `SELECT action, COALESCE(actor_id::text, ''), COALESCE(entity, ''), COALESCE(entity_id, ''),
		hospital_id::text, payload FROM audit_log WHERE actor_id = $1 AND action IN ('job.retried', 'job.cancelled') ORDER BY id`, u.id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var a auditRow
		var raw []byte
		if err := rows.Scan(&a.action, &a.actor, &a.entity, &a.entityID, &a.hospital, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &a.payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// backupCode gives u one backup code that step-up takes: the hash as
// auth stores it (HKDF of MFA_ENCRYPTION_KEY, HMAC-SHA256).
func backupCode(t *testing.T, u user) string {
	t.Helper()
	code := "synth" + uniq()
	k, err := hkdf.Key(sha256.New, []byte(testMFAKey), nil, "mhai-mfa-backup-hmac-v1", 32)
	if err != nil {
		t.Fatal(err)
	}
	m := hmac.New(sha256.New, k)
	m.Write([]byte(code))
	if _, err := super.Exec(context.Background(), `UPDATE user_mfa SET backup_code_hashes = array_append(backup_code_hashes, $2)
		WHERE user_id = $1`, u.id, hex.EncodeToString(m.Sum(nil))); err != nil {
		t.Fatal(err)
	}
	return code
}

func cancelBody(ids []string, reason string, note *string, backup string) string {
	m := map[string]any{"ids": ids, "reason": reason}
	if note != nil {
		m["note"] = *note
	}
	if backup != "" {
		m["backup_code"] = backup
	}
	b, _ := json.Marshal(m)
	return string(b)
}

var jobKeys = []string{"id", "type", "hospital", "created_at", "failed_at", "attempts", "kind", "status", "cancelled_by", "cancelled_at"}

// --- the list ------------------------------------------------------------

// C18: each job carries exactly the contract's keys: never the subject, the
// user, the request id or the error message. Filters, the 30-day window
// and the cursor.
func TestJobsListShape(t *testing.T) {
	s := newSite(t)
	admin := platformAdmin(t)
	tok := mint(t, admin)
	q := seedJobQuestion(t, s.hid, s.submitter.id, "failed", "deadline_exceeded")
	j, corr := s.failedApproval(t, "internal")
	cq := seedJobQuestion(t, s.hid, s.submitter.id, "cancelled", "")
	seedJobQuestion(t, s.hid, s.submitter.id, "completed", "") // never listed
	old := seedJobQuestion(t, s.hid, s.submitter.id, "cancelled", "")
	if _, err := super.Exec(context.Background(), `UPDATE clinical_queries SET completed_at = now() - interval '31 days' WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	base := "/platform/jobs?hospital=" + s.hid.String()
	r := get(t, base+"&status=all", tok)
	ok(t, "list", r)
	sameKeys(t, "list", r.body, "jobs", "next_cursor")
	got := map[string]map[string]any{}
	for _, x := range r.body["jobs"].([]any) {
		jb := x.(map[string]any)
		sameKeys(t, "job", jb, jobKeys...)
		got[jb["id"].(string)] = jb
	}
	if len(got) != 3 || got[qid(q)] == nil || got[jid(j)] == nil || got[qid(cq)] == nil {
		t.Fatalf("listed %v", keysOfJobs(got))
	}
	if g := got[qid(q)]; g["type"] != "run_query" || g["status"] != "failed" || g["kind"] != "timeout" ||
		g["attempts"].(float64) != 5 || g["cancelled_by"] != nil || g["cancelled_at"] != nil {
		t.Errorf("question: %v", g)
	}
	h := got[qid(q)]["hospital"].(map[string]any)
	sameKeys(t, "hospital", h, "id", "name")
	if h["id"] != s.hid.String() || !strings.HasPrefix(h["name"].(string), "Synthetic General") {
		t.Errorf("hospital: %v", h)
	}
	if g := got[jid(j)]; g["type"] != "approve_correction" || g["status"] != "failed" || g["kind"] != "other" {
		t.Errorf("approval: %v", g)
	}
	if g := got[qid(cq)]; g["status"] != "cancelled" || g["kind"] != "other" || g["cancelled_at"] == nil {
		t.Errorf("cancelled: %v", g)
	}
	// Never the subject, the creator, a request id, the question or a code.
	for _, leak := range []string{corr.String(), s.submitter.id.String(), s.approver.id.String(), "rid-", "Synthetic question",
		"deadline_exceeded", "internal\"", s.submitter.name} {
		if strings.Contains(r.raw, leak) {
			t.Errorf("the list carries %q", leak)
		}
	}
	// The default is failed; the filters narrow it.
	for path, want := range map[string][]string{
		base:                              {qid(q), jid(j)},
		base + "&status=cancelled":        {qid(cq)},
		base + "&type=run_query":          {qid(q)},
		base + "&type=approve_correction": {jid(j)},
		base + "&type=ingest_document":    {},
		base + "&kind=timeout":            {qid(q)},
		base + "&kind=other":              {jid(j)},
		base + "&kind=invalid":            {},
		base + "&status=retried":          {},
	} {
		r := get(t, path, tok)
		ok(t, path, r)
		var ids []string
		for _, x := range r.body["jobs"].([]any) {
			ids = append(ids, x.(map[string]any)["id"].(string))
		}
		if strings.Join(sortedCopy(ids), ",") != strings.Join(sortedCopy(want), ",") {
			t.Errorf("%s: %v, want %v", path, ids, want)
		}
	}
	// Auditors read it too.
	ok(t, "auditor", get(t, base, mint(t, auditor(t))))
	for _, bad := range []string{"?status=running", "?type=nope", "?hospital=x", "?kind=slow", "?limit=5", "?cursor=zz",
		"?status=failed&status=all"} {
		problem(t, "list "+bad, get(t, "/platform/jobs"+bad, tok), 422, "validation_failed")
	}
}

func keysOfJobs(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sortedCopy(s []string) []string {
	out := append([]string{}, s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// C33 (list part): pages of 50; the cursor is bound to the session and
// the filters.
func TestJobsListCursor(t *testing.T) {
	s := newSite(t)
	u := platformAdmin(t)
	tok := mint(t, u)
	for i := 0; i < platform.JobsPage+3; i++ {
		seedJobQuestion(t, s.hid, s.submitter.id, "failed", "internal")
	}
	base := "/platform/jobs?hospital=" + s.hid.String()
	r := get(t, base, tok)
	ok(t, "page 1", r)
	next, _ := r.body["next_cursor"].(string)
	if len(r.body["jobs"].([]any)) != platform.JobsPage || len(next) != 80 {
		t.Fatalf("page 1: %d jobs, cursor %q", len(r.body["jobs"].([]any)), next)
	}
	r2 := get(t, base+"&cursor="+next, tok)
	ok(t, "page 2", r2)
	if len(r2.body["jobs"].([]any)) != 3 || r2.body["next_cursor"] != nil {
		t.Fatalf("page 2: %s", r2.raw)
	}
	seen := map[string]bool{}
	for _, p := range []resp{r, r2} {
		for _, x := range p.body["jobs"].([]any) {
			id := x.(map[string]any)["id"].(string)
			if seen[id] {
				t.Errorf("%s on two pages", id)
			}
			seen[id] = true
		}
	}
	problem(t, "cursor with other filters", get(t, base+"&kind=other&cursor="+next, tok), 422, "validation_failed")
	problem(t, "another session's cursor", get(t, base+"&cursor="+next, mint(t, u)), 422, "validation_failed")
	problem(t, "forged cursor", get(t, base+"&cursor="+strings.Repeat("0", 80), tok), 422, "validation_failed")
}

// --- retry ---------------------------------------------------------------

// A retry re-queues a failed question and a failed approval (one failed
// unauthorized, as an emergency key rotation leaves them: carry item 7),
// and publishes each as its creator under a new message id; the list then
// shows them retrying. A second retry skips them (no longer failed).
func TestRetryRequeuesAndPublishes(t *testing.T) {
	s := newSite(t)
	tok := mint(t, platformAdmin(t))
	q := seedJobQuestion(t, s.hid, s.submitter.id, "failed", "unavailable")
	j, _ := s.failedApproval(t, "unauthorized")
	var queuedBefore time.Time
	if err := super.QueryRow(context.Background(), `SELECT queued_at FROM jobs WHERE id = $1`, j).Scan(&queuedBefore); err != nil {
		t.Fatal(err)
	}
	r := retry(t, tok, qid(q), jid(j))
	ok(t, "retry", r)
	sameKeys(t, "retry", r.body, "retried", "skipped")
	if got := strs(r.body["retried"]); strings.Join(got, ",") != qid(q)+","+jid(j) || len(strs(r.body["skipped"])) != 0 {
		t.Fatalf("retry: %s", r.raw)
	}
	if st, n, pub := queryState(t, q); st != "queued" || n != 1 || !pub {
		t.Errorf("question: %s retry_count %d published %v", st, n, pub)
	}
	if st, n, pub := jobState(t, j); st != "queued" || n != 1 || !pub {
		t.Errorf("approval: %s retry_count %d published %v", st, n, pub)
	}
	var queuedAfter time.Time
	var errCode *string
	if err := super.QueryRow(context.Background(), `SELECT queued_at, error FROM jobs WHERE id = $1`, j).Scan(&queuedAfter, &errCode); err != nil {
		t.Fatal(err)
	}
	if !queuedAfter.After(queuedBefore) || errCode != nil {
		t.Errorf("approval queued_at %v -> %v, error %v", queuedBefore, queuedAfter, errCode)
	}
	if got := published.msgIDsFor(q); len(got) != 1 || got[0] != "jobs.v1."+s.hid.String()+".run_query "+q.String()+".p1" {
		t.Errorf("question published: %v", got)
	}
	if got := published.msgIDsFor(j); len(got) != 1 || got[0] != "jobs.v1."+s.hid.String()+".approve_correction "+j.String()+".p1" {
		t.Errorf("approval published: %v", got)
	}
	l := get(t, "/platform/jobs?status=retrying&hospital="+s.hid.String(), tok)
	ok(t, "retrying", l)
	if n := len(l.body["jobs"].([]any)); n != 2 {
		t.Errorf("retrying: %s", l.raw)
	}
	r = retry(t, tok, qid(q), jid(j))
	ok(t, "retry again", r)
	if len(strs(r.body["retried"])) != 0 || len(strs(r.body["skipped"])) != 2 {
		t.Errorf("retry again: %s", r.raw)
	}
}

// A retried question starts clean: the failed run's progress, stage, start
// time and lease are cleared.
func TestRetryClearsRunProgress(t *testing.T) {
	s := newSite(t)
	tok := mint(t, platformAdmin(t))
	q := seedJobQuestion(t, s.hid, s.submitter.id, "failed", "internal")
	if _, err := super.Exec(context.Background(), `UPDATE clinical_queries
		SET progress = 0.6, current_stage = 'answer', started_at = now() - interval '5 minutes', lease_until = now() - interval '1 minute'
		WHERE id = $1`, q); err != nil {
		t.Fatal(err)
	}
	ok(t, "retry", retry(t, tok, qid(q)))
	var progress float64
	var stage *string
	var started, lease *time.Time
	if err := super.QueryRow(context.Background(), `SELECT progress, current_stage, started_at, lease_until FROM clinical_queries WHERE id = $1`, q).
		Scan(&progress, &stage, &started, &lease); err != nil {
		t.Fatal(err)
	}
	if st, n, _ := queryState(t, q); st != "queued" || n != 1 {
		t.Fatalf("question: %s retry_count %d", st, n)
	}
	if progress != 0 || stage != nil || started != nil || lease != nil {
		t.Errorf("progress %v stage %v started %v lease %v, want cleared", progress, stage, started, lease)
	}
}

// C27: a creator who was deactivated, or who no longer holds clinician, is
// never minted as: skipped, the row stays failed, nothing published, and
// nothing audited.
func TestRetryDeactivatedCreatorSkipped(t *testing.T) {
	s := newSite(t)
	admin := platformAdmin(t)
	tok := mint(t, admin)
	q := seedJobQuestion(t, s.submitter.hid, s.submitter.id, "failed", "internal")
	j, _ := s.failedApproval(t, "internal")
	for _, u := range []uuid.UUID{s.submitter.id, s.approver.id} {
		if _, err := super.Exec(context.Background(), `UPDATE users SET is_active = false WHERE id = $1`, u); err != nil {
			t.Fatal(err)
		}
	}
	r := retry(t, tok, qid(q), jid(j))
	ok(t, "retry", r)
	if len(strs(r.body["retried"])) != 0 || strings.Join(strs(r.body["skipped"]), ",") != qid(q)+","+jid(j) {
		t.Fatalf("retry: %s", r.raw)
	}
	if st, n, _ := queryState(t, q); st != "failed" || n != 0 {
		t.Errorf("question: %s %d", st, n)
	}
	if st, n, _ := jobState(t, j); st != "failed" || n != 0 {
		t.Errorf("approval: %s %d", st, n)
	}
	if len(published.msgIDsFor(q))+len(published.msgIDsFor(j)) != 0 {
		t.Error("published for a deactivated creator")
	}
	if a := jobAudits(t, admin); len(a) != 0 {
		t.Errorf("audited: %v", a)
	}
}

func TestRetryLostRoleSkipped(t *testing.T) {
	s := newSite(t)
	tok := mint(t, platformAdmin(t))
	q := seedJobQuestion(t, s.hid, s.submitter.id, "failed", "internal")
	j, _ := s.failedApproval(t, "internal")
	for _, u := range []uuid.UUID{s.submitter.id, s.approver.id} {
		if _, err := super.Exec(context.Background(), `UPDATE user_roles SET role = 'hospital_admin' WHERE user_id = $1`, u); err != nil {
			t.Fatal(err)
		}
	}
	r := retry(t, tok, qid(q), jid(j))
	ok(t, "retry", r)
	if len(strs(r.body["retried"])) != 0 || len(strs(r.body["skipped"])) != 2 {
		t.Fatalf("retry: %s", r.raw)
	}
	if st, _, _ := queryState(t, q); st != "failed" {
		t.Errorf("question: %s", st)
	}
	if st, _, _ := jobState(t, j); st != "failed" {
		t.Errorf("approval: %s", st)
	}
	if len(published.msgIDsFor(q))+len(published.msgIDsFor(j)) != 0 {
		t.Error("published for a creator without the role")
	}
}

// C28: an approval whose correction no longer has a second person (forced,
// as the superuser with the CHECK lifted and triggers off, to submitted_by
// = decided_by = the job's creator) is skipped and nothing is published.
func TestRetrySecondPersonRechecked(t *testing.T) {
	s := newSite(t)
	tok := mint(t, platformAdmin(t))
	j, corr := s.failedApproval(t, "internal")
	ctx := context.Background()
	// The triggers are off only inside force's own transaction
	// (session_replication_role is transaction-local); the CHECK is
	// dropped for the test and always put back.
	force := func(submitter uuid.UUID) error {
		tx, err := super.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE feedback_events SET submitted_by = $2 WHERE id = $1`, corr, submitter); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	t.Cleanup(func() {
		// Non-fatal throughout, so each step is tried whatever the last did.
		if err := force(s.submitter.id); err != nil {
			t.Errorf("restore the submitter: %v", err)
		}
		_, _ = super.Exec(ctx, `SET session_replication_role = origin`)
		if _, err := super.Exec(ctx, `ALTER TABLE feedback_events ADD CONSTRAINT feedback_not_self_decided
			CHECK (decided_by IS NULL OR decided_by <> submitted_by)`); err != nil {
			t.Errorf("restore the CHECK: %v", err)
			if _, err := super.Exec(ctx, `ALTER TABLE feedback_events ADD CONSTRAINT feedback_not_self_decided
				CHECK (decided_by IS NULL OR decided_by <> submitted_by) NOT VALID`); err != nil {
				t.Errorf("restore the CHECK (not valid): %v", err)
			}
		}
	})
	if _, err := super.Exec(ctx, `ALTER TABLE feedback_events DROP CONSTRAINT feedback_not_self_decided`); err != nil {
		t.Fatal(err)
	}
	if err := force(s.approver.id); err != nil {
		t.Fatal(err)
	}
	// The check itself refuses the row as it stands.
	row := intel.JobRow{ID: j, HospitalID: s.hid, CreatedBy: s.approver.id, RequestID: "rid-j-" + j.String(),
		Type: "approve_correction", SubjectID: corr, Deadline: time.Now().Add(time.Minute)}
	if err := intel.Publishable(ctx, app, intelClient, row); !errors.Is(err, intel.ErrJobSkipped) {
		t.Errorf("Publishable: %v, want ErrJobSkipped", err)
	}
	r := retry(t, tok, jid(j))
	ok(t, "retry", r)
	if len(strs(r.body["retried"])) != 0 || strings.Join(strs(r.body["skipped"]), ",") != jid(j) {
		t.Fatalf("retry: %s", r.raw)
	}
	if st, n, _ := jobState(t, j); st != "failed" || n != 0 {
		t.Errorf("approval: %s retry_count %d", st, n)
	}
	if got := published.msgIDsFor(j); len(got) != 0 {
		t.Errorf("published: %v", got)
	}
}

// Carry item 4: a failed job whose apply_failed fact is still in the outbox
// is skipped (re-queueing it first would make the drainer refuse the
// fact); once drained, the fact is on the chain and the retry goes ahead.
func TestRetryWaitsForFacts(t *testing.T) {
	s := newSite(t)
	tok := mint(t, platformAdmin(t))
	j, corr := s.failedApproval(t, "internal")
	ctx := context.Background()
	if _, err := super.Exec(ctx, `INSERT INTO intelligence_audit_outbox (hospital_id, job_id, action, entity, entity_id, payload)
		VALUES ($1, $2, 'correction.apply_failed', 'correction', $3, $4::jsonb)`, s.hid, j, corr.String(),
		fmt.Sprintf(`{"correction_id":%q,"error":"internal","attempts":5}`, corr)); err != nil {
		t.Fatal(err)
	}
	r := retry(t, tok, jid(j))
	ok(t, "retry", r)
	if len(strs(r.body["retried"])) != 0 {
		t.Fatalf("retried before its facts drained: %s", r.raw)
	}
	for {
		n, err := intel.DrainOnce(ctx, app, nil)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	var facts int
	if err := super.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'correction.apply_failed' AND entity_id = $1
		AND payload->>'job_id' = $2`, corr.String(), j.String()).Scan(&facts); err != nil || facts != 1 {
		t.Fatalf("apply_failed on the chain: %d %v", facts, err)
	}
	r = retry(t, tok, jid(j))
	ok(t, "retry after the drain", r)
	if strings.Join(strs(r.body["retried"]), ",") != jid(j) {
		t.Fatalf("retry after the drain: %s", r.raw)
	}
}

// Carry item 1: a retried approval joins QueueJob's budget: with 20 live
// approvals of its hospital it is re-queued unpublished, behind them, and
// the sweeper publishes it once a slot frees. A question at its hospital's
// cap is skipped (the sweeper has no cap to wait on).
func TestRetryKeepsTheBudgets(t *testing.T) {
	s := newSite(t)
	tok := mint(t, platformAdmin(t))
	ctx := context.Background()
	var live []uuid.UUID
	for i := 0; i < 20; i++ {
		c := seedCorrection(t, s.hid, s.submitter.id, s.approver.id)
		live = append(live, seedApproveJob(t, s.hid, c, s.approver.id, "queued", ""))
	}
	j, _ := s.failedApproval(t, "internal")
	for i := 0; i < testQueryCap; i++ {
		id := seedJobQuestion(t, s.hid, s.submitter.id, "queued", "")
		if _, err := super.Exec(ctx, `UPDATE clinical_queries SET deadline_at = now() + interval '5 minutes', published_at = now() WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	q := seedJobQuestion(t, s.hid, s.submitter.id, "failed", "internal")
	r := retry(t, tok, jid(j), qid(q))
	ok(t, "retry", r)
	if strings.Join(strs(r.body["retried"]), ",") != jid(j) || strings.Join(strs(r.body["skipped"]), ",") != qid(q) {
		t.Fatalf("retry: %s", r.raw)
	}
	if st, n, pub := jobState(t, j); st != "queued" || n != 1 || pub {
		t.Errorf("approval past the budget: %s %d published %v", st, n, pub)
	}
	if got := published.msgIDsFor(j); len(got) != 0 {
		t.Errorf("published past the budget: %v", got)
	}
	if st, _, _ := queryState(t, q); st != "failed" {
		t.Errorf("question at the cap: %s", st)
	}
}

// The sweeper's age gate for a jobs row is queued_at (fix round 1): a job
// the platform just retried is not swept at once on its old created_at;
// past the gate the sweeper publishes it once, under its retry's id.
func TestRetryAndSweepPublishOnce(t *testing.T) {
	s := newSite(t)
	tok := mint(t, platformAdmin(t))
	ctx := context.Background()
	var live []uuid.UUID
	for i := 0; i < 20; i++ {
		c := seedCorrection(t, s.hid, s.submitter.id, s.approver.id)
		live = append(live, seedApproveJob(t, s.hid, c, s.approver.id, "queued", ""))
	}
	j, _ := s.failedApproval(t, "internal")
	r := retry(t, tok, jid(j))
	ok(t, "retry", r)
	if strings.Join(strs(r.body["retried"]), ",") != jid(j) {
		t.Fatalf("retry: %s", r.raw)
	}
	// A slot frees at once; the row was queued under 10 s ago.
	if _, err := super.Exec(ctx, `UPDATE jobs SET status = 'completed', completed_at = now() WHERE id = $1`, live[0]); err != nil {
		t.Fatal(err)
	}
	_, _ = intel.SweepOnce(ctx, app, intelClient)
	if _, _, pub := jobState(t, j); pub {
		t.Error("the sweeper published a job retried under 10 s ago")
	}
	if _, err := super.Exec(ctx, `UPDATE jobs SET queued_at = queued_at - interval '11 seconds' WHERE id = $1`, j); err != nil {
		t.Fatal(err)
	}
	_, _ = intel.SweepOnce(ctx, app, intelClient)
	_, _ = intel.SweepOnce(ctx, app, intelClient)
	if _, _, pub := jobState(t, j); !pub {
		t.Error("the sweeper didn't publish the retried job")
	}
	if got := published.msgIDsFor(j); len(got) != 1 || !strings.HasSuffix(got[0], j.String()+".p1") {
		t.Errorf("published: %v", got)
	}
}

// One job.retried and one job.cancelled per call, the admin's, with no
// hospital, no entity and job_count; the audit list shows them as
// jobs_retried and jobs_cancelled.
func TestRetryCancelOneAuditRowPerCall(t *testing.T) {
	s := newSite(t)
	admin := platformAdmin(t)
	tok := mint(t, admin)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, qid(seedJobQuestion(t, s.hid, s.submitter.id, "failed", "internal")))
	}
	ok(t, "retry", retry(t, tok, append(ids, qid(uuid.New()))...))
	c1 := qid(seedJobQuestion(t, s.hid, s.submitter.id, "failed", "internal"))
	j, _ := s.failedApproval(t, "internal")
	r := post(t, "/platform/jobs/cancel", tok, cancelBody([]string{c1, jid(j)}, "cant_succeed", nil, backupCode(t, admin)))
	ok(t, "cancel", r)
	a := jobAudits(t, admin)
	if len(a) != 2 {
		t.Fatalf("job rows: %+v", a)
	}
	for i, want := range []struct {
		action string
		count  float64
	}{{"job.retried", 3}, {"job.cancelled", 2}} {
		x := a[i]
		if x.action != want.action || x.actor != admin.id.String() || x.hospital != nil || x.entity != "" || x.entityID != "" ||
			x.payload["job_count"] != want.count || x.payload["job_id"] != nil {
			t.Errorf("row %d: %+v", i, x)
		}
	}
	if a[1].payload["reason"] != "cant_succeed" || len(a[1].payload) != 2 {
		t.Errorf("cancel payload: %v", a[1].payload)
	}
	l := get(t, "/platform/audit?type=platform&person="+admin.id.String(), mint(t, auditor(t)))
	ok(t, "audit list", l)
	kinds := map[string]float64{}
	for _, x := range l.body["events"].([]any) {
		e := x.(map[string]any)
		if f, ok := e["facts"].(map[string]any); ok {
			if n, ok := f["job_count"].(float64); ok {
				kinds[e["kind"].(string)] = n
			}
		}
	}
	if kinds["jobs_retried"] != 3 || kinds["jobs_cancelled"] != 2 {
		t.Errorf("audit list kinds: %v (%s)", kinds, l.raw)
	}
}

// C32 (retry part): 10 retry calls a minute per session; the 11th is 429,
// another session is not held up.
func TestRetryRateLimited(t *testing.T) {
	u := platformAdmin(t)
	tok := mint(t, u)
	for i := 0; i < platform.RetriesPerMinute; i++ {
		ok(t, fmt.Sprintf("retry %d", i+1), retry(t, tok, qid(uuid.New())))
	}
	problem(t, "11th retry", retry(t, tok, qid(uuid.New())), 429, "rate_limited")
	ok(t, "another session", retry(t, mint(t, u), qid(uuid.New())))
}

// --- cancel --------------------------------------------------------------

// C29: no code is 403 mfa_required, a wrong one 403 mfa_invalid, and
// nothing is cancelled; the right one cancels.
func TestCancelNeedsCode(t *testing.T) {
	s := newSite(t)
	admin := platformAdmin(t)
	tok := mint(t, admin)
	q := seedJobQuestion(t, s.hid, s.submitter.id, "failed", "internal")
	ids := []string{qid(q)}
	problem(t, "no code", post(t, "/platform/jobs/cancel", tok, cancelBody(ids, "no_longer_needed", nil, "")), 403, "mfa_required")
	// No code at all is not a code check: nothing is audited (R38).
	if n := mfaRows(t, admin, "auth.mfa.failed", "method", "step_up") + mfaRows(t, admin, "auth.mfa.locked", "reason", "account_attempts"); n != 0 {
		t.Errorf("no code audited %d rows", n)
	}
	problem(t, "wrong code", post(t, "/platform/jobs/cancel", tok, cancelBody(ids, "no_longer_needed", nil, "wrong"+uniq())), 403, "mfa_invalid")
	// A wrong code is audited as the recovery-codes step-up does (R38).
	if n := mfaRows(t, admin, "auth.mfa.failed", "method", "step_up"); n != 1 {
		t.Errorf("auth.mfa.failed rows after a wrong code: %d", n)
	}
	if st, _, _ := queryState(t, q); st != "failed" {
		t.Fatalf("cancelled without a code: %s", st)
	}
	r := post(t, "/platform/jobs/cancel", tok, cancelBody(ids, "no_longer_needed", nil, backupCode(t, admin)))
	ok(t, "cancel", r)
	if strings.Join(strs(r.body["cancelled"]), ",") != qid(q) {
		t.Fatalf("cancel: %s", r.raw)
	}
	if st, _, _ := queryState(t, q); st != "cancelled" {
		t.Errorf("question: %s", st)
	}
}

// R38: wrong codes past the limit lock step-up, audited auth.mfa.locked;
// nothing is cancelled.
func TestCancelStepUpLockAudited(t *testing.T) {
	s := newSite(t)
	admin := platformAdmin(t)
	tok := mint(t, admin)
	q := seedJobQuestion(t, s.hid, s.submitter.id, "failed", "internal")
	body := func() string { return cancelBody([]string{qid(q)}, "no_longer_needed", nil, "wrong"+uniq()) }
	for i := 0; i < auth.MaxMFAFailures; i++ {
		problem(t, fmt.Sprintf("wrong code %d", i+1), post(t, "/platform/jobs/cancel", tok, body()), 403, "mfa_invalid")
	}
	problem(t, "locked", post(t, "/platform/jobs/cancel", tok, body()), 429, "mfa_locked")
	if n := mfaRows(t, admin, "auth.mfa.failed", "method", "step_up"); n != auth.MaxMFAFailures {
		t.Errorf("auth.mfa.failed rows: %d", n)
	}
	if n := mfaRows(t, admin, "auth.mfa.locked", "reason", "account_attempts"); n != 1 {
		t.Errorf("auth.mfa.locked rows: %d", n)
	}
	if st, _, _ := queryState(t, q); st != "failed" {
		t.Errorf("question: %s", st)
	}
}

// mfaRows counts u's action rows whose payload has key = value.
func mfaRows(t *testing.T, u user, action, key, value string) int {
	t.Helper()
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE actor_id = $1 AND action = $2 AND payload->>$3 = $4`,
		u.id, action, key, value).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// C29: a running job is skipped; a failed and a queued one are cancelled,
// with who and when (a question's in query_cancellations), and the list
// shows who cancelled them. A cancelled job can't be retried.
func TestCancelRunningSkipped(t *testing.T) {
	s := newSite(t)
	admin := platformAdmin(t)
	tok := mint(t, admin)
	ctx := context.Background()
	running := seedJobQuestion(t, s.hid, s.submitter.id, "running", "")
	c := seedCorrection(t, s.hid, s.submitter.id, s.approver.id)
	runningJob := seedApproveJob(t, s.hid, c, s.approver.id, "running", "")
	failed := seedJobQuestion(t, s.hid, s.submitter.id, "failed", "internal")
	queuedJob, _ := s.failedApproval(t, "internal")
	if _, err := super.Exec(ctx, `UPDATE jobs SET status = 'queued', error = NULL, retry_count = 1, deadline_at = now() + interval '5 minutes' WHERE id = $1`, queuedJob); err != nil {
		t.Fatal(err)
	}
	r := post(t, "/platform/jobs/cancel", tok, cancelBody([]string{qid(running), jid(runningJob), qid(failed), jid(queuedJob)},
		"no_longer_needed", nil, backupCode(t, admin)))
	ok(t, "cancel", r)
	if strings.Join(strs(r.body["cancelled"]), ",") != qid(failed)+","+jid(queuedJob) ||
		strings.Join(strs(r.body["skipped"]), ",") != qid(running)+","+jid(runningJob) {
		t.Fatalf("cancel: %s", r.raw)
	}
	if st, _, _ := queryState(t, running); st != "running" {
		t.Errorf("running question: %s", st)
	}
	if st, _, _ := jobState(t, runningJob); st != "running" {
		t.Errorf("running job: %s", st)
	}
	var by uuid.UUID
	var reason string
	if err := super.QueryRow(ctx, `SELECT c.cancelled_by, q.cancel_reason FROM query_cancellations c JOIN clinical_queries q ON q.id = c.query_id
		WHERE c.query_id = $1`, failed).Scan(&by, &reason); err != nil || by != admin.id || reason != "no_longer_needed" {
		t.Errorf("question cancellation: %v %s %v", by, reason, err)
	}
	if err := super.QueryRow(ctx, `SELECT cancelled_by, cancel_reason FROM jobs WHERE id = $1`, queuedJob).Scan(&by, &reason); err != nil ||
		by != admin.id || reason != "no_longer_needed" {
		t.Errorf("job cancellation: %v %s %v", by, reason, err)
	}
	l := get(t, "/platform/jobs?status=cancelled&hospital="+s.hid.String(), tok)
	ok(t, "list", l)
	n := 0
	for _, x := range l.body["jobs"].([]any) {
		jb := x.(map[string]any)
		cb, _ := jb["cancelled_by"].(map[string]any)
		if cb == nil || cb["id"] != admin.id.String() || cb["name"] != admin.name || jb["cancelled_at"] == nil {
			t.Errorf("cancelled job: %v", jb)
		}
		n++
	}
	if n != 2 {
		t.Errorf("cancelled listed: %d", n)
	}
	// Cancelled is final: a retry skips it.
	rr := retry(t, tok, qid(failed), jid(queuedJob))
	ok(t, "retry cancelled", rr)
	if len(strs(rr.body["retried"])) != 0 {
		t.Errorf("retried a cancelled job: %s", rr.raw)
	}
}

// C16 (cancel note): the note is stored with the cancellation and reaches
// no log line, no audit row and no stored reply.
func TestCancelNoteNeverLogged(t *testing.T) {
	s := newSite(t)
	admin := platformAdmin(t)
	tok := mint(t, admin)
	ctx := context.Background()
	q := seedJobQuestion(t, s.hid, s.submitter.id, "failed", "internal")
	j, _ := s.failedApproval(t, "internal")
	note := "Synthetic note " + cancelMarker + "\nsecond line"
	r := post(t, "/platform/jobs/cancel", tok, cancelBody([]string{qid(q), jid(j)}, "other", &note, backupCode(t, admin)))
	ok(t, "cancel", r)
	// Refused bodies carrying it too.
	long := strings.Repeat("x", platform.CancelNoteMax) + cancelMarker
	problem(t, "long note", post(t, "/platform/jobs/cancel", tok, cancelBody([]string{qid(q)}, "other", &long, "")), 422, "validation_failed")
	problem(t, "bad id", post(t, "/platform/jobs/cancel", tok, cancelBody([]string{"no such id"}, "other", &note, "")), 422, "validation_failed")
	var stored string
	if err := super.QueryRow(ctx, `SELECT cancel_note FROM query_cancellations WHERE query_id = $1`, q).Scan(&stored); err != nil || stored != note {
		t.Errorf("question note: %q %v", stored, err)
	}
	if err := super.QueryRow(ctx, `SELECT cancel_note FROM jobs WHERE id = $1`, j).Scan(&stored); err != nil || stored != note {
		t.Errorf("job note: %q %v", stored, err)
	}
	if strings.Contains(logs.String(), cancelMarker) {
		t.Error("the cancel note reached a log line")
	}
	if strings.Contains(r.raw, cancelMarker) {
		t.Error("the reply carries the note")
	}
	var n int
	if err := super.QueryRow(ctx, `SELECT (SELECT count(*) FROM audit_log WHERE payload::text LIKE '%'||$1||'%' OR entity_id LIKE '%'||$1||'%')
		+ (SELECT count(*) FROM idempotency_keys WHERE convert_from(COALESCE(body, ''), 'UTF8') LIKE '%'||$1||'%' OR request_hash LIKE '%'||$1||'%')`,
		cancelMarker).Scan(&n); err != nil || n != 0 {
		t.Errorf("the note in %d audit or stored rows (%v)", n, err)
	}
}

// C33 (jobs part): ids of no job are skipped; a bad list, reason or note
// is 422; nothing is audited when nothing changed.
func TestJobsUnknownIdSkipped(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	unknown := []string{qid(uuid.New()), jid(uuid.New()), "not-a-job", "q_" + strings.ToUpper(uuid.NewString())}
	r := retry(t, tok, unknown...)
	ok(t, "retry", r)
	if len(strs(r.body["retried"])) != 0 || strings.Join(strs(r.body["skipped"]), ",") != strings.Join(unknown, ",") {
		t.Errorf("retry: %s", r.raw)
	}
	c := post(t, "/platform/jobs/cancel", tok, cancelBody(unknown, "no_longer_needed", nil, backupCode(t, admin)))
	ok(t, "cancel", c)
	if len(strs(c.body["cancelled"])) != 0 || len(strs(c.body["skipped"])) != len(unknown) {
		t.Errorf("cancel: %s", c.raw)
	}
	if a := jobAudits(t, admin); len(a) != 0 {
		t.Errorf("audited: %v", a)
	}
	var fifty []string
	for i := 0; i <= platform.JobsMax; i++ {
		fifty = append(fifty, qid(uuid.New()))
	}
	blank := "   "
	tooLong := strings.Repeat("y", platform.CancelNoteMax+1)
	bidi := "synthetic ‮ note"
	for name, body := range map[string]string{
		"no ids":      `{"ids":[]}`,
		"51 ids":      idsBody(fifty...),
		"a bad id":    idsBody("q_x y"),
		"no body":     ``,
		"extra key":   `{"ids":["` + qid(uuid.New()) + `"],"force":true}`,
		"not strings": `{"ids":[1]}`,
	} {
		problem(t, "retry "+name, post(t, "/platform/jobs/retry", tok, body), 422, "validation_failed")
	}
	for name, body := range map[string]string{
		"bad reason":           cancelBody([]string{qid(uuid.New())}, "bored", nil, "x"),
		"other without a note": cancelBody([]string{qid(uuid.New())}, "other", nil, "x"),
		"other with a blank":   cancelBody([]string{qid(uuid.New())}, "other", &blank, "x"),
		"a note over 500":      cancelBody([]string{qid(uuid.New())}, "cant_succeed", &tooLong, "x"),
		"a bidi control":       cancelBody([]string{qid(uuid.New())}, "cant_succeed", &bidi, "x"),
		"no ids":               cancelBody([]string{}, "cant_succeed", nil, "x"),
		"51 ids":               cancelBody(fifty, "cant_succeed", nil, "x"),
	} {
		problem(t, "cancel "+name, post(t, "/platform/jobs/cancel", tok, body), 422, "validation_failed")
	}
}

// C26: an auditor gets 403 on both writes (the list is theirs to read).
func TestAuditorCantRetryOrCancel(t *testing.T) {
	s := newSite(t)
	tok := mint(t, auditor(t))
	q := seedJobQuestion(t, s.hid, s.submitter.id, "failed", "internal")
	problem(t, "auditor retry", retry(t, tok, qid(q)), 403, "forbidden")
	problem(t, "auditor cancel", post(t, "/platform/jobs/cancel", tok, cancelBody([]string{qid(q)}, "no_longer_needed", nil, "x")), 403, "forbidden")
	if st, _, _ := queryState(t, q); st != "failed" {
		t.Errorf("question: %s", st)
	}
	// A clinical account is refused too.
	problem(t, "clinician retry", retry(t, mint(t, s.submitter), qid(q)), 403, "not_platform")
}
