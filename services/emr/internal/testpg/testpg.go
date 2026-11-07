// Package testpg starts a throwaway Postgres 16 with core's migrations
// applied, for the emr service's integration tests. The migrations are
// read from disk (services/core/internal/db/migrations): SQL files, not
// core's Go code, so the emr module still imports nothing of core.
package testpg

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the server every integration test runs against (core's).
const Image = "postgres:16-alpine"

// MigrationsDir is core's migrations directory.
func MigrationsDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "core", "internal", "db", "migrations")
}

// Migrated starts Postgres, applies every core migration as the superuser
// and returns the superuser URL and a pool, both gone when the test ends.
func Migrated(t testing.TB) (string, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	c, err := postgres.Run(ctx, Image,
		postgres.WithDatabase("mhai"), postgres.WithUsername("mhai_super"), postgres.WithPassword("test-only-superuser-password"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(90*time.Second)))
	if err != nil {
		if c != nil {
			_ = testcontainers.TerminateContainer(c)
		}
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	url, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := stdlib.OpenDB(*cfg)
	defer func(d *sql.DB) { _ = d.Close() }(sqlDB)
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS(MigrationsDir()))
	if err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return url, pool
}
