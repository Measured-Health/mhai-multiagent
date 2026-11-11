//go:build integration

package scheduler_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/scheduler"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Plan 7c Task 8 against Postgres: What changed through scheduler.DB as
// mhai_analytics_job, in a database of its own (an evaluation closes every
// change it doesn't flag, so it can't share one with other tests). The
// stored bodies are written by hand for fixed past periods (January and
// February 2025); every name and count is synthetic.

type changeRec struct {
	id                   uuid.UUID
	hospital             *uuid.UUID
	measure, direction   string
	current, previous    string
	summary              string
	opened, last, closed *time.Time
}

func readChanges(t *testing.T, pool *pgxpool.Pool) []changeRec {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id, hospital_id, measure, direction, current::text, previous::text, summary,
		opened_week, last_week, closed_week FROM analytics_changes ORDER BY opened_week, hospital_id NULLS FIRST, measure`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []changeRec
	for rows.Next() {
		var c changeRec
		if err := rows.Scan(&c.id, &c.hospital, &c.measure, &c.direction, &c.current, &c.previous, &c.summary, &c.opened, &c.last, &c.closed); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func TestEvaluateChangesDB(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
	sup, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	login, err := testutil.AppLogin(ctx, pg, sup)
	if err != nil {
		t.Fatal(err)
	}
	defer login.Close()
	hid := uuid.New()
	if _, err := sup.Exec(ctx, `INSERT INTO hospitals (id, name, short_code, status, time_zone, created_at)
		VALUES ($1, 'Synthetic Changes Hospital', $2, 'active', 'UTC', '2024-06-01')`, hid, "AN"+strings.ReplaceAll(hid.String(), "-", "")[:8]); err != nil {
		t.Fatal(err)
	}
	store := func(p calendar.Period, q int64) {
		t.Helper()
		from, to := analytics.Dates(p)
		h, hs := smallgroup.Guarded{Suppressed: true}, smallgroup.GuardedShare{Suppressed: true}
		row := analytics.Row{Questions: smallgroup.Count(q), Answers: h, Pass: h, Regenerate: h, Escalate: h, Degraded: h,
			PassShare: hs, RegenerateShare: hs, EscalateShare: hs, Reviews: h, AgreementRate: hs, ActiveClinicians: h,
			QuestionsPerClinician: analytics.Ratio{Number: analytics.Number{Suppressed: true}},
			MedianReviewHours:     analytics.Hours{Number: analytics.Number{Suppressed: true}}}
		b := analytics.Body{Kind: string(p.Kind), From: from, To: to, All: row, Hospitals: []analytics.HospitalRow{
			{Hospital: analytics.Ref{ID: hid.String(), Name: "Synthetic Changes Hospital"}, Staff: analytics.StaffFigure(9), Row: row}}}
		raw, _ := json.Marshal(b)
		if _, err := sup.Exec(ctx, `INSERT INTO analytics_snapshots (kind, period_from, period_to, state, body) VALUES ($1, $2::date, $3::date, 'ok', $4::jsonb)`,
			string(p.Kind), from, to, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	jan := calendar.Of(calendar.Month, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	store(jan, 62) // 14.0 a 7-day week
	weeks := calendar.Children(calendar.Of(calendar.Month, time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)))
	for i, q := range []int64{40, 41, 15, 40} {
		store(weeks[i], q)
	}
	// The release before these periods: they are evaluated.
	if _, err := sup.Exec(ctx, `UPDATE analytics_epoch SET released_at = '2024-12-01'`); err != nil {
		t.Fatal(err)
	}
	src := scheduler.DB{Pool: login}
	b := scheduler.Builder{Src: src}
	due, err := src.ChangesDue(ctx)
	if err != nil || len(due) != 4 || !due[0].Equal(weeks[0]) || !due[3].Equal(weeks[3]) {
		t.Fatalf("due %v %v", due, err)
	}
	// Week 1: 40 against 14.0, for all and for the hospital.
	if err := b.EvaluateChanges(ctx, weeks[0]); err != nil {
		t.Fatal(err)
	}
	cs := readChanges(t, sup)
	if len(cs) != 2 || cs[0].hospital != nil || cs[1].hospital == nil || *cs[1].hospital != hid {
		t.Fatalf("after week 1: %+v", cs)
	}
	for _, c := range cs {
		if c.measure != "questions" || c.direction != "rising" || c.current != `{"value": 40, "suppressed": false}` ||
			c.previous != `{"value": 14.0, "suppressed": false}` || c.summary != "Questions up: 14.0 to 40 a week" || c.closed != nil {
			t.Errorf("week 1: %+v", c)
		}
	}
	first := cs[0].id
	// Week 2: flagged again, the same ids, values and last week moved on.
	if err := b.EvaluateChanges(ctx, weeks[1]); err != nil {
		t.Fatal(err)
	}
	cs = readChanges(t, sup)
	if len(cs) != 2 || cs[0].id != first || cs[0].current != `{"value": 41, "suppressed": false}` || !cs[0].last.Equal(weeks[1].From) {
		t.Fatalf("after week 2: %+v", cs)
	}
	// Twice is once.
	if err := b.EvaluateChanges(ctx, weeks[1]); err != nil || len(readChanges(t, sup)) != 2 {
		t.Errorf("a second evaluation: %v", err)
	}
	// Week 3: 15 against 14.0 is no change: both close.
	if err := b.EvaluateChanges(ctx, weeks[2]); err != nil {
		t.Fatal(err)
	}
	for _, c := range readChanges(t, sup) {
		if c.closed == nil || !c.closed.Equal(weeks[2].From) {
			t.Errorf("after week 3, open: %+v", c)
		}
	}
	// An older week now is refused; the next due is week 4 only.
	if err := b.EvaluateChanges(ctx, weeks[1]); !errors.Is(err, scheduler.ErrOutOfOrder) {
		t.Errorf("an older week: %v", err)
	}
	if due, err := src.ChangesDue(ctx); err != nil || len(due) != 1 || !due[0].Equal(weeks[3]) {
		t.Errorf("due after week 3: %v %v", due, err)
	}
	// Week 4: back, with new ids.
	if err := b.EvaluateChanges(ctx, weeks[3]); err != nil {
		t.Fatal(err)
	}
	cs = readChanges(t, sup)
	if len(cs) != 4 || cs[2].closed != nil || cs[2].id == first {
		t.Errorf("after week 4: %+v", cs)
	}
	var runs int
	if err := sup.QueryRow(ctx, `SELECT count(*) FROM analytics_change_runs WHERE month_from = '2025-01-01'`).Scan(&runs); err != nil || runs != 4 {
		t.Errorf("%d runs: %v", runs, err)
	}
}

// A first run (no run stored) evaluates only weeks that start at or after
// the release epoch (spec §18, 7c-3 item 12): a stored week from before it,
// such as the smoke test's synthetic 2020 snapshots, is never due.
func TestChangesDueFromEpoch(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
	sup, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	login, err := testutil.AppLogin(ctx, pg, sup)
	if err != nil {
		t.Fatal(err)
	}
	defer login.Close()
	if _, err := sup.Exec(ctx, `UPDATE analytics_epoch SET released_at = '2025-02-10T12:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	for _, w := range calendar.Children(calendar.Of(calendar.Month, time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC))) {
		from, to := analytics.Dates(w)
		if _, err := sup.Exec(ctx, `INSERT INTO analytics_snapshots (kind, period_from, period_to, state) VALUES ('week', $1::date, $2::date, 'unavailable')`,
			from, to); err != nil {
			t.Fatal(err)
		}
	}
	due, err := scheduler.DB{Pool: login}.ChangesDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 1–7 and 8–14 February started before the epoch (10 Feb, 12:00).
	if len(due) != 2 || due[0].From.Day() != 15 || due[1].From.Day() != 22 {
		t.Errorf("due %v, want the weeks of 15 and 22 February only", due)
	}
}
