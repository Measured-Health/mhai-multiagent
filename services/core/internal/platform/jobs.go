package platform

// The failed jobs (Plan 4 spec §6.2; contract §14.8.3):
//
//   - GET /platform/jobs?status&type&hospital&kind&cursor: platform admins
//     and auditors; a page of jobs through mhai_platform_jobs() (00016),
//     the contract's keys only, never the subject, the user or an error
//     message. Ids are q_<uuid> (a question, run_query) or j_<uuid> (a
//     jobs row) (plan departure 4). The cursor is HMAC-bound to the
//     session and the filters.
//   - POST /platform/jobs/retry {ids}: platform admins; 10 calls a minute
//     per session; each failed job is re-queued and re-minted as its
//     creator (intel.RetryJobs).
//   - POST /platform/jobs/cancel {ids, reason, note?, mfa_code |
//     backup_code}: platform admins, with step-up (auth.Service.StepUp);
//     the note is stored and never logged or audited (C16).
//
// Each write is one audit row per call, the admin's, with job_count.

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intel"
)

const (
	// JobsPage is a page of the failed jobs list.
	JobsPage = 50
	// JobsMax is the most ids one retry or cancel takes (mhai-web JOBS_MAX).
	JobsMax = 50
	// CancelNoteMax is the cancel note's length in characters.
	CancelNoteMax = 500
	// RetriesPerMinute is the retry calls one session may make a minute
	// (spec §11.3 P18, C32).
	RetriesPerMinute = 10
	// retryScope is the idempotency_keys scope of the retry rate counter;
	// no Idempotency-Key scope (a user id) takes this form.
	retryScope = "platform.jobs.retry_rate"
	// JobsCursorKeyLabel is the HKDF-SHA256 info label of the jobs cursor's
	// HMAC key (the same master key as the audit cursors, a distinct label).
	JobsCursorKeyLabel = "mhai platform jobs cursor key v1"
)

// JobTypes are the contract's job types (mhai-web JOB_TYPES). Only
// run_query and approve_correction have rows in Plan 4; the others list
// nothing.
var JobTypes = []string{"run_query", "submit_feedback", "approve_correction", "ingest_document", "run_triage",
	"kb_search", "feedback_buffer_reset", "retention_purge"}

// jobStatuses are the list's status filter values (failed when not given).
var jobStatuses = []string{"failed", "retrying", "retried", "cancelled", "all"}

var (
	// jobIDPattern is a job id as the platform sees it.
	jobIDPattern = regexp.MustCompile(`^[qj]_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// opaqueID is any id mhai-web may send (its id rule); one of this
	// shape that is no job's is skipped, anything else is 422.
	opaqueID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	errNoKey = errors.New("platform: no jobs cursor key")
)

func registerJobs(m *authz.Mux, d Deps) {
	read := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin, authz.RoleAuditor}}
	admin := []string{authz.RolePlatformAdmin}
	authz.Route(m, http.MethodGet, "/platform/jobs", read, http.HandlerFunc(d.jobsList))
	authz.Route(m, http.MethodPost, "/platform/jobs/retry",
		authz.Rule{Platform: true, NeedsMFA: true, AnyRole: admin, Write: true}, http.HandlerFunc(d.jobsRetry))
	authz.Route(m, http.MethodPost, "/platform/jobs/cancel",
		authz.Rule{Platform: true, NeedsMFA: true, AnyRole: admin, Write: true, StepUp: true}, http.HandlerFunc(d.jobsCancel))
}

// --- the list --------------------------------------------------------------

// jobFilters are the list's filters, as the cursor binds them.
type jobFilters struct {
	Status   string `json:"status"`
	Type     string `json:"type,omitempty"`
	Hospital string `json:"hospital,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

// jobCursors makes and checks one session's list cursors: (failed_at in
// microseconds, the row id, a tag over the session and the filters).
type jobCursors struct {
	key     []byte
	session string
}

const jobCursorLen = 8 + 16 + 16

func jobCursorKey(master []byte) ([]byte, error) {
	if len(master) < 32 {
		return nil, errNoKey
	}
	k, err := hkdf.Key(sha256.New, master, nil, JobsCursorKeyLabel, 32)
	if err != nil {
		return nil, errNoKey
	}
	return k, nil
}

func (c jobCursors) tag(us int64, id uuid.UUID, f jobFilters) []byte {
	raw, _ := json.Marshal(f)
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], uint64(us))
	m := hmac.New(sha256.New, c.key)
	for _, part := range [][]byte{[]byte("platform.jobs.cursor.v1"), []byte(c.session), raw, t[:], id[:]} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		m.Write(n[:])
		m.Write(part)
	}
	return m.Sum(nil)[:16]
}

func (c jobCursors) encode(at time.Time, id uuid.UUID, f jobFilters) string {
	us := at.UnixMicro()
	b := make([]byte, 8, jobCursorLen)
	binary.BigEndian.PutUint64(b, uint64(us))
	b = append(b, id[:]...)
	return hex.EncodeToString(append(b, c.tag(us, id, f)...))
}

func (c jobCursors) decode(s string, f jobFilters) (time.Time, uuid.UUID, bool) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != jobCursorLen || hex.EncodeToString(b) != s {
		return time.Time{}, uuid.Nil, false
	}
	us := int64(binary.BigEndian.Uint64(b[:8]))
	var id uuid.UUID
	copy(id[:], b[8:24])
	if !hmac.Equal(b[24:], c.tag(us, id, f)) {
		return time.Time{}, uuid.Nil, false
	}
	return time.UnixMicro(us).UTC(), id, true
}

// Ref is a hospital or person as the contract names one: {id, name}.
type Ref struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// JobOut is one job of the list (mhai-web Job): exactly these keys.
type JobOut struct {
	ID          string  `json:"id"`
	Type        string  `json:"type"`
	Hospital    *Ref    `json:"hospital"`
	CreatedAt   string  `json:"created_at"`
	FailedAt    string  `json:"failed_at"`
	Attempts    int     `json:"attempts"`
	Kind        string  `json:"kind"`
	Status      string  `json:"status"`
	CancelledBy *Ref    `json:"cancelled_by"`
	CancelledAt *string `json:"cancelled_at"`
}

// jobsQuery parses the list's query.
func jobsQuery(r *http.Request) (jobFilters, string, error) {
	v, err := params(r, []string{"status", "type", "hospital", "kind", "cursor"})
	if err != nil {
		return jobFilters{}, "", err
	}
	f := jobFilters{Status: "failed"}
	if s, ok := v["status"]; ok {
		if !slices.Contains(jobStatuses, s) {
			return f, "", errInvalid
		}
		f.Status = s
	}
	if s, ok := v["type"]; ok {
		if !slices.Contains(JobTypes, s) {
			return f, "", errInvalid
		}
		f.Type = s
	}
	if s, ok := v["hospital"]; ok {
		if !canonicalUUID(s) {
			return f, "", errInvalid
		}
		f.Hospital = s
	}
	if s, ok := v["kind"]; ok {
		if !slices.Contains(intel.ErrorKinds, s) {
			return f, "", errInvalid
		}
		f.Kind = s
	}
	return f, v["cursor"], nil
}

// jobRow is what mhai_platform_jobs returns for one job.
type jobRow struct {
	id, hospital         uuid.UUID
	hasHospital          bool
	typ, status          string
	errCode              *string
	createdAt, failedAt  time.Time
	attempts, retryCount int
	cancelledBy          *uuid.UUID
	cancelledAt          *time.Time
}

// jobsList is GET /platform/jobs → {jobs, next_cursor}, newest failure
// first. Not recorded: a read of job metadata, as health is (R32).
func (d Deps) jobsList(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	f, cursor, err := jobsQuery(r)
	if err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	key, err := jobCursorKey(d.MasterKey)
	if err != nil {
		d.log().Error("platform: jobs list refused: no cursor key", "request_id", httpx.RequestIDFrom(r.Context()),
			"reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return
	}
	cur := jobCursors{key: key, session: p.SessionID}
	var afterAt *time.Time
	var afterID *uuid.UUID
	if cursor != "" {
		at, id, ok := cur.decode(cursor, f)
		if !ok {
			httpx.Error(w, r, "validation_failed")
			return
		}
		afterAt, afterID = &at, &id
	}
	var hospital *uuid.UUID
	if f.Hospital != "" {
		h := uuid.MustParse(f.Hospital)
		hospital = &h
	}
	var typ *string
	if f.Type != "" {
		typ = &f.Type
	}
	var in, notIn []string
	if f.Kind != "" {
		in, notIn = intel.KindCodes(f.Kind)
	}
	var rows []jobRow
	hospitals := map[uuid.UUID]string{}
	people := map[uuid.UUID]string{}
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		q, err := tx.Query(r.Context(), `SELECT id, type, hospital_id, created_at, failed_at, attempts, error, status,
			       retry_count, cancelled_by, cancelled_at
			FROM mhai_platform_jobs($1, $2, $3, $4, $5, $6, $7, $8)`,
			f.Status, typ, hospital, in, notIn, afterAt, afterID, JobsPage+1)
		if err != nil {
			return err
		}
		for q.Next() {
			var j jobRow
			var hid *uuid.UUID
			if err := q.Scan(&j.id, &j.typ, &hid, &j.createdAt, &j.failedAt, &j.attempts, &j.errCode, &j.status,
				&j.retryCount, &j.cancelledBy, &j.cancelledAt); err != nil {
				q.Close()
				return err
			}
			if hid != nil {
				j.hospital, j.hasHospital = *hid, true
				hospitals[*hid] = ""
			}
			if j.cancelledBy != nil {
				people[*j.cancelledBy] = ""
			}
			rows = append(rows, j)
		}
		q.Close()
		if err := q.Err(); err != nil {
			return err
		}
		if err := names(r, tx, `SELECT id, name FROM hospitals WHERE id = ANY($1)`, hospitals); err != nil {
			return err
		}
		return names(r, tx, `SELECT id, display_name FROM users WHERE id = ANY($1)`, people)
	})
	if err != nil {
		d.fail(w, r, p, "jobs list")
		return
	}
	var next *string
	if len(rows) > JobsPage {
		last := rows[JobsPage-1]
		s := cur.encode(last.failedAt, last.id, f)
		next = &s
		rows = rows[:JobsPage]
	}
	out := make([]JobOut, 0, len(rows))
	for _, j := range rows {
		o, ok := jobOut(j, hospitals, people)
		if !ok {
			// A name mhai-web couldn't show: the job is left off the page
			// rather than sent half-named (ids only in the log).
			d.log().Warn("platform: job not listed: a name can't be shown", "request_id", httpx.RequestIDFrom(r.Context()),
				"job_id", j.id.String())
			continue
		}
		out = append(out, o)
	}
	httpx.JSON(w, http.StatusOK, struct {
		Jobs       []JobOut `json:"jobs"`
		NextCursor *string  `json:"next_cursor"`
	}{out, next})
}

// names fills m (id → "") with each id's name from q.
func names(r *http.Request, tx pgx.Tx, q string, m map[uuid.UUID]string) error {
	if len(m) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	rows, err := tx.Query(r.Context(), q, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		m[id] = name
	}
	return rows.Err()
}

func jobOut(j jobRow, hospitals, people map[uuid.UUID]string) (JobOut, bool) {
	prefix := "j_"
	if j.typ == "run_query" {
		prefix = "q_"
	}
	o := JobOut{ID: prefix + j.id.String(), Type: j.typ, CreatedAt: stamp(j.createdAt), FailedAt: stamp(j.failedAt),
		Attempts: j.attempts, Status: j.status, Kind: intel.KindOther}
	if j.errCode != nil {
		o.Kind = intel.ErrorKind(*j.errCode)
	}
	if !slices.Contains(JobTypes, j.typ) || !slices.Contains(jobStatuses, j.status) || j.status == "all" {
		return o, false
	}
	if j.hasHospital {
		name := hospitals[j.hospital]
		if !audit.ShowableName(name) {
			return o, false
		}
		o.Hospital = &Ref{ID: j.hospital.String(), Name: name}
	}
	if j.cancelledBy != nil {
		name := people[*j.cancelledBy]
		if !audit.ShowableName(name) {
			return o, false
		}
		o.CancelledBy = &Ref{ID: j.cancelledBy.String(), Name: name}
	}
	if j.cancelledAt != nil {
		s := stamp(*j.cancelledAt)
		o.CancelledAt = &s
	}
	return o, true
}

// --- retry and cancel ------------------------------------------------------

// jobIDs parses a retry or cancel's ids: 1 to 50 of mhai-web's id shape,
// or false (422). Each id of a job's shape becomes a ref; a repeat counts
// once. ids keeps the request's order, each id once.
func jobIDs(raw []string) (refs []intel.JobRef, ids []string, ok bool) {
	if len(raw) < 1 || len(raw) > JobsMax {
		return nil, nil, false
	}
	seen := map[string]bool{}
	for _, s := range raw {
		if !opaqueID.MatchString(s) {
			return nil, nil, false
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		ids = append(ids, s)
		if jobIDPattern.MatchString(s) {
			refs = append(refs, intel.JobRef{Query: s[0] == 'q', ID: uuid.MustParse(s[2:])})
		}
	}
	return refs, ids, true
}

// sortOut splits ids into those done and the rest (skipped), in order.
func sortOut(ids []string, done []intel.JobRef) (yes, no []string) {
	set := map[string]bool{}
	for _, d := range done {
		p := "j_"
		if d.Query {
			p = "q_"
		}
		set[p+d.ID.String()] = true
	}
	yes, no = []string{}, []string{}
	for _, id := range ids {
		if set[id] {
			yes = append(yes, id)
		} else {
			no = append(no, id)
		}
	}
	return yes, no
}

// reserveRetry counts one retry call against the session's minute (C32):
// a counter row in idempotency_keys, taken by one conditional upsert (the
// row lock orders parallel calls), before any job is looked at. false is
// over the limit. An error fails closed (the caller answers 503).
func (d Deps) reserveRetry(r *http.Request, p authz.Principal) (bool, error) {
	sum := sha256.Sum256([]byte("platform.jobs.retry.v1\x00" + p.SessionID))
	key := hex.EncodeToString(sum[:])
	allowed := false
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var n int
		err := tx.QueryRow(r.Context(), `
			INSERT INTO idempotency_keys AS k (scope, key, hospital_id, request_hash, status, expires_at)
			VALUES ($1, $2, NULL, $2, 1, now() + interval '1 minute')
			ON CONFLICT (scope, key) DO UPDATE
			SET status = CASE WHEN k.expires_at <= now() THEN 1 ELSE k.status + 1 END,
			    expires_at = CASE WHEN k.expires_at <= now() THEN now() + interval '1 minute' ELSE k.expires_at END
			WHERE k.expires_at <= now() OR k.status < $3
			RETURNING k.status`, retryScope, key, RetriesPerMinute).Scan(&n)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		allowed = true
		return nil
	})
	return allowed, err
}

func actorOf(r *http.Request, p authz.Principal) intel.Actor {
	return intel.Actor{UserID: p.UserID, RequestID: httpx.RequestIDFrom(r.Context())}
}

// jobsRetry is POST /platform/jobs/retry {ids} → {retried, skipped}.
func (d Deps) jobsRetry(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	refs, ids, ok := jobIDs(body.IDs)
	if !ok {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if d.Intel == nil || d.QueryCap < 1 {
		d.log().Error("platform: jobs retry refused: no intelligence client", "request_id", httpx.RequestIDFrom(r.Context()),
			"reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return
	}
	allowed, err := d.reserveRetry(r, p)
	switch {
	case err != nil:
		d.log().Error("platform: jobs retry refused: rate counter unavailable", "request_id", httpx.RequestIDFrom(r.Context()),
			"reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return
	case !allowed:
		httpx.Error(w, r, "rate_limited")
		return
	}
	var done []intel.JobRef
	if len(refs) > 0 {
		done, err = intel.RetryJobs(r.Context(), d.Pool, d.Intel, refs, d.QueryCap, actorOf(r, p), d.log())
		if err != nil {
			d.log().Error("platform: jobs retry failed", "request_id", httpx.RequestIDFrom(r.Context()),
				"user_id", p.UserID.String(), "reason", "unavailable")
			httpx.Error(w, r, "unavailable")
			return
		}
	}
	retried, skipped := sortOut(ids, done)
	httpx.JSON(w, http.StatusOK, struct {
		Retried []string `json:"retried"`
		Skipped []string `json:"skipped"`
	}{retried, skipped})
}

// cancelNoteOK is the cancel note's rule: text, not blank, at most 500
// characters (code points and UTF-16 units), newlines allowed, no other
// control or format character, no line or paragraph separator (R11), and
// nothing of a pseudonym's shape.
func cancelNoteOK(s string) bool {
	if !utf8.ValidString(s) || strings.TrimSpace(s) == "" ||
		utf8.RuneCountInString(s) > CancelNoteMax || len(utf16.Encode([]rune(s))) > CancelNoteMax {
		return false
	}
	for _, c := range s {
		switch {
		case c == '\n' || c == '\r':
		case unicode.Is(unicode.Cc, c), unicode.Is(unicode.Cf, c), unicode.Is(unicode.Zl, c), unicode.Is(unicode.Zp, c),
			c == utf8.RuneError:
			return false
		}
	}
	return !pseudonymIn.MatchString(s)
}

// jobsCancel is POST /platform/jobs/cancel {ids, reason, note?, mfa_code |
// backup_code} → {cancelled, skipped}. The body is checked first (422),
// then the code (403 mfa_required with none, mfa_invalid when wrong), then
// the jobs. The note goes to the job rows only: no log line, audit row or
// reply carries it.
func (d Deps) jobsCancel(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	var body struct {
		IDs        []string `json:"ids"`
		Reason     string   `json:"reason"`
		Note       *string  `json:"note"`
		MFACode    string   `json:"mfa_code"`
		BackupCode string   `json:"backup_code"`
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	refs, ids, ok := jobIDs(body.IDs)
	if !ok || !slices.Contains(intel.CancelReasons, body.Reason) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	// A blank note is no note; any other must pass the note's rule.
	var note *string
	if body.Note != nil && strings.TrimSpace(*body.Note) != "" {
		if !cancelNoteOK(*body.Note) {
			httpx.Error(w, r, "validation_failed")
			return
		}
		note = body.Note
	}
	if body.Reason == "other" && note == nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if d.Auth == nil {
		d.log().Error("platform: jobs cancel refused: no step-up", "request_id", httpx.RequestIDFrom(r.Context()),
			"reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return
	}
	if body.MFACode == "" && body.BackupCode == "" {
		httpx.Error(w, r, "mfa_required")
		return
	}
	if err := d.Auth.StepUp(r.Context(), nil, p.UserID.String(), body.MFACode, body.BackupCode); err != nil {
		d.Auth.WriteStepUpError(w, r, err)
		return
	}
	var done []intel.JobRef
	if len(refs) > 0 {
		var err error
		done, err = intel.CancelJobs(r.Context(), d.Pool, refs, body.Reason, note, actorOf(r, p))
		if err != nil {
			d.log().Error("platform: jobs cancel failed", "request_id", httpx.RequestIDFrom(r.Context()),
				"user_id", p.UserID.String(), "reason", "unavailable")
			httpx.Error(w, r, "unavailable")
			return
		}
	}
	cancelled, skipped := sortOut(ids, done)
	httpx.JSON(w, http.StatusOK, struct {
		Cancelled []string `json:"cancelled"`
		Skipped   []string `json:"skipped"`
	}{cancelled, skipped})
}
