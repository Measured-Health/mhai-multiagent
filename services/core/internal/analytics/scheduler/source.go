package scheduler

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
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
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrOutOfOrder) {
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

// ChangesDue lists the stored weeks newer than the newest evaluated one
// that start at or after the release epoch, oldest first: a first run
// evaluates only weeks that close after the release (spec §18, 7c-3 item
// 12), never a week stored from before it.
func (s DB) ChangesDue(ctx context.Context) ([]calendar.Period, error) {
	var out []calendar.Period
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT period_from FROM analytics_snapshots
			WHERE kind = 'week' AND period_from > coalesce((SELECT max(week_from) FROM analytics_change_runs), '-infinity'::date)
			  AND (period_from::timestamp AT TIME ZONE 'UTC') >= (SELECT released_at FROM analytics_epoch)
			ORDER BY period_from`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var from time.Time
			if err := rows.Scan(&from); err != nil {
				return err
			}
			out = append(out, calendar.Of(calendar.Week, from.UTC()))
		}
		return rows.Err()
	})
	return out, err
}

// ApplyChanges records week w's run and opens, updates and closes the open
// changes to match flags, in one transaction as mhai_analytics_job; nothing
// when w was evaluated already. Values and summaries go in as parameters;
// none is ever logged.
func (s DB) ApplyChanges(ctx context.Context, w, m calendar.Period, flags []analytics.Flag) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		// Weeks are evaluated in calendar order: an older week after a newer
		// one would close the newer week's changes.
		var newer bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM analytics_change_runs WHERE week_from > $1::date)`,
			date(w.From)).Scan(&newer); err != nil {
			return err
		}
		if newer {
			return ErrOutOfOrder
		}
		tag, err := tx.Exec(ctx, `INSERT INTO analytics_change_runs (week_from, week_to, month_from, month_to)
			VALUES ($1::date, $2::date, $3::date, $4::date) ON CONFLICT (week_from) DO NOTHING`,
			date(w.From), date(w.To), date(m.From), date(m.To))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return nil // evaluated already
		}
		rows, err := tx.Query(ctx, `SELECT id, hospital_id, measure, direction FROM analytics_changes
			WHERE closed_week IS NULL FOR UPDATE`)
		if err != nil {
			return err
		}
		open := map[string]uuid.UUID{}
		for rows.Next() {
			var id uuid.UUID
			var h *uuid.UUID
			var measure, dir string
			if err := rows.Scan(&id, &h, &measure, &dir); err != nil {
				rows.Close()
				return err
			}
			f := analytics.Flag{Measure: measure, Direction: dir}
			if h != nil {
				f.Hospital = &analytics.Ref{ID: h.String()}
			}
			open[f.Key()] = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		flagged := map[string]bool{}
		for _, f := range flags {
			flagged[f.Key()] = true
		}
		// Close first: a change that turned round closes before its
		// opposite opens.
		for k, id := range open {
			if flagged[k] {
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE analytics_changes SET closed_week = $2::date WHERE id = $1`, id, date(w.From)); err != nil {
				return err
			}
		}
		for _, f := range flags {
			cur, _ := f.Current.MarshalJSON()
			prev, _ := f.Previous.MarshalJSON()
			if id, ok := open[f.Key()]; ok {
				if _, err := tx.Exec(ctx, `UPDATE analytics_changes SET current = $2::jsonb, previous = $3::jsonb, summary = $4,
					last_week = $5::date WHERE id = $1`, id, string(cur), string(prev), f.Summary, date(w.From)); err != nil {
					return err
				}
				continue
			}
			var h any
			if f.Hospital != nil {
				id, err := uuid.Parse(f.Hospital.ID)
				if err != nil {
					return err
				}
				h = id
			}
			if _, err := tx.Exec(ctx, `INSERT INTO analytics_changes (hospital_id, area, measure, direction, current, previous, summary,
				opened_week, last_week) VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8::date, $8::date)`,
				h, f.Area, f.Measure, f.Direction, string(cur), string(prev), f.Summary, date(w.From)); err != nil {
				return err
			}
		}
		return nil
	})
}
