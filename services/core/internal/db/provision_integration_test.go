//go:build integration

package db_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

func provPool(t *testing.T) (ctx context.Context, prov *pgxpool.Pool) {
	t.Helper()
	pg, super := shared(t)
	ctx = context.Background()
	p, _, err := testutil.ProvisionLogin(ctx, pg, super)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return ctx, p
}

func wantDenied(t *testing.T, what string, err error) {
	t.Helper()
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != permissionDenied {
		t.Errorf("%s: want 42501, got %v", what, err)
	}
}

func TestProvisionRoleCannotBypassAnything(t *testing.T) {
	_, super := shared(t)
	var sup, byp, login, inh bool
	if err := super.QueryRow(context.Background(),
		`SELECT rolsuper, rolbypassrls, rolcanlogin, pg_has_role('mhai_app','mhai_provision','USAGE') FROM pg_roles WHERE rolname='mhai_provision'`).
		Scan(&sup, &byp, &login, &inh); err != nil {
		t.Fatal(err)
	}
	if sup || byp || login || inh {
		t.Fatalf("super=%v bypassrls=%v login=%v mhai_app-holds-it=%v", sup, byp, login, inh)
	}
	ctx, prov := provPool(t)
	_, err := prov.Exec(ctx, `SET ROLE mhai_migrate`)
	wantDenied(t, "SET ROLE mhai_migrate", err)
}

func TestProvisionReadsOnlyHospitalIDCodeAndStatus(t *testing.T) {
	ctx, prov := provPool(t)
	for _, q := range []string{
		`SELECT 1 FROM users`, `SELECT password_hash FROM users`, `SELECT 1 FROM hospital_settings`,
		`SELECT 1 FROM user_roles`, `SELECT 1 FROM user_mfa`, `SELECT 1 FROM sessions`,
		`SELECT 1 FROM refresh_tokens`, `SELECT 1 FROM trusted_devices`, `SELECT 1 FROM audit_log`,
		`SELECT 1 FROM idempotency_keys`, `SELECT 1 FROM clinical_queries`, `SELECT 1 FROM kb_chunks`,
		`SELECT name FROM hospitals`,
	} {
		_, err := prov.Exec(ctx, q)
		wantDenied(t, q, err)
	}
	if _, err := prov.Exec(ctx, `SELECT id, short_code, status FROM hospitals`); err != nil {
		t.Errorf("select id, short_code, status: %v", err)
	}
}

func TestProvisionCannotUpdateOrDelete(t *testing.T) {
	ctx, prov := provPool(t)
	for _, tbl := range []string{"hospitals", "hospital_settings", "users", "user_roles", "audit_log"} {
		_, err := prov.Exec(ctx, `DELETE FROM `+tbl)
		wantDenied(t, "DELETE "+tbl, err)
	}
	for _, q := range []string{
		`UPDATE hospitals SET name = 'x'`, `UPDATE hospitals SET short_code = 'x'`,
		`UPDATE hospitals SET time_zone = 'UTC'`, `UPDATE hospital_settings SET allow_trust_device = true`,
		`UPDATE users SET is_active = false`, `UPDATE user_roles SET role = 'clinician'`,
		`UPDATE audit_log SET action = 'x'`,
	} {
		_, err := prov.Exec(ctx, q)
		wantDenied(t, q, err)
	}
}

func TestProvisionInsertPoliciesHold(t *testing.T) {
	ctx, prov := provPool(t)
	// An active hospital, a user who need not change the password, and an
	// audit row for an action the CLI never takes are all refused.
	_, err := prov.Exec(ctx, `INSERT INTO hospitals (name, short_code, status) VALUES ('Synthetic', 'PRV1', 'active')`)
	wantDenied(t, "active hospital", err)
	_, err = prov.Exec(ctx, `INSERT INTO users (email, display_name, must_change_password) VALUES ('p@x.example', 'P', false)`)
	wantDenied(t, "user without must_change_password", err)
	_, err = prov.Exec(ctx, `INSERT INTO users (email, display_name, must_change_password, is_active) VALUES ('p@x.example', 'P', true, true)`)
	wantDenied(t, "user setting is_active", err)
	_, err = prov.Exec(ctx, `INSERT INTO audit_log (action, prev_hash, hash) VALUES ('login.ok', 'a', 'b')`)
	wantDenied(t, "foreign audit action", err)
	_, err = prov.Exec(ctx, `INSERT INTO audit_log (action, actor_id, prev_hash, hash) VALUES ('user.created', gen_random_uuid(), 'a', 'b2')`)
	wantDenied(t, "audit with actor", err)
}

// 00010: mhai_provision may move a hospital from setup_incomplete to active
// and nothing else: not to another status, not from another status (rows
// it can't update are simply not matched), and only the status column.
func TestProvisionActivatesOnlySetupIncomplete(t *testing.T) {
	_, super := shared(t)
	ctx, prov := provPool(t)
	mk := func(code, status string) string {
		t.Helper()
		var id string
		if err := super.QueryRow(ctx, `INSERT INTO hospitals (name, short_code, status) VALUES ('Synthetic', $1, $2) RETURNING id::text`,
			code, status).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	status := func(id string) string {
		t.Helper()
		var s string
		if err := super.QueryRow(ctx, `SELECT status FROM hospitals WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	setup := mk("PRA1", "setup_incomplete")
	for _, to := range []string{"deactivated", "removal_pending", "removed", "setup_incomplete"} {
		_, err := prov.Exec(ctx, `UPDATE hospitals SET status = $1 WHERE id = $2`, to, setup)
		wantDenied(t, "setup_incomplete -> "+to, err)
	}
	_, err := prov.Exec(ctx, `UPDATE hospitals SET status = 'active', name = 'x' WHERE id = $1`, setup)
	wantDenied(t, "status and name", err)
	for i, from := range []string{"active", "deactivated", "removal_pending", "removal_scheduled", "removed"} {
		id := mk(fmt.Sprintf("PRB%d", i), from)
		for _, to := range []string{"active", "deactivated", "setup_incomplete"} {
			tag, err := prov.Exec(ctx, `UPDATE hospitals SET status = $1 WHERE id = $2`, to, id)
			if err == nil && tag.RowsAffected() != 0 {
				t.Errorf("%s -> %s: %d rows", from, to, tag.RowsAffected())
			}
		}
		if s := status(id); s != from {
			t.Errorf("%s became %s", from, s)
		}
	}
	tag, err := prov.Exec(ctx, `UPDATE hospitals SET status = 'active' WHERE id = $1`, setup)
	if err != nil || tag.RowsAffected() != 1 || status(setup) != "active" {
		t.Fatalf("setup_incomplete -> active: %v %v", tag, err)
	}
	// Its audit action, still with no actor.
	_, err = prov.Exec(ctx, `INSERT INTO audit_log (action, actor_id, prev_hash, hash) VALUES ('hospital.activated', gen_random_uuid(), 'a', 'b3')`)
	wantDenied(t, "hospital.activated with actor", err)
	_, err = prov.Exec(ctx, `INSERT INTO audit_log (action, prev_hash, hash) VALUES ('hospital.deactivated', 'a', 'b4')`)
	wantDenied(t, "hospital.deactivated", err)
}
