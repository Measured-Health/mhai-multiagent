package intel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
)

// DefaultQueryCap is INTELLIGENCE_QUERY_CAP's default: questions one
// hospital may have queued or running at once (decision 8).
const DefaultQueryCap = 10

var errReserve = errors.New("intel: in-flight reservation failed")

// ReserveInFlight takes one of hospital hid's in-flight slots inside tx,
// the request transaction that then inserts the queued row (authz.Tx, so
// tx is hid's own). It locks the hospital's slot counter
// (pg_advisory_xact_lock, held until tx ends), then counts its queued and
// running questions that are still within their deadline (deadline_at, or
// created_at plus run_query's timeout when unset): at limit or more it
// returns ErrAtCap (429 rate_limited). Lock first, count second, insert in
// the same transaction: two parallel submits can't both take the last slot.
//
// A row past its deadline holds no slot, so a worker that died on its last
// delivery, or a published message that was lost, can't lock a hospital
// out; the publish sweeper then fails the row (mhai_fail_expired_queries).
//
// This is the main fairness control for JOBS (decision 8): one hospital
// can't hold more than limit live jobs, whatever the stream's own bounds.
func ReserveInFlight(ctx context.Context, tx pgx.Tx, hid uuid.UUID, limit int) error {
	op, ok := authz.OperationByName(runQuery)
	if hid == uuid.Nil || limit < 1 || !ok {
		return ErrMisuse
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('mhai.inflight'), hashtext($1::text))`, hid.String()); err != nil {
		return errReserve
	}
	// RLS shows a transaction only its own hospital's rows, so a count for
	// another hospital would be 0: refuse instead of letting it through.
	var n int
	var own bool
	if err := tx.QueryRow(ctx, `SELECT count(*), COALESCE(mhai_current_hospital() = $1, false)
		FROM clinical_queries
		WHERE hospital_id = $1 AND status IN ('queued', 'running')
		  AND COALESCE(deadline_at, created_at + make_interval(secs => $2)) > now()`,
		hid, op.Timeout.Seconds()).Scan(&n, &own); err != nil {
		return errReserve
	}
	if !own {
		return ErrNoHospital
	}
	if n >= limit {
		return ErrAtCap
	}
	return nil
}

// jobBudgets are the in-flight budgets of the jobs-table types (Plan 4
// spec §4.5): approve_correction jobs one hospital may have published and
// live at once. A type with no budget can't be queued (QueueJob refuses).
var jobBudgets = map[string]int{"approve_correction": 20}

// jobDeadlineSQL is a jobs row's deadline: deadline_at, or created_at plus
// 10 minutes when unset, as mhai_fail_expired_jobs() (00012) has it.
const jobDeadlineSQL = `COALESCE(%[1]sdeadline_at, %[1]screated_at + interval '10 minutes')`

func jobDeadline(alias string) string { return fmt.Sprintf(jobDeadlineSQL, alias) }

// NewJob is a jobs row to queue: ids from core, never from a request body.
type NewJob struct {
	ID, HospitalID, SubjectID, CreatedBy uuid.UUID
	Type                                 string // a jobBudgets key
	RequestID                            string
}

// QueueJob inserts j as a queued jobs row inside tx, the request
// transaction (db.WithHospital or WithHospitalUser of j's own hospital),
// with a deadline of the operation's timeout. It reports the deadline and
// whether the row is within its type's in-flight budget, that is, whether
// the caller may publish it after the commit.
//
// Reserve, then check: it locks the hospital's counter for the type
// (pg_advisory_xact_lock, held until tx ends), counts the type's live rows
// (queued or running, within their deadline), and inserts the row with
// created_at taken after the lock (clock_timestamp()), so the rows of a
// hospital and type are ordered as they took the lock. A row may be
// published only while fewer than the budget live rows are ahead of it,
// here and in the sweeper (SweepOnce): a row's place only moves up, so no
// more than the budget are ever published and live together. Past the
// budget the row still commits (the decision behind it stands) and waits,
// unpublished, for the sweeper.
//
// Errors: ErrMisuse (no such type, nil ids), ErrNoHospital (tx isn't j's
// hospital's), errReserve (the database). The driver's error is never
// passed on.
func QueueJob(ctx context.Context, tx pgx.Tx, j NewJob) (deadline time.Time, within bool, err error) {
	budget, ok := jobBudgets[j.Type]
	op, known := authz.OperationByName("pipeline." + j.Type)
	if !ok || !known || j.ID == uuid.Nil || j.HospitalID == uuid.Nil || j.SubjectID == uuid.Nil || j.CreatedBy == uuid.Nil {
		return time.Time{}, false, ErrMisuse
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('mhai.jobs.' || $1::text), hashtext($2::text))`,
		j.Type, j.HospitalID.String()); err != nil {
		return time.Time{}, false, errReserve
	}
	var n int
	var own bool
	if err := tx.QueryRow(ctx, `SELECT count(*), COALESCE(mhai_current_hospital() = $1, false)
		FROM jobs j
		WHERE j.hospital_id = $1 AND j.type = $2 AND j.status IN ('queued', 'running')
		  AND `+jobDeadline("j.")+` > now()`, j.HospitalID, j.Type).Scan(&n, &own); err != nil {
		return time.Time{}, false, errReserve
	}
	if !own {
		return time.Time{}, false, ErrNoHospital
	}
	var rid any
	if j.RequestID != "" {
		rid = j.RequestID
	}
	if err := tx.QueryRow(ctx, `INSERT INTO jobs (id, hospital_id, type, subject_id, created_by, request_id, created_at, deadline_at)
		VALUES ($1, $2, $3, $4, $5, $6, clock_timestamp(), now() + make_interval(secs => $7))
		RETURNING deadline_at`,
		j.ID, j.HospitalID, j.Type, j.SubjectID, j.CreatedBy, rid, op.Timeout.Seconds()).Scan(&deadline); err != nil {
		return time.Time{}, false, errReserve
	}
	return deadline, n < budget, nil
}
