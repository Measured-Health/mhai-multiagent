// Package db opens the gateway's Postgres pool, applies the embedded goose
// migrations and runs transactions under the right database role.
//
// Every query runs inside one of WithHospital, WithPlatform or WithAuth.
// The gateway's login user holds no table privileges of its own (it is a
// member of mhai_app, see 00003_rls_roles.sql), so a query outside them
// fails instead of reading anything.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

// The database roles a transaction can run as (design §7.4).
const (
	roleGateway  = "mhai_gateway"
	rolePlatform = "mhai_platform"
	roleAuth     = "mhai_auth"
)

// ErrNoHospital is returned by WithHospital for the nil hospital id.
var ErrNoHospital = errors.New("db: no hospital id")

// Open connects a pool and checks it with a ping.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("db: invalid database url")
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: open: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// WithHospital runs fn in a transaction as mhai_gateway, with
// mhai.hospital_id set so row-level security shows only that hospital's
// rows. The transaction commits if fn returns nil and rolls back otherwise.
func WithHospital(ctx context.Context, pool *pgxpool.Pool, hospitalID uuid.UUID, fn func(pgx.Tx) error) error {
	if hospitalID == uuid.Nil {
		return ErrNoHospital
	}
	return inRole(ctx, pool, roleGateway, hospitalID.String(), fn)
}

// WithPlatform runs fn in a transaction as mhai_platform: metadata across
// hospitals, no clinical tables.
func WithPlatform(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return inRole(ctx, pool, rolePlatform, "", fn)
}

// WithAuth runs fn in a transaction as mhai_auth. Only sign-in uses it,
// before a hospital is known.
func WithAuth(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return inRole(ctx, pool, roleAuth, "", fn)
}

func inRole(ctx context.Context, pool *pgxpool.Pool, role, hospital string, fn func(pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	// role is one of the constants above, never input.
	if _, err = tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
		return fmt.Errorf("db: set role: %w", err)
	}
	if hospital != "" {
		if _, err = tx.Exec(ctx, "SELECT set_config('mhai.hospital_id', $1, true)", hospital); err != nil {
			return fmt.Errorf("db: set hospital: %w", err)
		}
	}
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}

// MigrateUp applies every pending migration. The url's user must be able to
// create roles (a superuser, or a CREATEROLE admin on managed Postgres).
func MigrateUp(ctx context.Context, url string) error {
	return withProvider(url, func(p *goose.Provider) error {
		_, err := p.Up(ctx)
		return err
	})
}

// MigrateDownTo rolls back to version (0 = empty). For tests and local
// recovery; the roles, being cluster-wide, stay.
func MigrateDownTo(ctx context.Context, url string, version int64) error {
	return withProvider(url, func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, version)
		return err
	})
}

// Status reports each migration and whether it is applied.
func Status(ctx context.Context, url string) (st []*goose.MigrationStatus, err error) {
	err = withProvider(url, func(p *goose.Provider) error {
		st, err = p.Status(ctx)
		return err
	})
	return st, err
}

func withProvider(url string, fn func(*goose.Provider) error) error {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return errors.New("db: invalid database url")
	}
	sqlDB := stdlib.OpenDB(*cfg)
	defer func(d *sql.DB) { _ = d.Close() }(sqlDB)
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys)
	if err != nil {
		return fmt.Errorf("db: migrations: %w", err)
	}
	if err := fn(p); err != nil {
		return fmt.Errorf("db: migrate: %w", err)
	}
	return nil
}
