//go:build integration

package db_test

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00028 (analysis backend plan Task 12): lists, pids and the
// distinct-patient cap. Synthetic hospitals, subjects and facts; every
// period is written frozen by the superuser (the freeze itself is 00021's,
// tested there).

// drill is a switched-on hospital with frozen April 2026 periods (its
// first block, the month and nothing else) and n synthetic female subjects
// aged 40-64 with condition c in the block, half also with condition d.
type drill struct {
	h, user, week, month, block2 uuid.UUID
	c, d                         uuid.UUID
	subjects                     []uuid.UUID
}

func newDrill(t *testing.T, pool *pgxpool.Pool, n int) drill {
	t.Helper()
	ctx := context.Background()
	h := onHospital(t, pool, "Africa/Lagos")
	x := drill{h: h, user: uuid.New(), c: condition(t, pool, h, "I10"), d: condition(t, pool, h, "E11")}
	for _, p := range []struct {
		id          *uuid.UUID
		level, s, e string
	}{{&x.week, "week", "2026-04-01", "2026-04-07"}, {&x.block2, "week", "2026-04-08", "2026-04-14"}, {&x.month, "month", "2026-04-01", "2026-04-30"}} {
		*p.id = uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO analysis_periods (id, hospital_id, level, start, "end", engine_version)
			VALUES ($1, $2, $3, $4::date, $5::date, '1')`, *p.id, h, p.level, p.s, p.e); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		x.subjects = append(x.subjects, addSubject(t, pool, h, "2026-04-02", x.c, i%2 == 0, x.d))
	}
	return x
}

// addSubject is one synthetic subject with a diagnosis of c on day (and of
// d when both).
func addSubject(t *testing.T, pool *pgxpool.Pool, h uuid.UUID, day string, c uuid.UUID, both bool, d uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	s := uuid.New()
	r := newRef()
	if _, err := pool.Exec(ctx, `INSERT INTO analysis_subjects (hospital_id, subject_id, ref_hmac, created_day)
		VALUES ($1, $2, decode($3, 'hex'), '2026-09-01')`, h, s, r.hmac); err != nil {
		t.Fatal(err)
	}
	conds := []uuid.UUID{c}
	if both {
		conds = append(conds, d)
	}
	for _, k := range conds {
		if _, err := pool.Exec(ctx, `INSERT INTO analysis_facts (hospital_id, subject_id, day, kind, condition_id, age_band, sex, setting, period_late)
			VALUES ($1, $2, $3::date, 'diagnosis', $4, '40-64', 'female', 'outpatient', false)`, h, s, day, k); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// asPerson runs fn as the gateway for (h, user), as db.WithHospitalUser
// sets them, committing when fn returns nil.
func asPerson(ctx context.Context, pool *pgxpool.Pool, h, user uuid.UUID, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{`SET LOCAL ROLE mhai_gateway`} {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true), set_config('mhai.user_id', $2, true)`,
		h.String(), user.String()); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// asReader is asPerson as the reader role: the internal helpers' tests.
func asReader(ctx context.Context, pool *pgxpool.Pool, h, user uuid.UUID, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_analysis_reader`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true), set_config('mhai.user_id', $2, true)`,
		h.String(), user.String()); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var femaleGroup = `{"kind": "part", "dim": "sex", "key": "female"}`

type listRow struct {
	ord                  int
	pid, sex             string
	age, setting         *string
	conditions, findings []byte
}

// open lists group of period for (h, user), with its audit entry
// (unless noAudit), and returns the rows.
func open(ctx context.Context, pool *pgxpool.Pool, x drill, user, period uuid.UUID, filters, group string, expected, cap int,
	noAudit bool) ([]listRow, uuid.UUID, error) {
	view := uuid.New()
	var out []listRow
	err := asPerson(ctx, pool, x.h, user, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT ord, pid, age_band, sex, setting, conditions::text, findings::text
			FROM mhai_analysis_open($1, $2, $3::jsonb, $4::jsonb, 'affected', $5, $6, $7, NULL)`,
			view, period, filters, group, make([]byte, 32), expected, cap)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r listRow
			if err := rows.Scan(&r.ord, &r.pid, &r.age, &r.sex, &r.setting, &r.conditions, &r.findings); err != nil {
				rows.Close()
				return err
			}
			out = append(out, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if noAudit {
			return nil
		}
		return audit.Write(ctx, tx, audit.Event{ActorID: user.String(), HospitalID: x.h.String(), Action: "analysis.patients_viewed",
			Entity: "analysis_view", EntityID: view.String(), Payload: map[string]any{"view": "affected", "period": "week:2026-04-01",
				"filters": map[string]any{}, "patient_count": expected, "rows": min(expected, 200)}})
	})
	return out, view, err
}

var pidShape = regexp.MustCompile(`^P-[A-Z2-7]{8}$`)

// T28, Review Focus 4: 20 parallel reservations of 40 new subjects each
// against 300 open at most 300 distinct subjects; the counter equals the
// rows.
func TestReservePatientsRace(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	x := newDrill(t, pool, 0)
	var batches [][]uuid.UUID
	for i := 0; i < 20; i++ {
		var b []uuid.UUID
		for j := 0; j < 40; j++ {
			b = append(b, addSubject(t, pool, x.h, "2026-04-02", x.c, false, x.d))
		}
		batches = append(batches, b)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(batches))
	for i, b := range batches {
		wg.Add(1)
		go func(i int, b []uuid.UUID) {
			defer wg.Done()
			errs[i] = asReader(ctx, pool, x.h, x.user, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `SELECT mhai_analysis_reserve_patients('2026-04-01', $1, 300)`, b)
				return err
			})
		}(i, b)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if pgCode(err) != "P0001" {
			t.Fatalf("reservation: %v", err)
		}
	}
	opens := countRows(t, pool, `SELECT count(*) FROM analysis_patient_opens WHERE hospital_id = $1 AND user_id = $2`, x.h, x.user)
	counter := countRows(t, pool, `SELECT opened FROM analysis_patient_budget WHERE hospital_id = $1 AND user_id = $2`, x.h, x.user)
	if ok != 7 || opens != 280 || counter != opens {
		t.Errorf("%d reservations went through, %d opens, counter %d; want 7, 280, 280", ok, opens, counter)
	}
}

// A block's subject opened again in its month costs nothing: one cap per
// quarter, shared by the levels.
func TestSubjectCountsOnceAcrossLevels(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	x := newDrill(t, pool, 12)
	if _, _, err := open(ctx, pool, x, x.user, x.week, `{}`, femaleGroup, 12, 300, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := open(ctx, pool, x, x.user, x.month, `{}`, femaleGroup, 12, 300, false); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, `SELECT opened FROM analysis_patient_budget WHERE hospital_id = $1 AND user_id = $2 AND quarter = '2026-04-01'`,
		x.h, x.user); n != 12 {
		t.Errorf("opened %d after the block and its month, want 12", n)
	}
	// A cap of 12 still admits the month's list again.
	if _, _, err := open(ctx, pool, x, x.user, x.month, `{}`, femaleGroup, 12, 12, false); err != nil {
		t.Errorf("reopening within the quarter: %v", err)
	}
}

// The refused request leaves no list, no row and no open.
func TestCapRefusalRollsBackView(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	x := newDrill(t, pool, 12)
	_, _, err := open(ctx, pool, x, x.user, x.week, `{}`, femaleGroup, 12, 5, false)
	if pgCode(err) != "P0001" {
		t.Fatalf("over the cap: %v", err)
	}
	if n := countRows(t, pool, `SELECT (SELECT count(*) FROM analysis_views WHERE hospital_id = $1)
		+ (SELECT count(*) FROM analysis_view_rows WHERE hospital_id = $1)
		+ (SELECT count(*) FROM analysis_patient_opens WHERE hospital_id = $1)`, x.h); n != 0 {
		t.Errorf("%d rows left by a refused list", n)
	}
}

// A list opens only on its cell's exact count of 5 or more, in a context
// the rows describe (departure 19); anything else is too_small or refused.
func TestOpenRefusesWrongCountAndHiddenDimensions(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	x := newDrill(t, pool, 12)
	for _, c := range []struct {
		name, filters, group string
		expected             int
		code                 string
	}{
		{"count off by one", `{}`, femaleGroup, 11, "P0001"},
		{"a group of 4", `{}`, `{"kind": "part", "dim": "sex", "key": "male"}`, 0, "P0001"},
		{"by location", `{}`, `{"kind": "part", "dim": "location", "key": "x"}`, 12, "22023"},
		{"a location filter", `{"location": "` + uuid.NewString() + `"}`, femaleGroup, 12, "22023"},
		{"a specialty filter", `{"specialty": "cardiology"}`, femaleGroup, 12, "22023"},
		{"a source filter", `{"source": "` + uuid.NewString() + `"}`, femaleGroup, 12, "22023"},
		{"three filters", `{"sex": "female", "age_band": "40-64", "setting": "outpatient"}`, femaleGroup, 12, "22023"},
		{"pair out of order", `{}`, fmt.Sprintf(`{"kind": "pair", "a": "%s", "b": "%s"}`, maxID(x.c, x.d), minID(x.c, x.d)), 6, "22023"},
		{"an extra group key", `{}`, `{"kind": "part", "dim": "sex", "key": "female", "x": "y"}`, 12, "22023"},
	} {
		if _, _, err := open(ctx, pool, x, x.user, x.week, c.filters, c.group, c.expected, 300, false); pgCode(err) != c.code {
			t.Errorf("%s: %v, want %s", c.name, err, c.code)
		}
	}
	// The pair and the condition filter count as cells does.
	pair := fmt.Sprintf(`{"kind": "pair", "a": "%s", "b": "%s"}`, minID(x.c, x.d), maxID(x.c, x.d))
	if rows, _, err := open(ctx, pool, x, x.user, x.week, `{}`, pair, 6, 300, false); err != nil || len(rows) != 6 {
		t.Errorf("pair: %d rows (%v)", len(rows), err)
	}
	if rows, _, err := open(ctx, pool, x, x.user, x.week, `{"condition": "`+x.d.String()+`"}`, femaleGroup, 6, 300, false); err != nil || len(rows) != 6 {
		t.Errorf("condition filter: %d rows (%v)", len(rows), err)
	}
}

func minID(a, b uuid.UUID) string { return min(a.String(), b.String()) }
func maxID(a, b uuid.UUID) string { return max(a.String(), b.String()) }

// T18 (SQL half): a list whose audit entry isn't appended is refused at
// commit; so is one whose entry names another person.
func TestListWithoutAuditRefusedAtCommit(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	x := newDrill(t, pool, 12)
	if _, _, err := open(ctx, pool, x, x.user, x.week, `{}`, femaleGroup, 12, 300, true); pgCode(err) != "23514" {
		t.Fatalf("a list with no audit entry: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_views WHERE hospital_id = $1`, x.h); n != 0 {
		t.Errorf("%d unaudited lists kept", n)
	}
}

// Pids: the shape, the same subject's pid stable within a period, a
// different pid in another period and at another level.
func TestPidShapeAndPerPeriod(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	x := newDrill(t, pool, 12)
	// A second block with the same subjects.
	for _, s := range x.subjects {
		if _, err := pool.Exec(ctx, `INSERT INTO analysis_facts (hospital_id, subject_id, day, kind, condition_id, age_band, sex, setting, period_late)
			VALUES ($1, $2, '2026-04-09', 'diagnosis', $3, '40-64', 'female', 'outpatient', false)`, x.h, s, x.c); err != nil {
			t.Fatal(err)
		}
	}
	pids := func(period uuid.UUID, filters string, n int) []string {
		rows, _, err := open(ctx, pool, x, x.user, period, filters, femaleGroup, n, 300, false)
		if err != nil || len(rows) != n {
			t.Fatalf("list: %d rows (%v)", len(rows), err)
		}
		var out []string
		for i, r := range rows {
			if !pidShape.MatchString(r.pid) || r.ord != i+1 || r.sex != "female" || r.age == nil || *r.age != "40-64" {
				t.Fatalf("row %+v", r)
			}
			out = append(out, r.pid)
		}
		return out
	}
	a := pids(x.week, `{}`, 12)
	b := pids(x.week, `{"condition": "`+x.c.String()+`"}`, 12)
	if !slices.Equal(a, b) {
		t.Errorf("one period, two lists: pids differ")
	}
	other := append(pids(x.block2, `{}`, 12), pids(x.month, `{}`, 12)...)
	for _, p := range a {
		if slices.Contains(other, p) {
			t.Errorf("a pid of the first block is used again in another period or level")
		}
	}
	if n := countRows(t, pool, `SELECT count(DISTINCT pid) FROM analysis_pseudonyms WHERE hospital_id = $1`, x.h); n != 36 {
		t.Errorf("%d distinct pids, want 36", n)
	}
}

// N9: a pid already used in the period is drawn again. The draw is
// replaced, on a database of its own, by one that repeats a pid.
func TestPidCollisionRedrawn(t *testing.T) {
	_, pool := testutil.MigratedPostgres(t)
	ctx := context.Background()
	x := newDrill(t, pool, 6)
	if _, err := pool.Exec(ctx, `CREATE SEQUENCE pid_test_seq`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION mhai_analysis_pid_draw() RETURNS text LANGUAGE sql VOLATILE
		SET search_path = pg_catalog, pg_temp AS $$
		SELECT CASE WHEN nextval('public.pid_test_seq') <= 3 THEN 'P-AAAAAAAA'
		            ELSE 'P-' || upper(translate(substr(md5(random()::text), 1, 8), '0189', 'WXYZ')) END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `GRANT USAGE ON SEQUENCE pid_test_seq TO mhai_analysis_reader`); err != nil {
		t.Fatal(err)
	}
	rows, _, err := open(ctx, pool, x, x.user, x.week, `{}`, femaleGroup, 6, 300, false)
	if err != nil || len(rows) != 6 {
		t.Fatalf("list: %d rows (%v)", len(rows), err)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.pid] {
			t.Errorf("pid repeated in a period")
		}
		seen[r.pid] = true
	}
	if !seen["P-AAAAAAAA"] {
		t.Errorf("the first draw wasn't kept")
	}
}

// T19 (SQL half): no role writes a list directly; inside the function, a
// list is written only with its viewer the request's person.
func TestViewInsertNeedsSelfAsViewer(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	x := newDrill(t, pool, 6)
	insert := `INSERT INTO analysis_views (id, hospital_id, viewer, level, period_id, source_view, filters, drill_hash, matched, sent, day, at)
		VALUES (gen_random_uuid(), $1, $2, 'deidentified', $3, 'affected', '{}', decode(repeat('00', 32), 'hex'), 6, 6, '2026-11-01', now())`
	err := asPerson(ctx, pool, x.h, x.user, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, insert, x.h, x.user, x.week); return err })
	requireDenied(t, err, "the gateway writing a list")
	err = asReader(ctx, pool, x.h, x.user, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, insert, x.h, uuid.New(), x.week); return err })
	if pgCode(err) != "42501" {
		t.Errorf("a list for another viewer: %v, want an RLS refusal", err)
	}
	for _, q := range []string{`UPDATE analysis_views SET matched = 6`, `DELETE FROM analysis_view_rows`,
		`DELETE FROM analysis_patient_opens`, `UPDATE analysis_pseudonyms SET pid = 'P-AAAAAAAA'`} {
		err := asReader(ctx, pool, x.h, x.user, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
		requireDenied(t, err, q)
	}
}

// The platform reads no list, row or pid directly; the gateway runs no
// reveal; only the platform's function reveals, by audit id.
func TestPlatformCantReadRowsDirectly(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for _, table := range []string{"analysis_views", "analysis_view_rows", "analysis_pseudonyms", "analysis_patient_opens",
		"analysis_patient_budget", "analysis_drill_flags"} {
		err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
			var n int
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
		})
		requireDenied(t, err, "the platform reading "+table)
	}
}

func TestAuditViewPseudonymsOnlyForPlatform(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	x := newDrill(t, pool, 6)
	rows, view, err := open(ctx, pool, x, x.user, x.week, `{}`, femaleGroup, 6, 300, false)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := pool.QueryRow(ctx, `SELECT id FROM audit_log WHERE entity = 'analysis_view' AND entity_id = $1`, view.String()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mhai_audit_view_pseudonyms($1)`, id).Scan(&got)
	}); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, r := range rows {
		want = append(want, r.pid)
	}
	if !slices.Equal(got, want) {
		t.Errorf("revealed %d pids, not the list's %d in order", len(got), len(want))
	}
	// Another entry: null.
	var other []string
	if err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mhai_audit_view_pseudonyms(-1)`).Scan(&other)
	}); err != nil || other != nil {
		t.Errorf("a non drill-down entry revealed %v (%v)", other, err)
	}
	for _, role := range []string{"mhai_gateway", "mhai_auth", "mhai_provision", "mhai_analysis_job", "mhai_jobs", "mhai_analysis_reader"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT has_function_privilege($1, 'mhai_audit_view_pseudonyms(bigint)', 'EXECUTE')`, role).Scan(&ok); err != nil || ok {
			t.Errorf("%s may run the reveal (%v)", role, err)
		}
	}
}

// The request functions are the gateway's alone; the helpers nobody's.
func TestAnalysisDrillGrants(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	var bad int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_proc p, unnest(ARRAY['mhai_auth', 'mhai_platform', 'mhai_provision',
		'mhai_analysis_job', 'mhai_jobs', 'mhai_worker_read', 'mhai_worker_write']) r(role)
		WHERE p.proname IN ('mhai_analysis_list_admit', 'mhai_analysis_open', 'mhai_analysis_drill_flag')
		  AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r.role)
		  AND has_function_privilege(r.role, p.oid, 'EXECUTE')`).Scan(&bad); err != nil || bad != 0 {
		t.Errorf("%d grants of the list functions beyond the gateway (%v)", bad, err)
	}
	var internal int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_proc p, unnest(ARRAY['mhai_gateway', 'mhai_platform']) r(role)
		WHERE p.proname IN ('mhai_analysis_group', 'mhai_analysis_reserve_patients', 'mhai_analysis_pseudonyms',
			'mhai_analysis_pid_draw', 'mhai_analysis_view_audited')
		  AND has_function_privilege(r.role, p.oid, 'EXECUTE')`).Scan(&internal); err != nil || internal != 0 {
		t.Errorf("%d internal helpers executable by an app role (%v)", internal, err)
	}
	var members int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_auth_members m WHERE m.roleid = 'mhai_analysis_auditor'::regrole
		AND m.member <> (SELECT oid FROM pg_roles WHERE rolname = current_user)`).Scan(&members); err != nil || members != 0 {
		t.Errorf("mhai_analysis_auditor has %d members (%v)", members, err)
	}
}

// The list limits count a person's lists in the minute and the hospital
// day, under a lock.
func TestListAdmitLimits(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	x := newDrill(t, pool, 6)
	admit := func(user uuid.UUID, minute, day int) string {
		var got string
		if err := asPerson(ctx, pool, x.h, user, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT mhai_analysis_list_admit($1, $2, NULL)`, minute, day).Scan(&got)
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	for i := 0; i < 3; i++ {
		if got := admit(x.user, 3, 3); got != "ok" {
			t.Fatalf("list %d: %q", i+1, got)
		}
		if _, _, err := open(ctx, pool, x, x.user, x.week, `{}`, femaleGroup, 6, 300, false); err != nil {
			t.Fatal(err)
		}
	}
	if got := admit(x.user, 3, 100); got != "minute" {
		t.Errorf("a 4th list within the minute (3 allowed): %q", got)
	}
	if got := admit(x.user, 100, 3); got != "day" {
		t.Errorf("a 4th list in the hospital day (3 allowed): %q", got)
	}
	if got := admit(uuid.New(), 3, 3); got != "ok" {
		t.Errorf("another person: %q", got)
	}
}

// N10: opens and their counters go after the quarter and one month; a
// subject a list cites is kept.
func TestOpensSwept(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	x := newDrill(t, pool, 6)
	if _, _, err := open(ctx, pool, x, x.user, x.week, `{}`, femaleGroup, 6, 300, false); err != nil {
		t.Fatal(err)
	}
	sweep := func(day string) {
		if err := asAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT mhai_analysis_sweep($1, $2)`, x.h, at(t, "Africa/Lagos", day, 0))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	opens := `SELECT (SELECT count(*) FROM analysis_patient_opens WHERE hospital_id = $1) + (SELECT count(*) FROM analysis_patient_budget WHERE hospital_id = $1)`
	sweep("2026-07-31")
	if n := countRows(t, pool, opens, x.h); n != 7 {
		t.Errorf("before the quarter and a month have passed: %d rows, want 7", n)
	}
	sweep("2026-08-01")
	if n := countRows(t, pool, opens, x.h); n != 0 {
		t.Errorf("after: %d rows left", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analysis_view_rows r JOIN analysis_subjects s USING (hospital_id, subject_id)
		WHERE r.hospital_id = $1`, x.h); n != 6 {
		t.Errorf("subjects cited by the list: %d kept, want 6", n)
	}
}

// 00028 goes down to the migration before it and up again, leaving
// nothing behind and the platform's appends as 00022 left them.
func TestMigration00028DownUp(t *testing.T) {
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
	var before string
	if err := pool.QueryRow(ctx, `SELECT pg_get_expr(polwithcheck, polrelid) FROM pg_policy WHERE polname = 'platform_append'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 27); err != nil {
		t.Fatalf("down to 27: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_class WHERE relname IN ('analysis_views', 'analysis_view_rows', 'analysis_patient_budget',
			'analysis_patient_opens', 'analysis_drill_flags', 'audit_log_analysis_view_idx'))
		+ (SELECT count(*) FROM pg_proc WHERE proname IN ('mhai_analysis_pid_draw', 'mhai_analysis_group', 'mhai_analysis_reserve_patients',
			'mhai_analysis_pseudonyms', 'mhai_analysis_view_audited', 'mhai_analysis_list_admit', 'mhai_analysis_open',
			'mhai_analysis_drill_flag', 'mhai_audit_view_pseudonyms'))
		+ (SELECT count(*) FROM pg_policies WHERE policyname IN ('analysis_reader', 'analysis_auditor')
			AND tablename IN ('audit_log', 'analysis_pseudonyms'))
		+ (SELECT count(*) FROM unnest(ARRAY['audit_log', 'analysis_pseudonyms']) t
		   WHERE has_any_column_privilege('mhai_analysis_reader', t, 'SELECT') OR has_any_column_privilege('mhai_analysis_auditor', t, 'SELECT'))
		+ (SELECT count(*) FROM pg_proc WHERE proname = 'mhai_analysis_sweep' AND prosrc LIKE '%analysis_patient_opens%')
		+ (SELECT count(*) FROM pg_policy WHERE polname = 'platform_append'
			AND pg_get_expr(polwithcheck, polrelid) LIKE '%audit.patients_viewed%')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("after down, %d objects or grants of 00028 remain", left)
	}
	var down string
	if err := pool.QueryRow(ctx, `SELECT pg_get_expr(polwithcheck, polrelid) FROM pg_policy WHERE polname = 'platform_append'`).Scan(&down); err != nil {
		t.Fatal(err)
	}
	if want := regexp.MustCompile(`,\s*'audit\.patients_viewed'::text`).ReplaceAllString(before, ""); down != want {
		t.Errorf("platform_append after down:\n%s\nwant 00022's:\n%s", down, want)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
