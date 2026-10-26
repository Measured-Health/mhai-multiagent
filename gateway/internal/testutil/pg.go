// Package testutil starts real dependencies in containers for integration tests.
package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Measured-Health/mhai-multiagent/gateway/internal/db"
)

// PostgresImage is the image every integration test runs against.
const PostgresImage = "postgres:16-alpine"

// PG is a throwaway Postgres 16 server. URL connects as a superuser, which
// tests use only for setup and tampering; behaviour is asserted through the
// app roles.
type PG struct {
	URL string
}

// NewPostgres starts an empty Postgres 16 container (no migrations). Call
// stop when done; TestMain uses this to share one server across a package.
func NewPostgres(ctx context.Context) (pg *PG, stop func(), err error) {
	c, err := postgres.Run(ctx, PostgresImage,
		postgres.WithDatabase("mhai"),
		postgres.WithUsername("mhai_super"),
		postgres.WithPassword("test-only-superuser-password"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		if c != nil {
			_ = testcontainers.TerminateContainer(c)
		}
		return nil, nil, err
	}
	stop = func() { _ = testcontainers.TerminateContainer(c) }
	url, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		stop()
		return nil, nil, err
	}
	return &PG{URL: url}, stop, nil
}

// StartPostgres starts an empty Postgres 16 container and stops it when the
// test ends.
func StartPostgres(t testing.TB) *PG {
	t.Helper()
	pg, stop, err := NewPostgres(context.Background())
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(stop)
	return pg
}

// MigratedPostgres starts Postgres, applies every migration and returns a
// superuser pool, closed when the test ends.
func MigratedPostgres(t testing.TB) (*PG, *pgxpool.Pool) {
	t.Helper()
	pg := StartPostgres(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	pool, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pg, pool
}
