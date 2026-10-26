//go:build integration

package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/gateway/internal/testutil"
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

func TestProvisionReadsOnlyHospitalIDAndCode(t *testing.T) {
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
	if _, err := prov.Exec(ctx, `SELECT id, short_code FROM hospitals`); err != nil {
		t.Errorf("select id, short_code: %v", err)
	}
}

func TestProvisionCannotUpdateOrDelete(t *testing.T) {
	ctx, prov := provPool(t)
	for _, tbl := range []string{"hospitals", "hospital_settings", "users", "user_roles", "audit_log"} {
		_, err := prov.Exec(ctx, `DELETE FROM `+tbl)
		wantDenied(t, "DELETE "+tbl, err)
	}
	for _, q := range []string{
		`UPDATE hospitals SET name = 'x'`, `UPDATE hospital_settings SET allow_trust_device = true`,
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
