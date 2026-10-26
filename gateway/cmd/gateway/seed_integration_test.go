//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/gateway/internal/testutil"
)

func TestSeedCreatesHospitalAndUsers(t *testing.T) {
	pg, pool := testutil.MigratedPostgres(t)
	_, provURL, err := testutil.ProvisionLogin(context.Background(), pg, pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var out bytes.Buffer
	if err := seed(ctx, []string{"hospitals", "create", "--name", "Havana General", "--short-code", "hav", "--time-zone", "Africa/Lagos",
		"--require-mfa-for-clinicians"}, provURL, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	pw := strings.NewReader("tangerine-orbit-77-quill\n")
	if err := seed(ctx, []string{"users", "create", "--email", " Grace.Nwosu@Havana.example ", "--name", "Grace Nwosu",
		"--roles", "clinician,hospital_admin", "--hospital", "HAV"}, provURL, pw, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(out.String()), "grace") || strings.Contains(out.String(), "tangerine") {
		t.Fatalf("output echoes input: %s", out.String())
	}
	var roles int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM user_roles r JOIN users u ON u.id=r.user_id WHERE u.email='grace.nwosu@havana.example' AND u.must_change_password`).Scan(&roles)
	if roles != 2 {
		t.Fatalf("roles = %d", roles)
	}
	// A weak password is refused without creating anything.
	err = seed(ctx, []string{"users", "create", "--email", "ops@mh.example", "--name", "Ops", "--roles", "platform_admin"},
		provURL, strings.NewReader("password1234\n"), &out)
	if err == nil || strings.Contains(err.Error(), "password1234") {
		t.Fatalf("weak password: %v", err)
	}
}
