//go:build integration

package db_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00019 (Plan 7c Task 3): the analytics reader and job roles,
// frozen counts, the four functions, analytics_snapshots, the epoch, the
// rolling guardrail snapshots emptied and mhai_guardrail_counts revoked
// from mhai_platform. Every id, name and count is synthetic.

const invalidParameter = "22023"

// analyticsFunctions are the four functions only mhai_analytics_job runs.
var analyticsFunctions = []string{
	`SELECT mhai_analytics_freeze('week', '2001-01-01')`,
	`SELECT count(*) FROM mhai_analytics_frozen('2001-01-01')`,
	`SELECT count(*) FROM mhai_analytics_onboarding()`,
	`SELECT count(*) FROM mhai_analytics_staff()`,
}

// asJob runs fn as mhai_analytics_job.
func asJob(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return db.WithAnalyticsJob(ctx, pool, fn)
}

func freeze(ctx context.Context, pool *pgxpool.Pool, p calendar.Period) error {
	return asJob(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT mhai_analytics_freeze($1, $2::date)`, string(p.Kind), p.From.Format(time.DateOnly))
		return err
	})
}

// setEpoch moves the release epoch, as the superuser (tests only).
func setEpoch(t *testing.T, pool *pgxpool.Pool, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE analytics_epoch SET released_at = $1`, at); err != nil {
		t.Fatal(err)
	}
}

// recentWeeks are the twelve latest finished weeks, oldest first: each test
// takes weeks of its own, since a period is frozen once per database.
func recentWeeks() []calendar.Period {
	return calendar.Previous(calendar.Latest(calendar.Week, time.Now()), 12)
}

// seedQuestion adds a question of hospital h at at, with a decision unless
// decision is "": PASS, REGENERATE or anything else (escalate).
func seedQuestion(t *testing.T, pool *pgxpool.Pool, h, by uuid.UUID, at time.Time, decision string, degraded bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	q := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, created_by, created_at)
		VALUES ($1, $2, 'synthetic question', 'routine', $3, $4)`, q, h, by, at); err != nil {
		t.Fatal(err)
	}
	if decision != "" {
		if _, err := pool.Exec(ctx, `INSERT INTO clinical_decisions (id, hospital_id, query_id, hga_decision, degraded, response_text, created_at)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, 'synthetic answer', $5)`, h, q, decision, degraded, at.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	return q
}

type frozenRow struct {
	pass, passD, regen, regenD, esc, escD, unanswered, agreed, corrected, clinicians int64
}

func frozenRows(t *testing.T, pool *pgxpool.Pool, quarter time.Time) map[string]map[uuid.UUID]frozenRow {
	t.Helper()
	ctx := context.Background()
	out := map[string]map[uuid.UUID]frozenRow{}
	if err := asJob(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT kind, period_from::text, hospital_id, q_pass, q_pass_degraded, q_regenerate, q_regenerate_degraded,
			q_escalate, q_escalate_degraded, q_unanswered, r_agreed, r_corrected, active_clinicians
			FROM mhai_analytics_frozen($1::date)`, quarter.Format(time.DateOnly))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind, from string
			var h uuid.UUID
			var r frozenRow
			if err := rows.Scan(&kind, &from, &h, &r.pass, &r.passD, &r.regen, &r.regenD, &r.esc, &r.escD, &r.unanswered, &r.agreed, &r.corrected, &r.clinicians); err != nil {
				return err
			}
			k := kind + " " + from
			if out[k] == nil {
				out[k] = map[uuid.UUID]frozenRow{}
			}
			out[k][h] = r
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func key(p calendar.Period) string { return string(p.Kind) + " " + p.From.Format(time.DateOnly) }

// A4: the freeze refuses anything but a finished, eligible, nested period
// within reach.
func TestAnalyticsFunctionsRefuseRollingWindows(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	setEpoch(t, pool, time.Now().AddDate(0, -6, 0))
	now := time.Now().UTC()
	thisWeek := calendar.Of(calendar.Week, now)
	recent := calendar.Of(calendar.Week, now.AddDate(0, 0, -30)).From
	for _, c := range []struct{ kind, from string }{
		{"month", recent.AddDate(0, 0, 1).Format(time.DateOnly)}, // a rolling 30 days
		{"week", recent.AddDate(0, 0, 1).Format(time.DateOnly)},  // a week that isn't month-aligned
		{"week", recent.AddDate(0, 0, 3).Format(time.DateOnly)},
		{"week", thisWeek.From.Format(time.DateOnly)},                                            // not finished
		{"month", calendar.Of(calendar.Month, now).From.Format(time.DateOnly)},                   // not finished
		{"week", calendar.Of(calendar.Week, now.AddDate(0, 0, -125)).From.Format(time.DateOnly)}, // older than 118 days
		{"30d", recent.Format(time.DateOnly)},
		{"quarter", calendar.Of(calendar.Month, now.AddDate(0, -2, 0)).From.AddDate(0, 0, 0).Format(time.DateOnly)},
		{"week", ""},
	} {
		if c.kind == "quarter" {
			if d, _ := time.Parse(time.DateOnly, c.from); d.Month()%3 == 1 {
				c.from = d.AddDate(0, 1, 0).Format(time.DateOnly) // a month start that isn't a quarter's
			}
		}
		err := asJob(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT mhai_analytics_freeze($1, NULLIF($2, '')::date)`, c.kind, c.from)
			return err
		})
		if pgCode(err) != invalidParameter {
			t.Errorf("freeze(%s, %s): want 22023, got %v", c.kind, c.from, err)
		}
	}
	// The frozen rows of a block: a quarter start only.
	err := asJob(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT count(*) FROM mhai_analytics_frozen('2026-11-01')`)
		return err
	})
	if pgCode(err) != invalidParameter {
		t.Errorf("frozen(a month start): want 22023, got %v", err)
	}
}

// A4 (R41a-3): a week that started before the epoch, and a month or quarter
// with any such child, is refused, even long after it closed.
func TestFreezeRefusesIneligible(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	ws := recentWeeks()
	w := ws[2]
	setEpoch(t, pool, w.From.Add(time.Hour)) // just after the week started
	defer setEpoch(t, pool, time.Now().AddDate(0, -6, 0))
	if err := freeze(ctx, pool, w); pgCode(err) != invalidParameter {
		t.Errorf("a week before the epoch: want 22023, got %v", err)
	}
	m, _ := calendar.Parent(w)
	if !m.To.Add(calendar.Grace).After(time.Now()) {
		if err := freeze(ctx, pool, m); pgCode(err) != invalidParameter {
			t.Errorf("its month: want 22023, got %v", err)
		}
	}
	// An earlier month, long closed, whose weeks all started before the epoch.
	old := calendar.Prev(calendar.Of(calendar.Month, w.From))
	if err := freeze(ctx, pool, old); pgCode(err) != invalidParameter {
		t.Errorf("a month before the epoch: want 22023, got %v", err)
	}
	q := calendar.Prev(calendar.Of(calendar.Quarter, time.Now()))
	setEpoch(t, pool, q.From.Add(24*time.Hour))
	if err := freeze(ctx, pool, q); pgCode(err) != invalidParameter {
		t.Errorf("a quarter whose first week started before the epoch: want 22023, got %v", err)
	}
}

// A27: a month whose weeks aren't all frozen is refused; a frozen month is
// the sum of its frozen weeks, whatever was committed since; a second
// freeze changes nothing.
func TestFreezeParentIsSumOfChildren(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	setEpoch(t, pool, time.Now().AddDate(0, -6, 0))
	// The latest finished month within reach.
	m := calendar.Latest(calendar.Month, time.Now())
	h, _ := seedHospital(t, pool, uniqueCode("F"))
	u := seedClinician(t, pool, h)
	weeks := calendar.Children(m)
	for i, w := range weeks {
		for j := 0; j <= i; j++ {
			seedQuestion(t, pool, h, u, w.From.Add(time.Duration(j+1)*time.Hour), "PASS", j%2 == 0)
		}
	}
	// Weeks might be frozen already by another test of this database: the
	// sums are checked against what the weeks hold, per hospital.
	if err := freeze(ctx, pool, weeks[0]); err != nil {
		t.Fatalf("week 1: %v", err)
	}
	frozenAlready := map[string]bool{}
	if err := freeze(ctx, pool, m); pgCode(err) != invalidParameter {
		got := frozenRows(t, pool, calendar.BlockOf(m).From)
		for _, w := range weeks[1:] {
			if _, ok := got[key(w)]; ok {
				frozenAlready[key(w)] = true
			}
		}
		if len(frozenAlready) < 3 {
			t.Errorf("a month with a week not frozen: want 22023, got %v", err)
		}
	}
	for _, w := range weeks[1:] {
		if err := freeze(ctx, pool, w); err != nil {
			t.Fatalf("%v: %v", w, err)
		}
	}
	// A late row, committed after the weeks froze, dated inside week 2.
	seedQuestion(t, pool, h, u, weeks[1].From.Add(30*time.Minute), "ESCALATE", false)
	if err := freeze(ctx, pool, m); err != nil {
		t.Fatalf("month: %v", err)
	}
	if err := freeze(ctx, pool, m); err != nil {
		t.Fatalf("month again: %v", err)
	}
	got := frozenRows(t, pool, calendar.BlockOf(m).From)
	var sum frozenRow
	for _, w := range weeks {
		r := got[key(w)][h]
		sum.pass += r.pass
		sum.passD += r.passD
		sum.esc += r.esc
	}
	mr := got[key(m)][h]
	if mr.pass != sum.pass || mr.passD != sum.passD || mr.esc != sum.esc {
		t.Errorf("month %+v, weeks sum %+v", mr, sum)
	}
	if mr.pass+mr.passD != 10 || mr.esc != 0 {
		t.Errorf("month counts %+v, want 10 PASS and the late ESCALATE not counted", mr)
	}
	if got[key(weeks[0])][h].pass+got[key(weeks[0])][h].passD != 1 || got[key(weeks[0])][h].clinicians != 1 {
		t.Errorf("week 1 %+v", got[key(weeks[0])][h])
	}
}

// A4: two parallel freezes of one week, and a retry after a late row, leave
// exactly the first freeze's rows, for every hospital.
func TestFreezeOncePerPeriod(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	setEpoch(t, pool, time.Now().AddDate(0, -6, 0))
	// A week of the month before the latest: never one of
	// TestFreezeParentIsSumOfChildren's, which freezes the latest month's
	// weeks first (recentWeeks()[5] was one of them from the 22nd on).
	w := calendar.Children(calendar.Prev(calendar.Latest(calendar.Month, time.Now())))[1]
	h, _ := seedHospital(t, pool, uniqueCode("O"))
	u := seedClinician(t, pool, h)
	for i := 0; i < 6; i++ {
		seedQuestion(t, pool, h, u, w.From.Add(time.Duration(i+1)*time.Hour), "REGENERATE", false)
	}
	seedQuestion(t, pool, h, u, w.From.Add(10*time.Hour), "", false) // unanswered
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = freeze(ctx, pool, w)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("parallel freeze: %v", err)
		}
	}
	first := frozenRows(t, pool, calendar.BlockOf(w).From)[key(w)]
	if r := first[h]; r.regen != 6 || r.unanswered != 1 || r.clinicians != 1 {
		t.Fatalf("frozen %+v", r)
	}
	seedQuestion(t, pool, h, u, w.From.Add(2*time.Hour), "PASS", false)
	late, _ := seedHospital(t, pool, uniqueCode("O"))
	if err := freeze(ctx, pool, w); err != nil {
		t.Fatal(err)
	}
	again := frozenRows(t, pool, calendar.BlockOf(w).From)[key(w)]
	if len(again) != len(first) || again[h] != first[h] {
		t.Errorf("a second freeze changed the rows: %+v → %+v", first[h], again[h])
	}
	if _, ok := again[late]; ok {
		t.Error("a hospital added after the freeze has a row")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM analytics_frozen_periods WHERE kind = 'week' AND period_from = $1::date`, w.From.Format(time.DateOnly)).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d marker rows (%v)", n, err)
	}
}

// A29: mhai_platform can't read the frozen counts or run any analytics
// function; nobody can change or delete a frozen row.
func TestFrozenCountsNotSelectable(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for _, q := range append([]string{
		`SELECT count(*) FROM analytics_period_counts`, `SELECT count(*) FROM analytics_frozen_periods`,
	}, analyticsFunctions...) {
		err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
		requireDenied(t, err, "platform: "+q)
	}
	for _, role := range []string{"mhai_gateway", "mhai_auth", "mhai_worker", "mhai_jobs", "mhai_audit_drain", "mhai_provision", "mhai_guardrail_reader", "mhai_health_reader"} {
		for _, q := range analyticsFunctions {
			requireDenied(t, asRole(ctx, pool, role, q), role+": "+q)
		}
		requireDenied(t, asRole(ctx, pool, role, `SELECT count(*) FROM analytics_period_counts`), role+" reading counts")
	}
	for _, q := range []string{
		`SELECT count(*) FROM analytics_period_counts`, `SELECT count(*) FROM analytics_frozen_periods`,
		`UPDATE analytics_period_counts SET q_pass = 0`, `DELETE FROM analytics_period_counts`,
		`UPDATE analytics_frozen_periods SET frozen_at = now()`, `DELETE FROM analytics_frozen_periods`,
	} {
		requireDenied(t, asJob(ctx, pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err }), "job: "+q)
	}
	for _, q := range []string{`UPDATE analytics_period_counts SET q_pass = 0`, `DELETE FROM analytics_period_counts`, `TRUNCATE analytics_period_counts`} {
		requireDenied(t, asRole(ctx, pool, "mhai_analytics_reader", q), "reader: "+q)
	}
}

// A30: after 00019, mhai_platform can't run mhai_guardrail_counts; after
// its Down, it can, as 00017 left it.
func TestGuardrailCountsRevoked(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	call := func() error {
		return db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT * FROM mhai_guardrail_counts(now() - interval '7 days', now())`)
			return err
		})
	}
	requireDenied(t, call(), "platform after 00019")
	if err := db.MigrateDownTo(ctx, pg.URL, 18); err != nil {
		t.Fatalf("down to 18: %v", err)
	}
	if err := call(); err != nil {
		t.Errorf("after 00019's Down: %v", err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
	requireDenied(t, call(), "platform after 00019 again")
}

// R41a-3: 00019 empties guardrail_snapshots.
func TestGuardrailSnapshotsEmptied(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.MigrateDownTo(ctx, pg.URL, 18); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO guardrail_snapshots (period, day, body) VALUES ('7d', '2026-11-01', '{"period": "7d"}')`); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM guardrail_snapshots`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d rolling snapshots after 00019 (%v)", n, err)
	}
}

// Every new table is owned by mhai_migrate with RLS forced; the reader
// holds grants only; the roles can't log in, bypass RLS or be reached but
// through SET ROLE.
func TestOwnership(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for _, tbl := range []string{"analytics_snapshots", "analytics_period_counts", "analytics_frozen_periods", "analytics_epoch"} {
		var rls, force bool
		var owner string
		if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity, pg_get_userbyid(relowner) FROM pg_class WHERE relname = $1`, tbl).Scan(&rls, &force, &owner); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
		if !rls || !force || owner != "mhai_migrate" {
			t.Errorf("%s: rls %v force %v owner %s", tbl, rls, force, owner)
		}
		var public bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege('public', $1, 'SELECT')`, tbl).Scan(&public); err != nil || public {
			t.Errorf("%s: PUBLIC can select (%v)", tbl, err)
		}
	}
	var rels int
	var owned []string
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relowner = 'mhai_analytics_reader'::regrole`).Scan(&rels); err != nil || rels != 0 {
		t.Errorf("the reader owns %d relations (%v)", rels, err)
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(array_agg(proname::text ORDER BY proname), '{}') FROM pg_proc WHERE proowner = 'mhai_analytics_reader'::regrole`).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	if strings.Join(owned, ",") != "mhai_analytics_freeze,mhai_analytics_frozen,mhai_analytics_onboarding,mhai_analytics_staff" {
		t.Errorf("the reader owns %v", owned)
	}
	for _, role := range []string{"mhai_analytics_reader", "mhai_analytics_job"} {
		var login, super, bypass bool
		var members int
		if err := pool.QueryRow(ctx, `SELECT rolcanlogin, rolsuper, rolbypassrls,
			(SELECT count(*) FROM pg_auth_members m WHERE m.roleid = r.oid AND m.member <> 'mhai_app'::regrole)
			FROM pg_roles r WHERE rolname = $1`, role).Scan(&login, &super, &bypass, &members); err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		if login || super || bypass || members != 0 {
			t.Errorf("%s: login %v super %v bypassrls %v, %d other members", role, login, super, bypass, members)
		}
	}
	var inheritsJob bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.roleid = 'mhai_analytics_job'::regrole
		AND m.member = 'mhai_app'::regrole AND m.inherit_option)`).Scan(&inheritsJob); err != nil || inheritsJob {
		t.Errorf("mhai_app inherits mhai_analytics_job (%v)", err)
	}
	var readerMembers int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_auth_members WHERE roleid = 'mhai_analytics_reader'::regrole`).Scan(&readerMembers); err != nil || readerMembers != 0 {
		t.Errorf("the reader has %d members (%v)", readerMembers, err)
	}
}

// A14: the functions return their declared columns only (aggregates), every
// hospital with zeros; no user id.
func TestAnalyticsFunctionsReturnAggregatesOnly(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("S"))
	quiet, _ := seedHospital(t, pool, uniqueCode("S"))
	seedClinician(t, pool, h)
	seedClinician(t, pool, h)
	want := map[string]string{
		`SELECT * FROM mhai_analytics_onboarding()`: "hospital_id,name,status,created_at",
		`SELECT * FROM mhai_analytics_staff()`:      "hospital_id,staff",
		`SELECT * FROM mhai_analytics_frozen('2026-10-01')`: "kind,period_from,period_to,hospital_id,q_pass,q_pass_degraded,q_regenerate,q_regenerate_degraded," +
			"q_escalate,q_escalate_degraded,q_unanswered,r_agreed,r_corrected,active_clinicians,reviews_decided,median_review_hours",
	}
	for q, cols := range want {
		if err := asJob(ctx, pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, q)
			if err != nil {
				return err
			}
			defer rows.Close()
			var names []string
			for _, f := range rows.FieldDescriptions() {
				names = append(names, f.Name)
			}
			if strings.Join(names, ",") != cols {
				t.Errorf("%s: columns %v", q, names)
			}
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	staff := map[uuid.UUID]int64{}
	if err := asJob(ctx, pool, func(tx pgx.Tx) error {
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
			staff[id] = n
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if staff[h] != 2 {
		t.Errorf("staff %d, want 2", staff[h])
	}
	if n, ok := staff[quiet]; !ok || n != 0 {
		t.Errorf("a hospital with no staff: %d %v", n, ok)
	}
}

// A14, extended from C17 (TestPlatformRoleHasNoClinicalRows): neither the
// routes' role nor the scheduler's reads a clinical row, and nobody but the
// scheduler runs mhai_guardrail_counts' successors; mhai_guardrail_counts
// itself has no EXECUTE grantee left (A30).
func TestAnalyticsRolesHaveNoClinicalRows(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for _, tbl := range []string{"clinical_queries", "clinical_decisions", "feedback_events"} {
		q := `SELECT count(*) FROM ` + tbl
		requireDenied(t, db.WithPlatform(ctx, pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err }), "platform: "+tbl)
		requireDenied(t, asJob(ctx, pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err }), "job: "+tbl)
	}
	for _, q := range []string{`SELECT count(*) FROM users`, `SELECT count(*) FROM hospitals`, `SELECT count(*) FROM audit_log`} {
		requireDenied(t, asJob(ctx, pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err }), "job: "+q)
	}
	var grantees []string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(array_agg(CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END), '{}')
		FROM pg_proc p, aclexplode(p.proacl) a WHERE p.proname = 'mhai_guardrail_counts' AND a.privilege_type = 'EXECUTE' AND a.grantee <> p.proowner`).Scan(&grantees); err != nil {
		t.Fatal(err)
	}
	if len(grantees) != 0 {
		t.Errorf("mhai_guardrail_counts: EXECUTE held by %v, want nobody", grantees)
	}
	// The four analytics functions: owned by the reader, definer, fixed
	// search_path, EXECUTE for mhai_analytics_job only.
	rows, err := pool.Query(ctx, `SELECT p.proname, pg_get_userbyid(p.proowner), p.prosecdef, COALESCE(p.proconfig, '{}'),
		COALESCE((SELECT array_agg(CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END)
		          FROM aclexplode(p.proacl) a WHERE a.privilege_type = 'EXECUTE' AND a.grantee <> p.proowner), '{}')
		FROM pg_proc p WHERE p.proname LIKE 'mhai\_analytics\_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var name, owner string
		var definer bool
		var config, gs []string
		if err := rows.Scan(&name, &owner, &definer, &config, &gs); err != nil {
			t.Fatal(err)
		}
		n++
		if owner != "mhai_analytics_reader" || !definer || len(config) != 1 || config[0] != "search_path=pg_catalog, pg_temp" ||
			len(gs) != 1 || gs[0] != "mhai_analytics_job" {
			t.Errorf("%s: owner %s definer %v config %v EXECUTE %v", name, owner, definer, config, gs)
		}
	}
	if n != 4 {
		t.Errorf("%d analytics functions, want 4", n)
	}
}

// The reader: exactly its columns, rows of the last 120 days.
func TestAnalyticsReaderColumnsOnly(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("C"))
	u := seedClinician(t, pool, h)
	seedQuestion(t, pool, h, u, time.Now().Add(-time.Hour), "PASS", false)
	seedQuestion(t, pool, h, u, time.Now().AddDate(0, 0, -130), "PASS", false)
	// seedHospital adds a question of its own, now: two recent, one old.
	if n, err := countAs(t, pool, "mhai_analytics_reader", `SELECT count(created_at) FROM clinical_queries WHERE hospital_id = $1`, h); err != nil || n != 2 {
		t.Errorf("reader sees %d of two recent questions and an old one (%v)", n, err)
	}
	if n, err := countAs(t, pool, "mhai_analytics_reader", `SELECT count(created_at) FROM clinical_decisions WHERE hospital_id = $1`, h); err != nil || n != 1 {
		t.Errorf("reader sees %d of a recent and an old decision (%v)", n, err)
	}
	for _, q := range []string{
		`SELECT count(clinical_question) FROM clinical_queries`, `SELECT count(patient_data_json) FROM clinical_queries`,
		`SELECT count(response_text) FROM clinical_decisions`, `SELECT count(diagnosis) FROM clinical_decisions`,
		`SELECT count(reason) FROM feedback_events`, `SELECT count(corrected_diagnosis) FROM feedback_events`,
		`SELECT count(submitted_by) FROM feedback_events`, `SELECT count(email) FROM users`, `SELECT count(display_name) FROM users`,
		`SELECT count(*) FROM sessions`, `SELECT count(*) FROM audit_log`, `SELECT count(*) FROM user_mfa`,
	} {
		_, err := countAs(t, pool, "mhai_analytics_reader", q)
		requireDenied(t, err, "reader: "+q)
	}
	var cols []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(table_name || '.' || column_name ORDER BY table_name, column_name)
		FROM information_schema.column_privileges WHERE grantee = 'mhai_analytics_reader' AND privilege_type = 'SELECT'
		AND table_name IN ('clinical_queries', 'clinical_decisions', 'feedback_events', 'hospitals', 'users', 'user_roles')`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	want := "clinical_decisions.created_at,clinical_decisions.degraded,clinical_decisions.hga_decision,clinical_decisions.hospital_id,clinical_decisions.query_id," +
		"clinical_queries.created_at,clinical_queries.created_by,clinical_queries.hospital_id,clinical_queries.id," +
		"feedback_events.action,feedback_events.created_at,feedback_events.decided_at,feedback_events.hospital_id,feedback_events.status," +
		"hospitals.created_at,hospitals.id,hospitals.name,hospitals.status," +
		"user_roles.role,user_roles.user_id,users.hospital_id,users.id,users.is_active"
	if strings.Join(cols, ",") != want {
		t.Errorf("reader columns\n%s\nwant\n%s", strings.Join(cols, ","), want)
	}
}

// A17: the scheduler's role may insert a snapshot only for a finished,
// eligible period; nobody may change or delete one; the routes' role reads
// only; the CHECKs hold.
func TestSnapshotInsertPolicy(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	setEpoch(t, pool, time.Now().AddDate(0, -6, 0))
	ins := func(kind, from, to, state, body string) error {
		return asJob(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO analytics_snapshots (kind, period_from, period_to, state, body)
				VALUES ($1, $2::date, $3::date, $4, NULLIF($5, '')::jsonb)`, kind, from, to, state, body)
			return err
		})
	}
	w := recentWeeks()[8]
	from, to := w.From.Format(time.DateOnly), w.To.Format(time.DateOnly)
	if err := ins("week", from, to, "ok", `{"x": 1}`); err != nil {
		t.Fatalf("a finished week: %v", err)
	}
	if err := ins("week", from, to, "ok", `{"x": 1}`); pgCode(err) != "23505" {
		t.Errorf("a second row: want 23505, got %v", err)
	}
	cur := calendar.Of(calendar.Week, time.Now())
	if err := ins("week", cur.From.Format(time.DateOnly), cur.To.Format(time.DateOnly), "ok", `{}`); pgCode(err) != permissionDenied {
		t.Errorf("an unfinished week: want 42501 (the policy), got %v", err)
	}
	old := calendar.Prev(calendar.Of(calendar.Month, time.Now().AddDate(0, -7, 0)))
	if err := ins("month", old.From.Format(time.DateOnly), old.To.Format(time.DateOnly), "unavailable", ""); pgCode(err) != permissionDenied {
		t.Errorf("a month before the epoch: want 42501, got %v", err)
	}
	v := recentWeeks()[10]
	vf, vt := v.From.Format(time.DateOnly), v.To.Format(time.DateOnly)
	off := v.From.AddDate(0, 0, 1)
	for _, c := range []struct{ kind, from, to, state, body string }{
		{"week", off.Format(time.DateOnly), off.AddDate(0, 0, 7).Format(time.DateOnly), "unavailable", ""}, // not a week
		{"month", vf, vt, "unavailable", ""},
		{"week", vf, vt, "unavailable", `{}`}, // a body with unavailable
		{"week", vf, vt, "ok", ""},            // no body with ok
		{"week", vf, vt, "computing", ""},
		{"week", vf, vt, "ok", `[1]`},
	} {
		if err := ins(c.kind, c.from, c.to, c.state, c.body); pgCode(err) != "23514" {
			t.Errorf("%+v: want 23514, got %v", c, err)
		}
	}
	for _, q := range []string{`UPDATE analytics_snapshots SET state = 'unavailable', body = NULL`, `DELETE FROM analytics_snapshots`} {
		requireDenied(t, asJob(ctx, pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err }), "job: "+q)
		requireDenied(t, db.WithPlatform(ctx, pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err }), "platform: "+q)
	}
	err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO analytics_snapshots (kind, period_from, period_to, state) VALUES ('week', $1::date, $2::date, 'unavailable')`,
			recentWeeks()[9].From.Format(time.DateOnly), recentWeeks()[9].To.Format(time.DateOnly))
		return err
	})
	requireDenied(t, err, "platform inserting")
	var n int
	if err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM analytics_snapshots WHERE period_from = $1::date`, from).Scan(&n)
	}); err != nil || n != 1 {
		t.Errorf("platform reads %d (%v)", n, err)
	}
	for _, role := range []string{"mhai_gateway", "mhai_auth", "mhai_worker", "mhai_jobs", "mhai_provision"} {
		requireDenied(t, asRole(ctx, pool, role, `SELECT count(*) FROM analytics_snapshots`), role+" reading snapshots")
	}
}

// 00019 goes down to 00018 and up again, leaving nothing behind.
func TestMigration00019DownUp(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.MigrateDownTo(ctx, pg.URL, 18); err != nil {
		t.Fatalf("down to 18: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_class WHERE relname IN ('analytics_snapshots', 'analytics_period_counts', 'analytics_frozen_periods', 'analytics_epoch'))
		+ (SELECT count(*) FROM pg_proc WHERE proname LIKE 'mhai_analytics%' OR proname LIKE 'mhai_reporting%')
		+ (SELECT count(*) FROM pg_policies WHERE policyname LIKE 'analytics%')
		+ (SELECT count(*) FROM unnest(ARRAY['clinical_queries', 'clinical_decisions', 'feedback_events', 'hospitals', 'users', 'user_roles']) t
		   WHERE has_any_column_privilege('mhai_analytics_reader', t, 'SELECT'))
		+ (SELECT count(*) FROM pg_auth_members WHERE roleid = 'mhai_analytics_job'::regrole)
		+ (SELECT count(*) FROM pg_indexes WHERE indexname LIKE 'analytics%' OR indexname IN ('clinical_queries_created_idx', 'feedback_events_created_idx', 'feedback_events_decided_idx'))
		+ (SELECT count(*) FROM pg_namespace n, aclexplode(n.nspacl) a WHERE n.nspname = 'public'
		   AND a.grantee IN ('mhai_analytics_job'::regrole, 'mhai_analytics_reader'::regrole))`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("after down, %d objects, grants or memberships of 00019 remain", left)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
	var epochs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM analytics_epoch WHERE released_at > now() - interval '1 hour'`).Scan(&epochs); err != nil || epochs != 1 {
		t.Errorf("epoch rows %d (%v)", epochs, err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
}
