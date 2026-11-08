//go:build integration

package scheduler_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/scheduler"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup/history_test"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Task 4 against Postgres: the scheduler as the core login (SET ROLE
// mhai_analytics_job), real periods (the freeze reads the last 118 days
// only). Every row is synthetic. Tests share one database and take periods
// of their own.

var super, app *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()
	code := func() int {
		pg, stop, err := testutil.NewPostgres(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "start postgres:", err)
			return 1
		}
		defer stop()
		if err := db.MigrateUp(ctx, pg.URL); err != nil {
			fmt.Fprintln(os.Stderr, "migrate:", err)
			return 1
		}
		if super, err = db.Open(ctx, pg.URL); err != nil {
			fmt.Fprintln(os.Stderr, "open:", err)
			return 1
		}
		defer super.Close()
		if app, err = testutil.AppLogin(ctx, pg, super); err != nil {
			fmt.Fprintln(os.Stderr, "app login:", err)
			return 1
		}
		defer app.Close()
		return m.Run()
	}()
	os.Exit(code)
}

func exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := super.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func seedHospital(t *testing.T, created time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	code := "AN" + strings.ReplaceAll(id.String(), "-", "")[:8]
	exec(t, `INSERT INTO hospitals (id, name, short_code, status, time_zone, created_at) VALUES ($1, $2, $3, 'active', 'UTC', $4)`,
		id, "Synthetic Hospital "+code, code, created)
	return id
}

func seedClinician(t *testing.T, h uuid.UUID) uuid.UUID {
	t.Helper()
	u := uuid.New()
	exec(t, `INSERT INTO users (id, hospital_id, email, display_name) VALUES ($1, $2, $3, 'Synthetic Clinician')`, u, h, u.String()+"@example.test")
	exec(t, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'clinician')`, u)
	return u
}

// seedQuestions adds n questions of h at at, each with decision (or none).
func seedQuestions(t *testing.T, h, by uuid.UUID, at time.Time, decision string, degraded bool, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		q := uuid.New()
		exec(t, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, created_by, created_at)
			VALUES ($1, $2, 'synthetic question', 'routine', $3, $4)`, q, h, by, at)
		if decision != "" {
			exec(t, `INSERT INTO clinical_decisions (id, hospital_id, query_id, hga_decision, degraded, response_text, created_at)
				VALUES (gen_random_uuid(), $1, $2, $3, $4, 'synthetic answer', $5)`, h, q, decision, degraded, at.Add(time.Minute))
		}
	}
}

func setEpoch(t *testing.T, at time.Time) { exec(t, `UPDATE analytics_epoch SET released_at = $1`, at) }

func storedBody(t *testing.T, p calendar.Period) (string, []byte) {
	t.Helper()
	var state string
	var body *string
	if err := db.WithPlatform(context.Background(), app, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT state, body::text FROM analytics_snapshots WHERE kind = $1 AND period_from = $2::date`,
			string(p.Kind), p.From.Format(time.DateOnly)).Scan(&state, &body)
	}); err != nil {
		t.Fatalf("%v: %v", p, err)
	}
	if body == nil {
		return state, nil
	}
	return state, []byte(*body)
}

func rowOf(t *testing.T, raw []byte, p calendar.Period, h uuid.UUID) analytics.HospitalRow {
	t.Helper()
	b, err := analytics.DecodeBody(raw, p)
	if err != nil {
		t.Fatalf("%v: %v", p, err)
	}
	for _, r := range b.Hospitals {
		if r.Hospital.ID == h.String() {
			return r
		}
	}
	t.Fatalf("%v: hospital missing", p)
	return analytics.HospitalRow{}
}

// A2, A27: a decision committed after its week froze changes no served
// figure: not the week, not its month built later.
func TestLateCommitDoesNotMoveStoredFigures(t *testing.T) {
	ctx := context.Background()
	m := calendar.Prev(calendar.Latest(calendar.Month, time.Now())) // two months back: its own weeks only here
	setEpoch(t, m.From)
	h := seedHospital(t, m.From.AddDate(0, -1, 0))
	u := seedClinician(t, h)
	weeks := calendar.Children(m)
	for _, w := range weeks {
		seedQuestions(t, h, u, w.From.Add(2*time.Hour), "PASS", false, 9)
	}
	b := scheduler.Builder{Src: scheduler.DB{Pool: app}}
	for _, w := range weeks {
		if err := b.Build(ctx, w); err != nil {
			t.Fatalf("%v: %v", w, err)
		}
	}
	_, before := storedBody(t, weeks[1])
	// Late: dated inside week 2, committed after it froze.
	seedQuestions(t, h, u, weeks[1].From.Add(3*time.Hour), "ESCALATE", true, 7)
	if err := b.Build(ctx, weeks[1]); err != nil {
		t.Fatal(err)
	}
	if err := b.Build(ctx, m); err != nil {
		t.Fatal(err)
	}
	state, after := storedBody(t, weeks[1])
	if state != scheduler.StateOK || string(after) != string(before) {
		t.Errorf("week 2 moved:\n%s\n%s", before, after)
	}
	_, mb := storedBody(t, m)
	r := rowOf(t, mb, m, h)
	if !r.Questions.Shown() || *r.Questions.Value != 36 || !r.Escalate.Shown() || *r.Escalate.Value != 0 {
		t.Errorf("month questions %+v escalate %+v, want 36 and 0 (the late rows in no period)", r.Questions, r.Escalate)
	}
}

// latest is the latest finished month, seeded once with random synthetic
// counts for hospitals onboarded inside it (A7 end to end), shared by
// TestSchedulerOneCopy and TestHistoryEndToEnd.
var latest struct {
	once  sync.Once
	m     calendar.Period
	hosps []uuid.UUID
	truth map[string]map[string][7]int64 // week from → hospital → QG counts
	onb   map[string]time.Time
}

func seedLatest(t *testing.T) {
	latest.once.Do(func() {
		m := calendar.Latest(calendar.Month, time.Now())
		latest.m, latest.truth, latest.onb = m, map[string]map[string][7]int64{}, map[string]time.Time{}
		weeks := calendar.Children(m)
		r := rand.New(rand.NewSource(3))
		for i := 0; i < 4; i++ {
			created := weeks[i%4].From.Add(time.Duration(r.Intn(48)) * time.Hour)
			h := seedHospital(t, created)
			latest.hosps = append(latest.hosps, h)
			latest.onb[h.String()] = created
			u := seedClinician(t, h)
			for _, w := range weeks {
				at := w.From.Add(72 * time.Hour)
				if at.Before(created) {
					at = created.Add(time.Minute)
				}
				if !created.Before(w.To) || !at.Before(w.To) {
					continue
				}
				var q [7]int64
				for c, dec := range []string{"PASS", "REGENERATE", "ESCALATE"} {
					n := int64(r.Intn(9))
					seedQuestions(t, h, u, at, dec, false, int(n))
					q[2*c] = n
				}
				if r.Intn(2) == 0 {
					seedQuestions(t, h, u, at, "", false, 2)
					q[6] = 2
				}
				if latest.truth[w.From.String()] == nil {
					latest.truth[w.From.String()] = map[string][7]int64{}
				}
				latest.truth[w.From.String()][h.String()] = q
			}
		}
	})
}

// A25: four schedulers waking together store one snapshot per period, and
// every read serves the same bytes.
func TestSchedulerOneCopy(t *testing.T) {
	ctx := context.Background()
	seedLatest(t)
	m := latest.m
	setEpoch(t, m.From)
	s := scheduler.Scheduler{Pool: app, Builder: scheduler.Builder{Src: scheduler.DB{Pool: app}}}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Tick(ctx)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Tick(ctx); err != nil { // whatever a copy that lost the lock left
		t.Fatal(err)
	}
	var n, frozen int
	if err := super.QueryRow(ctx, `SELECT count(*) FROM analytics_snapshots WHERE period_from >= $1::date AND period_from < $2::date`,
		m.From.Format(time.DateOnly), m.To.Format(time.DateOnly)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := super.QueryRow(ctx, `SELECT count(*) FROM analytics_frozen_periods WHERE period_from >= $1::date AND period_from < $2::date`,
		m.From.Format(time.DateOnly), m.To.Format(time.DateOnly)).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if n != 5 || frozen != 5 {
		t.Errorf("%d snapshots and %d frozen periods for the month and its weeks, want 5 each", n, frozen)
	}
	state, a := storedBody(t, m)
	_, b := storedBody(t, m)
	if state != scheduler.StateOK || string(a) != string(b) {
		t.Errorf("two reads differ or %s", state)
	}
}

// A7 against the database, small: the latest month's hospitals, onboarded
// inside it, through the real scheduler; the A7 parser and solver over the
// stored bodies expose nothing of 1 to 4.
func TestHistoryEndToEnd(t *testing.T) {
	ctx := context.Background()
	seedLatest(t)
	m := latest.m
	setEpoch(t, m.From)
	s := scheduler.Scheduler{Pool: app, Builder: scheduler.Builder{Src: scheduler.DB{Pool: app}}}
	if _, err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	weeks := calendar.Children(m)
	// Every hospital of the database is listed (active ones with zeros):
	// the attacker knows each one's onboarding.
	onboard := map[string]time.Time{}
	rows, err := super.Query(ctx, `SELECT id::text, created_at FROM hospitals`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			t.Fatal(err)
		}
		onboard[id] = at
	}
	rows.Close()
	p := historytest.New(weeks, onboard)
	for _, per := range append(append([]calendar.Period{}, weeks...), m) {
		state, raw := storedBody(t, per)
		if state != scheduler.StateOK {
			t.Fatalf("%v stored %s", per, state)
		}
		if err := p.Add("stored", raw, per); err != nil {
			t.Fatalf("%v: %v", per, err)
		}
	}
	rep := p.Check(func(vars []int) int64 {
		var n int64
		for _, v := range vars {
			f, h, w, c := p.Decode(v)
			if f != 0 {
				continue
			}
			for _, hid := range latest.hosps {
				if hi, _ := p.HospitalIndex(hid.String()); hi == h {
					n += latest.truth[weeks[w].From.String()][hid.String()][c]
				}
			}
		}
		return n
	})
	if len(rep.SmallCells)+len(rep.SmallSums) > 0 {
		t.Errorf("exposed: %v %v", rep.SmallCells, rep.SmallSums)
	}
	if rep.Hidden == 0 {
		t.Error("nothing hidden: the case tests nothing")
	}
	t.Logf("%d cells parsed, %d hidden", rep.Cells, rep.Hidden)
}
