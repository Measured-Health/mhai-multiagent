package scheduler

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
)

// DB is the Source over Postgres, every statement as mhai_analytics_job
// (db.WithAnalyticsJob): the four functions of 00019 and
// analytics_snapshots. Errors carry no figure: a driver error is replaced by
// a fixed one.
type DB struct {
	Pool *pgxpool.Pool
}

var errDB = errors.New("scheduler: database")

func (s DB) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	if err := db.WithAnalyticsJob(ctx, s.Pool, fn); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "22023" {
			return ErrNotFreezable
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return errDB
	}
	return nil
}

func date(t time.Time) string { return t.Format(time.DateOnly) }

// Freeze runs mhai_analytics_freeze.
func (s DB) Freeze(ctx context.Context, p calendar.Period) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT mhai_analytics_freeze($1, $2::date)`, string(p.Kind), date(p.From))
		return err
	})
}

// Frozen reads one block's frozen rows.
func (s DB) Frozen(ctx context.Context, q calendar.Period) ([]FrozenRow, error) {
	var out []FrozenRow
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT kind, period_from, hospital_id, q_pass, q_pass_degraded, q_regenerate, q_regenerate_degraded,
			q_escalate, q_escalate_degraded, q_unanswered, r_agreed, r_corrected, active_clinicians, reviews_decided, median_review_hours
			FROM mhai_analytics_frozen($1::date)`, date(q.From))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r FrozenRow
			var kind string
			if err := rows.Scan(&kind, &r.From, &r.Hospital, &r.QG[0], &r.QG[1], &r.QG[2], &r.QG[3], &r.QG[4], &r.QG[5], &r.QG[6],
				&r.R[0], &r.R[1], &r.Clinicians, &r.Decided, &r.Median); err != nil {
				return err
			}
			r.Kind = calendar.Kind(kind)
			r.From = r.From.UTC()
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// Hospitals reads every hospital's name, status and creation.
func (s DB) Hospitals(ctx context.Context) ([]Hospital, error) {
	var out []Hospital
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT hospital_id, name, status, created_at FROM mhai_analytics_onboarding()`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h Hospital
			if err := rows.Scan(&h.ID, &h.Name, &h.Status, &h.CreatedAt); err != nil {
				return err
			}
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}

// Staff reads each active hospital's staff count.
func (s DB) Staff(ctx context.Context) (map[uuid.UUID]int64, error) {
	out := map[uuid.UUID]int64{}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT hospital_id, staff FROM mhai_analytics_staff()`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var n int64
			if err := rows.Scan(&id, &n); err != nil {
				return err
			}
			out[id] = n
		}
		return rows.Err()
	})
	return out, err
}

// Stored reads the block's stored snapshots.
func (s DB) Stored(ctx context.Context, q calendar.Period) ([]Stored, error) {
	var out []Stored
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT kind, period_from, state, body::text FROM analytics_snapshots
			WHERE period_from >= $1::date AND period_from < $2::date`, date(q.From), date(q.To))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind, state string
			var from time.Time
			var body *string
			if err := rows.Scan(&kind, &from, &state, &body); err != nil {
				return err
			}
			st := Stored{Period: calendar.Of(calendar.Kind(kind), from.UTC()), State: state}
			if body != nil {
				st.Body = []byte(*body)
			}
			out = append(out, st)
		}
		return rows.Err()
	})
	return out, err
}

// Insert stores p's snapshot once (ON CONFLICT DO NOTHING).
func (s DB) Insert(ctx context.Context, p calendar.Period, state string, body []byte) error {
	var b any
	if body != nil {
		b = string(body)
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO analytics_snapshots (kind, period_from, period_to, state, body)
			VALUES ($1, $2::date, $3::date, $4, $5::jsonb) ON CONFLICT (kind, period_from) DO NOTHING`,
			string(p.Kind), date(p.From), date(p.To), state, b)
		return err
	})
}

// Epoch reads the release epoch.
func (s DB) Epoch(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := s.tx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT released_at FROM analytics_epoch`).Scan(&t)
	})
	return t.UTC(), err
}

// Now is the database's clock.
func (s DB) Now(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := s.tx(ctx, func(tx pgx.Tx) error { return tx.QueryRow(ctx, `SELECT now()`).Scan(&t) })
	return t.UTC(), err
}

// StoredKeys lists the stored periods since a time.
func (s DB) StoredKeys(ctx context.Context, since time.Time) (map[string]bool, error) {
	out := map[string]bool{}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT kind, period_from FROM analytics_snapshots WHERE period_from >= $1::date`, date(since.AddDate(0, 0, -1)))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind string
			var from time.Time
			if err := rows.Scan(&kind, &from); err != nil {
				return err
			}
			out[key(calendar.Of(calendar.Kind(kind), from.UTC()))] = true
		}
		return rows.Err()
	})
	return out, err
}
