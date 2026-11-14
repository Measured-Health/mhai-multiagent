package analysis

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
)

// DBStore is the Store over Postgres: every statement as mhai_analysis_job
// (db.WithAnalysisJob), through migration 00021's functions.
type DBStore struct {
	Pool *pgxpool.Pool
	// Source replaces mhai_analysis_ingest_source (tests only). nil: the
	// function.
	Source func(ctx context.Context, tx pgx.Tx, hospital uuid.UUID, after *time.Time, until time.Time) ([]SourceRow, error)
}

func (s *DBStore) Due(ctx context.Context, now time.Time) ([]Due, error) {
	var out []Due
	err := db.WithAnalysisJob(ctx, s.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT hospital_id, analysis_on, demo, ingested_until FROM mhai_analysis_due($1)`, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d Due
			if err := rows.Scan(&d.Hospital, &d.On, &d.Demo, &d.IngestedUntil); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

func (s *DBStore) Ingest(ctx context.Context, hospital uuid.UUID, after *time.Time, until time.Time,
	build func([]SourceRow) ([]byte, error)) (int, error) {
	var late int
	err := db.WithAnalysisJob(ctx, s.Pool, func(tx pgx.Tx) error {
		src := s.Source
		if src == nil {
			src = sqlSource
		}
		rows, err := src(ctx, tx, hospital, after, until)
		if err != nil {
			return err
		}
		body, err := build(rows)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT mhai_analysis_ingest($1, $2, $3::jsonb)`, hospital, until, string(body)).Scan(&late)
	})
	return late, err
}

func sqlSource(ctx context.Context, tx pgx.Tx, hospital uuid.UUID, after *time.Time, until time.Time) ([]SourceRow, error) {
	rows, err := tx.Query(ctx, `SELECT ref, day, kind, condition_id, age_band, sex, setting, location_id, specialty, source_id,
		urgency, confidence, rating, correction, flag, severity, ack_minutes, dismiss_reason
		FROM mhai_analysis_ingest_source($1, $2, $3)`, hospital, after, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceRow
	for rows.Next() {
		var r SourceRow
		if err := rows.Scan(&r.Ref, &r.Day, &r.Kind, &r.Condition, &r.AgeBand, &r.Sex, &r.Setting, &r.Location, &r.Specialty,
			&r.Source, &r.Urgency, &r.Confidence, &r.Rating, &r.Correction, &r.Flag, &r.Severity, &r.AckMinutes,
			&r.Dismiss); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Changes reads the patient-index changes after the hospital's changes
// watermark, builds them with build and applies them, in one transaction;
// it returns the subjects changed.
func (s *DBStore) Changes(ctx context.Context, hospital uuid.UUID, until time.Time,
	build func([]ChangeRow) ([]byte, error)) (int, error) {
	var n int
	err := db.WithAnalysisJob(ctx, s.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT kind, ref, survivor FROM mhai_analysis_patient_changes($1, $2)`, hospital, until)
		if err != nil {
			return err
		}
		var list []ChangeRow
		for rows.Next() {
			var c ChangeRow
			if err := rows.Scan(&c.Kind, &c.Ref, &c.Survivor); err != nil {
				rows.Close()
				return err
			}
			list = append(list, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		body, err := build(list)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT mhai_analysis_apply_changes($1, $2, $3::jsonb)`, hospital, until, string(body)).Scan(&n)
	})
	return n, err
}

func (s *DBStore) Sweep(ctx context.Context, hospital uuid.UUID, now time.Time) error {
	return db.WithAnalysisJob(ctx, s.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT mhai_analysis_sweep($1, $2)`, hospital, now)
		return err
	})
}

func (s *DBStore) Freeze(ctx context.Context, hospital uuid.UUID, now time.Time) (int, error) {
	var n int
	err := db.WithAnalysisJob(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.analysis.engine', $1, true)`, EngineVersion); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT mhai_analysis_freeze($1, $2)`, hospital, now).Scan(&n)
	})
	return n, err
}

func (s *DBStore) Ran(ctx context.Context, hospital uuid.UUID, now time.Time) error {
	return db.WithAnalysisJob(ctx, s.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT mhai_analysis_ran($1, $2)`, hospital, now)
		return err
	})
}

// DBClock is the database's clock.
type DBClock struct{ Pool *pgxpool.Pool }

func (c DBClock) Now(ctx context.Context) (time.Time, error) {
	var now time.Time
	err := c.Pool.QueryRow(ctx, `SELECT now()`).Scan(&now)
	return now, err
}

// advisoryLock is a session advisory lock on one pooled connection, held
// for a tick (as 7c's scheduler).
func advisoryLock(pool *pgxpool.Pool, name string) func(ctx context.Context) (func(), bool, error) {
	return func(ctx context.Context) (func(), bool, error) {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			return nil, false, err
		}
		var locked bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, name).Scan(&locked); err != nil || !locked {
			conn.Release()
			return nil, false, err
		}
		return func() {
			uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock(hashtext($1))`, name); err != nil {
				_ = conn.Conn().Close(uctx) // a lock that may still be held: drop the connection
			}
			conn.Release()
		}, true, nil
	}
}
