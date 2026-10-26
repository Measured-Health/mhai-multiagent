//go:build integration

package db_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/gateway/internal/db"
)

// N1: a login that also holds a stronger role, directly or through another
// role, is refused.
func TestCheckLoginRoleRefusesInheritedRoles(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for _, strong := range []string{"mhai_migrate", "mhai_platform", "mhai_auth", "mhai_gateway", "mhai_worker"} {
		_, direct := loginPool(t, "IN ROLE mhai_app, "+strong)
		if err := db.CheckLoginRole(ctx, direct); err == nil || !strings.Contains(err.Error(), strong) {
			t.Errorf("login IN ROLE mhai_app, %s: want refusal naming it, got %v", strong, err)
		}
	}
	for _, strong := range []string{"mhai_migrate", "mhai_platform"} {
		mid := "mid_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
		if _, err := pool.Exec(ctx, `CREATE ROLE `+mid+` NOLOGIN IN ROLE `+strong); err != nil {
			t.Fatal(err)
		}
		_, indirect := loginPool(t, "IN ROLE mhai_app, "+mid)
		if err := db.CheckLoginRole(ctx, indirect); err == nil || !strings.Contains(err.Error(), strong) {
			t.Errorf("login holding %s through %s: want refusal naming it, got %v", strong, mid, err)
		}
	}
}

// N2: a login that owns the database or any schema is refused.
func TestCheckLoginRoleRefusesDatabaseAndSchemaOwners(t *testing.T) {
	pg, pool := shared(t)
	ctx := context.Background()

	owner, owning := loginPool(t, "IN ROLE mhai_app")
	if _, err := pool.Exec(ctx, `CREATE SCHEMA s_`+owner+` AUTHORIZATION `+owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DROP SCHEMA s_`+owner+` CASCADE`) })
	if err := db.CheckLoginRole(ctx, owning); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Errorf("schema-owning login: got %v", err)
	}

	dbOwner, _ := loginPool(t, "IN ROLE mhai_app")
	dbName := "d_" + dbOwner
	if _, err := pool.Exec(ctx, `CREATE DATABASE `+dbName+` OWNER `+dbOwner); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User = dbOwner
	cfg.ConnConfig.Password = "test-only-login-password"
	cfg.ConnConfig.Database = dbName
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	err = db.CheckLoginRole(ctx, p)
	p.Close()
	_, _ = pool.Exec(ctx, `DROP DATABASE `+dbName+` WITH (FORCE)`)
	if err == nil || !strings.Contains(err.Error(), "database") {
		t.Errorf("database-owning login: got %v", err)
	}
}

// N3: a role insert racing a hospital change on the same account can't
// both pass. The insert waits for the update and then sees the new
// hospital.
func TestRolePairingRace(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("A"))
	platform := seedUser(t, pool, nil)

	t1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = t1.Rollback(ctx) }()
	if _, err := t1.Exec(ctx, `UPDATE users SET hospital_id = $1 WHERE id = $2`, a, platform); err != nil {
		t.Fatalf("t1 update: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'platform_admin')`, platform)
		done <- err
	}()

	// Wait until the insert is blocked on a lock (or has already finished).
	var t2err error
	finished := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		select {
		case t2err = <-done:
			finished = true
		default:
		}
		if finished {
			break
		}
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE 'INSERT INTO user_roles%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := t1.Commit(ctx); err != nil {
		t.Fatalf("t1 commit: %v", err)
	}
	if !finished {
		t2err = <-done
	}
	if pgCode(t2err) != "23514" {
		t.Fatalf("racing platform_admin insert: want 23514, got %v", t2err)
	}
}
