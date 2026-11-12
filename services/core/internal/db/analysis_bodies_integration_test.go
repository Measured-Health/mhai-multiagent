//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00025 (analysis backend plan Task 9): stored bodies and the
// request functions. Their behaviour is tested in internal/analysis; here,
// the grants and the Down.

// The request functions are the gateway's alone, the tables nobody's but
// the definer roles'.
func TestAnalysisBodiesGrants(t *testing.T) {
	_, pool := testutil.MigratedPostgres(t)
	ctx := context.Background()
	var bad int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_proc p, unnest(ARRAY['mhai_auth', 'mhai_platform', 'mhai_provision',
		'mhai_analysis_job', 'mhai_jobs', 'mhai_worker_read', 'mhai_worker_write']) r(role)
		WHERE p.proname IN ('mhai_analysis_period', 'mhai_analysis_periods', 'mhai_analysis_lock', 'mhai_analysis_cells',
			'mhai_analysis_filters', 'mhai_analysis_snapshot_get', 'mhai_analysis_snapshot_put', 'mhai_analysis_records',
			'mhai_analysis_viewed', 'mhai_analysis_reserve_cold')
		  AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r.role)
		  AND has_function_privilege(r.role, p.oid, 'EXECUTE')`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Errorf("%d grants of the request functions beyond the gateway", bad)
	}
	var gw int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_proc p WHERE p.proname IN ('mhai_analysis_cells', 'mhai_analysis_snapshot_put')
		AND has_function_privilege('mhai_gateway', p.oid, 'EXECUTE')`).Scan(&gw); err != nil || gw != 2 {
		t.Errorf("the gateway executes %d of cells and put (%v)", gw, err)
	}
	var tables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM unnest(ARRAY['analysis_subject_attrs', 'analysis_hospital_start',
		'analysis_snapshots', 'analysis_view_log', 'analysis_unusual', 'analysis_cold_budget', 'analysis_cold_contexts']) t
		WHERE has_table_privilege('mhai_gateway', t, 'SELECT') OR has_table_privilege('mhai_gateway', t, 'INSERT')
		   OR has_table_privilege('mhai_analysis_reader', t, 'UPDATE') OR has_table_privilege('mhai_analysis_reader', t, 'DELETE')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Errorf("%d tables open to the gateway, or updatable by the reader", tables)
	}
	var internal int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_proc p WHERE p.proname IN ('mhai_analysis_hospital', 'mhai_analysis_frozen',
		'mhai_analysis_small_shown') AND has_function_privilege('mhai_gateway', p.oid, 'EXECUTE')`).Scan(&internal); err != nil || internal != 0 {
		t.Errorf("the gateway executes %d internal helpers (%v)", internal, err)
	}
}

// 00025 goes down to 00024 and up again, leaving nothing behind.
func TestMigration00025DownUp(t *testing.T) {
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
	if err := db.MigrateDownTo(ctx, pg.URL, 24); err != nil {
		t.Fatalf("down to 24: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_class WHERE relname IN ('analysis_subject_attrs', 'analysis_hospital_start', 'analysis_snapshots',
			'analysis_view_log', 'analysis_unusual', 'analysis_cold_budget', 'analysis_cold_contexts'))
		+ (SELECT count(*) FROM pg_proc WHERE proname IN ('mhai_analysis_period', 'mhai_analysis_periods', 'mhai_analysis_lock',
			'mhai_analysis_cells', 'mhai_analysis_filters', 'mhai_analysis_snapshot_get', 'mhai_analysis_snapshot_put',
			'mhai_analysis_records', 'mhai_analysis_viewed', 'mhai_analysis_hospital', 'mhai_analysis_frozen',
			'mhai_analysis_small_shown', 'mhai_analysis_subject_attrs_put', 'mhai_analysis_reserve_cold'))
		+ (SELECT count(*) FROM pg_policies WHERE policyname = 'analysis_reader')
		+ (SELECT count(*) FROM unnest(ARRAY['analysis_facts', 'analysis_first_seen', 'analysis_periods', 'hospitals']) t
		   WHERE has_any_column_privilege('mhai_analysis_reader', t, 'SELECT'))
		+ (SELECT count(*) FROM pg_proc WHERE proname = 'mhai_analysis_sweep' AND prosrc LIKE '%analysis_snapshots%')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("after down, %d objects or grants of 00025 remain", left)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
