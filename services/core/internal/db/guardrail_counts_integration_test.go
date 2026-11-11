//go:build integration

package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00017 (Plan 4 Task 13): mhai_guardrail_counts' body and its
// reader. Every id and string is synthetic.

// seedDecision adds a decision of hospital h, ago before now (a Postgres
// interval), as the superuser.
func seedDecision(t *testing.T, h uuid.UUID, decision *string, degraded bool, ago string) {
	t.Helper()
	if _, err := sharedPool.Exec(context.Background(), `INSERT INTO clinical_decisions (id, hospital_id, hga_decision, degraded, response_text, created_at)
		VALUES (gen_random_uuid(), $1, $2, $3, 'synthetic answer', now() - $4::interval)`, h, decision, degraded, ago); err != nil {
		t.Fatal(err)
	}
}

type counts struct{ answers, pass, regenerate, escalate, degraded int64 }

// asOwner runs fn in a transaction of the migrating superuser: 00019
// revoked mhai_guardrail_counts from mhai_platform (Plan 7c, A30), and the
// body these tests pin is still 00017's.
func asOwner(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, sharedPool, fn)
}

// guardrailCounts runs the function over the last days.
func guardrailCounts(t *testing.T, days int) map[uuid.UUID]counts {
	t.Helper()
	ctx := context.Background()
	out := map[uuid.UUID]counts{}
	err := asOwner(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT hospital_id, answers, pass, regenerate, escalate, degraded
			FROM mhai_guardrail_counts(now() - make_interval(days => $1), now())`, days)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var c counts
			if err := rows.Scan(&id, &c.answers, &c.pass, &c.regenerate, &c.escalate, &c.degraded); err != nil {
				return err
			}
			out[id] = c
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func strp(s string) *string { return &s }

// Per active hospital: answers, PASS, REGENERATE, the rest as escalate,
// degraded; inside the window only; zeros for a quiet hospital; nothing
// for a hospital that isn't active.
func TestGuardrailCountsFunction(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("G"))
	quiet, _ := seedHospital(t, pool, uniqueCode("G"))
	gone, _ := seedHospital(t, pool, uniqueCode("G"))
	if _, err := pool.Exec(ctx, `UPDATE hospitals SET status = 'deactivated' WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		seedDecision(t, a, strp("PASS"), false, "1 hour")
	}
	seedDecision(t, a, strp("REGENERATE"), true, "2 days")
	seedDecision(t, a, strp("ESCALATE"), true, "3 days")
	seedDecision(t, a, nil, false, "3 days")              // no decision: escalate
	seedDecision(t, a, strp("SOMETHING"), false, "1 day") // unknown: escalate
	seedDecision(t, a, strp("PASS"), false, "10 days")    // outside 7 days
	seedDecision(t, a, strp("PASS"), false, "100 days")   // outside every window
	seedDecision(t, gone, strp("PASS"), false, "1 hour")

	got := guardrailCounts(t, 7)
	if c := got[a]; c != (counts{7, 3, 1, 3, 2}) {
		t.Errorf("7 days: %+v", c)
	}
	if c, ok := got[quiet]; !ok || c != (counts{}) {
		t.Errorf("quiet hospital: %+v %v", c, ok)
	}
	if _, ok := got[gone]; ok {
		t.Error("a deactivated hospital has a row")
	}
	if c := guardrailCounts(t, 30)[a]; c.answers != 8 || c.pass != 4 {
		t.Errorf("30 days: %+v", c)
	}
	if c := guardrailCounts(t, 90)[a]; c.answers != 8 {
		t.Errorf("90 days: %+v", c)
	}

	// Fix round 3: 90 days ending yesterday's midnight, as the route asks
	// in the 15 minutes after midnight UTC (R43), starts just over 91 days
	// ago and is accepted.
	if err := asOwner(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT * FROM mhai_guardrail_counts(
			date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' - interval '91 days',
			date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' - interval '1 day')`)
		return err
	}); err != nil {
		t.Errorf("90 days to yesterday's midnight: %v", err)
	}
	// The window: at most 90 days, within the last 92, not far ahead, and
	// from before to.
	for _, w := range []string{
		`NULL, now()`, `now() - interval '1 day', NULL`, `now(), now()`, `now(), now() - interval '1 day'`,
		`now() - interval '91 days', now()`, `now() - interval '95 days', now() - interval '93 days'`,
		`now(), now() + interval '2 days'`,
	} {
		err := asOwner(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT * FROM mhai_guardrail_counts(`+w+`)`)
			return err
		})
		if pgCode(err) != "22023" {
			t.Errorf("window (%s): want 22023, got %v", w, err)
		}
	}
}

// mhai_guardrail_reader: owns only the function; reads only the four
// decision columns of the last 92 days and active hospitals' id and status;
// nobody can log in as it or reach it.
func TestGuardrailReaderScoped(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("R"))
	gone, _ := seedHospital(t, pool, uniqueCode("R"))
	if _, err := pool.Exec(ctx, `UPDATE hospitals SET status = 'deactivated' WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	seedDecision(t, a, strp("PASS"), false, "1 hour")
	seedDecision(t, a, strp("PASS"), false, "100 days")
	var login, inherit, bypass bool
	var members, memberOf int
	if err := pool.QueryRow(ctx, `
		SELECT r.rolcanlogin, r.rolinherit, r.rolbypassrls,
		       (SELECT count(*) FROM pg_auth_members m WHERE m.roleid = r.oid),
		       (SELECT count(*) FROM pg_auth_members m WHERE m.member = r.oid)
		FROM pg_roles r WHERE r.rolname = 'mhai_guardrail_reader'`).Scan(&login, &inherit, &bypass, &members, &memberOf); err != nil {
		t.Fatal(err)
	}
	if login || inherit || bypass || members != 0 || memberOf != 0 {
		t.Errorf("mhai_guardrail_reader: login %v inherit %v bypassrls %v, %d members, member of %d", login, inherit, bypass, members, memberOf)
	}
	var owned []string
	var rels int
	if err := pool.QueryRow(ctx, `SELECT COALESCE(array_agg(p.proname::text), '{}') FROM pg_proc p WHERE p.proowner = 'mhai_guardrail_reader'::regrole`).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relowner = 'mhai_guardrail_reader'::regrole`).Scan(&rels); err != nil {
		t.Fatal(err)
	}
	if strings.Join(owned, ",") != "mhai_guardrail_counts" || rels != 0 {
		t.Errorf("mhai_guardrail_reader owns %v and %d relations", owned, rels)
	}
	if n, err := countAs(t, pool, "mhai_guardrail_reader", `SELECT count(*) FROM clinical_decisions WHERE hospital_id = $1`, a); err != nil || n != 1 {
		t.Errorf("reader sees %d of a recent and an old decision (%v)", n, err)
	}
	if n, err := countAs(t, pool, "mhai_guardrail_reader", `SELECT count(*) FROM hospitals WHERE id = ANY($1)`, []uuid.UUID{a, gone}); err != nil || n != 1 {
		t.Errorf("reader sees %d of an active and a deactivated hospital (%v)", n, err)
	}
	// mhai_migrate sees none of the rows the reader's policies open.
	if n, err := countAs(t, pool, "mhai_migrate", `SELECT count(*) FROM clinical_decisions WHERE hospital_id = $1`, a); err != nil || n != 0 {
		t.Errorf("mhai_migrate sees %d decisions (%v)", n, err)
	}
	for _, q := range []string{
		`SELECT count(response_text) FROM clinical_decisions`, `SELECT count(diagnosis) FROM clinical_decisions`,
		`SELECT count(masked_question) FROM clinical_decisions`, `SELECT count(query_id) FROM clinical_decisions`,
		`SELECT count(id) FROM clinical_decisions`, `SELECT count(name) FROM hospitals`,
		`SELECT count(*) FROM clinical_queries`, `SELECT count(*) FROM users`, `SELECT count(*) FROM feedback_events`,
	} {
		_, err := countAs(t, pool, "mhai_guardrail_reader", q)
		requireDenied(t, err, "mhai_guardrail_reader: "+q)
	}
}

// 00017 goes down to 00016 and up again; the stub of 00014 is back while
// it is down.
func TestMigration00017RoundTrip(t *testing.T) {
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
	if err := db.MigrateDownTo(ctx, pg.URL, 16); err != nil {
		t.Fatalf("down to 16: %v", err)
	}
	var owner string
	if err := pool.QueryRow(ctx, `SELECT pg_get_userbyid(proowner) FROM pg_proc WHERE proname = 'mhai_guardrail_counts'`).Scan(&owner); err != nil || owner != "mhai_migrate" {
		t.Errorf("the stub's owner: %s %v", owner, err)
	}
	err = db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT * FROM mhai_guardrail_counts(now() - interval '7 days', now())`)
		return err
	})
	if pgCode(err) != "0A000" {
		t.Errorf("after down: want the stub's 0A000, got %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_policies WHERE policyname = 'guardrail_read')
		+ (SELECT count(*) FROM pg_class WHERE relname = 'guardrail_snapshots')
		+ (SELECT count(*) FROM pg_indexes WHERE indexname = 'clinical_decisions_created_idx')
		+ (SELECT count(*) FROM unnest(ARRAY['clinical_decisions', 'hospitals']) t
		   WHERE has_any_column_privilege('mhai_guardrail_reader', t, 'SELECT'))
		+ (SELECT count(*) FROM pg_proc WHERE proowner = 'mhai_guardrail_reader'::regrole)`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("after down, %d policies, indexes, grants or functions of 00017 remain", left)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
}

// guardrail_snapshots (R43): mhai_platform reads and inserts, and may not
// change or delete a row; nobody else may touch it; RLS is forced; the
// period is one of three and a day has one row per period.
func TestGuardrailSnapshotsScoped(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	const day = "2001-01-01" // a day no route computes
	if _, err := pool.Exec(ctx, `DELETE FROM guardrail_snapshots WHERE day = $1::date`, day); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO guardrail_snapshots (period, day, body) VALUES ('7d', '` + day + `', '{"period": "7d"}')`
	if err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, ins); return err }); err != nil {
		t.Fatalf("platform inserting: %v", err)
	}
	err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, ins); return err })
	if pgCode(err) != "23505" {
		t.Errorf("a second row for the day: want 23505, got %v", err)
	}
	if err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, ins+` ON CONFLICT (period, day) DO NOTHING`)
		return err
	}); err != nil {
		t.Errorf("ON CONFLICT DO NOTHING: %v", err)
	}
	var n int
	if err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM guardrail_snapshots WHERE day = $1::date`, day).Scan(&n)
	}); err != nil || n != 1 {
		t.Errorf("platform reads %d rows (%v)", n, err)
	}
	for _, q := range []string{`UPDATE guardrail_snapshots SET body = '{}'`, `DELETE FROM guardrail_snapshots`, `TRUNCATE guardrail_snapshots`} {
		err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
		requireDenied(t, err, "platform: "+q)
	}
	for _, role := range []string{"mhai_gateway", "mhai_auth", "mhai_worker", "mhai_jobs", "mhai_audit_drain", "mhai_provision", "mhai_guardrail_reader"} {
		requireDenied(t, asRole(ctx, pool, role, `SELECT count(*) FROM guardrail_snapshots`), role+" reading the snapshots")
		requireDenied(t, asRole(ctx, pool, role, ins), role+" inserting a snapshot")
	}
	err = db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO guardrail_snapshots (period, day, body) VALUES ('1d', '`+day+`', '{}')`)
		return err
	})
	if pgCode(err) != "23514" {
		t.Errorf("period 1d: want 23514, got %v", err)
	}
	// No row for a day that hasn't started.
	err = db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO guardrail_snapshots (period, day, body)
			VALUES ('7d', (now() AT TIME ZONE 'UTC')::date + 1, '{}')`)
		return err
	})
	if pgCode(err) != "42501" {
		t.Errorf("a snapshot for tomorrow: want 42501 (the policy), got %v", err)
	}
	var rls, force bool
	var owner string
	if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity, pg_get_userbyid(relowner) FROM pg_class WHERE relname = 'guardrail_snapshots'`).Scan(&rls, &force, &owner); err != nil {
		t.Fatal(err)
	}
	if !rls || !force || owner != "mhai_migrate" {
		t.Errorf("rls %v force %v owner %s", rls, force, owner)
	}
}
