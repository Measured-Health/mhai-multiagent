//go:build integration

package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

func appLogin(t *testing.T) (*testutil.PG, *pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	pg, super := shared(t)
	app, err := testutil.AppLogin(context.Background(), pg, super)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return pg, super, app
}

// The sweep deletes expired Idempotency-Key rows of every hospital and the
// hospital-less ones, and nothing else, run by the core service's own
// login (as mhai_auth, through a SECURITY DEFINER function).
func TestSweepIdempotencyDeletesExpiredOnly(t *testing.T) {
	_, super, app := appLogin(t)
	ctx := context.Background()
	var h1, h2 uuid.UUID
	for _, h := range []*uuid.UUID{&h1, &h2} {
		if err := super.QueryRow(ctx, `INSERT INTO hospitals (name, short_code) VALUES ('Synthetic Sweep', $1) RETURNING id`,
			"SW"+strings.ToUpper(uuid.NewString()[:6])).Scan(h); err != nil {
			t.Fatal(err)
		}
	}
	scope := "sweep-" + uuid.NewString()
	ins := `INSERT INTO idempotency_keys (scope, key, hospital_id, request_hash, expires_at) VALUES ($1, $2, $3, 'h', now() + $4::interval)`
	for _, r := range []struct {
		key string
		hid any
		in  string
	}{
		{"old-h1", h1, "-1 minute"}, {"old-h2", h2, "-3 days"}, {"old-none", nil, "-1 second"},
		{"live-h1", h1, "1 hour"}, {"live-none", nil, "72 hours"},
	} {
		if _, err := super.Exec(ctx, ins, scope, r.key, r.hid, r.in); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SweepIdempotency(ctx, app); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	rows, err := super.Query(ctx, `SELECT key FROM idempotency_keys WHERE scope = $1 ORDER BY key`, scope)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		left = append(left, k)
	}
	rows.Close()
	if strings.Join(left, ",") != "live-h1,live-none" {
		t.Fatalf("left after sweep: %v", left)
	}
}

// Only mhai_auth may run the sweep function: not PUBLIC, the provisioning
// login, the clinical role or the workers.
func TestSweepFunctionLeastPrivilege(t *testing.T) {
	pg, super, app := appLogin(t)
	ctx := context.Background()
	prov, _, err := testutil.ProvisionLogin(ctx, pg, super)
	if err != nil {
		t.Fatal(err)
	}
	defer prov.Close()
	_, err = prov.Exec(ctx, `SELECT mhai_sweep_idempotency(10)`)
	requireDenied(t, err, "provisioning login")
	for _, role := range []string{"mhai_gateway", "mhai_platform"} {
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, `SELECT mhai_sweep_idempotency(10)`)
		requireDenied(t, err, role)
		_ = tx.Rollback(ctx)
	}
	var public bool
	if err := super.QueryRow(ctx, `SELECT has_function_privilege('public', 'mhai_sweep_idempotency(integer)', 'EXECUTE')`).Scan(&public); err != nil {
		t.Fatal(err)
	}
	if public {
		t.Fatal("PUBLIC can run the sweep")
	}
}

// EnsureLogin creates (or resets) a login that is a member of exactly one
// app role; the core service's check accepts the mhai_app one and refuses
// a login with any other membership.
func TestEnsureLoginAndMembershipCheck(t *testing.T) {
	pg, super := shared(t)
	ctx := context.Background()
	name := "core_login_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	pw := "synthetic-login-password-" + uuid.NewString()
	for i := 0; i < 2; i++ { // twice: the second run resets, never duplicates
		if err := db.EnsureLogin(ctx, super, name, pw, "mhai_app"); err != nil {
			t.Fatalf("ensure (run %d): %v", i+1, err)
		}
	}
	pool := connectAs(t, pg, name, pw)
	if err := db.CheckLoginRole(ctx, pool); err != nil {
		t.Fatalf("ensured login refused: %v", err)
	}
	// A membership granted later, even without inheritance, is refused.
	if _, err := super.Exec(ctx, `GRANT mhai_provision TO `+name+` WITH INHERIT FALSE`); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckLoginRole(ctx, pool); err == nil || !strings.Contains(err.Error(), "mhai_provision") {
		t.Fatalf("extra membership accepted: %v", err)
	}
	// Ensuring again takes the extra membership away.
	if err := db.EnsureLogin(ctx, super, name, pw, "mhai_app"); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckLoginRole(ctx, pool); err != nil {
		t.Fatalf("after reconcile: %v", err)
	}
	pool.Close()

	prov := "prov_login_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	if err := db.EnsureLogin(ctx, super, prov, pw, "mhai_provision"); err != nil {
		t.Fatal(err)
	}
	pp := connectAs(t, pg, prov, pw)
	defer pp.Close()
	if err := db.CheckLoginRole(ctx, pp); err == nil {
		t.Fatal("the core service accepted the provisioning login")
	}
	for _, bad := range []struct{ name, role string }{
		{"mhai_gateway", "mhai_app"}, {"Bad-Name", "mhai_app"}, {"pg_x", "mhai_app"},
		{"ok_name", "mhai_migrate"}, {"ok_name", "mhai_gateway"},
	} {
		if err := db.EnsureLogin(ctx, super, bad.name, pw, bad.role); err == nil {
			t.Errorf("EnsureLogin(%q, %q) accepted", bad.name, bad.role)
		}
	}
}

func connectAs(t *testing.T, pg *testutil.PG, user, pw string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User, cfg.ConnConfig.Password = user, pw
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
