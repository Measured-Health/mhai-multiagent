//go:build integration

package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// notifyTables are the only relations mhai_notify may touch: its own (PR 3
// adds them). PR 2 has none.
var notifyTables = map[string]bool{}

// NT16 (notifications design §16.5): mhai_notify is refused on clinical
// tables, the EMR schema, users, sessions and the audit chain, and holds
// no privilege on any table or column but its own. Its login role holds
// it without inheriting it.
func TestNotifyRoleHasNoClinicalAccess(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, q := range []string{
		`SELECT 1 FROM alerts LIMIT 1`, `SELECT 1 FROM alert_deliveries LIMIT 1`, `SELECT 1 FROM emr.patients LIMIT 1`,
		`SELECT 1 FROM emr.findings LIMIT 1`, `SELECT 1 FROM clinical_queries LIMIT 1`, `SELECT 1 FROM users LIMIT 1`,
		`SELECT email FROM users LIMIT 1`, `SELECT 1 FROM sessions LIMIT 1`, `SELECT 1 FROM refresh_tokens LIMIT 1`,
		`SELECT 1 FROM audit_log LIMIT 1`, `SELECT 1 FROM account_tokens LIMIT 1`, `SELECT 1 FROM user_mfa LIMIT 1`,
		`SELECT 1 FROM documents LIMIT 1`, `SELECT 1 FROM feedback_events LIMIT 1`, `SELECT 1 FROM kb_chunks LIMIT 1`,
		`INSERT INTO audit_log DEFAULT VALUES`, `DELETE FROM users`,
	} {
		err := inRole(ctx, pool, "mhai_notify", q)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "42501" {
			t.Errorf("mhai_notify: %s: %v, want permission denied", q, err)
		}
	}
	rows, err := pool.Query(ctx, `SELECT n.nspname || '.' || c.relname, c.relname
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'v', 'm', 'p', 'f') AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND (has_table_privilege('mhai_notify', c.oid, 'SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER')
		       OR has_any_column_privilege('mhai_notify', c.oid, 'SELECT, INSERT, UPDATE, REFERENCES'))`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var full, name string
		if err := rows.Scan(&full, &name); err != nil {
			t.Fatal(err)
		}
		if !notifyTables[full] {
			t.Errorf("mhai_notify holds a privilege on %s", full)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var emrUsage, inherits, set bool
	if err := pool.QueryRow(ctx, `SELECT has_schema_privilege('mhai_notify', 'emr', 'USAGE'),
		pg_has_role('mhai_notify_app', 'mhai_notify', 'USAGE'), pg_has_role('mhai_notify_app', 'mhai_notify', 'SET')`).
		Scan(&emrUsage, &inherits, &set); err != nil {
		t.Fatal(err)
	}
	if emrUsage || inherits || !set {
		t.Fatalf("emr usage %v, mhai_notify_app inherits %v, may SET %v", emrUsage, inherits, set)
	}
	var super, bypass bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper OR rolcreaterole OR rolcreatedb, rolbypassrls FROM pg_roles
		WHERE rolname = 'mhai_notify'`).Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatalf("mhai_notify attributes: %v %v %v", super, bypass, err)
	}
	// Down takes back its grants; up again restores them.
	if err := db.MigrateDownTo(ctx, pg.URL, 50); err != nil {
		t.Fatal(err)
	}
	var member bool
	if err := pool.QueryRow(ctx, `SELECT pg_has_role('mhai_notify_app', 'mhai_notify', 'MEMBER')`).Scan(&member); err != nil || member {
		t.Fatalf("after down: member %v %v", member, err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
}

// inRole runs q as role in a transaction that is rolled back.
func inRole(ctx context.Context, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, role, q string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, q)
	return err
}
