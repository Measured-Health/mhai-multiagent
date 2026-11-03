//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

func TestHospitalsActivateCommand(t *testing.T) {
	pg, pool := testutil.MigratedPostgres(t)
	ctx := context.Background()
	_, provURL, err := testutil.ProvisionLogin(ctx, pg, pool)
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := seed(ctx, args, provURL, strings.NewReader(""), &out)
		return out.String(), err
	}
	if _, err := run("hospitals", "create", "--name", "Synthetic General", "--short-code", "HAV", "--time-zone", "UTC"); err != nil {
		t.Fatal(err)
	}
	out, err := run("hospitals", "activate", "hav")
	if err != nil || !strings.HasPrefix(out, "activated hospital ") {
		t.Fatalf("activate: %q %v", out, err)
	}
	out, err = run("hospitals", "activate", "HAV")
	if err != nil || !strings.Contains(out, "already active; nothing changed") {
		t.Fatalf("again: %q %v", out, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hospitals SET status = 'deactivated' WHERE short_code = 'HAV'`); err != nil {
		t.Fatal(err)
	}
	if _, err := run("hospitals", "activate", "HAV"); !errors.Is(err, account.ErrNotActivatable) ||
		!strings.Contains(err.Error(), "deactivated") {
		t.Fatalf("deactivated: %v", err)
	}
	if _, err := run("hospitals", "activate", "NOPE"); !errors.Is(err, account.ErrNoSuchHospital) {
		t.Fatalf("unknown: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'hospital.activated'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d hospital.activated rows (%v), want 1", n, err)
	}
}
