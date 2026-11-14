package intel

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
)

// The platform's retry and cancel of failed jobs (Plan 4 spec §6.2;
// contract §14.8.3). A platform admin acts on job rows of every hospital,
// which only mhai_jobs may touch (job columns only), so these run as
// mhai_jobs and append the call's one audit row as mhai_platform in the
// same transaction (db.WithJobsThenPlatform): the actor is the admin, from
// the verified principal, never the job's creator.

// JobRef names one job row: a clinical_queries row (run_query) or a jobs
// row, and its id. Which table comes from the platform id's own prefix (q_
// or j_; plan departure 4), never from anything else in a request.
type JobRef struct {
	Query bool // a clinical_queries row (run_query); else a jobs row
	ID    uuid.UUID
}

// Actor is the platform admin a retry or cancel is done for: the verified
// principal's user id and the request id.
type Actor struct {
	UserID    uuid.UUID
	RequestID string
}

// CancelReasons are the platform's cancel codes (contract §14.8.3).
var CancelReasons = []string{"no_longer_needed", "cant_succeed", "other"}

// ErrJobsUnavailable: the retry or cancel couldn't run (the database, or a
// check that couldn't be made). Nothing was changed.
var ErrJobsUnavailable = errors.New("intel: job retry or cancel failed")

// split sorts refs' ids by table.
func split(refs []JobRef) (queries, jobs []uuid.UUID) {
	for _, r := range refs {
		if r.Query {
			queries = append(queries, r.ID)
		} else {
			jobs = append(jobs, r.ID)
		}
	}
	return queries, jobs
}

// failedRows reads the failed rows among refs, as mhai_jobs: ids, creator,
// hospital, request id and subject only. A jobs row with no hospital (a
// global job; none in Plan 4) is left out: it can't be published as a
// hospital's.
func failedRows(ctx context.Context, pool *pgxpool.Pool, refs []JobRef) ([]JobRow, error) {
	queries, jobs := split(refs)
	var rows []JobRow
	err := db.WithJobs(ctx, pool, func(tx pgx.Tx) error {
		if len(queries) > 0 {
			r, err := tx.Query(ctx, `SELECT id, hospital_id, created_by, COALESCE(request_id, ''), attempts, retry_count, patient_id
				FROM clinical_queries WHERE id = ANY($1) AND status = 'failed'`, queries)
			if err != nil {
				return err
			}
			for r.Next() {
				var s JobRow
				var by, pid *uuid.UUID
				if err := r.Scan(&s.ID, &s.HospitalID, &by, &s.RequestID, &s.Attempts, &s.RetryCount, &pid); err != nil {
					r.Close()
					return err
				}
				if by != nil {
					s.CreatedBy = *by
				}
				if pid != nil {
					s.PatientID = *pid
				}
				rows = append(rows, s)
			}
			r.Close()
			if err := r.Err(); err != nil {
				return err
			}
		}
		if len(jobs) > 0 {
			r, err := tx.Query(ctx, `SELECT id, hospital_id, created_by, COALESCE(request_id, ''), attempts, retry_count, type, subject_id
				FROM jobs WHERE id = ANY($1) AND status = 'failed' AND hospital_id IS NOT NULL`, jobs)
			if err != nil {
				return err
			}
			for r.Next() {
				var s JobRow
				var by *uuid.UUID
				if err := r.Scan(&s.ID, &s.HospitalID, &by, &s.RequestID, &s.Attempts, &s.RetryCount, &s.Type, &s.SubjectID); err != nil {
					r.Close()
					return err
				}
				if by != nil {
					s.CreatedBy = *by
				}
				if _, ok := jobBudgets[s.Type]; ok {
					rows = append(rows, s)
				}
			}
			r.Close()
			return r.Err()
		}
		return nil
	})
	if err != nil {
		return nil, ErrJobsUnavailable
	}
	return rows, nil
}

// requeued is a row the retry put back in the queue, and whether it may be
// published now (within its type's in-flight budget).
type requeued struct {
	row     JobRow
	publish bool
}

// lockKey is the advisory lock a row's queue takes: the per-hospital
// in-flight lock of ReserveInFlight for a question, QueueJob's per-(type,
// hospital) lock for a jobs row. Rows are re-queued in lockKey order, so
// two retries never take each other's locks in opposite orders.
func (r JobRow) lockKey() string {
	if !r.jobsRow() {
		return "mhai.inflight/" + r.HospitalID.String()
	}
	return "mhai.jobs." + r.Type + "/" + r.HospitalID.String()
}

// requeue puts each of rows (failed, re-checked by Publishable) back in
// the queue inside tx (as mhai_jobs), under the same lock and count as a
// new job of its kind:
//   - a question (run_query): ReserveInFlight's per-hospital lock; at the
//     hospital's cap of live questions it is not re-queued (the sweeper
//     publishes queued questions without a cap, so it can't wait);
//   - a jobs row: QueueJob's per-(type, hospital) lock and count, and
//     queued_at set to clock_timestamp() after the lock, so it takes its
//     place behind the live rows; past the budget it is re-queued
//     unpublished and the sweeper publishes it when a slot frees.
//
// Each update re-checks the row is still failed, and that none of its
// facts wait in the outbox: the drainer appends a fact only while it
// states the row's status (a correction.apply_failed for a failed job), so
// re-queueing before the drain would lose it; such a row is left for a
// later retry. retry_count goes up by one (the workers' attempts budget is
// per retry) and the deadline starts again.
func requeue(ctx context.Context, tx pgx.Tx, rows []JobRow, queryCap int) ([]requeued, error) {
	qop, ok := authz.OperationByName(runQuery)
	if !ok || queryCap < 1 {
		return nil, ErrMisuse
	}
	sorted := slices.Clone(rows)
	slices.SortStableFunc(sorted, func(a, b JobRow) int { return strings.Compare(a.lockKey(), b.lockKey()) })
	var out []requeued
	for _, r := range sorted {
		if !r.jobsRow() {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('mhai.inflight'), hashtext($1::text))`, r.HospitalID.String()); err != nil {
				return nil, err
			}
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM clinical_queries
				WHERE hospital_id = $1 AND status IN ('queued', 'running')
				  AND COALESCE(deadline_at, created_at + make_interval(secs => $2)) > now()`,
				r.HospitalID, qop.Timeout.Seconds()).Scan(&n); err != nil {
				return nil, err
			}
			if n >= queryCap {
				continue
			}
			err := tx.QueryRow(ctx, `UPDATE clinical_queries q
				SET status = 'queued', retry_count = q.retry_count + 1, error = NULL, published_at = NULL,
				    completed_at = NULL, progress = 0, current_stage = NULL, started_at = NULL, lease_until = NULL,
				    deadline_at = now() + make_interval(secs => $3)
				WHERE q.id = $1 AND q.hospital_id = $2 AND q.status = 'failed'
				  AND NOT EXISTS (SELECT 1 FROM intelligence_audit_outbox o WHERE o.query_id = q.id)
				RETURNING q.retry_count, q.deadline_at`, r.ID, r.HospitalID, qop.Timeout.Seconds()).Scan(&r.RetryCount, &r.Deadline)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out = append(out, requeued{row: r, publish: true})
			continue
		}
		budget, ok := jobBudgets[r.Type]
		op, known := authz.OperationByName("pipeline." + r.Type)
		if !ok || !known {
			continue
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('mhai.jobs.' || $1::text), hashtext($2::text))`,
			r.Type, r.HospitalID.String()); err != nil {
			return nil, err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM jobs j
			WHERE j.hospital_id = $1 AND j.type = $2 AND j.status IN ('queued', 'running')
			  AND `+jobDeadline("j.")+` > now()`, r.HospitalID, r.Type).Scan(&n); err != nil {
			return nil, err
		}
		err := tx.QueryRow(ctx, `UPDATE jobs j
			SET status = 'queued', retry_count = j.retry_count + 1, error = NULL, published_at = NULL,
			    completed_at = NULL, deadline_at = now() + make_interval(secs => $4), queued_at = clock_timestamp()
			WHERE j.id = $1 AND j.hospital_id = $2 AND j.type = $3 AND j.status = 'failed'
			  AND NOT EXISTS (SELECT 1 FROM intelligence_audit_outbox o WHERE o.job_id = j.id)
			RETURNING j.retry_count, j.deadline_at`, r.ID, r.HospitalID, r.Type, op.Timeout.Seconds()).Scan(&r.RetryCount, &r.Deadline)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, requeued{row: r, publish: n < budget})
	}
	return out, nil
}

// RetryJobs is the platform's retry (spec §6.2): each of refs that is
// failed now, whose creator can still use the service and still holds the
// operation's role, and (for a second_person job) whose subject still
// passes the two-person rule, is re-queued as a retry (requeue), with one
// job.retried audit row {job_count} for the call, the admin's, appended as
// mhai_platform in the same transaction. After the commit each row within
// its budget is re-minted as its creator with their current roles
// (Republish, which checks them again) under a new message id
// (PlatformRetryMsgID). A publish that fails leaves the row queued and
// unpublished for the sweeper. It returns the refs retried; every other
// ref was skipped (unknown, not failed, its creator unusable, the
// two-person rule, its facts still draining, or its hospital at the cap).
// Nothing is audited when nothing was retried.
//
// ErrJobsUnavailable: nothing was changed (the database, or a creator or
// subject check that couldn't be made: fail closed).
func RetryJobs(ctx context.Context, pool *pgxpool.Pool, client *Client, refs []JobRef, queryCap int, actor Actor, log *slog.Logger) ([]JobRef, error) {
	if client == nil || pool == nil || actor.UserID == uuid.Nil || !validRID(actor.RequestID) {
		return nil, ErrMisuse
	}
	rows, err := failedRows(ctx, pool, refs)
	if err != nil {
		return nil, err
	}
	var usable []JobRow
	for _, r := range rows {
		switch err := Publishable(ctx, pool, client, r); {
		case err == nil:
			usable = append(usable, r)
		case errors.Is(err, ErrCreatorUnusable), errors.Is(err, ErrJobSkipped), errors.Is(err, ErrMisuse),
			errors.Is(err, ErrNoPatient):
			// Skipped: never minted as someone else, never past the rule,
			// never for a patient who is no longer active.
		default:
			return nil, ErrJobsUnavailable
		}
	}
	var done []requeued
	if len(usable) > 0 {
		err = db.WithJobsThenPlatform(ctx, pool, func(tx pgx.Tx) error {
			var err error
			done, err = requeue(ctx, tx, usable, queryCap)
			return err
		}, func(tx pgx.Tx) error {
			if len(done) == 0 {
				return nil
			}
			return audit.Write(ctx, tx, audit.Event{ActorID: actor.UserID.String(), RequestID: actor.RequestID,
				Action: "job.retried", Payload: map[string]any{"job_count": len(done)}})
		})
		if err != nil {
			return nil, ErrJobsUnavailable
		}
	}
	out := make([]JobRef, 0, len(done))
	for _, d := range done {
		out = append(out, JobRef{Query: !d.row.jobsRow(), ID: d.row.ID})
		if !d.publish {
			continue
		}
		switch err := Republish(ctx, pool, client, d.row, PlatformRetryMsgID(d.row.ID, d.row.RetryCount)); {
		case err == nil:
			if markPublished(ctx, pool, d.row) != nil && log != nil {
				log.Warn("intel: retried job published, mark failed; the sweeper's copy is dropped as a duplicate",
					"job_id", d.row.ID.String(), "request_id", actor.RequestID)
			}
		case log != nil:
			// Ids and the classified error only. The sweeper publishes the
			// row, or cancels it if its creator can't use the service.
			log.Warn("intel: retried job not published; left for the sweeper", "job_id", d.row.ID.String(),
				"request_id", actor.RequestID, "error", err)
		}
	}
	return out, nil
}

// CancelJobs is the platform's cancel (spec §6.2): each of refs that is
// queued (a retry waiting included) or failed, and none of whose facts
// wait in the outbox (as requeue), is set cancelled with reason, cancelled
// by the admin, now, with the note (nil for none); a question's who, when
// and note go in query_cancellations (00016), never on clinical_queries.
// A running or finished job is left (mhai_jobs can't touch one), and so is
// one already cancelled. One job.cancelled audit row {job_count, reason}
// for the call, the admin's, as mhai_platform in the same transaction;
// never the note. It returns the refs cancelled; the rest were skipped.
func CancelJobs(ctx context.Context, pool *pgxpool.Pool, refs []JobRef, reason string, note *string, actor Actor) ([]JobRef, error) {
	if pool == nil || actor.UserID == uuid.Nil || !validRID(actor.RequestID) || !slices.Contains(CancelReasons, reason) {
		return nil, ErrMisuse
	}
	queries, jobs := split(refs)
	var out []JobRef
	err := db.WithJobsThenPlatform(ctx, pool, func(tx pgx.Tx) error {
		if len(queries) > 0 {
			r, err := tx.Query(ctx, `UPDATE clinical_queries q SET status = 'cancelled', cancel_reason = $2, completed_at = now()
				WHERE q.id = ANY($1) AND q.status IN ('queued', 'failed')
				  AND NOT EXISTS (SELECT 1 FROM intelligence_audit_outbox o WHERE o.query_id = q.id)
				RETURNING q.id, q.hospital_id`, queries, reason)
			if err != nil {
				return err
			}
			type gone struct{ id, hid uuid.UUID }
			var g []gone
			for r.Next() {
				var x gone
				if err := r.Scan(&x.id, &x.hid); err != nil {
					r.Close()
					return err
				}
				g = append(g, x)
			}
			r.Close()
			if err := r.Err(); err != nil {
				return err
			}
			for _, x := range g {
				if _, err := tx.Exec(ctx, `INSERT INTO query_cancellations (query_id, hospital_id, cancelled_by, cancel_note)
					VALUES ($1, $2, $3, $4)`, x.id, x.hid, actor.UserID, note); err != nil {
					return err
				}
				out = append(out, JobRef{Query: true, ID: x.id})
			}
		}
		if len(jobs) > 0 {
			r, err := tx.Query(ctx, `UPDATE jobs j SET status = 'cancelled', cancel_reason = $2, cancelled_by = $3,
				    cancelled_at = now(), cancel_note = $4, completed_at = now()
				WHERE j.id = ANY($1) AND j.status IN ('queued', 'failed')
				  AND NOT EXISTS (SELECT 1 FROM intelligence_audit_outbox o WHERE o.job_id = j.id)
				RETURNING j.id`, jobs, reason, actor.UserID, note)
			if err != nil {
				return err
			}
			for r.Next() {
				var id uuid.UUID
				if err := r.Scan(&id); err != nil {
					r.Close()
					return err
				}
				out = append(out, JobRef{ID: id})
			}
			r.Close()
			return r.Err()
		}
		return nil
	}, func(tx pgx.Tx) error {
		if len(out) == 0 {
			return nil
		}
		return audit.Write(ctx, tx, audit.Event{ActorID: actor.UserID.String(), RequestID: actor.RequestID,
			Action: "job.cancelled", Payload: map[string]any{"job_count": len(out), "reason": reason}})
	})
	if err != nil {
		return nil, ErrJobsUnavailable
	}
	return out, nil
}
