package intel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

// JobRow is what the sweeper (and later the dead-letter consumer) knows of
// a queued job: ids and times, never the question.
type JobRow struct {
	ID, HospitalID, CreatedBy uuid.UUID // CreatedBy is uuid.Nil when none
	RequestID                 string
	Attempts                  int
	Deadline                  time.Time
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

// Republish re-mints and publishes row's run_query job with message id
// msgID: the job id for a sweep, RetryMsgID for a retry after a dead letter
// (spec §7.4: a retry never reuses the old message). It is the one re-mint
// helper (F23). It first re-checks the creator (as mhai_auth): the account
// must still be usable (authz.AccountUsable) and belong to the row's
// hospital, else ErrCreatorUnusable. The token is minted as the creator,
// with the creator's current roles (run_query allows no svc:core); none of
// them allowed is ErrCreatorUnusable too. A failed lookup is an error to
// retry later, never a cancellation. The request id is JobRID.
func Republish(ctx context.Context, pool *pgxpool.Pool, client *Client, row JobRow, msgID string) error {
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
	caller := Caller{Sub: row.CreatedBy.String(), Hid: row.HospitalID.String(), Rid: JobRID(row.ID, row.RequestID), Roles: roles}
	err = client.publishJob(ctx, caller, runQuery, msgID, &intelpb.RunQueryJob{QueryId: row.ID.String()}, row.Deadline)
	if errors.Is(err, ErrNotAllowed) {
		return ErrCreatorUnusable
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
				"cancelled", res.Cancelled)
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
//     created_at plus run_query's timeout when unset), published or not,
//     is failed with deadline_exceeded and audited query.failed, through
//     mhai_fail_expired_queries() (up to 100 a sweep). So a worker that
//     died on its last delivery, or a lost publish, ends and frees its
//     in-flight slot.
//  2. Up to 100 queued rows within their deadline that have waited over
//     10 s with no published_at, oldest first: a creator who can't use the
//     service any more gets the row cancelled (account_unusable, audited
//     query.cancelled); otherwise Republish with the job id as Nats-Msg-Id
//     (JetStream drops the duplicate if the first publish did land), then
//     published_at.
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
	for _, r := range rows {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		switch err := Republish(ctx, pool, client, r, r.ID.String()); {
		case err == nil:
			if markPublished(ctx, pool, r.ID) != nil {
				res.Left++
			} else {
				res.Published++
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
	if res.Left > 0 {
		return res, errSweep
	}
	return res, nil
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

func markPublished(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) error {
	return db.WithJobs(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE clinical_queries SET published_at = now()
			WHERE id = $1 AND status = 'queued' AND published_at IS NULL`, id)
		if err != nil {
			return errSweep
		}
		return nil
	})
}

// cancelRow cancels a queued, unpublished row whose creator can't use the
// service (account_unusable) and audits it in the same transaction, with
// the row's hospital and request id (JobRID) and no actor.
func cancelRow(ctx context.Context, pool *pgxpool.Pool, row JobRow) error {
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
