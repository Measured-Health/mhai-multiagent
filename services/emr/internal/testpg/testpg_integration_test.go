//go:build integration

package testpg

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
)

// The emr tests run on core's real schema: its roles and row-level
// security, reached through gokit's WithHospital.
func TestMigratedSchemaHasCoresRoles(t *testing.T) {
	_, pool := Migrated(t)
	ctx := context.Background()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname IN ('mhai_gateway', 'mhai_migrate')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("roles: %d %v", n, err)
	}
	var who string
	if err := dbx.WithHospital(ctx, pool, dbx.Gateway, uuid.New(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_user`).Scan(&who)
	}); err != nil || who != "mhai_gateway" {
		t.Fatalf("WithHospital: %q %v", who, err)
	}
}
