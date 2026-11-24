//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00059 (adding a hospital from the platform app) goes down
// cleanly, taking its columns, function and grants with it, and up again.
func TestMigration00059DownUp(t *testing.T) {
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
	if err := db.MigrateDownTo(ctx, pg.URL, 58); err != nil {
		t.Fatalf("down to 58: %v", err)
	}
	var cols, fns int
	var insert bool
	if err := pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM information_schema.columns WHERE table_name = 'hospitals'
			   AND column_name IN ('country', 'email_domains', 'first_admin_id')),
			(SELECT count(*) FROM pg_proc WHERE proname = 'mhai_hospital_added_now'),
			has_any_column_privilege('mhai_platform', 'account_tokens', 'INSERT')`).Scan(&cols, &fns, &insert); err != nil {
		t.Fatal(err)
	}
	if cols != 0 || fns != 0 || insert {
		t.Fatalf("after down: %d columns, %d functions, account_tokens insert %v", cols, fns, insert)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
