package intel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
)

const (
	// SweepEvery is how often the publish sweeper runs (spec §9.2).
	SweepEvery = 10 * time.Second
	// sweepAge is how long a queued row waits for its first publish before
	// the sweeper publishes it: the submit publishes right after its
	// commit, so a row this old with no published_at lost that publish.
	sweepAge   = 10 * time.Second
	sweepBatch = 100
	// runQuery is the job behind every clinical_queries row. Its timeout is
	// also the deadline of a row with no deadline_at, here and in
	// mhai_fail_expired_queries() (00007_jobs.sql; a test holds the two
	// together).
	runQuery = "pipeline.run_query"
	// cancelUnusable is the cancel reason when the creator can't use the
	// service any more.
	cancelUnusable = "account_unusable"
)

var (
	errSweep  = errors.New("intel: publish sweep failed")
	errLookup = errors.New("intel: account lookup failed")
)

// JobRow is what the sweeper and the dead-letter consumer know of a queued
// job: ids and times, never the question or the correction's text.
//
// Type is "" for a clinical_queries row (run_query); otherwise the jobs
// table's type (approve_correction), with SubjectID the row the job is
// about.
type JobRow struct {
	ID, HospitalID, CreatedBy uuid.UUID // CreatedBy is uuid.Nil when none
	RequestID                 string
	Attempts                  int
	Deadline                  time.Time
	Type                      string
	SubjectID                 uuid.UUID
}

// jobsRow reports whether r is a jobs-table row (not clinical_queries).
func (r JobRow) jobsRow() bool { return r.Type != "" && r.Type != jobType }

// rid is the request id for work on r: JobRID for a clinical_queries row;
// for a jobs row, its own when usable, else job-<id>.
func (r JobRow) rid() string {
	if !r.jobsRow() {
		return JobRID(r.ID, r.RequestID)
	}
	return JobsRowRID(r.ID, r.RequestID)
}

// JobsRowRID is JobRID for a jobs-table row: the row's own request id when
// usable, else one derived from the job id.
func JobsRowRID(id uuid.UUID, rid string) string {
	if validRID(rid) {
		return rid
	}
	return "job-" + id.String()
}

// entity is the audit entity of a row: the query, or the job.
func (r JobRow) entity() string {
	if r.jobsRow() {
		return "job"
	}
	return "query"
}

// message is the job message for r: RunQueryJob for a clinical_queries
// row; for a jobs row, its type's message from the row's own ids (the
// type and the subject come from the row, never from elsewhere).
func (r JobRow) message() (string, proto.Message, bool) {
	switch {
	case !r.jobsRow():
		return runQuery, &intelpb.RunQueryJob{QueryId: r.ID.String()}, true
	case r.Type == "approve_correction" && r.SubjectID != uuid.Nil:
		return "pipeline.approve_correction", &intelpb.ApproveCorrectionJob{JobId: r.ID.String(), CorrectionId: r.SubjectID.String()}, true
	}
	return "", nil, false
}

// JobRID is the request id for work on a job row: the row's own when it
// has a usable one, else one derived from the job id, so the token core
// mints (Republish) and the audit rows the sweeper writes name the same id.
func JobRID(id uuid.UUID, rid string) string {
	if validRID(rid) {
		return rid
	}
	return "query-" + id.String()
}

// Republish re-mints and publishes row's job with message id msgID: the
// job id for a sweep, RetryMsgID for a retry after a dead letter (spec
// §7.4: a retry never reuses the old message). It is the one re-mint
// helper (F23). The operation and its message come from the row (its type
// and subject: JobRow.message), never from elsewhere. It first re-checks
// the creator (as mhai_auth): the account must still be usable
// (authz.AccountUsable) and belong to the row's hospital, else
// ErrCreatorUnusable. The token is minted as the creator, with the
// creator's current roles (neither job allows svc:core); none of them
// allowed is ErrCreatorUnusable too. A second_person job the two-person
// rule refuses (the creator isn't the correction's approver) is
// ErrJobSkipped: never retried as transient, never published. A failed
// lookup is an error to retry later, never a cancellation. The request id
// is the row's (JobRID, JobsRowRID).
func Republish(ctx context.Context, pool *pgxpool.Pool, client *Client, row JobRow, msgID string) error {
	opName, msg, ok := row.message()
	if !ok {
		return ErrMisuse
	}
	if row.CreatedBy == uuid.Nil {
		return ErrCreatorUnusable
	}
	var roles []string
	usable := false
	err := db.WithAuth(ctx, pool, func(tx pgx.Tx) error {
		var active bool
		var hid *uuid.UUID
		var status *string
		err := tx.QueryRow(ctx, `SELECT u.is_active, u.hospital_id, h.status,
			       ARRAY(SELECT r.role FROM user_roles r WHERE r.user_id = u.id)
			FROM users u LEFT JOIN hospitals h ON h.id = u.hospital_id
			WHERE u.id = $1`, row.CreatedBy).Scan(&active, &hid, &status, &roles)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return errLookup
		}
		roles = authz.SortRoles(roles)
		usable = authz.AccountUsable(active, hid, status, roles) && hid != nil && *hid == row.HospitalID
		return nil
	})
	if err != nil {
		return errLookup // driver errors can carry row values: classify, never wrap
	}
	if !usable {
		return ErrCreatorUnusable
	}
	caller := Caller{Sub: row.CreatedBy.String(), Hid: row.HospitalID.String(), Rid: row.rid(), Roles: roles}
	err = client.publishJob(ctx, caller, opName, msgID, msg, row.Deadline)
	switch {
	case errors.Is(err, ErrNotAllowed):
		return ErrCreatorUnusable
	case errors.Is(err, ErrSecondPerson):
		return ErrJobSkipped
	}
	return err
}

// RetryMsgID is the Nats-Msg-Id of a job re-minted after a dead letter:
// <job id>.r<attempts>, so a second copy of the same retry (a duplicated or
// replayed dead letter) is dropped by JetStream's duplicate window.
func RetryMsgID(id uuid.UUID, attempts int) string {
	return fmt.Sprintf("%s.r%d", id, attempts)
}

// SweepResult counts what one sweep did.
type SweepResult struct {
	Expired, Published, Cancelled, Left int // Left: rows kept for the next sweep after an error
	// Skipped: jobs rows the two-person rule refuses as they stand; not an
	// error, and never cancelled here: the deadline ends them.
	Skipped int
}

// RunPublishSweeper runs SweepOnce now and then every interval (SweepEvery
// when not positive) until ctx ends. Each tick that did or failed anything
// logs one summary line with the counts.
func RunPublishSweeper(ctx context.Context, pool *pgxpool.Pool, client *Client, every time.Duration) {
	if every <= 0 {
		every = SweepEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		res, err := SweepOnce(ctx, pool, client)
		switch {
		case err != nil && ctx.Err() == nil:
			client.log().Error("intel: publish sweep failed", "expired", res.Expired, "published", res.Published,
				"cancelled", res.Cancelled, "left", res.Left)
		case res != (SweepResult{}):
			client.log().Info("intel: publish sweep", "expired", res.Expired, "published", res.Published,
				"cancelled", res.Cancelled, "skipped", res.Skipped)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SweepOnce does one sweep, as mhai_jobs:
//  1. Every queued or running row past its deadline (deadline_at, or
//     created_at plus run_query's timeout when unset; 10 minutes for a
//     jobs row), published or not, is failed with deadline_exceeded,
//     through mhai_fail_expired_queries() and mhai_fail_expired_jobs() (up
//     to 100 each a sweep), and audited: query.failed for a question,
//     job.dead_lettered against the job for a jobs row. So a worker that
//     died on its last delivery, or a lost publish, ends and frees its
//     in-flight slot.
//  2. Up to 100 queued clinical_queries rows within their deadline that
//     have waited over 10 s with no published_at, oldest first: a creator
//     who can't use the service any more gets the row cancelled
//     (account_unusable, audited query.cancelled); otherwise Republish with
//     the job id as Nats-Msg-Id (JetStream drops the duplicate if the first
//     publish did land), then published_at.
//  3. The same for jobs rows, per type, but only rows within their type's
//     in-flight budget (fewer than the budget live rows of the hospital and
//     type ahead of them; QueueJob). A creator who can't use the service
//     gets the row cancelled (account_unusable, audited job.cancelled). A
//     row the two-person rule refuses is skipped (a sampled warning): its
//     deadline ends it, and the platform's retry is the way back.
//
// An error in the jobs table's steps doesn't stop the questions' sweep:
// it is recorded and the sweep returns an error at the end.
//
// A row that fails (NATS down, JOBS full) stays as it is for the next
// sweep, counted in Left, and the sweep returns an error. Each update
// re-checks the row is still queued and unpublished.
func SweepOnce(ctx context.Context, pool *pgxpool.Pool, client *Client) (SweepResult, error) {
	var res SweepResult
	op, ok := authz.OperationByName(runQuery)
	if !ok {
		return res, errSweep
	}
	n, err := failExpired(ctx, pool)
	res.Expired = n
	if err != nil {
		return res, errSweep
	}
	// The jobs table's steps fail on their own: an error there is
	// recorded and the questions are still swept (and the reverse).
	jobsFailed := false
	n, err = failExpiredJobs(ctx, pool)
	res.Expired += n
	if err != nil {
		jobsFailed = true
	}
	var rows []JobRow
	err = db.WithJobs(ctx, pool, func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `
			SELECT id, hospital_id, created_by, COALESCE(request_id, ''), attempts, d
			FROM (SELECT id, hospital_id, created_by, request_id, attempts, created_at,
			             COALESCE(deadline_at, created_at + make_interval(secs => $2)) AS d
			      FROM clinical_queries
			      WHERE status = 'queued' AND published_at IS NULL
			        AND created_at < now() - make_interval(secs => $1)) q
			WHERE d > now()
			ORDER BY created_at
			LIMIT $3`, sweepAge.Seconds(), op.Timeout.Seconds(), sweepBatch)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var s JobRow
			var by *uuid.UUID
			if err := r.Scan(&s.ID, &s.HospitalID, &by, &s.RequestID, &s.Attempts, &s.Deadline); err != nil {
				return err
			}
			if by != nil {
				s.CreatedBy = *by
			}
			rows = append(rows, s)
		}
		return r.Err()
	})
	if err != nil {
		return res, errSweep
	}
	if jobs, err := sweepableJobs(ctx, pool); err != nil {
		jobsFailed = true
	} else {
		rows = append(rows, jobs...)
	}
	for _, r := range rows {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		switch err := Republish(ctx, pool, client, r, r.ID.String()); {
		case err == nil:
			if markPublished(ctx, pool, r) != nil {
				res.Left++
			} else {
				res.Published++
			}
		case r.jobsRow() && errors.Is(err, ErrJobSkipped):
			// The two-person rule refuses the row as it stands: left for
			// its deadline, never retried as transient. Sampled: the same
			// row comes back every sweep until it expires.
			res.Skipped++
			if k := client.metrics().SweepSkipped.Add(1); k == 1 || k%deadLogEvery == 0 {
				client.log().Warn("intel: job not publishable; left for its deadline", "job_id", r.ID.String(),
					"job_type", r.Type, "hospital_id", r.HospitalID.String(), "skipped_total", k)
			}
		case errors.Is(err, ErrCreatorUnusable):
			if cancelRow(ctx, pool, r) != nil {
				res.Left++
			} else {
				res.Cancelled++
			}
		case errors.Is(err, ErrDeadline):
			// Under a second left: the next sweep fails it (step 1).
		default:
			res.Left++
		}
	}
	if res.Left > 0 || jobsFailed {
		return res, errSweep
	}
	return res, nil
}

// jobsTableFault is a test hook (export_test.go): an error it returns
// fails the sweep's jobs-table steps. Nil in production.
var jobsTableFault func() error

// sweepableJobs is step 3's rows: per type with a budget, up to 100 queued,
// unpublished jobs rows older than sweepAge and within their deadline,
// with fewer than the budget live rows of their hospital and type ahead of
// them (created_at, id), oldest first.
func sweepableJobs(ctx context.Context, pool *pgxpool.Pool) ([]JobRow, error) {
	if jobsTableFault != nil && jobsTableFault() != nil {
		return nil, errSweep
	}
	var rows []JobRow
	err := db.WithJobs(ctx, pool, func(tx pgx.Tx) error {
		for typ, budget := range jobBudgets {
			r, err := tx.Query(ctx, `
				SELECT j.id, j.hospital_id, j.created_by, COALESCE(j.request_id, ''), j.attempts, `+jobDeadline("j.")+`, j.type, j.subject_id
				FROM jobs j
				WHERE j.type = $1 AND j.status = 'queued' AND j.published_at IS NULL
				  AND j.created_at < now() - make_interval(secs => $2)
				  AND `+jobDeadline("j.")+` > now()
				  AND (SELECT count(*) FROM jobs a
				       WHERE a.hospital_id = j.hospital_id AND a.type = j.type AND a.status IN ('queued', 'running')
				         AND `+jobDeadline("a.")+` > now()
				         AND (a.created_at, a.id) < (j.created_at, j.id)) < $3
				ORDER BY j.created_at, j.id
				LIMIT $4`, typ, sweepAge.Seconds(), budget, sweepBatch)
			if err != nil {
				return err
			}
			for r.Next() {
				var s JobRow
				var by, hid *uuid.UUID
				if err := r.Scan(&s.ID, &hid, &by, &s.RequestID, &s.Attempts, &s.Deadline, &s.Type, &s.SubjectID); err != nil {
					r.Close()
					return err
				}
				if hid == nil {
					continue // a global job: none in Plan 4, and none publishable as a hospital's
				}
				s.HospitalID = *hid
				if by != nil {
					s.CreatedBy = *by
				}
				rows = append(rows, s)
			}
			r.Close()
			if err := r.Err(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, errSweep
	}
	return rows, nil
}

// failExpired fails expired queued and running rows and audits each one in
// the same transaction, with the row's hospital and request id (JobRID) and
// no actor: core did it.
func failExpired(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	n := 0
	err := db.WithJobs(ctx, pool, func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `SELECT id, hospital_id, COALESCE(request_id, ''), attempts FROM mhai_fail_expired_queries($1)`, sweepBatch)
		if err != nil {
			return errSweep
		}
		var rows []JobRow
		for r.Next() {
			var j JobRow
			if err := r.Scan(&j.ID, &j.HospitalID, &j.RequestID, &j.Attempts); err != nil {
				r.Close()
				return errSweep
			}
			rows = append(rows, j)
		}
		r.Close()
		if r.Err() != nil {
			return errSweep
		}
		for _, j := range rows {
			if err := audit.Write(ctx, tx, audit.Event{HospitalID: j.HospitalID.String(), RequestID: JobRID(j.ID, j.RequestID),
				Action: "query.failed", Entity: "query", EntityID: j.ID.String(),
				Payload: map[string]any{"query_id": j.ID.String(), "error": "deadline_exceeded", "attempts": j.Attempts}}); err != nil {
				return errSweep
			}
		}
		n = len(rows)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// failExpiredJobs is failExpired for jobs rows (mhai_fail_expired_jobs,
// 00012): each is audited job.dead_lettered against the job, with error
// deadline_exceeded, the row's hospital and request id and no actor. The
// jobs table has no failure fact of its own that core may write
// (correction.apply_failed is the worker's, through the outbox), and
// job.dead_lettered with deadline_exceeded is what the dead-letter path
// records for a job that expired in the queue (F6). A global row (no
// hospital; none in Plan 4) can't be tied to a hospital's chain and is
// failed without a row.
func failExpiredJobs(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	if jobsTableFault != nil && jobsTableFault() != nil {
		return 0, errSweep
	}
	n := 0
	err := db.WithJobs(ctx, pool, func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `SELECT id, hospital_id, type, COALESCE(request_id, ''), attempts FROM mhai_fail_expired_jobs($1)`, sweepBatch)
		if err != nil {
			return errSweep
		}
		var rows []JobRow
		for r.Next() {
			var j JobRow
			var hid *uuid.UUID
			if err := r.Scan(&j.ID, &hid, &j.Type, &j.RequestID, &j.Attempts); err != nil {
				r.Close()
				return errSweep
			}
			if hid != nil {
				j.HospitalID = *hid
				rows = append(rows, j)
			}
			n++
		}
		r.Close()
		if r.Err() != nil {
			return errSweep
		}
		for _, j := range rows {
			if err := audit.Write(ctx, tx, jobEvent(j, "job.dead_lettered", map[string]any{
				"job_id": j.ID.String(), "type": j.Type, "error": "deadline_exceeded", "attempts": j.Attempts,
			})); err != nil {
				return errSweep
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// markPublished sets published_at on row, if still queued and unpublished,
// in its own table.
func markPublished(ctx context.Context, pool *pgxpool.Pool, row JobRow) error {
	q := `UPDATE clinical_queries SET published_at = now()
			WHERE id = $1 AND status = 'queued' AND published_at IS NULL`
	if row.jobsRow() {
		q = `UPDATE jobs SET published_at = now()
			WHERE id = $1 AND status = 'queued' AND published_at IS NULL`
	}
	return db.WithJobs(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, q, row.ID); err != nil {
			return errSweep
		}
		return nil
	})
}

// cancelRow cancels a queued, unpublished row whose creator can't use the
// service (account_unusable) and audits it in the same transaction, with
// the row's hospital and request id and no actor: query.cancelled for a
// question, job.cancelled against the job for a jobs row.
func cancelRow(ctx context.Context, pool *pgxpool.Pool, row JobRow) error {
	if row.jobsRow() {
		return cancelJobsRow(ctx, pool, row)
	}
	return db.WithJobs(ctx, pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE clinical_queries SET status = 'cancelled', cancel_reason = '`+cancelUnusable+`', completed_at = now()
			WHERE id = $1 AND status = 'queued' AND published_at IS NULL`, row.ID)
		if err != nil {
			return errSweep
		}
		if tag.RowsAffected() != 1 {
			return nil // someone else moved it on
		}
		if err := audit.Write(ctx, tx, audit.Event{HospitalID: row.HospitalID.String(), RequestID: JobRID(row.ID, row.RequestID),
			Action: "query.cancelled", Entity: "query", EntityID: row.ID.String(),
			Payload: map[string]any{"query_id": row.ID.String(), "reason": cancelUnusable}}); err != nil {
			return errSweep
		}
		return nil
	})
}

// cancelJobsRow is cancelRow for a jobs row, as mhai_jobs (jobs_append
// allows job.cancelled, tied to a job of the row's hospital).
func cancelJobsRow(ctx context.Context, pool *pgxpool.Pool, row JobRow) error {
	return db.WithJobs(ctx, pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE jobs SET status = 'cancelled', cancel_reason = '`+cancelUnusable+`',
			cancelled_at = now(), completed_at = now()
			WHERE id = $1 AND hospital_id = $2 AND status = 'queued' AND published_at IS NULL`, row.ID, row.HospitalID)
		if err != nil {
			return errSweep
		}
		if tag.RowsAffected() != 1 {
			return nil // someone else moved it on
		}
		if err := audit.Write(ctx, tx, jobEvent(row, "job.cancelled", map[string]any{
			"job_id": row.ID.String(), "type": row.Type, "reason": cancelUnusable,
		})); err != nil {
			return errSweep
		}
		return nil
	})
}

// statsTimeout bounds one reading for the metrics.
const statsTimeout = 3 * time.Second

// QueueStats is how many run_query jobs wait to be picked up (status
// queued, across hospitals) and how long the oldest has waited, for the
// jobs_queue_depth and jobs_oldest_wait_seconds gauges. Counts and an age
// only: no row, id or hospital leaves the query.
func QueueStats(ctx context.Context, pool *pgxpool.Pool) (depth int64, oldest time.Duration, err error) {
	ctx, cancel := context.WithTimeout(ctx, statsTimeout)
	defer cancel()
	err = db.WithJobs(ctx, pool, func(tx pgx.Tx) error {
		var secs float64
		if err := tx.QueryRow(ctx, `SELECT count(*), COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0)::float8
			FROM clinical_queries WHERE status = 'queued'`).Scan(&depth, &secs); err != nil {
			return err
		}
		oldest = time.Duration(secs * float64(time.Second))
		return nil
	})
	return depth, oldest, err
}
