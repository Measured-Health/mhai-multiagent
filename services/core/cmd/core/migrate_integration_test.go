//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

func TestMigrateUpCommand(t *testing.T) {
	pg := testutil.StartPostgres(t)
	var out bytes.Buffer
	if err := migrate(context.Background(), []string{"up"}, pg.URL, &out); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if got := strings.Count(out.String(), "applied"); got != 35 { // 00001-00033, 00035, 00038
		t.Fatalf("want 35 applied migrations, got output:\n%s", out.String())
	}
}

func TestServeRefusesSuperuserLogin(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := migrate(ctx, []string{"up"}, pg.URL, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	pool, err := openDB(ctx, pg.URL)
	if err == nil {
		pool.Close()
		t.Fatal("serve accepted a superuser login")
	}
	if !strings.Contains(err.Error(), "superuser") || !strings.Contains(err.Error(), "mhai_app") {
		t.Fatalf("unclear refusal: %v", err)
	}
}

// core migrate up provisions the pipeline's login: a member of
// mhai_worker_app only, which holds mhai_worker without inheriting it.
func TestMigrateUpProvisionsIntelligenceLogin(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	env := map[string]string{"INTELLIGENCE_DB_PASSWORD": "test-only-intelligence-db-password"}
	var out bytes.Buffer
	if err := migrateWithLogins(ctx, []string{"up"}, pg.URL, func(k string) string { return env[k] }, &out); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if !strings.Contains(out.String(), "login intelligence_app ready (member of mhai_worker_app only)") {
		t.Fatalf("output:\n%s", out.String())
	}
	if strings.Contains(out.String(), env["INTELLIGENCE_DB_PASSWORD"]) {
		t.Fatal("output carries the password")
	}
	pool, err := pgxpool.New(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var member, usesWorker bool
	if err := pool.QueryRow(ctx, `SELECT pg_has_role('intelligence_app', 'mhai_worker_app', 'MEMBER'),
		pg_has_role('intelligence_app', 'mhai_worker', 'USAGE')`).Scan(&member, &usesWorker); err != nil {
		t.Fatal(err)
	}
	if !member || usesWorker {
		t.Fatalf("intelligence_app: member of mhai_worker_app %v, uses mhai_worker %v", member, usesWorker)
	}
}

// core migrate up provisions the emr service's login (00020): a member of
// mhai_emr_app only, which holds mhai_emr without inheriting it.
func TestMigrateUpProvisionsEMRLogin(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	env := map[string]string{"EMR_DB_PASSWORD": "test-only-emr-db-password"}
	var out bytes.Buffer
	if err := migrateWithLogins(ctx, []string{"up"}, pg.URL, func(k string) string { return env[k] }, &out); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if !strings.Contains(out.String(), "login emr_app ready (member of mhai_emr_app only)") {
		t.Fatalf("output:\n%s", out.String())
	}
	if strings.Contains(out.String(), env["EMR_DB_PASSWORD"]) {
		t.Fatal("output carries the password")
	}
	pool, err := pgxpool.New(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var member, usesEMR, setEMR bool
	if err := pool.QueryRow(ctx, `SELECT pg_has_role('emr_app', 'mhai_emr_app', 'MEMBER'),
		pg_has_role('emr_app', 'mhai_emr', 'USAGE'), pg_has_role('emr_app', 'mhai_emr', 'SET')`).Scan(&member, &usesEMR, &setEMR); err != nil {
		t.Fatal(err)
	}
	if !member || usesEMR || !setEMR {
		t.Fatalf("emr_app: member of mhai_emr_app %v, uses mhai_emr %v, may SET mhai_emr %v", member, usesEMR, setEMR)
	}
}
