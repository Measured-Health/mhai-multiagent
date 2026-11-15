// Package testutil starts real dependencies in containers for integration tests.
package testutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
)

// PostgresImage is the image every integration test runs against.
const PostgresImage = "postgres:16-alpine"

// PG is a throwaway Postgres 16 server. URL connects as a superuser, which
// tests use only for setup and tampering; behaviour is asserted through the
// app roles.
type PG struct {
	URL string
	c   testcontainers.Container

	admin, name string // a MigratedPostgres clone: the maintenance URL and its database
}

// Logs returns everything the server has written to its log (the
// container's stdout and stderr) so far.
func (pg *PG) Logs(ctx context.Context) (string, error) {
	if pg.c == nil {
		return "", errors.New("testutil: no container")
	}
	r, err := pg.c.Logs(ctx)
	if err != nil {
		return "", err
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	return string(b), err
}

// StatementStats is the server setting that loads pg_stat_statements, for
// a test that counts the statements a path runs (NewPostgres(ctx,
// StatementStats), then CREATE EXTENSION pg_stat_statements).
const StatementStats = "shared_preload_libraries=pg_stat_statements"

// NewPostgres starts an empty Postgres 16 container (no migrations). Call
// stop when done; TestMain uses this to share one server across a package.
// Each setting is a server setting ("name=value", postgres -c).
func NewPostgres(ctx context.Context, settings ...string) (pg *PG, stop func(), err error) {
	opts := []testcontainers.ContainerCustomizer{
		postgres.WithDatabase("mhai"),
		postgres.WithUsername("mhai_super"),
		postgres.WithPassword("test-only-superuser-password"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(90 * time.Second)),
	}
	for _, s := range settings {
		opts = append(opts, testcontainers.WithCmdArgs("-c", s))
	}
	c, err := postgres.Run(ctx, PostgresImage, opts...)
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
	return &PG{URL: url, c: c}, stop, nil
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

// MigratedPostgres returns a fresh database with every migration applied
// and a superuser pool on it, both gone when the test ends.
//
// The database is a copy (CREATE DATABASE ... TEMPLATE) of one migrated
// once per test binary, on a server the binary shares: a copy takes a
// fraction of a second where a new server and the migrations took seconds.
// Each test still gets its own tables, rows, sequences, grants and policies.
// The server is shared, so cluster-wide state is too: roles (the
// migrations' roles exist once, and logins made by AppLogin and
// ProvisionLogin have unique names) and the server log (Logs). A test that
// changes roles or server settings, or needs a server of its own, uses
// StartPostgres or NewPostgres instead.
func MigratedPostgres(t testing.TB) (*PG, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pg, err := cloneMigrated(ctx)
	if err != nil {
		t.Fatalf("migrated database: %v", err)
	}
	t.Cleanup(func() { dropClone(pg) })
	pool, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pg, pool
}

// templateDB is the migrated database every clone copies. It takes no
// connections once migrated, as CREATE DATABASE ... TEMPLATE requires.
const templateDB = "mhai"

var shared struct {
	once  sync.Once
	pg    *PG    // the server, connected to templateDB
	admin string // the same server's maintenance database (postgres)
	err   error
}

// sharedTemplate starts the binary's server and migrates templateDB, once.
// The server lives until the test binary exits (testcontainers' reaper
// removes it).
func sharedTemplate(ctx context.Context) (*PG, string, error) {
	shared.once.Do(func() {
		pg, _, err := NewPostgres(ctx)
		if err != nil {
			shared.err = fmt.Errorf("start postgres: %w", err)
			return
		}
		if err := db.MigrateUp(ctx, pg.URL); err != nil {
			shared.err = fmt.Errorf("migrate up: %w", err)
			return
		}
		admin := withDatabase(pg.URL, "postgres")
		if err := exec(ctx, admin, `ALTER DATABASE `+templateDB+` WITH ALLOW_CONNECTIONS false`); err != nil {
			shared.err = fmt.Errorf("close the template: %w", err)
			return
		}
		shared.pg, shared.admin = pg, admin
	})
	return shared.pg, shared.admin, shared.err
}

// cloneMigrated copies templateDB into a new database with a unique name.
func cloneMigrated(ctx context.Context) (*PG, error) {
	pg, admin, err := sharedTemplate(ctx)
	if err != nil {
		return nil, err
	}
	name := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := exec(ctx, admin, `CREATE DATABASE `+name+` TEMPLATE `+templateDB); err != nil {
		return nil, fmt.Errorf("clone: %w", err)
	}
	return &PG{URL: withDatabase(pg.URL, name), c: pg.c, admin: admin, name: name}, nil
}

// dropClone drops a database cloneMigrated made, closing any connection a
// test left open.
func dropClone(pg *PG) {
	_ = exec(context.Background(), pg.admin, `DROP DATABASE IF EXISTS `+pg.name+` WITH (FORCE)`)
}

// exec runs one statement on dsn over a connection of its own.
func exec(ctx context.Context, dsn, sql string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, sql)
	return err
}

// withDatabase is url with its database replaced by name.
func withDatabase(raw, name string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Path = "/" + name
	return u.String()
}

// AppLogin creates a fresh login role that is only a member of mhai_app,
// like the gateway's own login, and returns a pool connected as it. super
// must be a superuser pool on pg.
func AppLogin(ctx context.Context, pg *PG, super *pgxpool.Pool) (*pgxpool.Pool, error) {
	return AppLoginWith(ctx, pg, super, nil)
}

// AppLoginWith is AppLogin with configure applied to the pool's config
// before it connects (a query tracer, say); nil changes nothing.
func AppLoginWith(ctx context.Context, pg *PG, super *pgxpool.Pool, configure func(*pgxpool.Config)) (*pgxpool.Pool, error) {
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
	if configure != nil {
		configure(cfg)
	}
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
