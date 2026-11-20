// Package testpg starts a throwaway Postgres 16 with core's migrations
// applied, for the notifications service's integration tests. The migrations are
// read from disk (services/core/internal/db/migrations): SQL files, not
// core's Go code, so the notifications module still imports nothing of core.
package testpg

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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

// Login creates a login role name (password generated for the test) that
// is a member of each of roles, and returns its URL. It stands in for
// core migrate's db.EnsureLogin, which the notifications module can't import.
func Login(t testing.TB, superURL string, super *pgxpool.Pool, name string, roles ...string) string {
	t.Helper()
	ctx := context.Background()
	pw := "test-only-" + uuid.NewString() // made at run time, never a literal
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := super.Exec(ctx, "CREATE ROLE "+ident+" LOGIN NOINHERIT PASSWORD '"+pw+"'"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	for _, r := range roles {
		if _, err := super.Exec(ctx, "GRANT "+pgx.Identifier{r}.Sanitize()+" TO "+ident); err != nil {
			t.Fatalf("grant %s: %v", r, err)
		}
	}
	cfg, err := pgx.ParseConfig(superURL)
	if err != nil {
		t.Fatal(err)
	}
	return "postgres://" + name + ":" + pw + "@" + cfg.Host + ":" + strconv.Itoa(int(cfg.Port)) + "/" + cfg.Database +
		"?sslmode=disable"
}

// Hospital inserts an active synthetic hospital and returns its id.
func Hospital(t testing.TB, super *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	code := "D" + strings.ReplaceAll(id.String()[:8], "-", "")
	if _, err := super.Exec(context.Background(),
		`INSERT INTO hospitals (id, name, short_code, status, time_zone) VALUES ($1, $2, $3, 'active', 'Africa/Lagos')`,
		id, "Synthetic Hospital "+code, code); err != nil {
		t.Fatalf("seed hospital: %v", err)
	}
	return id
}
