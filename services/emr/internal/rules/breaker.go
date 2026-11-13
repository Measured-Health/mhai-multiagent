package rules

// The per-source breaker (EMR design §12.4, §15.2; plan Task 29). A source
// whose critical findings spike, more than Multiple times its 14-day median
// per 15 minutes and more than FloorCritical in the last 15 minutes, is
// paused: the scheduler stops fetching from it, its status is behind at
// once (emr_platform_health, emr_sources_status), the trip is recorded and
// breaker_tripped is signalled to core by ids. Findings already made are
// kept and announced; the backlog waits for a platform admin's resume
// (plan Task 37). Historical findings don't count (a first sync would trip
// it); shadow ones do (a flood is a flood, alerting or not), but the
// consumer only runs the breaker while the rule set alerts for the
// hospital: a source is never paused for findings no one is shown. A
// source with less than 14 days of findings is held to YoungBaseline.

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/signals"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
)

// Breaker defaults: EMR_BREAKER_MULTIPLE and EMR_BREAKER_FLOOR_CRITICAL,
// as recommended until the clinical lead sets them (H4).
const (
	DefaultBreakerMultiple      = 5.0
	DefaultBreakerFloorCritical = 10
	breakerBaselineWindows      = 14 * 96 // 14 days of 15-minute windows
	// YoungBaseline is the baseline a source is held to until it has 14
	// days of findings: its median is still mostly empty windows, so the
	// floor alone would pause a new source on its first busy ward round.
	YoungBaseline = 4.0
)

// Breaker trips per source on critical findings.
type Breaker struct {
	Pool          *pgxpool.Pool
	Pub           signals.Publisher
	Multiple      float64
	FloorCritical int
	Now           func() time.Time
	Log           *slog.Logger
}

func (b *Breaker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Observe checks connection conn of hospital hid and pauses it when its
// critical findings in the last 15 minutes exceed both the floor and the
// multiple of its baseline. It reports whether this call paused it.
func (b *Breaker) Observe(ctx context.Context, hid, conn uuid.UUID) (bool, error) {
	now := b.now()
	var tripID uuid.UUID
	paused := false
	err := dbx.WithHospital(ctx, b.Pool, dbx.EMR, hid, func(tx pgx.Tx) error {
		paused = false
		// One check at a time per source.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('emr-breaker:' || $1::text, 0))`, conn); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM emr.findings WHERE hospital_id = $1 AND connection_id = $2
			AND severity = 'critical' AND status <> 'historical' AND created_at > $3::timestamptz - interval '15 minutes'
			AND created_at <= $3::timestamptz`, hid, conn, now).Scan(&count); err != nil {
			return err
		}
		if count <= b.FloorCritical {
			return nil
		}
		var baseline float64
		if err := tx.QueryRow(ctx, `WITH counts AS (
				SELECT floor(extract(epoch FROM ($3::timestamptz - interval '15 minutes' - f.created_at)) / 900)::int AS w,
				       count(*) AS n
				FROM emr.findings f
				WHERE f.hospital_id = $1 AND f.connection_id = $2 AND f.severity = 'critical' AND f.status <> 'historical'
				  AND f.created_at > $3::timestamptz - interval '15 minutes' - make_interval(mins => 15 * $4)
				  AND f.created_at <= $3::timestamptz - interval '15 minutes'
				GROUP BY 1)
			SELECT coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY coalesce(c.n, 0)), 0)::float8
			FROM generate_series(0, $4 - 1) AS g(w) LEFT JOIN counts c USING (w)`, hid, conn, now, breakerBaselineWindows).
			Scan(&baseline); err != nil {
			return err
		}
		var young bool
		if err := tx.QueryRow(ctx, `SELECT coalesce(min(created_at) > $3::timestamptz - interval '14 days', true)
			FROM emr.findings WHERE hospital_id = $1 AND connection_id = $2 AND severity = 'critical'
			AND status <> 'historical'`, hid, conn, now).Scan(&young); err != nil {
			return err
		}
		if young && baseline < YoungBaseline {
			baseline = YoungBaseline
		}
		if float64(count) <= b.Multiple*baseline {
			return nil
		}
		if err := tx.QueryRow(ctx, `SELECT emr_pause_connection($1)`, conn).Scan(&paused); err != nil {
			return err
		}
		if !paused {
			return nil // already paused, or switched off
		}
		return tx.QueryRow(ctx, `INSERT INTO emr.breaker_trips (hospital_id, connection_id, severity, window_start,
			window_count, baseline) VALUES ($1, $2, 'critical', $3::timestamptz - interval '15 minutes', $4, $5) RETURNING id`,
			hid, conn, now, count, baseline).Scan(&tripID)
	})
	if err != nil || !paused {
		return false, err
	}
	if b.Log != nil {
		// Pages on-call (design §15.2): ids and counts only.
		b.Log.Error("rules: source paused by the breaker", "hospital", hid.String(), "connection", conn.String())
	}
	if err := signals.Send(ctx, b.Pub, hid, signals.Signal{Kind: "breaker_tripped", ConnectionID: conn, RefID: tripID}); err != nil {
		// The pause stands and is visible (behind); core learns of it from
		// the connection's state too.
		if b.Log != nil {
			b.Log.Warn("rules: breaker signal not sent", "connection", conn.String())
		}
	}
	return true, nil
}
