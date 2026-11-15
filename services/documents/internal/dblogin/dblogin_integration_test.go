//go:build integration

package dblogin_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/dblogin"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/testpg"
)

// Departure 10: the documents login as core migrate makes it passes; a
// superuser, a login that can act as the tables' owner, one with another
// service's role, and one without mhai_documents are refused by code.
func TestCheckOnRealLogins(t *testing.T) {
	url, super := testpg.Migrated(t)
	ctx := context.Background()
	open := func(u string) *pgxpool.Pool {
		p, err := pgxpool.New(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}
	if err := dblogin.Check(ctx, open(testpg.Login(t, url, super, "documents_app", "mhai_documents_app"))); err != nil {
		t.Fatalf("documents_app: %v", err)
	}
	for name, tc := range map[string]struct {
		pool   *pgxpool.Pool
		reason string
	}{
		"superuser":        {super, "superuser"},
		"migrate member":   {open(testpg.Login(t, url, super, "docs_owner", "mhai_documents_app", "mhai_migrate")), "migrate_member"},
		"gateway too":      {open(testpg.Login(t, url, super, "docs_gateway", "mhai_documents_app", "mhai_gateway")), "other_roles"},
		"emr's login role": {open(testpg.Login(t, url, super, "docs_emr", "mhai_emr_app")), "other_roles"},
		"no documents":     {open(testpg.Login(t, url, super, "docs_nothing")), "not_documents"},
	} {
		err := dblogin.Check(ctx, tc.pool)
		var pe *dblogin.PrivilegedError
		if !errors.As(err, &pe) || pe.Reason != tc.reason {
			t.Errorf("%s: %v, want %s", name, err, tc.reason)
		}
	}
	// A database that doesn't answer: unavailable, not privileged.
	cfg, err := pgxpool.ParseConfig("postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	down, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer down.Close()
	if err := dblogin.Check(ctx, down); !errors.Is(err, dblogin.ErrUnavailable) {
		t.Fatalf("down: %v", err)
	}
}
