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

// loginAttributes are the attributes every deployment login has.
var loginAttributes = []string{"LOGIN", "INHERIT", "NOSUPERUSER", "NOBYPASSRLS", "NOCREATEDB", "NOCREATEROLE", "NOREPLICATION"}

// wrongAttributes lists the ALTER ROLE options that bring an existing role
// back to loginAttributes, naming only the ones that differ.
func wrongAttributes(canLogin, inherit, super, bypass, createDB, createRole, replication bool) []string {
	var fix []string
	for _, a := range []struct {
		ok  bool
		opt string
	}{
		{canLogin, "LOGIN"}, {inherit, "INHERIT"}, {!super, "NOSUPERUSER"}, {!bypass, "NOBYPASSRLS"},
		{!createDB, "NOCREATEDB"}, {!createRole, "NOCREATEROLE"}, {!replication, "NOREPLICATION"},
	} {
		if !a.ok {
			fix = append(fix, a.opt)
		}
	}
	return fix
}

// testPasswordSQL, when set by a test, rewrites the password statement
// before it is sent (to make it fail on the server). Always nil otherwise.
var testPasswordSQL func(string) string

// loginRoles are the roles a deployment login may be made a member of:
// mhai_app (the core service), mhai_provision (the seed CLI) and
// mhai_worker_app (the pipeline, 00008_intelligence_worker.sql). Each holds
// no privilege of its own and inherits none.
var loginRoles = map[string]bool{"mhai_app": true, "mhai_provision": true, "mhai_worker_app": true}

var loginName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// MinLoginPasswordLen is the shortest password EnsureLogin accepts.
const MinLoginPasswordLen = 16

// EnsureLogin creates the login role name with password, a member of
// exactly role (mhai_app, mhai_provision or mhai_worker_app), or brings an existing one back
// to that: LOGIN, no superuser, BYPASSRLS, CREATEDB or CREATEROLE, no
// session defaults (ALTER ROLE ... RESET ALL, role-wide and for this
// database), the new password, and every other membership revoked. pool must be able to
// create roles (the migrate connection). Errors never carry the password,
// and neither does any SQL text or parameter sent to Postgres: the password
// goes as a SCRAM-SHA-256 verifier built here (see scramVerifier), which
// Postgres stores as it is.
func EnsureLogin(ctx context.Context, pool *pgxpool.Pool, name, password, role string) error {
	if !loginRoles[role] {
		return fmt.Errorf("db: a login may only be a member of mhai_app, mhai_provision or mhai_worker_app, not %q", role)
	}
	if !loginName.MatchString(name) || strings.HasPrefix(name, "mhai_") || strings.HasPrefix(name, "pg_") {
		return errors.New("db: login name must be lower-case letters, digits and _, and not start with mhai_ or pg_")
	}
	if len(password) < MinLoginPasswordLen {
		return fmt.Errorf("db: login password must be at least %d characters", MinLoginPasswordLen)
	}
	if !printableASCII(password) {
		// Postgres runs SASLprep on non-ASCII passwords at sign-in; a
		// verifier built here without it would not match.
		return errors.New("db: login password must be printable ASCII")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errors.New("db: ensure login: begin failed")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// 1. The role and its attributes, in a statement without the password.
	// For an existing role only the attributes that are wrong are named:
	// on Postgres 16 a non-superuser may not even name BYPASSRLS or
	// REPLICATION (as NO...), so the CREATEROLE owner of managed Postgres
	// could otherwise never run this twice.
	var (
		exists                                     bool
		canLogin, inherit, super, bypass, createDB bool
		createRole, replication                    bool
	)
	err = tx.QueryRow(ctx, `SELECT rolcanlogin, rolinherit, rolsuper, rolbypassrls, rolcreatedb, rolcreaterole, rolreplication
		FROM pg_roles WHERE rolname = $1`, name).Scan(&canLogin, &inherit, &super, &bypass, &createDB, &createRole, &replication)
	switch {
	case err == nil:
		exists = true
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return errors.New("db: ensure login: lookup failed")
	}
	ident := pgx.Identifier{name}.Sanitize()
	if !exists {
		if _, err := tx.Exec(ctx, "CREATE ROLE "+ident+" "+strings.Join(loginAttributes, " ")); err != nil {
			return fmt.Errorf("db: ensure login %s: CREATE ROLE failed", name)
		}
	} else if fix := wrongAttributes(canLogin, inherit, super, bypass, createDB, createRole, replication); len(fix) > 0 {
		if _, err := tx.Exec(ctx, "ALTER ROLE "+ident+" "+strings.Join(fix, " ")); err != nil {
			return fmt.Errorf("db: ensure login %s: ALTER ROLE failed", name)
		}
	}

	// 1b. No session defaults on the role, here or for this database: a
	// default role (ALTER ROLE ... SET role) would start every connection
	// in that role, outside db.With* and with_hospital, and RESET ROLE
	// would fall back to it; a default mhai.hospital_id would scope a
	// connection before any transaction asked for it.
	var dbName string
	if err := tx.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		return errors.New("db: ensure login: lookup failed")
	}
	if _, err := tx.Exec(ctx, "ALTER ROLE "+ident+" RESET ALL"); err != nil {
		return fmt.Errorf("db: ensure login %s: RESET ALL failed", name)
	}
	if _, err := tx.Exec(ctx, "ALTER ROLE "+ident+" IN DATABASE "+pgx.Identifier{dbName}.Sanitize()+" RESET ALL"); err != nil {
		return fmt.Errorf("db: ensure login %s: RESET ALL in database failed", name)
	}

	// 2. Probe, with no secret in it, that this connection may set the
	// role's password. It needs the same privilege as the statement in
	// step 4 and takes the same row lock, so after it step 4 can't fail for
	// want of either. That matters because Postgres logs a failed statement
	// in full (log_min_error_statement), and only a superuser can turn that
	// off for the transaction.
	if _, err := tx.Exec(ctx, "ALTER ROLE "+ident+" PASSWORD NULL"); err != nil {
		return fmt.Errorf("db: ensure login %s: may not set its password", name)
	}

	// 3. Where this connection may (a superuser), keep the password
	// statement out of the server log, whether it succeeds or fails. It
	// carries only a verifier, but that is still no business of the log.
	var amSuper bool
	if err := tx.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&amSuper); err != nil {
		return errors.New("db: ensure login: lookup failed")
	}
	if amSuper {
		if _, err := tx.Exec(ctx, `SET LOCAL log_statement = 'none'`); err != nil {
			return errors.New("db: ensure login: set log_statement failed")
		}
		if _, err := tx.Exec(ctx, `SET LOCAL log_min_error_statement = 'panic'`); err != nil {
			return errors.New("db: ensure login: set log_min_error_statement failed")
		}
	}

	// 4. The password, as a SCRAM verifier, in a statement of its own.
	stmt, err := passwordStatement(name, password)
	if err != nil {
		return err
	}
	if testPasswordSQL != nil {
		stmt = testPasswordSQL(stmt)
	}
	if _, err := tx.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("db: ensure login %s: setting the password failed", name)
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
