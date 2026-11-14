package analysis

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
)

// ClearancesDue lists, as mhai_analysis_job (the nightly run's role), the
// hospitals with a clearance letter to end or a reminder to record at now
// (migration 00034, mhai_clearance_due). internal/analysis/letters then
// serves each as that hospital (its Nightly); the job's pool stays in this
// package (TestRoutesCantReachAnalysisJobPool).
func ClearancesDue(ctx context.Context, pool *pgxpool.Pool, now time.Time) ([]uuid.UUID, error) {
	var due []uuid.UUID
	err := db.WithAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT mhai_clearance_due($1)`, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h uuid.UUID
			if err := rows.Scan(&h); err != nil {
				return err
			}
			due = append(due, h)
		}
		return rows.Err()
	})
	return due, err
}
