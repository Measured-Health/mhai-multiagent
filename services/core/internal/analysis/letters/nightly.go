package letters

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
)

// Nightly ends letters whose last day has passed in their hospital's time
// zone and records the reminders due (design §8.1, §11 step 3; plan Task
// 20), for each hospital in due (analysis.ClearancesDue, whatever its
// switches). Access itself ended at the hospital's midnight through the
// cover check; this records it. Each hospital runs as the gateway for that
// hospital, with no user, with its entries (clearance.ended,
// clearance.reminder_sent with delivery pending) in the same transaction.
// A failed hospital is logged by id and the others still run. It returns
// the entries written.
func Nightly(ctx context.Context, pool *pgxpool.Pool, due []uuid.UUID, now time.Time, log *slog.Logger) (int, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	total := 0
	var first error
	for _, h := range due {
		written := 0
		err := db.WithHospital(ctx, pool, h, func(tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(ctx, `SELECT mhai_clearance_nightly($1)`, now).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return nil
			}
			var err error
			written, err = AuditPending(ctx, tx, "")
			return err
		})
		if err != nil {
			log.Error("letters: nightly step failed", "hospital_id", h.String(), "reason", "internal_error")
			if first == nil {
				first = err
			}
			continue
		}
		total += written
	}
	return total, first
}
