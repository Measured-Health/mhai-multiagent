//go:build integration

package analysis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/cells"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
)

// Migration 00025 and the engine on a migrated database, as the core
// login (mhai_gateway through authz.Tx). Every hospital, subject and fact
// is synthetic, and every date pinned: the facts are in October 2026, the
// freeze runs at 6 November 2026 and the engine's clock is 12 November
// 2026, all in the past for the database, so no test depends on today.

var (
	freezeAt = time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)
	clockAt  = time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC)
	octWeek1 = calendar.Period{Kind: calendar.Week, From: day("2026-10-01"), To: day("2026-10-08")}
	october  = calendar.Period{Kind: calendar.Month, From: day("2026-10-01"), To: day("2026-11-01")}
	novWeek1 = calendar.Period{Kind: calendar.Week, From: day("2026-11-01"), To: day("2026-11-08")}
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

// world is a demo hospital switched on, with its dictionaries.
type world struct {
	super, app *pgxpool.Pool
	h          uuid.UUID
	conds      []uuid.UUID
	loc, src   uuid.UUID
	src2       uuid.UUID
}

func newWorld(t *testing.T, super, app *pgxpool.Pool) *world {
	t.Helper()
	ctx := context.Background()
	w := &world{super: super, app: app, h: uuid.New(), loc: uuid.New(), src: uuid.New(), src2: uuid.New()}
	if _, err := super.Exec(ctx, `INSERT INTO hospitals (id, name, short_code, status, time_zone, is_demo)
		VALUES ($1, 'Synthetic Demo Hospital', $2, 'active', 'Africa/Lagos', true)`, w.h, "Y"+w.h.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if count(t, super, `SELECT count(*) FROM analysis_switch WHERE hospital_id IS NULL`) == 0 {
		switchOn(t, super, nil, "DEMO-SYNTHETIC")
	}
	switchOn(t, super, &w.h, "DEMO-SYNTHETIC")
	exec(t, super, `INSERT INTO analysis_locations (hospital_id, id, label, setting) VALUES ($1, $2, 'Synthetic Ward', 'admitted')`, w.h, w.loc)
	exec(t, super, `INSERT INTO analysis_sources (hospital_id, id, label) VALUES ($1, $2, 'Synthetic EMR'), ($1, $3, 'Synthetic Lab')`, w.h, w.src, w.src2)
	for i, code := range []string{"I10", "E11", "J45"} {
		id := uuid.New()
		w.conds = append(w.conds, id)
		exec(t, super, `INSERT INTO analysis_conditions (hospital_id, id, icd10, name) VALUES ($1, $2, $3, $4)`,
			w.h, id, code, fmt.Sprintf("Synthetic condition %d", i))
	}
	return w
}

func exec(t *testing.T, super *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	if _, err := super.Exec(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

// subject adds one synthetic subject with a diagnosis of cond on each day.
func (w *world) subject(t *testing.T, sex string, src uuid.UUID, cond uuid.UUID, days ...string) uuid.UUID {
	t.Helper()
	s := uuid.New()
	ref := bytes.Repeat([]byte{0}, 32)
	copy(ref, s[:])
	exec(t, w.super, `INSERT INTO analysis_subjects (hospital_id, subject_id, ref_hmac, created_day) VALUES ($1, $2, $3, '2026-10-01')`, w.h, s, ref)
	for _, d := range days {
		exec(t, w.super, `INSERT INTO analysis_facts (hospital_id, subject_id, day, kind, condition_id, age_band, sex, setting, location_id, source_id, period_late)
			VALUES ($1, $2, $3::date, 'diagnosis', $4, '40-64', $5, 'admitted', $6, $7, false)`, w.h, s, d, cond, sex, w.loc, src)
		exec(t, w.super, `INSERT INTO analysis_first_seen (hospital_id, subject_id, condition_id, first_day, first_late)
			VALUES ($1, $2, $3, $4::date, false) ON CONFLICT (hospital_id, subject_id, condition_id)
			DO UPDATE SET first_day = LEAST(analysis_first_seen.first_day, EXCLUDED.first_day)`, w.h, s, cond, d)
	}
	return s
}

// cohort adds n subjects per sex with condition 0 in the first week, and
// a few with condition 1.
func (w *world) cohort(t *testing.T, perSex map[string]int) {
	t.Helper()
	for sex, n := range perSex {
		for i := 0; i < n; i++ {
			src := w.src
			if i%2 == 1 {
				src = w.src2
			}
			c := w.conds[0]
			if i%5 == 4 {
				c = w.conds[1]
			}
			w.subject(t, sex, src, c, "2026-10-02")
		}
	}
}

func (w *world) freeze(t *testing.T) {
	t.Helper()
	w.freezeAt(t, freezeAt)
}

func (w *world) freezeAt(t *testing.T, at time.Time) {
	t.Helper()
	ctx := context.Background()
	tx, err := w.app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{`SET LOCAL ROLE mhai_analysis_job`, `SELECT set_config('mhai.analysis.engine', '1', true)`} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `SELECT mhai_analysis_freeze($1, $2)`, w.h, at); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// as is a request context for an admin of hospital h, with two-step.
func as(h uuid.UUID) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uuid.New(), HospitalID: h,
		Roles: []string{authz.RoleHospitalAdmin}, MFA: true, SessionID: uuid.NewString()})
}

func (w *world) engine() *Engine {
	e := NewEngine(w.app, nil)
	e.Now = func() time.Time { return clockAt }
	e.Budget = time.Minute // the budget has its own test
	return e
}

func (w *world) period(t *testing.T, e *Engine, p calendar.Period) Period {
	t.Helper()
	per, err := e.Lookup(as(w.h), p)
	if err != nil {
		t.Fatalf("%v: %v", p, err)
	}
	return per
}

func readCells(ctx context.Context, tx pgx.Tx, p Period, f disclose.Filters) (disclose.Raw, error) {
	return cells.Read(ctx, tx, p.ID, p.Period, f)
}

func female() disclose.Filters { var f disclose.Filters; f[disclose.DimSex] = "female"; return f }

// The stored body is served unchanged (P3): a late fact, a new dictionary
// entry and a second engine (another core copy) change nothing.
func TestFreezeServesByteIdentical(t *testing.T) {
	super, app := migrated(t)
	w := newWorld(t, super, app)
	w.cohort(t, map[string]int{"female": 30, "male": 20, "other": 3})
	w.freeze(t)
	e := w.engine()
	p := w.period(t, e, october)
	first, err := e.Body(as(w.h), p, disclose.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Views) != len(disclose.Views) || !first.Record.Patients.Shown() || *first.Record.Patients.Value != 53 {
		t.Fatalf("october: %+v", first.Record.Patients)
	}
	// A late fact in October, a new condition: nothing moves.
	s := w.subject(t, "female", w.src, w.conds[2], "2026-10-03")
	exec(t, super, `UPDATE analysis_facts SET period_late = true WHERE subject_id = $1`, s)
	exec(t, super, `INSERT INTO analysis_conditions (hospital_id, id, icd10, name) VALUES ($1, $2, 'G40', 'Synthetic late entry')`, w.h, uuid.New())
	again, err := w.engine().Body(as(w.h), p, disclose.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range disclose.Views {
		if !bytes.Equal(first.Views[string(v)], again.Views[string(v)]) {
			t.Errorf("%s changed after the freeze", v)
		}
	}
	if !first.StoredAt.Equal(again.StoredAt) {
		t.Error("stored_at changed")
	}
	// A body first computed now uses the frozen inputs only: the late
	// subject is in no count.
	var raw disclose.Raw
	if err := gateway(t, app, w.h, func(tx pgx.Tx) error {
		var err error
		raw, err = readCells(context.Background(), tx, p, female())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if raw.Patients != 30 {
		t.Errorf("female october counts %d, want 30 (the late fact left out)", raw.Patients)
	}
	if _, err := e.Body(as(w.h), p, female()); err != nil {
		t.Errorf("a body first computed after the late fact: %v", err)
	}
}

// T21: 20 parallel cold requests on each of two engines (two core copies)
// compute each context once and store one row.
func TestColdBodyOnceUnderRace(t *testing.T) {
	super, app := migrated(t)
	w := newWorld(t, super, app)
	w.cohort(t, map[string]int{"female": 30, "male": 20})
	w.freeze(t)
	var computed atomic.Int64
	engines := []*Engine{w.engine(), w.engine()}
	for _, e := range engines {
		e.OnCompute = func() { computed.Add(1) }
	}
	p := w.period(t, engines[0], octWeek1)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(e *Engine) {
			defer wg.Done()
			if _, err := e.Body(as(w.h), p, female()); err != nil {
				errs <- err
			}
		}(engines[i%2])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// The context and its one filter parent, each once.
	if n := computed.Load(); n != 2 {
		t.Errorf("%d computations, want 2", n)
	}
	if n := count(t, super, `SELECT count(*) FROM analysis_snapshots WHERE hospital_id = $1 AND period_id = $2`, w.h, p.ID); n != 2 {
		t.Errorf("%d rows, want 2", n)
	}
}

// PR 3 review, condition 1: two contexts of the same period guarded at
// once, from two core copies (separate ledgers), run one at a time, and
// the second knows the first's body: the period's guard can't be bypassed
// by racing two siblings ({female} and {male} both against {}).
func TestPeriodGuardsSerialised(t *testing.T) {
	super, app := migrated(t)
	w := newWorld(t, super, app)
	w.cohort(t, map[string]int{"female": 30, "male": 20, "other": 12})
	w.freeze(t)
	e1, e2 := w.engine(), w.engine()
	p := w.period(t, e1, octWeek1)
	if _, err := e1.Body(as(w.h), p, disclose.Filters{}); err != nil {
		t.Fatal(err)
	}
	type span struct {
		from, to time.Time
		known    int
	}
	var mu sync.Mutex
	var spans []span
	hook := func(known int, start time.Time) {
		mu.Lock()
		spans = append(spans, span{start, time.Now(), known})
		mu.Unlock()
	}
	e1.onGuard, e2.onGuard = hook, hook
	e1.guardDelay, e2.guardDelay = 300*time.Millisecond, 300*time.Millisecond
	var male disclose.Filters
	male[disclose.DimSex] = "male"
	var wg sync.WaitGroup
	for _, x := range []struct {
		e *Engine
		f disclose.Filters
	}{{e1, female()}, {e2, male}} {
		wg.Add(1)
		go func(e *Engine, f disclose.Filters) {
			defer wg.Done()
			if _, err := e.Body(as(w.h), p, f); err != nil {
				t.Error(err)
			}
		}(x.e, x.f)
	}
	wg.Wait()
	if len(spans) != 2 {
		t.Fatalf("%d guards ran, want 2", len(spans))
	}
	a, b := spans[0], spans[1]
	if b.from.Before(a.from) {
		a, b = b, a
	}
	if b.from.Before(a.to) {
		t.Errorf("the two guards overlapped: %v–%v and %v–%v", a.from, a.to, b.from, b.to)
	}
	if b.known != a.known+1 {
		t.Errorf("the second guard knew %d bodies, the first %d: it didn't see the first's body", b.known, a.known)
	}
}

// T21: at the cap, a new context is ErrCap, and nothing is stored.
func TestOverCapIs503AndUnstored(t *testing.T) {
	super, app := migrated(t)
	w := newWorld(t, super, app)
	w.cohort(t, map[string]int{"female": 30, "male": 20})
	w.freeze(t)
	e := w.engine()
	p := w.period(t, e, octWeek1)
	exec(t, super, `INSERT INTO analysis_snapshots (hospital_id, period_id, query_hash, body)
		SELECT $1, $2, sha256(convert_to('filler' || g, 'UTF8')), '{}'::jsonb FROM generate_series(1, 20000) g`, w.h, p.ID)
	if _, err := e.Body(as(w.h), p, disclose.Filters{}); !errors.Is(err, ErrCap) {
		t.Fatalf("at the cap: %v, want ErrCap", err)
	}
	if n := count(t, super, `SELECT count(*) FROM analysis_snapshots WHERE period_id = $1`, p.ID); n != 20000 {
		t.Errorf("%d rows after a refusal", n)
	}
	if e.CapReached.Load() != 1 {
		t.Error("no cap alert")
	}
}

// gateway runs fn as the request's gateway transaction for hospital h.
func gateway(t *testing.T, app *pgxpool.Pool, h uuid.UUID, fn func(pgx.Tx) error) error {
	t.Helper()
	return authz.Tx(as(h), app, fn)
}

// N7: snapshot_put refuses a shown count of 1 to 4 anywhere, and an
// unfrozen or foreign period; shares and medians are not counts.
func TestSnapshotPutRefusesSmallShownAndUnfrozen(t *testing.T) {
	super, app := migrated(t)
	w := newWorld(t, super, app)
	w.cohort(t, map[string]int{"female": 10})
	w.freeze(t)
	p := w.period(t, w.engine(), octWeek1)
	put := func(h, period uuid.UUID, body string) error {
		return gateway(t, app, h, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `SELECT mhai_analysis_snapshot_put($1, sha256(convert_to($3, 'UTF8')), $2::jsonb)`, period, body, body)
			return err
		})
	}
	for _, bad := range []string{`{"a": {"value": 3, "suppressed": false}}`, `{"x": [{"y": {"value": 1, "suppressed": false}}]}`} {
		if err := put(w.h, p.ID, bad); err == nil {
			t.Errorf("stored %s", bad)
		}
	}
	for _, ok := range []string{`{"s": {"value": 1.0000, "suppressed": false}}`, `{"median_ack_minutes": {"value": 3, "suppressed": false}}`,
		`{"a": {"value": 5, "suppressed": false}, "b": {"value": 0, "suppressed": false}}`} {
		if err := put(w.h, p.ID, ok); err != nil {
			t.Errorf("refused %s: %v", ok, err)
		}
	}
	if err := put(w.h, uuid.New(), `{}`); err == nil {
		t.Error("stored a body for a period that isn't frozen")
	}
	other := newWorld(t, super, app)
	if err := put(other.h, p.ID, `{"z": 1}`); err == nil {
		t.Error("another hospital stored a body for this hospital's period")
	}
}

// T10 (SQL half): cells refuses an unfrozen period and another hospital's.
func TestCellsRefusesUnfrozenAndForeignPeriod(t *testing.T) {
	super, app := migrated(t)
	w := newWorld(t, super, app)
	w.cohort(t, map[string]int{"female": 10})
	w.freeze(t)
	p := w.period(t, w.engine(), octWeek1)
	cells := func(h, period uuid.UUID) error {
		return gateway(t, app, h, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `SELECT mhai_analysis_cells($1, '{}'::jsonb)`, period)
			return err
		})
	}
	if err := cells(w.h, p.ID); err != nil {
		t.Fatalf("own frozen period: %v", err)
	}
	if err := cells(w.h, uuid.New()); err == nil {
		t.Error("an unknown period answered")
	}
	other := newWorld(t, super, app)
	if err := cells(other.h, p.ID); err == nil {
		t.Error("another hospital read this hospital's period")
	}
	// A hospital whose switch is off reads nothing, even its own period.
	exec(t, super, `UPDATE analysis_switch SET position = 'off' WHERE hospital_id = $1`, w.h)
	if err := cells(w.h, p.ID); err == nil {
		t.Error("cells answered with the switch off")
	}
	// Too many filters, and an unknown one.
	exec(t, super, `UPDATE analysis_switch SET position = 'on' WHERE hospital_id = $1`, w.h)
	for _, f := range []string{`{"sex": "female", "age_band": "40-64", "setting": "admitted"}`, `{"name": "x"}`, `{"sex": "nobody"}`} {
		err := gateway(t, app, w.h, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `SELECT mhai_analysis_cells($1, $2::jsonb)`, p.ID, f)
			return err
		})
		if err == nil {
			t.Errorf("cells took %s", f)
		}
	}
}

// The SQL counts the engine's definitions: patients, cases and new (a
// case is new where its first day is), the partitions, the sources (given
// to the guard) and pairs.
func TestCellsCountTheDefinitions(t *testing.T) {
	super, app := migrated(t)
	w := newWorld(t, super, app)
	a := w.subject(t, "female", w.src, w.conds[0], "2026-10-02", "2026-10-09")
	exec(t, super, `INSERT INTO analysis_facts (hospital_id, subject_id, day, kind, condition_id, sex, period_late)
		VALUES ($1, $2, '2026-10-09', 'diagnosis', $3, 'male', false)`, w.h, a, w.conds[1]) // a's attributes stay its first fact's
	exec(t, super, `INSERT INTO analysis_first_seen (hospital_id, subject_id, condition_id, first_day, first_late) VALUES ($1, $2, $3, '2026-10-09', false)`, w.h, a, w.conds[1])
	w.subject(t, "male", w.src2, w.conds[0], "2026-10-09")
	w.freeze(t)
	e := w.engine()
	p := w.period(t, e, october)
	var raw disclose.Raw
	err := gateway(t, app, w.h, func(tx pgx.Tx) error {
		var err error
		raw, err = readCells(context.Background(), tx, p, disclose.Filters{})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if raw.Patients != 2 || raw.Total.Cases != 4 || raw.Total.New != 3 {
		t.Errorf("patients %d, cases %d, new %d; want 2, 4, 3", raw.Patients, raw.Total.Cases, raw.Total.New)
	}
	sexes := map[string]int64{}
	for _, s := range raw.BySex {
		sexes[s.Key] = s.Patients
	}
	if sexes["female"] != 1 || sexes["male"] != 1 {
		t.Errorf("by sex %v: a subject's sex is its first fact's", sexes)
	}
	if len(raw.Sources) != 2 {
		t.Errorf("sources %v, want both", raw.Sources)
	}
	if len(raw.Pairs) != 1 || raw.Pairs[0].Patients != 1 {
		t.Errorf("pairs %+v", raw.Pairs)
	}
	if len(raw.Series) != 4 || raw.Series[0].Cases != 1 || raw.Series[1].Cases != 3 {
		t.Errorf("series %+v", raw.Series)
	}
	// Under a condition filter, a block counts where the subject had it.
	var f disclose.Filters
	f[disclose.DimCondition] = w.conds[1].String()
	err = gateway(t, app, w.h, func(tx pgx.Tx) error {
		var err error
		raw, err = readCells(context.Background(), tx, p, f)
		return err
	})
	if err != nil || raw.Patients != 1 || raw.Total.Cases != 2 {
		t.Errorf("condition filter: patients %d, cases %d (%v); want 1, 2", raw.Patients, raw.Total.Cases, err)
	}
}

// P6: a condition with 4 patients isn't offered, one with 5 is, and the
// stored list for a frozen period never changes.
func TestFilterListPerFrozenPeriod(t *testing.T) {
	super, app := migrated(t)
	w := newWorld(t, super, app)
	for i := 0; i < 5; i++ {
		w.subject(t, "female", w.src, w.conds[0], "2026-10-02")
	}
	for i := 0; i < 4; i++ {
		w.subject(t, "female", w.src, w.conds[1], "2026-10-02")
	}
	w.freeze(t)
	e := w.engine()
	p := w.period(t, e, octWeek1)
	lists, at, err := e.Filters(as(w.h), p)
	if err != nil {
		t.Fatal(err)
	}
	var l struct {
		Conditions []struct{ ID string } `json:"conditions"`
	}
	if err := json.Unmarshal(lists, &l); err != nil {
		t.Fatal(err)
	}
	if len(l.Conditions) != 1 || l.Conditions[0].ID != w.conds[0].String() {
		t.Errorf("conditions offered %+v, want only the one with 5", l.Conditions)
	}
	exec(t, super, `INSERT INTO analysis_locations (hospital_id, label, setting) VALUES ($1, 'Synthetic New Ward', 'outpatient')`, w.h)
	again, at2, err := e.Filters(as(w.h), p)
	if err != nil || !bytes.Equal(lists, again) || !at.Equal(at2) {
		t.Errorf("the frozen list changed (%v)", err)
	}
}

// The budget: a body that can't be computed in time is ErrNotReady, and
// the computation goes on, so the next request finds it.
func TestBudgetExceededIsNotReady(t *testing.T) {
	super, app := migrated(t)
	w := newWorld(t, super, app)
	w.cohort(t, map[string]int{"female": 30, "male": 20})
	w.freeze(t)
	e := w.engine()
	p := w.period(t, e, october)
	e.Budget = time.Nanosecond
	if _, err := e.Body(as(w.h), p, female()); !errors.Is(err, ErrNotReady) {
		t.Fatalf("over the budget: %v, want ErrNotReady", err)
	}
	e.Budget = time.Minute
	if _, err := e.Body(as(w.h), p, female()); err != nil {
		t.Fatalf("the retry: %v", err)
	}
}

// Lookup: a frozen period, one ended but not frozen (503), and one that
// isn't answerable (422).
func TestLookupStates(t *testing.T) {
	super, app := migrated(t)
	w := newWorld(t, super, app)
	w.freeze(t)
	e := w.engine()
	if _, err := e.Lookup(as(w.h), octWeek1); err != nil {
		t.Errorf("october week: %v", err)
	}
	if _, err := e.Lookup(as(w.h), novWeek1); !errors.Is(err, ErrPending) {
		t.Errorf("november's first week: %v, want ErrPending", err)
	}
	old := calendar.Period{Kind: calendar.Month, From: day("2024-09-01"), To: day("2024-10-01")}
	if _, err := e.Lookup(as(w.h), old); !errors.Is(err, ErrNoPeriod) {
		t.Errorf("26 months back: %v, want ErrNoPeriod", err)
	}
	cur := calendar.Period{Kind: calendar.Month, From: day("2026-11-01"), To: day("2026-12-01")}
	if _, err := e.Lookup(as(w.h), cur); !errors.Is(err, ErrNoPeriod) {
		t.Errorf("the current month: %v, want ErrNoPeriod", err)
	}
}

// D7: past 100 distinct filter sets in a hospital day, one flag.
func TestUnusualQueryingOnce(t *testing.T) {
	super, app := migrated(t)
	w := newWorld(t, super, app)
	w.freeze(t)
	p := w.period(t, w.engine(), octWeek1)
	user := uuid.New()
	flags := 0
	for i := 0; i < 130; i++ {
		f := []byte(fmt.Sprintf("%032d", i))
		err := gateway(t, app, w.h, func(tx pgx.Tx) error {
			var first, unusual bool
			if err := tx.QueryRow(context.Background(), `SELECT first, unusual FROM mhai_analysis_viewed($1, 's1', $2, $3, $3, $4)`,
				user, p.ID, f, clockAt).Scan(&first, &unusual); err != nil {
				return err
			}
			if !first {
				return fmt.Errorf("read %d not first", i)
			}
			if unusual {
				flags++
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if flags != 1 {
		t.Errorf("%d flags, want 1", flags)
	}
}
