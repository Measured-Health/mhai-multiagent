//go:build integration

package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
)

// scramMarker starts every SCRAM-SHA-256 verifier EnsureLogin builds.
const scramMarker = "SCRAM-SHA-256$"

// createRoleOwner makes a non-superuser login with CREATEROLE that may
// grant mhai_app and mhai_provision: the migrate login on managed Postgres
// (services/core/README.md), which can't change its own log settings.
func createRoleOwner(t *testing.T, super *pgxpool.Pool) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	name := "owner_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	pw := "synthetic-owner-password-" + uuid.NewString()
	if _, err := super.Exec(ctx, `CREATE ROLE `+name+` LOGIN NOSUPERUSER CREATEROLE PASSWORD '`+pw+`'`); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `GRANT mhai_app, mhai_provision TO `+name+` WITH ADMIN OPTION`); err != nil {
		t.Fatal(err)
	}
	pg, _ := shared(t)
	p := connectAs(t, pg, name, pw)
	t.Cleanup(p.Close)
	return p, name
}

// serverLogHasVerifier reports whether the Postgres server log holds any
// SCRAM verifier. Nothing in this package's tests writes one on purpose:
// the logins the tests make with plain passwords never contain the marker.
func serverLogHasVerifier(t *testing.T) (bool, string) {
	t.Helper()
	pg, _ := shared(t)
	logs, err := pg.Logs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(logs, "\n") {
		if i := strings.Index(line, scramMarker); i >= 0 {
			// Only what comes before the verifier, so the test output never
			// carries one either.
			return true, line[:i] + "<verifier>"
		}
	}
	return false, ""
}

// A CREATEROLE owner (not a superuser) can run EnsureLogin again on a login
// it made: the second run (every deploy after the first) must not send
// attributes only a superuser may name, such as NOBYPASSRLS.
func TestEnsureLoginRerunsAsCreateRoleOwner(t *testing.T) {
	pg, _ := shared(t)
	_, super := shared(t)
	owner, _ := createRoleOwner(t, super)
	ctx := context.Background()
	name := "core_login_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	pw := "synthetic-login-password-" + uuid.NewString()
	for i := 0; i < 2; i++ {
		if err := db.EnsureLogin(ctx, owner, name, pw, "mhai_app"); err != nil {
			t.Fatalf("ensure (run %d): %v", i+1, err)
		}
	}
	pw2 := "synthetic-login-password-" + uuid.NewString()
	if err := db.EnsureLogin(ctx, owner, name, pw2, "mhai_app"); err != nil {
		t.Fatalf("ensure with a new password: %v", err)
	}
	p := connectAs(t, pg, name, pw2)
	defer p.Close()
	if err := db.CheckLoginRole(ctx, p); err != nil {
		t.Fatalf("ensured login refused: %v", err)
	}
	old := connectAs(t, pg, name, pw)
	defer old.Close()
	if err := old.Ping(ctx); err == nil {
		t.Fatal("the old password still signs in")
	}
	if found, line := serverLogHasVerifier(t); found {
		t.Fatalf("a SCRAM verifier is in the server log: %q", line)
	}
}

// When EnsureLogin fails because the migrate login may not change an
// existing role (a CREATEROLE owner without ADMIN on it), the error is
// returned and the server log, which records failed statements
// (log_min_error_statement), holds no verifier.
func TestEnsureLoginFailureLeavesNoVerifierInServerLog(t *testing.T) {
	_, super := shared(t)
	owner, _ := createRoleOwner(t, super)
	ctx := context.Background()
	// A login the owner did not create, so it has no ADMIN on it.
	name := "foreign_login_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	if _, err := super.Exec(ctx, `CREATE ROLE `+name+` LOGIN IN ROLE mhai_app`); err != nil {
		t.Fatal(err)
	}
	pw := "synthetic-login-password-" + uuid.NewString()
	err := db.EnsureLogin(ctx, owner, name, pw, "mhai_app")
	if err == nil {
		t.Fatal("EnsureLogin changed a role the owner may not change")
	}
	if strings.Contains(err.Error(), scramMarker) || strings.Contains(err.Error(), pw) {
		t.Fatalf("error carries the secret: %v", err)
	}
	if found, line := serverLogHasVerifier(t); found {
		t.Fatalf("a SCRAM verifier is in the server log: %q", line)
	}
}

// Even when the statement that carries the verifier fails on the server,
// as a superuser (the compose owner), the server log doesn't record it:
// EnsureLogin turns log_min_error_statement off for its transaction. The
// failure is forced by breaking that one statement's syntax.
func TestEnsureLoginPasswordStatementFailureNotLogged(t *testing.T) {
	_, super := shared(t)
	ctx := context.Background()
	name := "core_login_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	pw := "synthetic-login-password-" + uuid.NewString()
	if err := db.EnsureLogin(ctx, super, name, pw, "mhai_app"); err != nil {
		t.Fatal(err)
	}
	db.SetTestPasswordSQL(func(s string) string { return s + " NOT VALID SYNTAX" })
	t.Cleanup(func() { db.SetTestPasswordSQL(nil) })
	err := db.EnsureLogin(ctx, super, name, pw+"-new", "mhai_app")
	if err == nil || !strings.Contains(err.Error(), "setting the password failed") {
		t.Fatalf("want the password statement to fail, got %v", err)
	}
	if strings.Contains(err.Error(), scramMarker) {
		t.Fatalf("error carries the verifier: %v", err)
	}
	db.SetTestPasswordSQL(nil)
	if found, line := serverLogHasVerifier(t); found {
		t.Fatalf("a SCRAM verifier is in the server log: %q", line)
	}
	// The transaction rolled back: the old password still signs in.
	pg, _ := shared(t)
	p := connectAs(t, pg, name, pw)
	defer p.Close()
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("old password after the failed reset: %v", err)
	}
}
