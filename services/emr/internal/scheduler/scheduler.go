// Package scheduler polls the enabled connections (EMR design §7.1, §15;
// plan Task 24): every Tick it reads which connections are due, as the
// scheduler's role across hospitals (ids and due times only), and runs each
// one's fetch under a per-connection advisory lock, so two emr copies never
// fetch one connection at once. Each run is one emr.sync_runs row with a
// closed outcome code, an emr.connection_errors row on failure, and the
// connection's state (cursor, due time, failure counts, backfill end,
// reconciliation day) in emr.sync_state.
//
// Fail closed: a run is finished in the database only after it has
// ended; a run that dies leaves no finished row, so the source's last
// successful poll ages and it reads as stale, never fresh. The secret
// store down is unreachable, never sign_in_refused; three sign-in refusals
// in a row stop polling until a test passes (Resume). Nothing is logged
// but ids, codes and counts.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/creds"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/secrets"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrtypes"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
)

// Timing (design §7.1, §5.1, §15.2).
const (
	Tick            = 10 * time.Second
	RunTimeout      = 10 * time.Minute
	StopAfterSignIn = 3
	ReconcileHour   = 2
	ReconcileWindow = 24 * time.Hour
	// maxDue bounds how many due connections one tick takes.
	maxDue = 50
)

// Sink stores one run's records (sink.Sink).
type Sink interface {
	// PutStored stores r once; stored is false for a replay.
	PutStored(ctx context.Context, r connector.RawRecord) (stored bool, err error)
	// Flush announces what was stored.
	Flush(ctx context.Context) error
}

// Metrics are counts only.
type Metrics struct {
	Runs, Failed, Skipped atomic.Int64
	// ReconciledLate is emr_reconciled_late_total: records only the daily
	// reconciliation found.
	ReconciledLate atomic.Int64
}

// Scheduler runs the polls.
type Scheduler struct {
	Pool       *pgxpool.Pool
	Connectors *connector.Registry
	Store      secrets.Store
	// Env is APP_ENV (the store's path segment; development allows a
	// loopback address).
	Env string
	// NewSink makes a run's sink; backfill flags its records.
	NewSink func(cfg connector.Config, backfill bool) Sink
	Now     func() time.Time
	// Jitter returns a value in [-1, 1] (nil: random).
	Jitter  func() float64
	Log     *slog.Logger
	Metrics *Metrics
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Scheduler) jitter() float64 {
	if s.Jitter != nil {
		return s.Jitter()
	}
	return rand.Float64()*2 - 1
}

func (s *Scheduler) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (s *Scheduler) metrics() *Metrics {
	if s.Metrics == nil {
		s.Metrics = &Metrics{}
	}
	return s.Metrics
}

// Run polls every Tick until ctx ends.
func (s *Scheduler) Run(ctx context.Context) error {
	t := time.NewTicker(Tick)
	defer t.Stop()
	for {
		if _, err := s.Once(ctx); err != nil && ctx.Err() == nil {
			s.log().Warn("emr scheduler: due connections not read", "code", "database_unavailable")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

type due struct{ id, hospital uuid.UUID }

// Once runs every connection due now, one at a time, and returns how many
// ran.
func (s *Scheduler) Once(ctx context.Context) (int, error) {
	list, err := s.due(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range list {
		if ctx.Err() != nil {
			break
		}
		ran, err := s.runOne(ctx, d)
		if err != nil {
			s.log().Warn("emr scheduler: run not recorded", "connection", d.id.String(), "code", "database_unavailable")
		}
		if ran {
			n++
		}
	}
	return n, nil
}

// due reads, as the scheduler's role, the enabled, unpaused connections
// that are due and not stopped by sign-in refusals.
func (s *Scheduler) due(ctx context.Context) ([]due, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_emr_sched`); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT c.id, c.hospital_id FROM public.emr_connections c
		LEFT JOIN emr.sync_state s ON s.connection_id = c.id
		WHERE c.enabled AND NOT c.paused
		  AND (s.connection_id IS NULL OR (s.next_due_at <= $1 AND s.sign_in_failures < $2))
		ORDER BY s.next_due_at NULLS FIRST LIMIT $3`, s.now(), StopAfterSignIn, maxDue)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
		var d due
		err := r.Scan(&d.id, &d.hospital)
		return d, err
	})
}

// conn is a connection and its state, read under its hospital.
type conn struct {
	cfg        connector.Config
	stored     []emrurl.Origin
	enabled    bool
	paused     bool
	active     bool
	tz         string
	cursor     []byte
	live       *time.Time
	nextDue    time.Time
	failures   int
	signIn     int
	reconciled *time.Time
	hasState   bool
}

var errGone = errors.New("scheduler: connection not found")

func (s *Scheduler) load(ctx context.Context, d due) (conn, error) {
	var c conn
	err := dbx.WithHospital(ctx, s.Pool, dbx.EMR, d.hospital, func(tx pgx.Tx) error {
		var settings map[string]string
		var origin []string
		var status string
		err := tx.QueryRow(ctx, `SELECT c.type, c.base_url, c.settings, c.credentials_origin, c.poll_seconds, c.enabled, c.paused,
			h.status, h.time_zone
			FROM public.emr_connections c JOIN public.hospitals h ON h.id = c.hospital_id
			WHERE c.id = $1 AND c.hospital_id = $2`, d.id, d.hospital).
			Scan(&c.cfg.Type, &c.cfg.BaseURL, &settings, &origin, &c.cfg.PollSeconds, &c.enabled, &c.paused, &status, &c.tz)
		if errors.Is(err, pgx.ErrNoRows) {
			return errGone
		}
		if err != nil {
			return err
		}
		c.cfg.ConnectionID, c.cfg.HospitalID, c.cfg.Settings, c.active = d.id, d.hospital, settings, status == "active"
		for _, o := range origin {
			c.stored = append(c.stored, emrurl.Origin(o))
		}
		slices.Sort(c.stored)
		err = tx.QueryRow(ctx, `SELECT cursor, live_since, next_due_at, failures, sign_in_failures, reconciled_on
			FROM emr.sync_state WHERE connection_id = $1`, d.id).
			Scan(&c.cursor, &c.live, &c.nextDue, &c.failures, &c.signIn, &c.reconciled)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			_, err = tx.Exec(ctx, `INSERT INTO emr.sync_state (connection_id, hospital_id, next_due_at) VALUES ($1, $2, $3)
				ON CONFLICT (connection_id) DO NOTHING`, d.id, d.hospital, s.now())
			c.nextDue = s.now()
			return err
		case err != nil:
			return err
		}
		c.hasState = true
		return nil
	})
	return c, err
}

// runOne runs d under its lock; ran is false when it was skipped.
func (s *Scheduler) runOne(ctx context.Context, d due) (bool, error) {
	unlock, ok, err := lock(ctx, s.Pool, d.id)
	if err != nil {
		return false, err
	}
	if !ok {
		s.metrics().Skipped.Add(1)
		return false, nil
	}
	defer unlock()
	// Read again under the lock: another copy may have just run it.
	c, err := s.load(ctx, d)
	if errors.Is(err, errGone) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := s.now()
	if !c.enabled || c.paused || !c.active || c.signIn >= StopAfterSignIn || c.nextDue.After(now) {
		return false, nil
	}
	impl, built := s.Connectors.Get(c.cfg.Type)
	ti, known := emrtypes.Lookup(c.cfg.Type)
	if !built || !known {
		return false, nil
	}
	runID, err := s.startRun(ctx, c)
	if err != nil {
		return false, err
	}
	res := s.fetch(ctx, c, impl, ti, now)
	s.metrics().Runs.Add(1)
	if res.code != "ok" {
		s.metrics().Failed.Add(1)
	}
	if err := s.finish(ctx, c, runID, res, now); err != nil {
		return true, err
	}
	s.log().Info("emr sync", "connection", c.cfg.ConnectionID.String(), "outcome", res.code, "records", res.records,
		"backfill", c.live == nil, "reconcile", res.reconciled)
	return true, nil
}

func (s *Scheduler) startRun(ctx context.Context, c conn) (uuid.UUID, error) {
	var id uuid.UUID
	err := dbx.WithHospital(ctx, s.Pool, dbx.EMR, c.cfg.HospitalID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO emr.sync_runs (hospital_id, connection_id, started_at) VALUES ($1, $2, $3) RETURNING id`,
			c.cfg.HospitalID, c.cfg.ConnectionID, s.now()).Scan(&id)
	})
	return id, err
}

type result struct {
	code       string
	retry      time.Duration
	cursor     []byte
	records    int
	caughtUp   bool
	reconciled bool
}

// reconcileDue reports whether this run is the day's reconciliation: the
// first sync is over, it is 02:00 or later in the hospital's time zone,
// and today (there) hasn't been reconciled. An unknown zone never
// reconciles (no inferred time zone).
func reconcileDue(c conn, now time.Time) (bool, time.Time) {
	if c.live == nil {
		return false, time.Time{}
	}
	loc, err := time.LoadLocation(c.tz)
	if err != nil {
		return false, time.Time{}
	}
	local := now.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	if local.Hour() < ReconcileHour {
		return false, day
	}
	if c.reconciled != nil && !c.reconciled.UTC().Before(day) {
		return false, day
	}
	return true, day
}

// counting is the RawSink a connector writes to: it counts new records, and
// in a reconciliation those already old (updated before the last two
// polls) as reconciled late.
type counting struct {
	sink      Sink
	stored    int
	late      int
	reconcile bool
	before    time.Time
}

func (c *counting) Put(ctx context.Context, r connector.RawRecord) error {
	stored, err := c.sink.PutStored(ctx, r)
	if err != nil {
		return err
	}
	if stored {
		c.stored++
		if c.reconcile && !r.SourceUpdatedAt.IsZero() && r.SourceUpdatedAt.Before(c.before) {
			c.late++
		}
	}
	return nil
}

func (s *Scheduler) fetch(ctx context.Context, c conn, impl connector.Connector, ti emrtypes.TypeInfo, now time.Time) result {
	if impl.Validate(c.cfg) != nil {
		return result{code: "unreadable_data"}
	}
	// Credentials go only to the origin they were entered for (design
	// §6.3): the saved addresses must still give it.
	origin, err := emrurl.OriginsOf(c.cfg.Type, c.cfg.BaseURL, c.cfg.Settings, s.Env == "development")
	if err != nil || !slices.Equal(origin, c.stored) {
		return result{code: "sign_in_refused"}
	}
	c.cfg.Origin = origin
	cr, ok, err := creds.Load(ctx, s.Pool, s.Store, s.Env, ti, c.cfg)
	defer cr.Zero()
	switch {
	case err != nil:
		return result{code: "unreachable"} // the store or our database: never sign_in_refused
	case !ok:
		return result{code: "sign_in_refused"}
	}
	cur := connector.Cursor(c.cursor)
	reconcile, _ := reconcileDue(c, now)
	if r, isR := impl.(connector.Reconciler); reconcile && isR {
		cur = r.Rewind(cur, now.Add(-ReconcileWindow))
	} else {
		reconcile = false
	}
	poll := time.Duration(c.cfg.PollSeconds) * time.Second
	sk := &counting{sink: s.NewSink(c.cfg, c.live == nil), reconcile: reconcile, before: now.Add(-2 * poll)}
	fctx, cancel := context.WithTimeout(ctx, RunTimeout)
	defer cancel()
	next, ferr := impl.Fetch(fctx, c.cfg, cr, cur, sk)
	// What was stored is announced whatever the outcome; a refused
	// announcement is the normaliser's sweep's to repeat.
	if err := sk.sink.Flush(context.WithoutCancel(ctx)); err != nil {
		s.log().Warn("emr sync: records not announced", "connection", c.cfg.ConnectionID.String(), "code", "publish_failed")
	}
	res := result{code: connector.CodeOf(ferr), cursor: next, records: sk.stored, reconciled: reconcile}
	var fe *connector.FetchError
	if errors.As(ferr, &fe) {
		res.retry = fe.RetryAfter
	}
	if cu, ok := impl.(connector.CatchUp); ok && ferr == nil {
		res.caughtUp = cu.CaughtUp(next)
	}
	if reconcile && sk.late > 0 {
		s.metrics().ReconciledLate.Add(int64(sk.late))
	}
	return res
}

// finish records the run's end and the connection's state, in one
// transaction.
func (s *Scheduler) finish(ctx context.Context, c conn, runID uuid.UUID, res result, started time.Time) error {
	now := s.now()
	poll := time.Duration(c.cfg.PollSeconds) * time.Second
	failures, signIn := c.failures, c.signIn
	if res.code == "ok" {
		failures, signIn = 0, 0
	} else {
		failures++
		if res.code == "sign_in_refused" {
			signIn++
		}
	}
	next := NextDue(now, poll, res.code, failures, res.retry, s.jitter())
	live := c.live
	if live == nil && res.code == "ok" && res.caughtUp {
		live = &now
	}
	var reconciledOn *time.Time
	if res.reconciled && res.code == "ok" {
		if _, day := reconcileDue(c, started); !day.IsZero() {
			reconciledOn = &day
		}
	}
	// The run's end is recorded even when the service is stopping.
	ctx = context.WithoutCancel(ctx)
	return dbx.WithHospital(ctx, s.Pool, dbx.EMR, c.cfg.HospitalID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE emr.sync_runs SET finished_at = $2, ok = $3, records = $4, outcome = $5
			WHERE id = $1`, runID, now, res.code == "ok", res.records, res.code); err != nil {
			return err
		}
		if res.code != "ok" {
			if _, err := tx.Exec(ctx, `INSERT INTO emr.connection_errors (hospital_id, connection_id, at, code) VALUES ($1, $2, $3, $4)`,
				c.cfg.HospitalID, c.cfg.ConnectionID, now, res.code); err != nil {
				return err
			}
		}
		var cursor any
		if len(res.cursor) > 0 {
			cursor = string(res.cursor)
		}
		_, err := tx.Exec(ctx, `UPDATE emr.sync_state SET cursor = COALESCE($2::jsonb, cursor), live_since = $3, next_due_at = $4,
			failures = $5, sign_in_failures = $6, reconciled_on = COALESCE($7::date, reconciled_on), updated_at = $8
			WHERE connection_id = $1`, c.cfg.ConnectionID, cursor, live, next, failures, signIn, reconciledOn, now)
		return err
	})
}

// Resume clears a connection's failure counts and makes it due at now: a
// passing test after sign-in refusals (design §15.2).
func Resume(ctx context.Context, pool *pgxpool.Pool, hospital, id uuid.UUID, now time.Time) error {
	return dbx.WithHospital(ctx, pool, dbx.EMR, hospital, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE emr.sync_state SET failures = 0, sign_in_failures = 0, next_due_at = $3, updated_at = $3
			WHERE connection_id = $1 AND hospital_id = $2`, id, hospital, now)
		return err
	})
}
