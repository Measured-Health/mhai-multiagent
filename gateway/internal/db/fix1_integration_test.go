//go:build integration

package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/gateway/internal/db"
)

// loginPool creates a LOGIN role with the given attributes (as superuser)
// and returns a pool connected as it.
func loginPool(t *testing.T, attrs string) (name string, p *pgxpool.Pool) {
	t.Helper()
	pg, pool := shared(t)
	ctx := context.Background()
	name = "login_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := pool.Exec(ctx, `CREATE ROLE `+name+` LOGIN PASSWORD 'test-only-login-password' `+attrs); err != nil {
		t.Fatalf("create login: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User = name
	cfg.ConnConfig.Password = "test-only-login-password"
	p, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return name, p
}

func seedUser(t *testing.T, pool *pgxpool.Pool, hospital *uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (hospital_id, email, password_hash, display_name) VALUES ($1, $2, 'synthetic-hash', 'Synthetic User') RETURNING id`,
		hospital, uniqueCode("u")+"@example.test").Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// I1: hospitals.status takes exactly the contract's values.
func TestHospitalStatusValues(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for _, st := range []string{"active", "setup_incomplete", "deactivated", "removal_pending", "removal_scheduled", "removed"} {
		if _, err := pool.Exec(ctx, `INSERT INTO hospitals (name, short_code, status) VALUES ('Synthetic', $1, $2)`, uniqueCode("S"), st); err != nil {
			t.Errorf("status %s: %v", st, err)
		}
	}
	for _, st := range []string{"suspended", "Active", ""} {
		_, err := pool.Exec(ctx, `INSERT INTO hospitals (name, short_code, status) VALUES ('Synthetic', $1, $2)`, uniqueCode("S"), st)
		if pgCode(err) != "23514" {
			t.Errorf("status %q: want check violation, got %v", st, err)
		}
	}
}

// I2: idempotency keys are separated by hospital; auth and platform see
// only rows with no hospital.
func TestIdempotencyKeysRLS(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("A"))
	b, _ := seedHospital(t, pool, uniqueCode("B"))
	keyA, keyB, keyNull := uniqueCode("ka"), uniqueCode("kb"), uniqueCode("kn")
	if _, err := pool.Exec(ctx, `INSERT INTO idempotency_keys (scope, key, request_hash, hospital_id) VALUES
		('s', $1, 'h', $4), ('s', $2, 'h', $5), ('s', $3, 'h', NULL)`, keyA, keyB, keyNull, a, b); err != nil {
		t.Fatalf("seed keys: %v", err)
	}
	keys := []string{keyA, keyB, keyNull}
	visible := func(tx pgx.Tx) (map[string]bool, error) {
		rows, err := tx.Query(ctx, `SELECT key FROM idempotency_keys WHERE key = ANY($1)`, keys)
		if err != nil {
			return nil, err
		}
		got := map[string]bool{}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				return nil, err
			}
			got[k] = true
		}
		return got, rows.Err()
	}

	err := db.WithHospital(ctx, pool, a, func(tx pgx.Tx) error {
		got, err := visible(tx)
		if err != nil {
			return err
		}
		if !got[keyA] || got[keyB] || got[keyNull] {
			t.Errorf("gateway(A) sees %v, want only A's key", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []*uuid.UUID{&b, nil} {
		err := db.WithHospital(ctx, pool, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO idempotency_keys (scope, key, request_hash, hospital_id) VALUES ('s', $1, 'h', $2)`, uniqueCode("k"), h)
			return err
		})
		requireDenied(t, err, "gateway(A) writing a key for another hospital or none")
	}

	for name, with := range map[string]func(context.Context, *pgxpool.Pool, func(pgx.Tx) error) error{"auth": db.WithAuth, "platform": db.WithPlatform} {
		err := with(ctx, pool, func(tx pgx.Tx) error {
			got, err := visible(tx)
			if err != nil {
				return err
			}
			if got[keyA] || got[keyB] || !got[keyNull] {
				t.Errorf("%s sees %v, want only the null-hospital key", name, got)
			}
			_, err = tx.Exec(ctx, `INSERT INTO idempotency_keys (scope, key, request_hash) VALUES ('s', $1, 'h')`, uniqueCode("k"))
			return err
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		err = with(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO idempotency_keys (scope, key, request_hash, hospital_id) VALUES ('s', $1, 'h', $2)`, uniqueCode("k"), a)
			return err
		})
		requireDenied(t, err, name+" writing a hospital key")
	}
}

// I3: the gateway can attach only clinical roles, and every writer must
// pair platform roles with platform accounts.
func TestUserRolesPairing(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("A"))
	clinical := seedUser(t, pool, &a)
	platform := seedUser(t, pool, nil)

	for _, role := range []string{"platform_admin", "auditor"} {
		err := db.WithHospital(ctx, pool, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, clinical, role)
			return err
		})
		requireDenied(t, err, "gateway granting "+role)
	}
	err := db.WithHospital(ctx, pool, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'clinician')`, clinical)
		return err
	})
	if err != nil {
		t.Fatalf("gateway granting clinician: %v", err)
	}

	// The trigger binds every writer, including the owner connection.
	for _, c := range []struct {
		user uuid.UUID
		role string
	}{{clinical, "platform_admin"}, {clinical, "auditor"}, {platform, "clinician"}, {platform, "hospital_admin"}, {platform, "analyst"}} {
		_, err := pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, c.user, c.role)
		if pgCode(err) != "23514" {
			t.Errorf("%s on the wrong kind of account: want 23514, got %v", c.role, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'platform_admin')`, platform); err != nil {
		t.Fatalf("platform_admin on a platform account: %v", err)
	}
	// Moving an account across the line is refused too.
	if _, err := pool.Exec(ctx, `UPDATE users SET hospital_id = NULL WHERE id = $1`, clinical); pgCode(err) != "23514" {
		t.Errorf("clinician account losing its hospital: want 23514, got %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET hospital_id = $1 WHERE id = $2`, a, platform); pgCode(err) != "23514" {
		t.Errorf("platform account gaining a hospital: want 23514, got %v", err)
	}
}

// I4: mhai_auth reads accounts and writes only what sign-in needs.
func TestAuthRoleNarrow(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("A"))
	if _, err := pool.Exec(ctx, `INSERT INTO hospital_settings (hospital_id) VALUES ($1)`, a); err != nil {
		t.Fatal(err)
	}
	u := seedUser(t, pool, &a)
	denied := map[string][]any{
		`INSERT INTO users (hospital_id, email, display_name) VALUES ($1, 'x@example.test', 'x')`: {a},
		`INSERT INTO user_roles (user_id, role) VALUES ($1, 'clinician')`:                         {u},
		`UPDATE user_roles SET role = 'analyst' WHERE user_id = $1`:                               {u},
		`DELETE FROM user_roles WHERE user_id = $1`:                                               {u},
		`UPDATE hospital_settings SET allow_trust_device = true WHERE hospital_id = $1`:           {a},
		`UPDATE users SET hospital_id = NULL WHERE id = $1`:                                       {u},
		`UPDATE users SET email = 'y@example.test' WHERE id = $1`:                                 {u},
		`UPDATE users SET is_active = false WHERE id = $1`:                                        {u},
		`DELETE FROM user_mfa WHERE user_id = $1`:                                                 {u},
		`DELETE FROM users WHERE id = $1`:                                                         {u},
	}
	for q, args := range denied {
		err := db.WithAuth(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, args...)
			return err
		})
		requireDenied(t, err, "auth: "+q)
	}
	err := db.WithAuth(ctx, pool, func(tx pgx.Tx) error {
		for _, q := range []string{
			`UPDATE users SET last_login_at = now(), password_hash = 'h2', must_change_password = false, terms_version = 'v1', terms_accepted_at = now() WHERE id = $1`,
			`INSERT INTO user_mfa (user_id, secret_enc) VALUES ($1, '\x00')`,
			`UPDATE user_mfa SET enabled = true, last_used_step = 1, backup_code_hashes = '{}', secret_enc = '\x01' WHERE user_id = $1`,
			`INSERT INTO sessions (user_id) VALUES ($1)`,
			`INSERT INTO trusted_devices (user_id, token_hash) VALUES ($1, gen_random_uuid()::text)`,
			`DELETE FROM trusted_devices WHERE user_id = $1`,
			`DELETE FROM sessions WHERE user_id = $1`,
		} {
			if _, err := tx.Exec(ctx, q, u); err != nil {
				return err
			}
		}
		for _, q := range []string{`SELECT count(*) FROM user_roles`, `SELECT count(*) FROM hospital_settings`, `SELECT count(*) FROM hospitals`} {
			var n int
			if err := tx.QueryRow(ctx, q).Scan(&n); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("auth allowed writes: %v", err)
	}
}

// I5: the startup check refuses any login that could see past RLS.
func TestCheckLoginRole(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()

	err := db.CheckLoginRole(ctx, pool)
	if err == nil || !strings.Contains(err.Error(), "superuser") {
		t.Fatalf("superuser login: want refusal naming superuser, got %v", err)
	}

	_, good := loginPool(t, "IN ROLE mhai_app")
	if err := db.CheckLoginRole(ctx, good); err != nil {
		t.Fatalf("mhai_app member: %v", err)
	}

	_, bypass := loginPool(t, "BYPASSRLS IN ROLE mhai_app")
	if err := db.CheckLoginRole(ctx, bypass); err == nil || !strings.Contains(err.Error(), "BYPASSRLS") {
		t.Fatalf("bypassrls login: got %v", err)
	}

	_, outsider := loginPool(t, "")
	if err := db.CheckLoginRole(ctx, outsider); err == nil || !strings.Contains(err.Error(), "mhai_app") {
		t.Fatalf("non-member login: got %v", err)
	}

	owner, owning := loginPool(t, "IN ROLE mhai_app")
	if _, err := pool.Exec(ctx, `CREATE TABLE owned_`+owner+` (x int); ALTER TABLE owned_`+owner+` OWNER TO `+owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DROP TABLE owned_`+owner) })
	if err := db.CheckLoginRole(ctx, owning); err == nil || !strings.Contains(err.Error(), "owns") {
		t.Fatalf("table-owning login: got %v", err)
	}
}

// RESET ROLE inside a request transaction drops back to the login user,
// which can read nothing; it can't escalate.
func TestResetRoleCannotEscalate(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("A"))
	name, app := loginPool(t, "IN ROLE mhai_app")
	for _, reset := range []string{`RESET ROLE`, `SET LOCAL ROLE NONE`, `SET LOCAL ROLE mhai_app`} {
		err := db.WithHospital(ctx, app, a, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, reset); err != nil {
				return err
			}
			var who string
			if err := tx.QueryRow(ctx, `SELECT current_user`).Scan(&who); err != nil {
				return err
			}
			if who != name && who != "mhai_app" {
				t.Errorf("%s: current_user = %s", reset, who)
			}
			var n int
			return tx.QueryRow(ctx, `SELECT count(*) FROM clinical_queries`).Scan(&n)
		})
		requireDenied(t, err, "after "+reset)
	}
}
