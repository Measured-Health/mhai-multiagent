//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00026 (EMR plan Tasks 23-24): the scheduler's state per
// connection (cursor, due time, failure counts), the backfill flag on raw
// records, and platform health that turns to errors when a connection on
// goes silent (design §15.4: a stale source never looks fresh).

// The emr role keeps its hospital's sync state; the scheduler's role reads
// ids, due times and the sign-in count across hospitals, never the cursor;
// no other role holds anything.
func TestEMRSyncStateGrants(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("SA"))
	b, _ := seedHospital(t, pool, uniqueCode("SB"))
	ca, cb := seedConn(t, pool, a), seedConn(t, pool, b)
	if n, err := asEMR(ctx, pool, &a, `INSERT INTO emr.sync_state (connection_id, hospital_id, cursor) VALUES ($1, $2, '{"v":1}')`, ca, a); err != nil || n != 1 {
		t.Fatalf("emr insert own: %d %v", n, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO emr.sync_state (connection_id, hospital_id) VALUES ($1, $2), ($3, $4)`, ca, a, cb, b); err != nil {
		t.Fatal(err)
	}
	if _, err := asEMR(ctx, pool, &a, `INSERT INTO emr.sync_state (connection_id, hospital_id) VALUES ($1, $2)`, cb, b); err == nil {
		t.Fatal("emr wrote another hospital's sync state")
	}
	// A connection of another hospital under this hospital's id: the key
	// refuses it.
	if _, err := asEMR(ctx, pool, &a, `INSERT INTO emr.sync_state (connection_id, hospital_id) VALUES ($1, $2)`, cb, a); err == nil {
		t.Fatal("sync state for another hospital's connection")
	}
	if n, err := asEMR(ctx, pool, &a, `UPDATE emr.sync_state SET next_due_at = now(), failures = 1 WHERE connection_id IN ($1, $2)`, ca, cb); err != nil || n != 1 {
		t.Fatalf("emr update: %d rows %v, want its own only", n, err)
	}
	if _, err := asEMR(ctx, pool, &a, `UPDATE emr.sync_state SET hospital_id = $2 WHERE connection_id = $1`, ca, b); err == nil {
		t.Fatal("emr moved a sync state row")
	}
	if _, err := asEMR(ctx, pool, &a, `DELETE FROM emr.sync_state WHERE connection_id = $1`, ca); err == nil {
		t.Fatal("emr deleted sync state")
	}
	sched := []string{`SET LOCAL ROLE mhai_emr_sched`}
	if _, err := inTx(ctx, pool, sched, nil, `SELECT connection_id, hospital_id, next_due_at, sign_in_failures FROM emr.sync_state`); err != nil {
		t.Fatalf("sched read: %v", err)
	}
	for _, q := range []string{`SELECT cursor FROM emr.sync_state`, `UPDATE emr.sync_state SET next_due_at = now()`,
		`SELECT payload FROM emr.raw_records`} {
		if _, err := inTx(ctx, pool, sched, nil, q); err == nil {
			t.Errorf("sched: %s allowed", q)
		}
	}
	holds := func(role string) bool {
		var any bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege($1, 'emr.sync_state', 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
			OR has_any_column_privilege($1, 'emr.sync_state', 'SELECT,INSERT,UPDATE,REFERENCES')`, role).Scan(&any); err != nil {
			t.Fatal(err)
		}
		return any
	}
	for _, role := range []string{"mhai_gateway", "mhai_auth", "mhai_worker", "mhai_jobs", "mhai_platform", "mhai_app", "mhai_emr_app",
		"mhai_emr_core", "mhai_emr_admin", "public"} {
		if holds(role) {
			t.Errorf("%s holds a privilege on emr.sync_state", role)
		}
	}
	// The emr role reads its hospital's time zone (the reconciliation hour).
	if _, err := asEMR(ctx, pool, &a, `SELECT time_zone FROM hospitals`); err != nil {
		t.Fatalf("emr time zone: %v", err)
	}
	// The backfill flag on a raw record, written by the emr role.
	if n, err := asEMR(ctx, pool, &a, `INSERT INTO emr.raw_records (hospital_id, source_kind, connection_id, resource_kind, source_id,
		payload, payload_sha256, connector_type, connector_version, backfill)
		VALUES ($1, 'connector', $2, 'observation', 'o-bf', '{}'::jsonb, $3, 'fhir_r4', 1, true)`, a, ca, make([]byte, 32)); err != nil || n != 1 {
		t.Fatalf("backfill insert: %d %v", n, err)
	}
	if _, err := asEMR(ctx, pool, &a, `UPDATE emr.raw_records SET backfill = false WHERE hospital_id = $1`, a); err == nil {
		t.Fatal("emr changed a raw record's backfill flag")
	}
}

// Design §15.4: a connection that is on and has had no successful poll for
// three poll intervals is errors, even when its last runs were ok.
func TestPlatformHealthStaleIsErrors(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("SH"))
	c := seedConn(t, pool, h)
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET enabled = true, poll_seconds = 120 WHERE id = $1`, c); err != nil {
		t.Fatal(err)
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
	if _, err := pool.Exec(ctx, `INSERT INTO emr.sync_runs (hospital_id, connection_id, started_at, finished_at, ok, records, outcome)
		VALUES ($1, $2, now() - interval '5 minutes', now() - interval '5 minutes', true, 1, 'ok')`, h, c); err != nil {
		t.Fatal(err)
	}
	if got := health(); got != "ok" {
		t.Fatalf("5 minutes since an ok poll at 120 s: %q, want ok", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE emr.sync_runs SET finished_at = now() - interval '7 minutes' WHERE connection_id = $1`, c); err != nil {
		t.Fatal(err)
	}
	if got := health(); got != "errors" {
		t.Fatalf("7 minutes since an ok poll at 120 s: %q, want errors", got)
	}
	// Off: not polled, so not stale.
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET enabled = false WHERE id = $1`, c); err != nil {
		t.Fatal(err)
	}
	if got := health(); got != "ok" {
		t.Fatalf("off: %q, want ok", got)
	}
}

// 00026 goes down (to 25: whatever is below it stays) and up again.
func TestMigration00026DownUp(t *testing.T) {
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
	if err := db.MigrateDownTo(ctx, pg.URL, 25); err != nil {
		t.Fatalf("down to 25: %v", err)
	}
	var left int
	var usage, tz bool
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'emr' AND c.relname = 'sync_state')
		+ (SELECT count(*) FROM pg_attribute WHERE attrelid = 'emr.raw_records'::regclass AND attname = 'backfill' AND NOT attisdropped)`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT has_schema_privilege('mhai_emr_sched', 'emr', 'USAGE'),
		has_column_privilege('mhai_emr', 'hospitals', 'time_zone', 'SELECT')`).Scan(&usage, &tz); err != nil {
		t.Fatal(err)
	}
	if left != 0 || usage || tz {
		t.Fatalf("after down: %d objects left, sched usage %v, emr time zone %v", left, usage, tz)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
