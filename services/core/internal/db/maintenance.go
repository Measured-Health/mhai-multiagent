package db

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SweepBatch is how many expired Idempotency-Key rows one sweep call
// deletes at most; SweepIdempotency repeats until a call deletes fewer.
const SweepBatch = 5000

// SweepIdempotency deletes every expired Idempotency-Key row, of every
// hospital, in batches, as mhai_auth through mhai_sweep_idempotency (a
// SECURITY DEFINER function that can see only expired rows). It returns how
// many rows went. Errors carry no row data.
func SweepIdempotency(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var total int64
	for i := 0; i < 1000; i++ {
		var n int64
		err := WithAuth(ctx, pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT mhai_sweep_idempotency($1)`, SweepBatch).Scan(&n)
		})
		if err != nil {
			return total, errors.New("db: idempotency sweep failed")
		}
		total += n
		if n < SweepBatch {
			break
		}
	}
	return total, nil
}

// loginRoles are the roles a deployment login may be made a member of:
// mhai_app (the core service) and mhai_provision (the seed CLI).
var loginRoles = map[string]bool{"mhai_app": true, "mhai_provision": true}

var loginName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// MinLoginPasswordLen is the shortest password EnsureLogin accepts.
const MinLoginPasswordLen = 16

// EnsureLogin creates the login role name with password, a member of
// exactly role (mhai_app or mhai_provision), or brings an existing one back
// to that: LOGIN, no superuser, BYPASSRLS, CREATEDB or CREATEROLE, the new
// password, and every other membership revoked. pool must be able to
// create roles (the migrate connection). Errors never carry the password.
func EnsureLogin(ctx context.Context, pool *pgxpool.Pool, name, password, role string) error {
	if !loginRoles[role] {
		return fmt.Errorf("db: a login may only be a member of mhai_app or mhai_provision, not %q", role)
	}
	if !loginName.MatchString(name) || strings.HasPrefix(name, "mhai_") || strings.HasPrefix(name, "pg_") {
		return errors.New("db: login name must be lower-case letters, digits and _, and not start with mhai_ or pg_")
	}
	if len(password) < MinLoginPasswordLen {
		return fmt.Errorf("db: login password must be at least %d characters", MinLoginPasswordLen)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errors.New("db: ensure login: begin failed")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, name).Scan(&exists); err != nil {
		return errors.New("db: ensure login: lookup failed")
	}
	verb := "CREATE"
	if exists {
		verb = "ALTER"
	}
	// format() quotes the name and the password; the statement is built by
	// Postgres, never by string concatenation of the inputs.
	var stmt string
	if err := tx.QueryRow(ctx,
		`SELECT format('%s ROLE %I LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD %L', $1::text, $2::text, $3::text)`,
		verb, name, password).Scan(&stmt); err != nil {
		return errors.New("db: ensure login: format failed")
	}
	if _, err := tx.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("db: ensure login %s: %s ROLE failed", name, verb)
	}
	rows, err := tx.Query(ctx, `SELECT g.rolname FROM pg_auth_members m
		JOIN pg_roles g ON g.oid = m.roleid JOIN pg_roles u ON u.oid = m.member
		WHERE u.rolname = $1 AND g.rolname <> $2`, name, role)
	if err != nil {
		return errors.New("db: ensure login: membership lookup failed")
	}
	var others []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			rows.Close()
			return errors.New("db: ensure login: membership lookup failed")
		}
		others = append(others, g)
	}
	rows.Close()
	for _, g := range others {
		if _, err := tx.Exec(ctx, "REVOKE "+pgx.Identifier{g}.Sanitize()+" FROM "+pgx.Identifier{name}.Sanitize()); err != nil {
			return fmt.Errorf("db: ensure login %s: revoke %s failed", name, g)
		}
	}
	if _, err := tx.Exec(ctx, "GRANT "+pgx.Identifier{role}.Sanitize()+" TO "+pgx.Identifier{name}.Sanitize()); err != nil {
		return fmt.Errorf("db: ensure login %s: grant %s failed", name, role)
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.New("db: ensure login: commit failed")
	}
	return nil
}
