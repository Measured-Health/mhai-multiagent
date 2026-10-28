package intel

import (
	"context"
	"errors"
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
	// runQuery is the job behind every clinical_queries row.
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

// Republish re-mints and publishes row's run_query job with message id
// msgID: the job id for a sweep, a new id for a retry after a dead letter
// (spec §7.4: a retry never reuses the old message). It is the one re-mint
// helper (F23). It first re-checks the creator (as mhai_auth): the account
// must still be usable (authz.AccountUsable) and belong to the row's
// hospital, else ErrCreatorUnusable. The token is minted as the creator,
// with the creator's current roles (run_query allows no svc:core); none of
// them allowed is ErrCreatorUnusable too. A failed lookup is an error to
// retry later, never a cancellation.
func Republish(ctx context.Context, pool *pgxpool.Pool, client *Client, row JobRow, msgID uuid.UUID) error {
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
	rid := row.RequestID
	if !validRID(rid) {
		rid = uuid.NewString()
	}
	caller := Caller{Sub: row.CreatedBy.String(), Hid: row.HospitalID.String(), Rid: rid, Roles: roles}
	err = client.PublishJob(ctx, caller, runQuery, msgID, &intelpb.RunQueryJob{QueryId: row.ID.String()}, row.Deadline)
	if errors.Is(err, ErrNotAllowed) {
		return ErrCreatorUnusable
	}
	return err
}

// RunPublishSweeper runs SweepOnce now and then every interval (SweepEvery
// when not positive) until ctx ends. A failed sweep is logged and tried
// again next tick.
func RunPublishSweeper(ctx context.Context, pool *pgxpool.Pool, client *Client, every time.Duration) {
	if every <= 0 {
		every = SweepEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := SweepOnce(ctx, pool, client); err != nil && ctx.Err() == nil {
			client.log().Error("intel: publish sweep failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type sweepRow struct {
	JobRow
	expired bool
}

// SweepOnce handles up to 100 queued rows that have waited over 10 s with
// no published_at, oldest first, as mhai_jobs:
//   - past its deadline (deadline_at, or created_at plus run_query's
//     timeout when unset): failed / deadline_exceeded, audited query.failed;
//   - creator unusable: cancelled / account_unusable, audited
//     query.cancelled, nothing published;
//   - otherwise Republish with the job id as Nats-Msg-Id (JetStream drops
//     the duplicate if the first publish did land), then published_at.
//
// A row that fails (NATS down, JOBS full) stays as it is for the next
// sweep. Each update re-checks the row is still queued and unpublished.
func SweepOnce(ctx context.Context, pool *pgxpool.Pool, client *Client) error {
	op, ok := authz.OperationByName(runQuery)
	if !ok {
		return errSweep
	}
	var rows []sweepRow
	err := db.WithJobs(ctx, pool, func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `
			SELECT id, hospital_id, created_by, COALESCE(request_id, ''), attempts, d, d <= now()
			FROM (SELECT id, hospital_id, created_by, request_id, attempts, created_at,
			             COALESCE(deadline_at, created_at + make_interval(secs => $2)) AS d
			      FROM clinical_queries
			      WHERE status = 'queued' AND published_at IS NULL
			        AND created_at < now() - make_interval(secs => $1)) q
			ORDER BY created_at
			LIMIT $3`, sweepAge.Seconds(), op.Timeout.Seconds(), sweepBatch)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var s sweepRow
			var by *uuid.UUID
			if err := r.Scan(&s.ID, &s.HospitalID, &by, &s.RequestID, &s.Attempts, &s.Deadline, &s.expired); err != nil {
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
		return errSweep
	}
	failed := 0
	for _, r := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := sweepRowOnce(ctx, pool, client, r)
		if err != nil {
			failed++
			client.log().Warn("intel: sweep left a job for next time", "query_id", r.ID.String(),
				"hospital_id", r.HospitalID.String(), "error", err)
		}
	}
	if failed > 0 {
		return errSweep
	}
	return nil
}

func sweepRowOnce(ctx context.Context, pool *pgxpool.Pool, client *Client, r sweepRow) error {
	if r.expired {
		return finish(ctx, pool, r.JobRow, "failed")
	}
	switch err := Republish(ctx, pool, client, r.JobRow, r.ID); {
	case err == nil:
		return markPublished(ctx, pool, r.ID)
	case errors.Is(err, ErrCreatorUnusable):
		return finish(ctx, pool, r.JobRow, "cancelled")
	case errors.Is(err, ErrDeadline):
		return finish(ctx, pool, r.JobRow, "failed")
	default:
		return err
	}
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

// finish ends a queued, unpublished row as failed (deadline_exceeded) or
// cancelled (account_unusable), and audits it in the same transaction, with
// the row's hospital and request id and no actor (core did it).
func finish(ctx context.Context, pool *pgxpool.Pool, row JobRow, status string) error {
	q := `UPDATE clinical_queries SET status = 'failed', error = 'deadline_exceeded', completed_at = now()
		WHERE id = $1 AND status = 'queued' AND published_at IS NULL`
	ev := audit.Event{HospitalID: row.HospitalID.String(), RequestID: row.RequestID, Action: "query.failed",
		Entity: "query", EntityID: row.ID.String(),
		Payload: map[string]any{"query_id": row.ID.String(), "error": "deadline_exceeded", "attempts": row.Attempts}}
	if status == "cancelled" {
		q = `UPDATE clinical_queries SET status = 'cancelled', cancel_reason = '` + cancelUnusable + `', completed_at = now()
			WHERE id = $1 AND status = 'queued' AND published_at IS NULL`
		ev.Action = "query.cancelled"
		ev.Payload = map[string]any{"query_id": row.ID.String(), "reason": cancelUnusable}
	}
	return db.WithJobs(ctx, pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, q, row.ID)
		if err != nil {
			return errSweep
		}
		if tag.RowsAffected() != 1 {
			return nil // someone else moved it on
		}
		if err := audit.Write(ctx, tx, ev); err != nil {
			return errSweep
		}
		return nil
	})
}
