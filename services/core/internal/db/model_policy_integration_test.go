//go:build integration

package db_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00018 (cloud model providers plan, Task 1): the per-hospital
// model policy, clinical_queries.model_route set by a definer trigger, the
// effective-route function the worker's claim calls, and the token ledger
// the worker reaches only through mhai_cloud_reserve and mhai_cloud_settle.

// appRoles are every application role; none but those named in a test may
// hold a privilege on the policy or the ledger.
var appRoles = []string{"mhai_gateway", "mhai_platform", "mhai_auth", "mhai_worker", "mhai_worker_app", "mhai_app",
	"mhai_provision", "mhai_jobs", "mhai_audit_drain", "mhai_health_reader", "mhai_jobs_reader", "mhai_guardrail_reader"}

// setPolicy writes hospital h's policy as the superuser; a nil provider is local.
func setPolicy(t *testing.T, pool *pgxpool.Pool, h uuid.UUID, provider *string, budget int64) {
	t.Helper()
	ref := (*string)(nil)
	if provider != nil {
		r := "SYNTH-AGREEMENT-1"
		ref = &r
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO hospital_model_policy (hospital_id, cloud_provider, agreement_ref, monthly_token_budget)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (hospital_id) DO UPDATE
		SET cloud_provider = EXCLUDED.cloud_provider, agreement_ref = EXCLUDED.agreement_ref,
		    monthly_token_budget = EXCLUDED.monthly_token_budget`, h, provider, ref, budget); err != nil {
		t.Fatalf("set policy: %v", err)
	}
}

// commitAs runs fn in a transaction as role (and hospital, when given) on
// the superuser pool, and commits when fn returns nil.
func commitAs(ctx context.Context, pool *pgxpool.Pool, role string, hospital *uuid.UUID, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+pgx.Identifier{role}.Sanitize()); err != nil {
		return err
	}
	if hospital != nil {
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, hospital.String()); err != nil {
			return err
		}
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// gatewayInsert inserts a question for h as mhai_gateway, naming route as
// its model_route (or not naming one when route is ""), and returns the id.
func gatewayInsert(t *testing.T, pool *pgxpool.Pool, h uuid.UUID, route string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := db.WithHospital(context.Background(), pool, h, func(tx pgx.Tx) error {
		if route == "" {
			_, err := tx.Exec(context.Background(),
				`INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency) VALUES ($1, $2, 'synthetic question', 'routine')`, id, h)
			return err
		}
		_, err := tx.Exec(context.Background(),
			`INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, model_route) VALUES ($1, $2, 'synthetic question', 'routine', $3)`,
			id, h, route)
		return err
	})
	if err != nil {
		t.Fatalf("gateway insert: %v", err)
	}
	return id
}

func routeOf(t *testing.T, pool *pgxpool.Pool, q uuid.UUID) string {
	t.Helper()
	var r string
	if err := pool.QueryRow(context.Background(), `SELECT model_route FROM clinical_queries WHERE id = $1`, q).Scan(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

// reserve calls mhai_cloud_reserve(n) as the worker for h, committing.
// ok is false when it returned no row.
func reserve(ctx context.Context, pool *pgxpool.Pool, h uuid.UUID, n any) (id uuid.UUID, percent int, ok bool, err error) {
	err = commitAs(ctx, pool, "mhai_worker", &h, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT reservation, percent FROM mhai_cloud_reserve($1)`, n).Scan(&id, &percent)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		ok = err == nil
		return err
	})
	return id, percent, ok, err
}

func settle(ctx context.Context, pool *pgxpool.Pool, h uuid.UUID, id any, used any) (bool, error) {
	var done bool
	err := commitAs(ctx, pool, "mhai_worker", &h, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mhai_cloud_settle($1, $2)`, id, used).Scan(&done)
	})
	return done, err
}

// ledger reads h's row for the current month as the superuser (0, 0 when none).
func ledger(t *testing.T, pool *pgxpool.Pool, h uuid.UUID) (used, reserved int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(sum(used), 0), COALESCE(sum(reserved), 0) FROM cloud_usage
		WHERE hospital_id = $1 AND month = date_trunc('month', now() AT TIME ZONE 'UTC')::date`, h).Scan(&used, &reserved); err != nil {
		t.Fatal(err)
	}
	return used, reserved
}

func ptr(s string) *string { return &s }

// A13: the route comes from core's policy, whatever the insert names.
func TestModelRouteSetByTriggerNotByInsert(t *testing.T) {
	_, pool := shared(t)
	a, seeded := seedHospital(t, pool, uniqueCode("MR"))
	if r := routeOf(t, pool, seeded); r != "local" {
		t.Errorf("a superuser insert with no policy: route %q, want local", r)
	}
	if r := routeOf(t, pool, gatewayInsert(t, pool, a, "cloud:xai")); r != "local" {
		t.Errorf("no policy, insert naming cloud:xai: route %q, want local", r)
	}
	setPolicy(t, pool, a, nil, 1000)
	if r := routeOf(t, pool, gatewayInsert(t, pool, a, "cloud:openai")); r != "local" {
		t.Errorf("policy off, insert naming cloud:openai: route %q, want local", r)
	}
	setPolicy(t, pool, a, ptr("anthropic"), 1000)
	if r := routeOf(t, pool, gatewayInsert(t, pool, a, "cloud:xai")); r != "cloud:anthropic" {
		t.Errorf("anthropic policy, insert naming cloud:xai: route %q, want cloud:anthropic", r)
	}
	if r := routeOf(t, pool, gatewayInsert(t, pool, a, "local")); r != "cloud:anthropic" {
		t.Errorf("anthropic policy, insert naming local: route %q, want cloud:anthropic", r)
	}
	if r := routeOf(t, pool, gatewayInsert(t, pool, a, "")); r != "cloud:anthropic" {
		t.Errorf("anthropic policy, insert naming none: route %q, want cloud:anthropic", r)
	}
	// A value outside the vocabulary is overwritten too, not refused.
	if r := routeOf(t, pool, gatewayInsert(t, pool, a, "cloud:elsewhere")); r != "cloud:anthropic" {
		t.Errorf("anthropic policy, insert naming cloud:elsewhere: route %q, want cloud:anthropic", r)
	}
	// Another hospital's policy never reaches this one.
	b, _ := seedHospital(t, pool, uniqueCode("MR"))
	if r := routeOf(t, pool, gatewayInsert(t, pool, b, "cloud:anthropic")); r != "local" {
		t.Errorf("hospital without a policy: route %q, want local", r)
	}
	// The column's own check holds for the owner too.
	if _, err := pool.Exec(context.Background(), `UPDATE clinical_queries SET model_route = 'cloud:elsewhere' WHERE id = $1`, seeded); pgCode(err) != "23514" {
		t.Errorf("model_route outside the vocabulary: want 23514, got %v", err)
	}
}

// A13: the gateway (a hospital-side request) can't read or write the
// policy, or change a route; no application role may update model_route.
func TestGatewayCannotTouchPolicyOrRoute(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, q := seedHospital(t, pool, uniqueCode("GW"))
	setPolicy(t, pool, a, ptr("openai"), 1000)
	for what, stmt := range map[string]string{
		"select policy": `SELECT count(*) FROM hospital_model_policy`,
		"insert policy": `INSERT INTO hospital_model_policy (hospital_id, cloud_provider, agreement_ref) VALUES ('` + a.String() + `', 'xai', 'SYNTH-1')`,
		"update policy": `UPDATE hospital_model_policy SET cloud_provider = 'xai'`,
		"delete policy": `DELETE FROM hospital_model_policy`,
		"update route":  `UPDATE clinical_queries SET model_route = 'cloud:xai' WHERE id = '` + q.String() + `'`,
	} {
		err := db.WithHospital(ctx, pool, a, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, stmt); return err })
		requireDenied(t, err, "gateway "+what)
	}
	if r := routeOf(t, pool, q); r != "local" {
		t.Errorf("route changed to %q", r)
	}
	// The gateway's UPDATE on clinical_queries is published_at only (00008).
	var cols []string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(array_agg(attname::text ORDER BY attname), '{}') FROM pg_attribute
		WHERE attrelid = 'clinical_queries'::regclass AND attnum > 0 AND NOT attisdropped
		  AND has_column_privilege('mhai_gateway', attrelid, attnum, 'UPDATE')`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if len(cols) != 1 || cols[0] != "published_at" {
		t.Errorf("mhai_gateway may update clinical_queries %v, want [published_at]", cols)
	}
	for _, role := range appRoles {
		var can bool
		if err := pool.QueryRow(ctx, `SELECT has_column_privilege($1, 'clinical_queries', 'model_route', 'UPDATE')`, role).Scan(&can); err != nil {
			t.Fatal(err)
		}
		if can {
			t.Errorf("%s may update clinical_queries.model_route", role)
		}
	}
}

// K15: a temporary table can't stand in for the policy or the ledger in a
// definer function (search_path pinned, names schema-qualified).
func TestTempTableCannotShadowPolicy(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("TT"))

	// The gateway's own hospital_model_policy, naming xai: the trigger ignores it.
	id := uuid.New()
	var route string
	err := db.WithHospital(ctx, pool, a, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE hospital_model_policy (hospital_id uuid, cloud_provider text,
			agreement_ref text, monthly_token_budget bigint) ON COMMIT DROP`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO hospital_model_policy VALUES ($1, 'xai', 'SYNTH-1', 1000000)`, a); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency) VALUES ($1, $2, 'synthetic question', 'routine')`, id, a); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT model_route FROM public.clinical_queries WHERE id = $1`, id).Scan(&route)
	})
	if err != nil {
		t.Fatalf("gateway with a temporary policy: %v", err)
	}
	if route != "local" {
		t.Errorf("a temporary policy chose route %q, want local", route)
	}

	// The worker's own tables, against the real policy (anthropic, budget
	// 100, all of it used): neither function reads them.
	setPolicy(t, pool, a, ptr("anthropic"), 100)
	if _, err := pool.Exec(ctx, `INSERT INTO cloud_usage (hospital_id, month, used) VALUES ($1, date_trunc('month', now() AT TIME ZONE 'UTC')::date, 100)`, a); err != nil {
		t.Fatal(err)
	}
	var eff string
	var reserved int
	err = commitAs(ctx, pool, "mhai_worker", &a, func(tx pgx.Tx) error {
		for _, s := range []string{
			`CREATE TEMP TABLE hospital_model_policy (hospital_id uuid, cloud_provider text, agreement_ref text, monthly_token_budget bigint) ON COMMIT DROP`,
			`CREATE TEMP TABLE cloud_usage (hospital_id uuid, month date, used bigint, reserved bigint) ON COMMIT DROP`,
			`CREATE TEMP TABLE cloud_reservations (id uuid DEFAULT gen_random_uuid(), hospital_id uuid, month date, tokens bigint, created_at timestamptz) ON COMMIT DROP`,
		} {
			if _, err := tx.Exec(ctx, s); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO hospital_model_policy VALUES ($1, 'xai', 'SYNTH-1', 100000000)`, a); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO cloud_usage VALUES ($1, date_trunc('month', now() AT TIME ZONE 'UTC')::date, 0, 0)`, a); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT mhai_effective_model_route($1, 'cloud:xai')`, a).Scan(&eff); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM mhai_cloud_reserve(10)`).Scan(&reserved)
	})
	if err != nil {
		t.Fatalf("worker with temporary tables: %v", err)
	}
	if eff != "local" {
		t.Errorf("a temporary policy made the effective route %q, want local", eff)
	}
	if reserved != 0 {
		t.Errorf("a temporary ledger let %d reservations through a spent budget", reserved)
	}
	if used, res := ledger(t, pool, a); used != 100 || res != 0 {
		t.Errorf("real ledger: used %d reserved %d, want 100 and 0", used, res)
	}
}

// K4: the worker holds no grant on the policy or the ledger; it reaches
// them only through the functions. The policy is the platform's and the
// provisioning CLI's; the ledger is no application role's.
func TestWorkerHasNoGrantOnPolicyOrLedger(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("WK"))
	setPolicy(t, pool, a, ptr("openai"), 1000)
	for _, table := range []string{"hospital_model_policy", "cloud_usage", "cloud_reservations"} {
		_, err := asWorker(ctx, pool, a, `SELECT count(*) FROM `+table)
		requireDenied(t, err, "worker reading "+table)
		_, err = asWorker(ctx, pool, a, `DELETE FROM `+table)
		requireDenied(t, err, "worker deleting "+table)
	}
	_, err := asWorker(ctx, pool, a, `UPDATE cloud_usage SET used = 0`)
	requireDenied(t, err, "worker updating cloud_usage")
	_, err = asWorker(ctx, pool, a, `INSERT INTO cloud_reservations (hospital_id, month, tokens) VALUES ($1, current_date, 1)`, a)
	requireDenied(t, err, "worker inserting cloud_reservations")

	want := map[string][]string{
		"hospital_model_policy": {"mhai_platform", "mhai_provision"},
		"cloud_usage":           {},
		"cloud_reservations":    {},
	}
	for table, roles := range want {
		var got []string
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(array_agg(r ORDER BY r), '{}') FROM unnest($1::text[]) r
			WHERE has_table_privilege(r, $2, 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
			   OR has_any_column_privilege(r, $2, 'SELECT,INSERT,UPDATE,REFERENCES')`, appRoles, table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, ",") != strings.Join(roles, ",") {
			t.Errorf("%s: privileges held by %v, want %v", table, got, roles)
		}
		var rls, force bool
		var owner string
		if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity, pg_get_userbyid(relowner) FROM pg_class WHERE oid = $1::regclass`, table).
			Scan(&rls, &force, &owner); err != nil {
			t.Fatal(err)
		}
		if !rls || !force || owner != "mhai_migrate" {
			t.Errorf("%s: rls %v force %v owner %s", table, rls, force, owner)
		}
	}

	// Every definer function of 00018: owner mhai_migrate, the pinned
	// search_path, no PUBLIC execute, and EXECUTE for its one role.
	funcs := map[string]string{
		"mhai_set_model_route":       "",
		"mhai_effective_model_route": "mhai_worker",
		"mhai_cloud_reserve":         "mhai_worker",
		"mhai_cloud_settle":          "mhai_worker",
		"mhai_cloud_usage":           "mhai_platform",
	}
	for name, grantee := range funcs {
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
		if owner != "mhai_migrate" || !definer {
			t.Errorf("%s: owner %s, security definer %v", name, owner, definer)
		}
		if len(config) != 1 || config[0] != "search_path=pg_catalog, pg_temp" {
			t.Errorf("%s: config %v, want the fixed search_path", name, config)
		}
		wantGrantees := ""
		if grantee != "" {
			wantGrantees = grantee
		}
		if strings.Join(grantees, ",") != wantGrantees {
			t.Errorf("%s: EXECUTE held by %v, want [%s]", name, grantees, wantGrantees)
		}
	}
	for _, role := range []string{"mhai_gateway", "mhai_platform", "mhai_auth", "mhai_jobs", "mhai_provision", "mhai_audit_drain"} {
		for _, q := range []string{`SELECT * FROM mhai_cloud_reserve(1)`, `SELECT mhai_cloud_settle(gen_random_uuid(), 1)`,
			`SELECT mhai_effective_model_route(gen_random_uuid(), 'cloud:openai')`} {
			_, err := inTx(ctx, pool, []string{`SET LOCAL ROLE ` + role}, &a, q)
			requireDenied(t, err, role+" "+q)
		}
	}
	for _, role := range []string{"mhai_gateway", "mhai_worker", "mhai_auth", "mhai_jobs", "mhai_provision", "mhai_audit_drain"} {
		_, err := inTx(ctx, pool, []string{`SET LOCAL ROLE ` + role}, &a, `SELECT * FROM mhai_cloud_usage($1)`, a)
		requireDenied(t, err, role+" calling mhai_cloud_usage")
	}
}

// A11: 20 parallel reservations against a budget for 5: exactly 5 win.
func TestReserveIsAtomicUnderRace(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("RC"))
	const n = 1000
	setPolicy(t, pool, a, ptr("anthropic"), 5*n)

	cfg := pool.Config().Copy()
	cfg.MaxConns, cfg.MinConns = 20, 20
	wide, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer wide.Close()

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		won   = map[uuid.UUID]bool{}
		errs  []error
		start = make(chan struct{})
	)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, _, ok, err := reserve(ctx, wide, a, n)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
			if ok {
				won[id] = true
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("reserve errors: %v", errs)
	}
	if len(won) != 5 {
		t.Fatalf("%d reservations won, want exactly 5", len(won))
	}
	if used, res := ledger(t, pool, a); used != 0 || res != 5*n {
		t.Errorf("ledger: used %d reserved %d, want 0 and %d", used, res, 5*n)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM cloud_reservations WHERE hospital_id = $1`, a).Scan(&rows); err != nil || rows != 5 {
		t.Errorf("cloud_reservations rows %d (%v), want 5", rows, err)
	}
}

// K4: the reservation refuses a non-positive, huge or missing amount, and a
// caller with no hospital.
func TestReserveRefusesNonPositiveHugeAndNoHospital(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("RF"))
	setPolicy(t, pool, a, ptr("openai"), 10_000_000)
	for _, bad := range []any{0, -1, 100001, nil} {
		if _, _, ok, err := reserve(ctx, pool, a, bad); pgCode(err) != "22023" || ok {
			t.Errorf("reserve(%v): want 22023, got ok=%v err=%v", bad, ok, err)
		}
	}
	if _, pct, ok, err := reserve(ctx, pool, a, 100000); err != nil || !ok || pct != 1 {
		t.Errorf("reserve(100000): ok=%v percent=%d err=%v, want a reservation at 1%%", ok, pct, err)
	}
	// No hospital set: refused, not run for "no hospital".
	_, err := inTx(ctx, pool, []string{`SET LOCAL ROLE mhai_worker`}, nil, `SELECT * FROM mhai_cloud_reserve(10)`)
	if pgCode(err) != permissionDenied {
		t.Errorf("reserve with no hospital: want 42501, got %v", err)
	}
	if used, res := ledger(t, pool, a); used != 0 || res != 100000 {
		t.Errorf("ledger: used %d reserved %d, want 0 and 100000", used, res)
	}
}

// No policy, or a policy switched off: nothing is reserved.
func TestReserveWithoutEnabledPolicyReturnsNothing(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("RN"))
	if _, _, ok, err := reserve(ctx, pool, a, 10); err != nil || ok {
		t.Errorf("no policy: ok=%v err=%v, want no reservation", ok, err)
	}
	setPolicy(t, pool, a, nil, 1000)
	if _, _, ok, err := reserve(ctx, pool, a, 10); err != nil || ok {
		t.Errorf("policy off: ok=%v err=%v, want no reservation", ok, err)
	}
	setPolicy(t, pool, a, ptr("xai"), 100)
	if _, pct, ok, err := reserve(ctx, pool, a, 80); err != nil || !ok || pct != 80 {
		t.Errorf("enabled: ok=%v percent=%d err=%v, want a reservation at 80%%", ok, pct, err)
	}
	if _, _, ok, err := reserve(ctx, pool, a, 21); err != nil || ok {
		t.Errorf("over budget: ok=%v err=%v, want no reservation", ok, err)
	}
	if _, pct, ok, err := reserve(ctx, pool, a, 20); err != nil || !ok || pct != 100 {
		t.Errorf("exactly the budget: ok=%v percent=%d err=%v, want a reservation at 100%%", ok, pct, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM cloud_reservations WHERE hospital_id = $1`, a).Scan(&rows); err != nil || rows != 2 {
		t.Errorf("cloud_reservations rows %d (%v), want 2", rows, err)
	}
}

// K5: a reservation settles once, for its own hospital only; used never
// grows by more than was reserved and reserved never goes negative.
func TestSettleIsOncePerReservationAndOwnHospitalOnly(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("SA"))
	b, _ := seedHospital(t, pool, uniqueCode("SB"))
	setPolicy(t, pool, a, ptr("anthropic"), 10_000)
	setPolicy(t, pool, b, ptr("anthropic"), 10_000)
	mustReserve := func(h uuid.UUID, n int) uuid.UUID {
		t.Helper()
		id, _, ok, err := reserve(ctx, pool, h, n)
		if err != nil || !ok {
			t.Fatalf("reserve %d: ok=%v err=%v", n, ok, err)
		}
		return id
	}
	check := func(what string, wantUsed, wantReserved int64) {
		t.Helper()
		if used, res := ledger(t, pool, a); used != wantUsed || res != wantReserved {
			t.Errorf("%s: used %d reserved %d, want %d and %d", what, used, res, wantUsed, wantReserved)
		}
	}
	settleWant := func(what string, h uuid.UUID, id, used any, want bool) {
		t.Helper()
		got, err := settle(ctx, pool, h, id, used)
		if err != nil || got != want {
			t.Errorf("%s: settled %v err %v, want %v", what, got, err, want)
		}
	}

	r1 := mustReserve(a, 100)
	settleWant("first settle", a, r1, 40, true)
	check("after first settle", 40, 0)
	settleWant("second settle", a, r1, 40, false)
	check("after second settle", 40, 0)

	r2 := mustReserve(a, 100)
	settleWant("another hospital's settle", b, r2, 10, false)
	check("after another hospital's settle", 40, 100)
	settleWant("settle above the reservation", a, r2, 500, true)
	check("after settling above the reservation", 140, 0)

	r3 := mustReserve(a, 50)
	settleWant("negative used", a, r3, -5, true)
	check("after negative used", 140, 0)

	r4 := mustReserve(a, 30)
	settleWant("unknown reply", a, r4, nil, true)
	check("after unknown reply", 170, 0)

	settleWant("an id never reserved", a, uuid.New(), 1, false)
	settleWant("a null id", a, nil, 1, false)
	check("after bogus settles", 170, 0)

	// With no hospital set, nothing settles.
	r5 := mustReserve(a, 10)
	var got bool
	err := commitAs(ctx, pool, "mhai_worker", nil, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mhai_cloud_settle($1, 1)`, r5).Scan(&got)
	})
	if err != nil || got {
		t.Errorf("settle with no hospital: %v %v, want false", got, err)
	}
	check("after a settle with no hospital", 170, 10)
	if used, res := ledger(t, pool, b); used != 0 || res != 0 {
		t.Errorf("hospital b's ledger moved: used %d reserved %d", used, res)
	}

	// The platform reads this month's usage through its function only.
	var used, res int64
	err = db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT used, reserved FROM mhai_cloud_usage($1)
			WHERE month = date_trunc('month', now() AT TIME ZONE 'UTC')::date`, a).Scan(&used, &res)
	})
	if err != nil || used != 170 || res != 10 {
		t.Errorf("platform usage: used %d reserved %d err %v, want 170 and 10", used, res, err)
	}
	c, _ := seedHospital(t, pool, uniqueCode("SC"))
	err = db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT used, reserved FROM mhai_cloud_usage($1)`, c).Scan(&used, &res)
	})
	if err != nil || used != 0 || res != 0 {
		t.Errorf("platform usage with no ledger row: used %d reserved %d err %v, want zeros", used, res, err)
	}
}

// A14 and Review Focus 5: the route stays cloud only while the hospital's
// policy still names that provider, for the caller's own hospital.
func TestEffectiveRoute(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("ER"))
	b, _ := seedHospital(t, pool, uniqueCode("ES"))
	setPolicy(t, pool, a, ptr("openai"), 1000)
	setPolicy(t, pool, b, ptr("openai"), 1000)
	q := gatewayInsert(t, pool, a, "")
	if r := routeOf(t, pool, q); r != "cloud:openai" {
		t.Fatalf("inserted route %q, want cloud:openai", r)
	}
	claim := func(what, want string) {
		t.Helper()
		var got string
		err := commitAs(ctx, pool, "mhai_worker", &a, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT mhai_effective_model_route(hospital_id, model_route) FROM clinical_queries WHERE id = $1`, q).Scan(&got)
		})
		if err != nil || got != want {
			t.Errorf("%s: effective route %q err %v, want %q", what, got, err, want)
		}
	}
	effective := func(what string, hospital *uuid.UUID, h uuid.UUID, route any, want string) {
		t.Helper()
		var got string
		err := commitAs(ctx, pool, "mhai_worker", hospital, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT mhai_effective_model_route($1, $2)`, h, route).Scan(&got)
		})
		if err != nil || got != want {
			t.Errorf("%s: %q err %v, want %q", what, got, err, want)
		}
	}
	claim("while openai", "cloud:openai")
	setPolicy(t, pool, a, nil, 1000)
	claim("policy switched off", "local")
	setPolicy(t, pool, a, ptr("anthropic"), 1000)
	claim("policy switched to anthropic", "local")
	setPolicy(t, pool, a, ptr("openai"), 1000)
	claim("back to openai", "cloud:openai")
	effective("another hospital's id", &a, b, "cloud:openai", "local")
	effective("no hospital set", nil, a, "cloud:openai", "local")
	effective("local stays local", &a, a, "local", "local")
	effective("an unknown route", &a, a, "cloud:elsewhere", "local")
	effective("a null route", &a, a, nil, "local")
	if _, err := pool.Exec(ctx, `DELETE FROM hospital_model_policy WHERE hospital_id = $1`, a); err != nil {
		t.Fatal(err)
	}
	claim("policy row gone", "local")
}

// The policy's own checks, whoever writes.
func TestPolicyChecks(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("PC"))
	ins := `INSERT INTO hospital_model_policy (hospital_id, cloud_provider, agreement_ref, monthly_token_budget) VALUES ($1, $2, $3, $4)`
	for what, args := range map[string][]any{
		"unknown provider":           {a, "gemini", "SYNTH-1", 1000},
		"lower-case agreement_ref":   {a, "openai", "synth-1", 1000},
		"agreement_ref with a space": {a, "openai", "SYNTH 1", 1000},
		"agreement_ref too short":    {a, "openai", "AB", 1000},
		"agreement_ref too long":     {a, "openai", strings.Repeat("A", 65), 1000},
		"provider without ref":       {a, "openai", nil, 1000},
		"budget 0":                   {a, "openai", "SYNTH-1", 0},
		"negative budget":            {a, nil, nil, -1},
	} {
		if _, err := pool.Exec(ctx, ins, args...); pgCode(err) != "23514" {
			t.Errorf("%s: want 23514, got %v", what, err)
		}
	}
	if _, err := pool.Exec(ctx, ins, uuid.New(), nil, nil, 1000); pgCode(err) != "23503" {
		t.Errorf("unknown hospital: want 23503, got %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO hospital_model_policy (hospital_id) VALUES ($1)`, a); err != nil {
		t.Fatalf("local policy with defaults: %v", err)
	}
	var budget int64
	if err := pool.QueryRow(ctx, `SELECT monthly_token_budget FROM hospital_model_policy WHERE hospital_id = $1`, a).Scan(&budget); err != nil || budget != 2_000_000 {
		t.Errorf("default budget %d (%v), want 2000000", budget, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hospital_model_policy SET cloud_provider = 'xai', agreement_ref = 'DEMO-SYNTHETIC' WHERE hospital_id = $1`, a); err != nil {
		t.Errorf("valid policy: %v", err)
	}

	// The platform and the provisioning CLI write it; the platform reads usage.
	err := commitAs(ctx, pool, "mhai_platform", nil, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE hospital_model_policy SET cloud_provider = 'openai', monthly_token_budget = 5000, updated_at = now() WHERE hospital_id = $1`, a)
		return err
	})
	if err != nil {
		t.Errorf("platform update: %v", err)
	}
	b, _ := seedHospital(t, pool, uniqueCode("PD"))
	err = commitAs(ctx, pool, "mhai_provision", nil, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO hospital_model_policy (hospital_id, cloud_provider, agreement_ref) VALUES ($1, 'anthropic', 'DEMO-SYNTHETIC')
			ON CONFLICT (hospital_id) DO UPDATE SET cloud_provider = EXCLUDED.cloud_provider, agreement_ref = EXCLUDED.agreement_ref`, b); err != nil {
			return err
		}
		var p string
		return tx.QueryRow(ctx, `SELECT cloud_provider FROM hospital_model_policy WHERE hospital_id = $1`, b).Scan(&p)
	})
	if err != nil {
		t.Errorf("provision upsert: %v", err)
	}
	for _, role := range []string{"mhai_platform", "mhai_provision"} {
		_, err := inTx(ctx, pool, []string{`SET LOCAL ROLE ` + role}, nil, `DELETE FROM hospital_model_policy WHERE hospital_id = $1`, a)
		requireDenied(t, err, role+" deleting a policy")
		_, err = inTx(ctx, pool, []string{`SET LOCAL ROLE ` + role}, nil, `UPDATE hospital_model_policy SET hospital_id = $2 WHERE hospital_id = $1`, a, b)
		requireDenied(t, err, role+" moving a policy to another hospital")
	}
}

const auditInsert = `INSERT INTO audit_log (hospital_id, actor_id, action, entity, payload, prev_hash, hash) VALUES ($1, $2, $3, 'hospital', '{}', 'p', $4)`

// K3: mhai_platform may append hospital.model_policy_changed with no
// hospital, and its eight actions still; nothing else.
func TestPlatformAppendAllowsModelPolicyChangedOnlyWithoutHospital(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("PA"))
	setup := []string{`SET LOCAL ROLE mhai_platform`}
	actor := uuid.New()
	if _, err := inTx(ctx, pool, setup, nil, auditInsert, nil, actor, "hospital.model_policy_changed", uuid.NewString()); err != nil {
		t.Errorf("platform appending hospital.model_policy_changed: %v", err)
	}
	if _, err := inTx(ctx, pool, setup, nil, auditInsert, nil, actor, "job.cancelled", uuid.NewString()); err != nil {
		t.Errorf("platform appending job.cancelled (00014): %v", err)
	}
	_, err := inTx(ctx, pool, setup, nil, auditInsert, a, actor, "hospital.model_policy_changed", uuid.NewString())
	requireDenied(t, err, "platform appending hospital.model_policy_changed with a hospital")
	for _, action := range []string{"hospital.created", "hospital.activated", "hospital.model_policy_changed_x", "query.completed"} {
		_, err := inTx(ctx, pool, setup, nil, auditInsert, nil, actor, action, uuid.NewString())
		requireDenied(t, err, "platform appending "+action)
	}
}

// K3: mhai_provision may append it with no actor, and its four actions
// still; nothing else.
func TestProvisionAppendAllowsItOnlyWithoutActor(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	setup := []string{`SET LOCAL ROLE mhai_provision`}
	for _, action := range []string{"hospital.model_policy_changed", "hospital.activated", "intelligence.key_rotated"} {
		if _, err := inTx(ctx, pool, setup, nil, auditInsert, nil, nil, action, uuid.NewString()); err != nil {
			t.Errorf("provision appending %s: %v", action, err)
		}
	}
	_, err := inTx(ctx, pool, setup, nil, auditInsert, nil, uuid.New(), "hospital.model_policy_changed", uuid.NewString())
	requireDenied(t, err, "provision appending hospital.model_policy_changed with an actor")
	for _, action := range []string{"audit.viewed", "job.cancelled", "login.ok"} {
		_, err := inTx(ctx, pool, setup, nil, auditInsert, nil, nil, action, uuid.NewString())
		requireDenied(t, err, "provision appending "+action)
	}
}

// 00018 goes down to 00017 and up again: everything it added is gone, and
// the two audit policies are 00014's and 00010's exactly.
func TestMigration00018DownUp(t *testing.T) {
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
	policyChecks := func() map[string]string {
		t.Helper()
		got := map[string]string{}
		rows, err := pool.Query(ctx, `SELECT policyname, with_check FROM pg_policies WHERE tablename = 'audit_log' AND policyname IN ('platform_append', 'provision_append')`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var name, check string
			if err := rows.Scan(&name, &check); err != nil {
				t.Fatal(err)
			}
			got[name] = check
		}
		return got
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 17); err != nil {
		t.Fatalf("down to 17: %v", err)
	}
	before := policyChecks()
	var left int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_class WHERE relname IN ('hospital_model_policy', 'cloud_usage', 'cloud_reservations'))
		+ (SELECT count(*) FROM pg_attribute WHERE attrelid = 'clinical_queries'::regclass AND attname = 'model_route' AND NOT attisdropped)
		+ (SELECT count(*) FROM pg_trigger WHERE tgname = 'clinical_queries_model_route')
		+ (SELECT count(*) FROM pg_proc WHERE proname IN ('mhai_set_model_route', 'mhai_effective_model_route',
		       'mhai_cloud_reserve', 'mhai_cloud_settle', 'mhai_cloud_usage'))`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("after down, %d tables, columns, triggers or functions of 00018 remain", left)
	}
	for name, check := range before {
		if strings.Contains(check, "model_policy_changed") {
			t.Errorf("after down, %s still admits hospital.model_policy_changed: %s", name, check)
		}
	}
	if !strings.Contains(before["platform_append"], "job.cancelled") || !strings.Contains(before["platform_append"], "hospital_id IS NULL") {
		t.Errorf("after down, platform_append is not 00014's: %s", before["platform_append"])
	}
	if !strings.Contains(before["provision_append"], "hospital.activated") || !strings.Contains(before["provision_append"], "actor_id IS NULL") {
		t.Errorf("after down, provision_append is not 00010's: %s", before["provision_append"])
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
	after := policyChecks()
	for name, check := range after {
		if !strings.Contains(check, "hospital.model_policy_changed") {
			t.Errorf("after up, %s does not admit hospital.model_policy_changed: %s", name, check)
		}
		// The rest of the list is the one Down restored.
		if strings.Replace(check, ", 'hospital.model_policy_changed'::text", "", 1) != before[name] {
			t.Errorf("after up, %s differs from Down's beyond the new action:\n up:   %s\n down: %s", name, check, before[name])
		}
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
}
