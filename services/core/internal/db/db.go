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
	"strings"

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

// CheckLoginRole refuses a login user that row-level security can't hold:
//   - a superuser, or a role with BYPASSRLS, CREATEROLE, CREATEDB or
//     REPLICATION (CREATEROLE on PG 15 and older can grant itself the
//     owner role);
//   - a role that isn't a member of mhai_app, or is a direct member of any
//     other role (with or without inheritance);
//   - a role that inherits (pg_has_role USAGE) any of the database roles
//     directly or through another role: only SET ROLE inside db.With* may
//     reach them;
//   - a role that is, or can act as, the owner of the database, a schema or
//     a table.
//
// The gateway's login must be a plain member of mhai_app, so that a query
// outside With* fails and RESET ROLE inside one falls back to a role with
// no privileges.
func CheckLoginRole(ctx context.Context, pool *pgxpool.Pool) error {
	var (
		name           string
		super, bypass  bool
		createRole     bool
		createDB       bool
		replication    bool
		member         bool
		inherited      []string
		ownsDatabase   bool
		ownedSchemas   []string
		ownedRelations int
		otherRoles     []string
	)
	err := pool.QueryRow(ctx, `
		SELECT r.rolname, r.rolsuper, r.rolbypassrls, r.rolcreaterole, r.rolcreatedb, r.rolreplication,
		       pg_has_role(session_user, 'mhai_app', 'MEMBER'),
		       ARRAY(SELECT x FROM unnest(ARRAY['mhai_migrate', 'mhai_gateway', 'mhai_platform', 'mhai_auth', 'mhai_worker', 'mhai_provision']) AS x
		             WHERE EXISTS (SELECT 1 FROM pg_roles p WHERE p.rolname = x)
		               AND pg_has_role(session_user, x, 'USAGE')),
		       pg_has_role(session_user, (SELECT d.datdba FROM pg_database d WHERE d.datname = current_database()), 'MEMBER'),
		       ARRAY(SELECT n.nspname::text FROM pg_namespace n
		             WHERE pg_has_role(session_user, n.nspowner, 'MEMBER')
		               AND n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'
		             ORDER BY 1),
		       (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		        WHERE pg_has_role(session_user, c.relowner, 'MEMBER')
		          AND n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'),
		       ARRAY(SELECT g.rolname::text FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid
		             WHERE m.member = r.oid AND g.rolname <> 'mhai_app' ORDER BY 1)
		FROM pg_roles r WHERE r.rolname = session_user`).
		Scan(&name, &super, &bypass, &createRole, &createDB, &replication, &member, &inherited, &ownsDatabase, &ownedSchemas, &ownedRelations, &otherRoles)
	if err != nil {
		return fmt.Errorf("db: check login role: %w", err)
	}
	var problems []string
	if super {
		problems = append(problems, "it is a superuser")
	}
	if bypass {
		problems = append(problems, "it has BYPASSRLS")
	}
	if createRole {
		problems = append(problems, "it has CREATEROLE")
	}
	if createDB {
		problems = append(problems, "it has CREATEDB")
	}
	if replication {
		problems = append(problems, "it has REPLICATION")
	}
	if !super && !member {
		problems = append(problems, "it is not a member of mhai_app")
	}
	if len(otherRoles) > 0 {
		problems = append(problems, "it is also a member of "+strings.Join(otherRoles, ", "))
	}
	if len(inherited) > 0 {
		problems = append(problems, "it inherits the privileges of "+strings.Join(inherited, ", "))
	}
	if ownsDatabase {
		problems = append(problems, "it owns the database")
	}
	if len(ownedSchemas) > 0 {
		problems = append(problems, "it owns schema "+strings.Join(ownedSchemas, ", "))
	}
	if ownedRelations > 0 {
		problems = append(problems, fmt.Sprintf("it owns %d tables or other relations", ownedRelations))
	}
	if len(problems) > 0 {
		return fmt.Errorf("db: refusing database login %q: %s; connect as a login role that is only a member of mhai_app (CREATE ROLE ... LOGIN IN ROLE mhai_app)",
			name, strings.Join(problems, ", "))
	}
	return nil
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
