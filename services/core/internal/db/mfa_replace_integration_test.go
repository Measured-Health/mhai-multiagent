//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00057 (moving two-step to a new app): mhai_auth may set and
// clear the pending secret; the two pending columns are set together.
func TestMigration00057PendingSecret(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("R"))
	u := seedUser(t, pool, &a)
	if _, err := pool.Exec(ctx, `INSERT INTO user_mfa (user_id, secret_enc, enabled) VALUES ($1, '\x00', true)`, u); err != nil {
		t.Fatal(err)
	}
	err := db.WithAuth(ctx, pool, func(tx pgx.Tx) error {
		for _, q := range []string{
			`UPDATE user_mfa SET pending_secret_enc = '\x02', pending_created_at = now() WHERE user_id = $1`,
			`UPDATE user_mfa SET secret_enc = pending_secret_enc, pending_secret_enc = NULL, pending_created_at = NULL WHERE user_id = $1`,
		} {
			if _, err := tx.Exec(ctx, q, u); err != nil {
				return err
			}
		}
		var n int
		return tx.QueryRow(ctx, `SELECT count(*) FROM user_mfa WHERE user_id = $1 AND pending_secret_enc IS NULL`, u).Scan(&n)
	})
	if err != nil {
		t.Fatalf("mhai_auth on the pending columns: %v", err)
	}
	for _, q := range []string{
		`UPDATE user_mfa SET pending_secret_enc = '\x02' WHERE user_id = $1`,
		`UPDATE user_mfa SET pending_created_at = now() WHERE user_id = $1`,
	} {
		if _, err := pool.Exec(ctx, q, u); pgCode(err) != "23514" {
			t.Errorf("%s: want 23514 (half a pending secret), got %v", q, err)
		}
	}
}

func TestMigration00057DownUp(t *testing.T) {
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
	if err := db.MigrateDownTo(ctx, pg.URL, 56); err != nil {
		t.Fatalf("down to 56: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'user_mfa' AND column_name IN ('pending_secret_enc', 'pending_created_at')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after down, %d pending columns (%v)", n, err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
