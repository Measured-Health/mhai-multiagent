//go:build integration

package db_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00027 (EMR plan Tasks 26, 28-30; design §11.3, §12.4, §15.3,
// §15.4): findings, the breaker's trips and pause, a hospital's activated
// rule set, the EMR job types, and the sources' status.

// seedFinding inserts an open critical finding of patient on obs, with
// dedup key key, as the superuser.
func seedFinding(t *testing.T, pool *pgxpool.Pool, hospital, patient, obs uuid.UUID, key string) uuid.UUID {
	t.Helper()
	id, err := insertFinding(context.Background(), pool, hospital, patient, obs, key)
	if err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	return id
}

type execer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func insertFinding(ctx context.Context, q execer, hospital, patient, obs uuid.UUID, key string) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `INSERT INTO emr.findings (hospital_id, patient_id, rule_set_version, rule_set_sha256, rule_id,
		kind, category, severity, primary_observation_id, effective_at, received_at, status, shadow, preliminary,
		arrived_late, standardised, dedup_key)
		VALUES ($1, $2, '1', $3, 'potassium-critical-high', 'threshold', 'blood_chemistry', 'critical', $4, now(), now(),
		        'open', true, false, false, true, $5) RETURNING id`, hospital, patient, emrSHA[:], obs, key).Scan(&id)
	return id, err
}

// A19 (database): one finding per dedup key and hospital.
func TestFindingDedupKeyUnique(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("FD"))
	c := seedConn(t, pool, h)
	p := seedPatient(t, pool, h, c, "p-1", "MRN-1")
	o := seedObservation(t, pool, h, c, p, "p-1")
	seedFinding(t, pool, h, p, o, "1:k:"+o.String())
	if _, err := insertFinding(ctx, pool, h, p, o, "1:k:"+o.String()); pgCode(err) != uniqueViolation {
		t.Fatalf("a second finding with the same key: %v", err)
	}
	// As the emr service, the conflict is a no-op.
	if err := asRoleIn(ctx, pool, "mhai_emr", h, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO emr.findings (hospital_id, patient_id, rule_set_version, rule_set_sha256, rule_id,
			kind, category, severity, primary_observation_id, effective_at, received_at, status, shadow, preliminary,
			arrived_late, standardised, dedup_key)
			VALUES ($1, $2, '1', $3, 'potassium-critical-high', 'threshold', 'blood_chemistry', 'critical', $4, now(), now(),
			        'open', true, false, false, true, $5) ON CONFLICT (hospital_id, dedup_key) DO NOTHING`,
			h, p, emrSHA[:], o, "1:k:"+o.String())
		if err == nil && tag.RowsAffected() != 0 {
			t.Error("the conflicting insert wrote a row")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A date-only value is never fresh: such a finding must be historical.
	if _, err := pool.Exec(ctx, `INSERT INTO emr.findings (hospital_id, patient_id, rule_set_version, rule_set_sha256, rule_id,
		kind, category, severity, primary_observation_id, effective_date, received_at, status, shadow, preliminary,
		arrived_late, standardised, dedup_key)
		VALUES ($1, $2, '1', $3, 'x', 'threshold', 'vital_signs', 'critical', $4, current_date, now(), 'open', true, false,
		        false, true, 'date-only')`, h, p, emrSHA[:], o); pgCode(err) != "23514" {
		t.Fatalf("a date-only open finding: %v", err)
	}
}

// A13: findings are their hospital's only, for the emr service and core's
// consumer; the gateway reads none; the emr service can't delete or
// rewrite one.
func TestFindingsRLS(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("FA"))
	b, _ := seedHospital(t, pool, uniqueCode("FB"))
	ca, cb := seedConn(t, pool, a), seedConn(t, pool, b)
	pa, pb := seedPatient(t, pool, a, ca, "p-a", "MRN-A"), seedPatient(t, pool, b, cb, "p-b", "MRN-B")
	oa, ob := seedObservation(t, pool, a, ca, pa, "p-a"), seedObservation(t, pool, b, cb, pb, "p-b")
	fa := seedFinding(t, pool, a, pa, oa, "a")
	seedFinding(t, pool, b, pb, ob, "b")
	for _, role := range []string{"mhai_emr", "mhai_emr_core"} {
		var ids []uuid.UUID
		if err := asRoleIn(ctx, pool, role, a, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT id FROM emr.findings`)
			if err != nil {
				return err
			}
			ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			return err
		}); err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		if len(ids) != 1 || ids[0] != fa {
			t.Errorf("%s sees %v, want only its hospital's finding", role, ids)
		}
	}
	// Core's consumer reads the evidence of its own hospital only.
	if err := asRoleIn(ctx, pool, "mhai_emr_core", a, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM emr.observations WHERE id IN ($1, $2)`, oa, ob).Scan(&n); err != nil || n != 1 {
			t.Errorf("core reads %d observations (%v), want its own", n, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Core's consumer can't read the raw payloads or write a finding.
	for _, q := range []string{`SELECT count(*) FROM emr.raw_records`, `SELECT dedup_key FROM emr.findings`,
		`UPDATE emr.findings SET status = 'withdrawn'`, `SELECT payload FROM emr.raw_records`} {
		err := asRoleIn(ctx, pool, "mhai_emr_core", a, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q)
			return err
		})
		requireDenied(t, err, "mhai_emr_core: "+q)
	}
	// The emr service: no delete, and no change to what a finding says.
	for _, q := range []string{`DELETE FROM emr.findings`, `UPDATE emr.findings SET severity = 'info'`,
		`UPDATE emr.findings SET shadow = false`, `UPDATE emr.findings SET hospital_id = '` + b.String() + `'`} {
		err := asRoleIn(ctx, pool, "mhai_emr", a, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q)
			return err
		})
		requireDenied(t, err, "mhai_emr: "+q)
	}
	// A finding of another hospital's observation, written as hospital A,
	// fails the key (and the policy).
	err := asRoleIn(ctx, pool, "mhai_emr", a, func(tx pgx.Tx) error {
		_, err := insertFinding(ctx, tx, a, pa, ob, "cross")
		return err
	})
	if err == nil {
		t.Fatal("a finding on another hospital's observation was written")
	}
	// The gateway, core's clinical role, holds nothing on findings.
	err = asRoleIn(ctx, pool, "mhai_gateway", a, func(tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT count(*) FROM emr.findings`).Scan(&n)
	})
	requireDenied(t, err, "gateway reading findings")
}

// Departure 12: EMR dead letters are jobs rows of the two stages.
func TestJobsTypeAllowsEMRStages(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("JT"))
	for typ, ok := range map[string]bool{"emr_normalise": true, "emr_rules": true, "emr_fetch": false, "anything": false} {
		_, err := pool.Exec(ctx, `INSERT INTO jobs (id, hospital_id, type, subject_id, request_id, status)
			VALUES ($1, $2, $3, $4, 'req-synthetic', 'failed')`, uuid.New(), h, typ, uuid.New())
		if (err == nil) != ok {
			t.Errorf("job type %s: %v", typ, err)
		}
	}
}

// Exclusion (design §10.6) takes the patient's findings with its
// observations; a merge moves them to the kept patient.
func TestFindingsFollowExclusionAndMerge(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("FX"))
	c := seedConn(t, pool, h)
	gone := seedPatient(t, pool, h, c, "p-gone", "MRN-G")
	p := seedPatient(t, pool, h, c, "p-x", "MRN-X")
	fx := seedFinding(t, pool, h, p, seedObservation(t, pool, h, c, p, "p-x"), "x")
	if err := asRoleIn(ctx, pool, "mhai_gateway", h, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT * FROM emr_exclude_patient($1, 'objection')`, p)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM emr.findings WHERE id = $1`, fx).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the excluded patient's finding remains: %d %v", n, err)
	}
	keep := seedPatient(t, pool, h, c, "p-keep", "MRN-K")
	fg := seedFinding(t, pool, h, gone, seedObservation(t, pool, h, c, gone, "p-gone"), "g")
	m := seedMatch(t, pool, h, keep, gone)
	if err := asRoleIn(ctx, pool, "mhai_gateway", h, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT emr_merge_patients($1, 'a')`, m)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var owner uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT patient_id FROM emr.findings WHERE id = $1`, fg).Scan(&owner); err != nil || owner != keep {
		t.Fatalf("after the merge the finding is %v's (%v), want the kept patient's", owner, err)
	}
}

// Design §12.4: the emr service pauses an enabled source of its own
// hospital only, through the definer; it can't switch anything else.
func TestEMRPauseConnection(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("PC"))
	o, _ := seedHospital(t, pool, uniqueCode("PO"))
	c := seedConn(t, pool, h)
	pause := func(as, conn uuid.UUID) (bool, error) {
		var changed bool
		err := asRoleIn(ctx, pool, "mhai_emr", as, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT emr_pause_connection($1)`, conn).Scan(&changed)
		})
		return changed, err
	}
	if changed, err := pause(h, c); err != nil || changed {
		t.Fatalf("a switched-off source paused: %v %v", changed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET enabled = true WHERE id = $1`, c); err != nil {
		t.Fatal(err)
	}
	if changed, err := pause(o, c); err != nil || changed {
		t.Fatalf("another hospital paused it: %v %v", changed, err)
	}
	if changed, err := pause(h, c); err != nil || !changed {
		t.Fatalf("own source: %v %v", changed, err)
	}
	if changed, err := pause(h, c); err != nil || changed {
		t.Fatalf("paused twice: %v %v", changed, err)
	}
	err := asRoleIn(ctx, pool, "mhai_emr", h, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE emr_connections SET paused = false WHERE id = $1`, c)
		return err
	})
	requireDenied(t, err, "the emr service resuming a source")
	for _, role := range []string{"mhai_gateway", "mhai_platform", "mhai_emr_core", "mhai_emr_sched"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT has_function_privilege($1, 'emr_pause_connection(uuid)', 'EXECUTE')`, role).Scan(&ok); err != nil || ok {
			t.Errorf("%s may pause sources: %v %v", role, ok, err)
		}
	}
}

// A23/A42 (database): a switched-on source is behind when paused, at once,
// or when it hasn't polled successfully for three intervals; the platform
// sees a paused source as errors at once. Only the caller's hospital's
// sources, labels and ids only.
func TestSourcesStatusBehindWhenStaleOrPaused(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("SS"))
	o, _ := seedHospital(t, pool, uniqueCode("SO"))
	c := seedConn(t, pool, h)
	off := seedConn(t, pool, h)
	other := seedConn(t, pool, o)
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET enabled = true, poll_seconds = 120 WHERE id IN ($1, $2)`, c, other); err != nil {
		t.Fatal(err)
	}
	type row struct {
		id     uuid.UUID
		status string
	}
	status := func() []row {
		var out []row
		if err := asRoleIn(ctx, pool, "mhai_gateway", h, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT connection_id, status FROM emr_sources_status()`)
			if err != nil {
				return err
			}
			out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
				var x row
				return x, r.Scan(&x.id, &x.status)
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := status(); len(got) != 1 || got[0].id != c || got[0].status != "behind" {
		t.Fatalf("never polled: %+v (want only the switched-on source, behind)", got)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO emr.sync_runs (hospital_id, connection_id, started_at, finished_at, ok, records, outcome)
		VALUES ($1, $2, now() - interval '1 minute', now() - interval '1 minute', true, 1, 'ok')`, h, c); err != nil {
		t.Fatal(err)
	}
	if got := status(); got[0].status != "current" {
		t.Fatalf("polled a minute ago: %+v", got)
	}
	health := func() string {
		var s string
		if err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT health FROM emr_platform_health() WHERE connection_id = $1`, c).Scan(&s)
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if got := health(); got != "ok" {
		t.Fatalf("platform health %q", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET paused = true WHERE id = $1`, c); err != nil {
		t.Fatal(err)
	}
	if got := status(); got[0].status != "behind" {
		t.Fatalf("paused: %+v, want behind at once", got)
	}
	if got := health(); got != "errors" {
		t.Fatalf("paused: platform health %q, want errors at once", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET paused = false WHERE id = $1`, c); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE emr.sync_runs SET finished_at = now() - interval '7 minutes' WHERE connection_id = $1`, c); err != nil {
		t.Fatal(err)
	}
	if got := status(); got[0].status != "behind" {
		t.Fatalf("7 minutes at 120 s: %+v, want behind", got)
	}
	_ = off
	for _, role := range []string{"mhai_emr", "mhai_platform", "mhai_emr_core", "mhai_worker"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT has_function_privilege($1, 'emr_sources_status()', 'EXECUTE')`, role).Scan(&ok); err != nil || ok {
			t.Errorf("%s may read the sources' status: %v %v", role, ok, err)
		}
	}
}

// The activation: the platform writes it; the emr service reads its own
// hospital's; nobody else reads it.
func TestRuleSetActivationGrants(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("RA"))
	b, _ := seedHospital(t, pool, uniqueCode("RB"))
	if err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		for _, h := range []uuid.UUID{a, b} {
			if _, err := tx.Exec(ctx, `INSERT INTO emr_rule_set_activation (hospital_id, version, sha256) VALUES ($1, '1', $2)`,
				h, emrSHA[:]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var versions []string
	if err := asRoleIn(ctx, pool, "mhai_emr", a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT version FROM emr_rule_set_activation`)
		if err != nil {
			return err
		}
		versions, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil || len(versions) != 1 {
		t.Fatalf("emr reads %v (%v), want its hospital's only", versions, err)
	}
	for _, q := range []string{`UPDATE emr_rule_set_activation SET version = '2'`, `DELETE FROM emr_rule_set_activation`,
		`SELECT activated_by FROM emr_rule_set_activation`} {
		err := asRoleIn(ctx, pool, "mhai_emr", a, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q)
			return err
		})
		requireDenied(t, err, "emr: "+q)
	}
	for _, role := range []string{"mhai_gateway", "mhai_emr_core", "mhai_worker"} {
		err := asRoleIn(ctx, pool, role, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT count(*) FROM emr_rule_set_activation`)
			return err
		})
		requireDenied(t, err, role+" reading the activation")
	}
	for _, bad := range []string{"Version One", ""} {
		if _, err := pool.Exec(ctx, `UPDATE emr_rule_set_activation SET version = $2 WHERE hospital_id = $1`, a, bad); err == nil {
			t.Errorf("version %q accepted", bad)
		}
	}
}

// Every 00027 table: RLS on and forced, owner mhai_migrate; the definers'
// rules (owner, pinned search_path, EXECUTE only as granted).
func TestEMRFindingsObjects(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for _, table := range []string{"emr.findings", "emr.breaker_trips", "emr_rule_set_activation"} {
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
	for name, want := range map[string]string{"emr_pause_connection": "mhai_emr", "emr_sources_status": "mhai_gateway"} {
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
		slices.Sort(grantees)
		if owner != "mhai_migrate" || !definer || len(config) != 1 || config[0] != "search_path=pg_catalog, pg_temp" ||
			strings.Join(grantees, ",") != want {
			t.Errorf("%s: owner %s definer %v config %v EXECUTE %v", name, owner, definer, config, grantees)
		}
	}
}

// 00027 goes down to the version before it (EMR PR 6's, 00025 or 00026 once renumbered) and up again,
// leaving nothing behind.
func TestMigration00027DownUp(t *testing.T) {
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
	if err := db.MigrateDownTo(ctx, pg.URL, 26); err != nil {
		t.Fatalf("down to 26: %v", err)
	}
	var left int
	var usage bool
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE (n.nspname = 'emr' AND c.relname IN ('findings', 'breaker_trips')) OR c.relname = 'emr_rule_set_activation')
		+ (SELECT count(*) FROM pg_proc WHERE proname IN ('emr_pause_connection', 'emr_sources_status'))
		+ (SELECT count(*) FROM pg_policy WHERE polname IN ('owner_pause', 'core_read'))`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT has_schema_privilege('mhai_emr_core', 'emr', 'USAGE')`).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if left != 0 || usage {
		t.Fatalf("after down: %d objects left, core usage %v", left, usage)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO hospitals (id, name, short_code, status) VALUES (gen_random_uuid(), 'Synthetic', 'DU26', 'active')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (id, hospital_id, type, subject_id, status)
		SELECT gen_random_uuid(), id, 'emr_rules', gen_random_uuid(), 'failed' FROM hospitals WHERE short_code = 'DU26'`); err == nil {
		t.Fatal("after down, an emr_rules job row was accepted")
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
