// Package dblogin checks the notifications service's database login before
// it is used (notifications design §6, §16.4; secure-build: app roles never
// own tables and never bypass RLS). The login must hold mhai_notify
// (through mhai_notify_app) and nothing more: not a superuser, not
// BYPASSRLS, CREATEROLE, CREATEDB or REPLICATION, not a member of
// mhai_migrate or the database's owner, owning no schema or relation, and
// a member of no other mhai_ role. The service refuses to start with such
// a login, and health reports database_ready false.
package dblogin

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrUnavailable: the database didn't answer (the service starts and
	// stays not ready; the pool connects lazily).
	ErrUnavailable = errors.New("dblogin: the database did not answer")
	// ErrPrivileged: the login can do more than the notifications service
	// needs. The service refuses to start.
	ErrPrivileged = errors.New("dblogin: the database login is too privileged")
)

// PrivilegedError names what is wrong with the login, by a closed code.
type PrivilegedError struct{ Reason string }

func (e *PrivilegedError) Error() string { return ErrPrivileged.Error() + " (" + e.Reason + ")" }

// Unwrap is ErrPrivileged.
func (e *PrivilegedError) Unwrap() error { return ErrPrivileged }

// Querier is what the check needs (a pgxpool.Pool).
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// The roles a notifications login may be a member of.
const query = `
SELECT r.rolsuper, r.rolbypassrls, r.rolcreaterole, r.rolcreatedb, r.rolreplication,
       EXISTS (SELECT 1 FROM pg_roles m WHERE m.rolname = 'mhai_migrate' AND pg_has_role(session_user, m.oid, 'MEMBER')),
       pg_has_role(session_user, (SELECT d.datdba FROM pg_database d WHERE d.datname = current_database()), 'MEMBER'),
       (SELECT count(*) FROM pg_namespace n
         WHERE pg_has_role(session_user, n.nspowner, 'MEMBER')
           AND n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'),
       (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE pg_has_role(session_user, c.relowner, 'MEMBER')
           AND n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'),
       (SELECT count(*) FROM pg_roles g
         WHERE g.rolname LIKE 'mhai\_%' AND g.rolname NOT IN ('mhai_notify', 'mhai_notify_app')
           AND pg_has_role(session_user, g.oid, 'MEMBER')),
       EXISTS (SELECT 1 FROM pg_roles d WHERE d.rolname = 'mhai_notify' AND pg_has_role(session_user, d.oid, 'MEMBER'))
FROM pg_roles r WHERE r.rolname = session_user`

// Check runs the check. nil: the login is fit; ErrUnavailable: no answer;
// a *PrivilegedError: refuse.
func Check(ctx context.Context, q Querier) error {
	var (
		super, bypass, createRole, createDB, replication bool
		migrate, dbOwner                                 bool
		schemas, relations, otherRoles                   int64
		notify                                           bool
	)
	err := q.QueryRow(ctx, query).Scan(&super, &bypass, &createRole, &createDB, &replication, &migrate, &dbOwner,
		&schemas, &relations, &otherRoles, &notify)
	if err != nil {
		return ErrUnavailable
	}
	for _, c := range []struct {
		bad    bool
		reason string
	}{
		{super, "superuser"},
		{bypass, "bypassrls"},
		{createRole, "createrole"},
		{createDB, "createdb"},
		{replication, "replication"},
		{migrate, "migrate_member"},
		{dbOwner, "database_owner"},
		{schemas > 0, "owns_schemas"},
		{relations > 0, "owns_relations"},
		{otherRoles > 0, "other_roles"},
		{!notify, "not_notify"},
	} {
		if c.bad {
			return &PrivilegedError{Reason: c.reason}
		}
	}
	return nil
}
