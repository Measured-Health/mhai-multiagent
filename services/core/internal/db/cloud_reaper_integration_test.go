//go:build integration

package db_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00047: the stale cloud reservation reaper. mhai_cloud_reap
// settles the caller's hospital's reservations older than an age (at least
// 30 minutes) as fully used. A reservation row is consumed once, by the
// reap or the worker's settle, whichever commits first; the other changes
// nothing. All data is synthetic.

// age is a backdated reservation's age in these tests.
const staleAge = "2 hours"

// backdate makes reservation id look as old as staleAge (as the superuser).
func backdate(t *testing.T, pool *pgxpool.Pool, ids ...uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE cloud_reservations SET created_at = now() - interval '`+staleAge+`' WHERE id = ANY($1)`, ids); err != nil {
		t.Fatal(err)
	}
}

// reapedRow is one row mhai_cloud_reap returned.
type reapedRow struct {
	ID     uuid.UUID
	Tokens int64
	Age    int64
}

// reapIn runs mhai_cloud_reap(age, limit) in tx and returns its rows.
func reapIn(ctx context.Context, tx pgx.Tx, age string, limit int) ([]reapedRow, error) {
	rows, err := tx.Query(ctx, `SELECT reservation, tokens, age_seconds FROM mhai_cloud_reap($1::interval, $2)`, age, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (reapedRow, error) {
		var x reapedRow
		err := r.Scan(&x.ID, &x.Tokens, &x.Age)
		return x, err
	})
}

// reapAs reaps hospital h as mhai_cloud_reaper, committing.
func reapAs(ctx context.Context, pool *pgxpool.Pool, h uuid.UUID, age string, limit int) ([]reapedRow, error) {
	var out []reapedRow
	err := commitAs(ctx, pool, "mhai_cloud_reaper", &h, func(tx pgx.Tx) error {
		var err error
		out, err = reapIn(ctx, tx, age, limit)
		return err
	})
	return out, err
}

func reservationRows(t *testing.T, pool *pgxpool.Pool, h uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM cloud_reservations WHERE hospital_id = $1`, h).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustReserveN(t *testing.T, pool *pgxpool.Pool, h uuid.UUID, provider string, n int) uuid.UUID {
	t.Helper()
	id, _, ok, err := reserve(context.Background(), pool, h, provider, n)
	if err != nil || !ok {
		t.Fatalf("reserve %d: ok=%v err=%v", n, ok, err)
	}
	return id
}

// Only stale rows of the caller's own hospital are reaped, charged in full
// (reserved -> used, so used + reserved never moves), oldest first, up to
// the limit; a second reap finds nothing.
func TestReapSettlesOnlyStaleOwnRowsInFull(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("RA"))
	b, _ := seedHospital(t, pool, uniqueCode("RB"))
	setPolicy(t, pool, a, ptr("openai"), 100_000)
	setPolicy(t, pool, b, ptr("openai"), 100_000)

	old1 := mustReserveN(t, pool, a, "openai", 100)
	old2 := mustReserveN(t, pool, a, "openai", 200)
	old3 := mustReserveN(t, pool, a, "openai", 300)
	fresh := mustReserveN(t, pool, a, "openai", 50)
	otherOld := mustReserveN(t, pool, b, "openai", 70)
	backdate(t, pool, old1, old2, old3, otherOld)
	// Distinct ages, oldest first: old2, old1, old3.
	if _, err := pool.Exec(ctx, `UPDATE cloud_reservations SET created_at = created_at - interval '1 minute' WHERE id = $1`, old2); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE cloud_reservations SET created_at = now() - interval '90 minutes' WHERE id = $1`, old3); err != nil {
		t.Fatal(err)
	}
	if used, res := ledger(t, pool, a); used != 0 || res != 650 {
		t.Fatalf("before: used %d reserved %d", used, res)
	}

	// The hospitals with a stale row: a and b, not one without.
	var stale []uuid.UUID
	if err := commitAs(ctx, pool, "mhai_cloud_reaper", nil, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT h FROM mhai_cloud_stale_hospitals('1 hour') h`)
		if err != nil {
			return err
		}
		stale, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(stale, a) || !slices.Contains(stale, b) {
		t.Errorf("stale hospitals %v miss a or b", stale)
	}

	got, err := reapAs(ctx, pool, a, "1 hour", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != old2 || got[1].ID != old1 {
		t.Fatalf("first reap (limit 2) %v, want old2 then old1", got)
	}
	if got[0].Tokens != 200 || got[0].Age < 7200 || got[0].Age > 7200+600 {
		t.Errorf("first row %+v", got[0])
	}
	if used, res := ledger(t, pool, a); used != 300 || res != 350 {
		t.Errorf("after the first reap: used %d reserved %d, want 300 and 350", used, res)
	}
	got, err = reapAs(ctx, pool, a, "1 hour", 100)
	if err != nil || len(got) != 1 || got[0].ID != old3 {
		t.Fatalf("second reap %v %v, want old3 only", got, err)
	}
	if used, res := ledger(t, pool, a); used != 600 || res != 50 {
		t.Errorf("after the second reap: used %d reserved %d, want 600 and 50", used, res)
	}
	if got, err = reapAs(ctx, pool, a, "1 hour", 100); err != nil || len(got) != 0 {
		t.Errorf("third reap %v %v, want nothing", got, err)
	}
	// The fresh one is the worker's to settle; b's row is still b's.
	if done, err := settle(ctx, pool, a, fresh, 20); err != nil || !done {
		t.Errorf("settle of the fresh reservation: %v %v", done, err)
	}
	if used, res := ledger(t, pool, a); used != 620 || res != 0 {
		t.Errorf("after the settle: used %d reserved %d, want 620 and 0", used, res)
	}
	if n := reservationRows(t, pool, b); n != 1 {
		t.Errorf("b's rows %d, want 1 (a's reap never reaches b)", n)
	}
	if used, res := ledger(t, pool, b); used != 0 || res != 70 {
		t.Errorf("b's ledger moved: used %d reserved %d", used, res)
	}
	// A larger age leaves rows younger than it.
	if got, err = reapAs(ctx, pool, b, "3 hours", 100); err != nil || len(got) != 0 {
		t.Errorf("reap with a 3 hour age %v %v, want nothing", got, err)
	}
}

// Fail safe: an age under 30 minutes, a bad limit, or no hospital is
// refused, so a misconfigured caller can't reap a reservation in flight.
func TestReapRefusesYoungAgeBadLimitAndNoHospital(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("RF"))
	setPolicy(t, pool, a, ptr("openai"), 100_000)
	id := mustReserveN(t, pool, a, "openai", 10)
	if _, err := pool.Exec(ctx, `UPDATE cloud_reservations SET created_at = now() - interval '29 minutes' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	for _, age := range []string{"29 minutes 59 seconds", "0", "-1 hour", "1 minute"} {
		if _, err := reapAs(ctx, pool, a, age, 10); pgCode(err) != "22023" {
			t.Errorf("age %s: want 22023, got %v", age, err)
		}
		err := commitAs(ctx, pool, "mhai_cloud_reaper", nil, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT * FROM mhai_cloud_stale_hospitals($1::interval)`, age)
			return err
		})
		if pgCode(err) != "22023" {
			t.Errorf("stale hospitals, age %s: want 22023, got %v", age, err)
		}
	}
	err := commitAs(ctx, pool, "mhai_cloud_reaper", &a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT * FROM mhai_cloud_reap(NULL, 10)`)
		return err
	})
	if pgCode(err) != "22023" {
		t.Errorf("null age: want 22023, got %v", err)
	}
	for _, limit := range []int{0, -1, 1001} {
		if _, err := reapAs(ctx, pool, a, "30 minutes", limit); pgCode(err) != "22023" {
			t.Errorf("limit %d: want 22023, got %v", limit, err)
		}
	}
	err = commitAs(ctx, pool, "mhai_cloud_reaper", nil, func(tx pgx.Tx) error {
		_, err := reapIn(ctx, tx, "30 minutes", 10)
		return err
	})
	if pgCode(err) != "42501" {
		t.Errorf("no hospital: want 42501, got %v", err)
	}
	// At exactly the floor, the 29-minute row is still too young.
	if got, err := reapAs(ctx, pool, a, "30 minutes", 10); err != nil || len(got) != 0 {
		t.Errorf("29-minute row reaped at the floor: %v %v", got, err)
	}
	if used, res := ledger(t, pool, a); used != 0 || res != 10 {
		t.Errorf("ledger moved: used %d reserved %d", used, res)
	}
}

// The race rule, deterministic: a reap that commits first consumes the
// row, and a settle waiting on it then finds nothing (false, no change, no
// refund, never negative); a settle in progress keeps its row from a reap
// (SKIP LOCKED: the reap neither waits nor counts it).
func TestReapAndSettleConsumeEachRowOnce(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("RS"))
	setPolicy(t, pool, a, ptr("anthropic"), 100_000)

	// 1. Reap first, holding its transaction open; the settle waits.
	r1 := mustReserveN(t, pool, a, "anthropic", 500)
	backdate(t, pool, r1)
	reapTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reapTx.Rollback(ctx) }()
	for _, q := range []string{`SET LOCAL ROLE mhai_cloud_reaper`, `SELECT set_config('mhai.hospital_id', '` + a.String() + `', true)`} {
		if _, err := reapTx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	got, err := reapIn(ctx, reapTx, "1 hour", 10)
	if err != nil || len(got) != 1 || got[0].ID != r1 {
		t.Fatalf("reap %v %v", got, err)
	}
	type result struct {
		done bool
		err  error
	}
	late := make(chan result, 1)
	go func() {
		done, err := settle(ctx, pool, a, r1, 3)
		late <- result{done, err}
	}()
	waitForLockWait(t, pool)
	select {
	case r := <-late:
		t.Fatalf("the settle did not wait for the reap: %+v", r)
	default:
	}
	if err := reapTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-late:
		if r.err != nil || r.done {
			t.Errorf("late settle after the reap: done %v err %v, want false", r.done, r.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the late settle never returned")
	}
	if used, res := ledger(t, pool, a); used != 500 || res != 0 {
		t.Errorf("after reap then late settle: used %d reserved %d, want 500 and 0 (charged in full, no refund)", used, res)
	}
	// A settle well after the reap: the same.
	if done, err := settle(ctx, pool, a, r1, 1); err != nil || done {
		t.Errorf("a settle long after the reap: %v %v", done, err)
	}
	if used, res := ledger(t, pool, a); used != 500 || res != 0 {
		t.Errorf("after a second late settle: used %d reserved %d", used, res)
	}

	// 2. A settle in progress: the reap skips its row without waiting.
	r2 := mustReserveN(t, pool, a, "anthropic", 400)
	backdate(t, pool, r2)
	setTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = setTx.Rollback(ctx) }()
	for _, q := range []string{`SET LOCAL ROLE mhai_worker`, `SELECT set_config('mhai.hospital_id', '` + a.String() + `', true)`} {
		if _, err := setTx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var done bool
	if err := setTx.QueryRow(ctx, `SELECT mhai_cloud_settle($1, 40)`, r2).Scan(&done); err != nil || !done {
		t.Fatalf("settle in progress: %v %v", done, err)
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	got, err = reapAs(rctx, pool, a, "1 hour", 10)
	cancel()
	if err != nil || len(got) != 0 {
		t.Errorf("reap beside a settle in progress: %v %v, want nothing without waiting", got, err)
	}
	if err := setTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err = reapAs(ctx, pool, a, "1 hour", 10); err != nil || len(got) != 0 {
		t.Errorf("reap after the settle: %v %v, want nothing", got, err)
	}
	if used, res := ledger(t, pool, a); used != 540 || res != 0 {
		t.Errorf("after settle then reap: used %d reserved %d, want 540 and 0", used, res)
	}
	// 3. A settle that rolled back leaves the row for the reaper.
	r3 := mustReserveN(t, pool, a, "anthropic", 60)
	backdate(t, pool, r3)
	err = commitAs(ctx, pool, "mhai_worker", &a, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT mhai_cloud_settle($1, 1)`, r3); err != nil {
			return err
		}
		return errors.New("synthetic failure after the settle")
	})
	if err == nil {
		t.Fatal("the failing settle committed")
	}
	if got, err = reapAs(ctx, pool, a, "1 hour", 10); err != nil || len(got) != 1 || got[0].ID != r3 {
		t.Errorf("reap after a rolled-back settle: %v %v", got, err)
	}
	if used, res := ledger(t, pool, a); used != 600 || res != 0 {
		t.Errorf("after the failed settle's reap: used %d reserved %d, want 600 and 0", used, res)
	}
}

// waitForLockWait waits until some backend waits on a lock.
func waitForLockWait(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no backend waited on the reap's lock")
}

// Concurrent settles and reaps over the same stale rows, on separate
// connections: every row is consumed exactly once, reserved ends at 0 and
// never goes negative, and used is exactly what the winners charged.
func TestConcurrentSettleAndReap(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("RX"))
	const rows, n, reported = 40, 100, 7
	setPolicy(t, pool, a, ptr("xai"), rows*n)
	ids := make([]uuid.UUID, rows)
	for i := range ids {
		ids[i] = mustReserveN(t, pool, a, "xai", n)
	}
	backdate(t, pool, ids...)

	cfg := pool.Config().Copy()
	cfg.MaxConns, cfg.MinConns = 24, 8
	wide, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer wide.Close()

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		settled = map[uuid.UUID]int{}
		reaped  = map[uuid.UUID]int{}
		errs    []error
		start   = make(chan struct{})
	)
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			done, err := settle(ctx, wide, a, id, reported)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
			if done {
				settled[id]++
			}
		}()
	}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 5 {
				got, err := reapAs(ctx, wide, a, "1 hour", 3)
				mu.Lock()
				if err != nil {
					errs = append(errs, err)
				}
				for _, r := range got {
					reaped[r.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	// Whatever the reaps left (none won a row a settle still held) goes now.
	got, err := reapAs(ctx, pool, a, "1 hour", 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		reaped[r.ID]++
	}
	for _, id := range ids {
		if settled[id]+reaped[id] != 1 {
			t.Errorf("reservation consumed %d times (settled %d, reaped %d), want once", settled[id]+reaped[id], settled[id], reaped[id])
		}
	}
	used, res := ledger(t, pool, a)
	want := int64(len(settled)*reported + len(reaped)*n)
	if res != 0 || used != want {
		t.Errorf("ledger: used %d reserved %d, want %d and 0 (%d settled, %d reaped)", used, res, want, len(settled), len(reaped))
	}
	if r := reservationRows(t, pool, a); r != 0 {
		t.Errorf("%d reservation rows left", r)
	}
	t.Logf("%d settled, %d reaped", len(settled), len(reaped))
}

// Least privilege: the reaper role holds EXECUTE on its two functions and
// its audit append, nothing on the ledger; no other application role may
// call them; core's login reaches the role only by SET ROLE.
func TestCloudReaperPrivileges(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("RG"))
	setPolicy(t, pool, a, ptr("openai"), 1000)
	reaper := []string{`SET LOCAL ROLE mhai_cloud_reaper`}
	for _, table := range []string{"hospital_model_policy", "cloud_usage", "cloud_reservations", "clinical_queries", "hospitals"} {
		_, err := inTx(ctx, pool, reaper, &a, `SELECT count(*) FROM `+table)
		requireDenied(t, err, "reaper reading "+table)
	}
	for _, q := range []string{`DELETE FROM cloud_reservations`, `UPDATE cloud_usage SET used = 0`,
		`SELECT mhai_cloud_settle(gen_random_uuid(), 1)`, `SELECT * FROM mhai_cloud_reserve('openai', 1)`,
		`SELECT * FROM mhai_cloud_usage(gen_random_uuid())`} {
		_, err := inTx(ctx, pool, reaper, &a, q)
		requireDenied(t, err, "reaper: "+q)
	}
	// Its audit append: its hospital, no actor, its action only.
	for _, q := range []string{
		`INSERT INTO audit_log (ts, entity, hospital_id, action, payload, prev_hash, hash) VALUES (now(), 'cloud_reservation', gen_random_uuid(), 'cloud.reservation_reaped', '{}', 'x', 'y')`,
		`INSERT INTO audit_log (ts, entity, hospital_id, action, payload, prev_hash, hash) VALUES (now(), 'cloud_reservation', '` + a.String() + `', 'job.cancelled', '{}', 'x', 'y')`,
		`INSERT INTO audit_log (ts, entity, actor_id, hospital_id, action, payload, prev_hash, hash) VALUES (now(), 'cloud_reservation', gen_random_uuid(), '` + a.String() + `', 'cloud.reservation_reaped', '{}', 'x', 'y')`,
	} {
		_, err := inTx(ctx, pool, reaper, &a, q)
		if pgCode(err) != "42501" {
			t.Errorf("reaper append %q: want 42501, got %v", q, err)
		}
	}
	for _, name := range []string{"mhai_cloud_reap", "mhai_cloud_stale_hospitals"} {
		var owner string
		var definer bool
		var config, grantees []string
		if err := pool.QueryRow(ctx, `
			SELECT pg_get_userbyid(p.proowner), p.prosecdef, COALESCE(p.proconfig, '{}'),
			       COALESCE((SELECT array_agg(CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END ORDER BY 1)
			                 FROM aclexplode(p.proacl) a
			                 WHERE a.privilege_type = 'EXECUTE' AND a.grantee <> p.proowner), '{}')
			FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'public' AND p.proname = $1`, name).Scan(&owner, &definer, &config, &grantees); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if owner != "mhai_migrate" || !definer || len(config) != 1 || config[0] != "search_path=pg_catalog, pg_temp" ||
			strings.Join(grantees, ",") != "mhai_cloud_reaper" {
			t.Errorf("%s: owner %s definer %v config %v grantees %v", name, owner, definer, config, grantees)
		}
	}
	for _, role := range append(slices.Clone(appRoles), "mhai_emr_maint", "mhai_analytics_job", "mhai_analysis_job") {
		for _, q := range []string{`SELECT * FROM mhai_cloud_reap('1 hour', 1)`, `SELECT * FROM mhai_cloud_stale_hospitals('1 hour')`} {
			_, err := inTx(ctx, pool, []string{`SET LOCAL ROLE ` + role}, &a, q)
			requireDenied(t, err, role+" "+q)
		}
	}
	var login, inherit bool
	if err := pool.QueryRow(ctx, `
		SELECT r.rolcanlogin,
		       (SELECT m.inherit_option FROM pg_auth_members m WHERE m.roleid = r.oid AND m.member = 'mhai_app'::regrole)
		FROM pg_roles r WHERE r.rolname = 'mhai_cloud_reaper'`).Scan(&login, &inherit); err != nil {
		t.Fatal(err)
	}
	if login || inherit {
		t.Errorf("mhai_cloud_reaper: login %v, inherited by mhai_app %v; want neither", login, inherit)
	}
}

// 00047 goes down and up cleanly: the functions, the index and the append
// policy go, and a reservation still in the table stays as it was.
func TestMigration00047DownUp(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	pool, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
	a, _ := seedHospital(t, pool, uniqueCode("RD"))
	setPolicy(t, pool, a, ptr("openai"), 1000)
	id := mustReserveN(t, pool, a, "openai", 10)
	if err := db.MigrateDownTo(ctx, pg.URL, 46); err != nil {
		t.Fatalf("down to 46: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_proc WHERE proname IN ('mhai_cloud_reap', 'mhai_cloud_stale_hospitals'))
		+ (SELECT count(*) FROM pg_class WHERE relname = 'cloud_reservations_created')
		+ (SELECT count(*) FROM pg_policy WHERE polname IN ('cloud_reaper_append', 'owner_reap_lock'))
		+ (SELECT count(*) FROM pg_auth_members WHERE roleid = 'mhai_cloud_reaper'::regrole)`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("after down, %d objects of 00047 remain", left)
	}
	if done, err := settle(ctx, pool, a, id, 4); err != nil || !done {
		t.Errorf("settle after down: %v %v", done, err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
}
