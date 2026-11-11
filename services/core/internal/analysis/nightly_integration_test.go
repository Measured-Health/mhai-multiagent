//go:build integration

package analysis

import (
	"context"
	"sync"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// The nightly run on a migrated database, as the core login (a plain
// member of mhai_app reaching mhai_analysis_job by SET ROLE). Every
// hospital, reference and fact is synthetic; the clock is pinned.

func migrated(t *testing.T) (super, app *pgxpool.Pool) {
	t.Helper()
	pg, super := testutil.MigratedPostgres(t)
	app, err := testutil.AppLogin(context.Background(), pg, super)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return super, app
}

func hospital(t *testing.T, super *pgxpool.Pool, zone string) uuid.UUID {
	t.Helper()
	h := uuid.New()
	if _, err := super.Exec(context.Background(), `INSERT INTO hospitals (id, name, short_code, status, time_zone)
		VALUES ($1, 'Synthetic Hospital', $2, 'active', $3)`, h, "N"+h.String()[:8], zone); err != nil {
		t.Fatal(err)
	}
	return h
}

func switchOn(t *testing.T, super *pgxpool.Pool, h *uuid.UUID, ref string) {
	t.Helper()
	if _, err := super.Exec(context.Background(), `INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		VALUES ($1, 'on', $2, $2, $2)`, h, ref); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, super *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := super.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// syntheticSource stands in for consultations that carry a patient
// reference (PR 10): one row per hospital asked, and a record of who was.
type syntheticSource struct {
	mu    sync.Mutex
	asked []uuid.UUID
}

func (s *syntheticSource) rows(_ context.Context, _ pgx.Tx, h uuid.UUID, _ *time.Time, until time.Time) ([]SourceRow, error) {
	s.mu.Lock()
	s.asked = append(s.asked, h)
	s.mu.Unlock()
	return []SourceRow{{Ref: "emr-index-" + h.String()[:8], Day: until.AddDate(0, 0, -1), Kind: "consultation", Sex: "female"}}, nil
}

// T25 (Go half): two hospitals, one off; after a run the off one has no
// facts and no periods, and its source was never read.
func TestNightlyRunsOnlySwitchedOnHospitals(t *testing.T) {
	super, app := migrated(t)
	on, off := hospital(t, super, "Africa/Lagos"), hospital(t, super, "Africa/Lagos")
	switchOn(t, super, nil, "SO-2026-0001")
	switchOn(t, super, &on, "SO-2026-0002")
	now := time.Date(2026, 2, 10, 1, 0, 0, 0, time.UTC) // 02:00 in Lagos
	src := &syntheticSource{}
	n := NewNightly(app, testKeys(t), nil)
	n.Clock = fixedClock(now)
	n.Store.(*DBStore).Source = src.rows
	ran, err := n.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || ran[0] != on || len(src.asked) != 1 || src.asked[0] != on {
		t.Fatalf("ran %v, source asked for %v", ran, src.asked)
	}
	if c := count(t, super, `SELECT count(*) FROM analysis_facts WHERE hospital_id = $1`, on); c != 1 {
		t.Errorf("the on hospital has %d facts", c)
	}
	if c := count(t, super, `SELECT count(*) FROM analysis_subjects WHERE hospital_id = $1 AND ref_enc IS NOT NULL`, on); c != 1 {
		t.Errorf("the on hospital has %d sealed subjects", c)
	}
	if c := count(t, super, `SELECT count(*) FROM analysis_periods WHERE hospital_id = $1 AND level = 'month' AND start = '2026-01-01'`, on); c != 1 {
		t.Errorf("January not frozen for the on hospital")
	}
	if c := count(t, super, `SELECT (SELECT count(*) FROM analysis_facts WHERE hospital_id = $1)
		+ (SELECT count(*) FROM analysis_periods WHERE hospital_id = $1)
		+ (SELECT count(*) FROM analysis_subjects WHERE hospital_id = $1)`, off); c != 0 {
		t.Errorf("the off hospital has %d rows", c)
	}
	// Run again the same hospital day: nothing is due.
	if ran, err := n.Tick(context.Background()); err != nil || len(ran) != 0 {
		t.Errorf("second run the same day: %v (%v)", ran, err)
	}
}

// Two core copies wake together on one database: the hospital runs once.
func TestNightlyOneCopyOnTheDatabase(t *testing.T) {
	super, app := migrated(t)
	h := hospital(t, super, "Africa/Lagos")
	switchOn(t, super, nil, "SO-2026-0001")
	switchOn(t, super, &h, "SO-2026-0002")
	now := time.Date(2026, 2, 10, 1, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	results := make([][]uuid.UUID, 4)
	for i := range results {
		n := NewNightly(app, testKeys(t), nil)
		n.Clock = fixedClock(now)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = n.Tick(context.Background())
		}(i)
	}
	wg.Wait()
	total := 0
	for _, r := range results {
		total += len(r)
	}
	if total != 1 {
		t.Fatalf("the hospital ran %d times", total)
	}
}

// The production clock is the database's.
func TestDBClock(t *testing.T) {
	_, app := migrated(t)
	now, err := DBClock{Pool: app}.Now(context.Background())
	if err != nil || time.Since(now).Abs() > time.Minute {
		t.Fatalf("db clock %v (%v)", now, err)
	}
}
