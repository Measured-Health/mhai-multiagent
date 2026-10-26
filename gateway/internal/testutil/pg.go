// Package testutil starts real dependencies in containers for integration tests.
package testutil

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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

// AppLogin creates a fresh login role that is only a member of mhai_app,
// like the gateway's own login, and returns a pool connected as it. super
// must be a superuser pool on pg.
func AppLogin(ctx context.Context, pg *PG, super *pgxpool.Pool) (*pgxpool.Pool, error) {
	name := "app_login_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if _, err := super.Exec(ctx, `CREATE ROLE `+name+` LOGIN PASSWORD 'test-only-login-password' IN ROLE mhai_app`); err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(pg.URL)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.User = name
	cfg.ConnConfig.Password = "test-only-login-password"
	return pgxpool.NewWithConfig(ctx, cfg)
}

// ProvisionLogin creates a fresh non-superuser login that is a member only of
// mhai_provision, like the seed CLI's provisioning login, and returns a pool
// connected as it plus its URL. super must be a superuser pool on pg.
func ProvisionLogin(ctx context.Context, pg *PG, super *pgxpool.Pool) (*pgxpool.Pool, string, error) {
	name := "prov_login_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if _, err := super.Exec(ctx, `CREATE ROLE `+name+` LOGIN PASSWORD 'test-only-login-password' IN ROLE mhai_provision`); err != nil {
		return nil, "", err
	}
	cfg, err := pgxpool.ParseConfig(pg.URL)
	if err != nil {
		return nil, "", err
	}
	cfg.ConnConfig.User = name
	cfg.ConnConfig.Password = "test-only-login-password"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, "", err
	}
	return pool, replaceUser(pg.URL, name, "test-only-login-password"), nil
}

func replaceUser(raw, user, pw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = url.UserPassword(user, pw)
	return u.String()
}
