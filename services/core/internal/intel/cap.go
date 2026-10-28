package intel

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DefaultQueryCap is INTELLIGENCE_QUERY_CAP's default: questions one
// hospital may have queued or running at once (decision 8).
const DefaultQueryCap = 10

var errReserve = errors.New("intel: in-flight reservation failed")

// ReserveInFlight takes one of hospital hid's in-flight slots inside tx,
// the request transaction that then inserts the queued row (authz.Tx, so
// tx is hid's own). It locks the hospital's slot counter
// (pg_advisory_xact_lock, held until tx ends), then counts its queued and
// running questions: at limit or more it returns ErrAtCap (429
// rate_limited). Lock first, count second, insert in the same
// transaction: two parallel submits can't both take the last slot.
//
// This is the main fairness control for JOBS (decision 8): one hospital
// can't hold more than limit jobs, whatever the stream's own bounds.
func ReserveInFlight(ctx context.Context, tx pgx.Tx, hid uuid.UUID, limit int) error {
	if hid == uuid.Nil || limit < 1 {
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
		FROM clinical_queries WHERE hospital_id = $1 AND status IN ('queued', 'running')`, hid).Scan(&n, &own); err != nil {
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
