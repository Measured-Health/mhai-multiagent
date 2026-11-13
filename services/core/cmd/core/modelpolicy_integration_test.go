//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// K6: the CLI twin sets a hospital's policy as mhai_provision and audits
// hospital.model_policy_changed with via cli, no actor and no hospital; an
// unknown or disabled provider is refused with nothing written; none
// switches off. It prints the hospital id and the provider only.
func TestCLIModelPolicySet(t *testing.T) {
	pg, pool := testutil.MigratedPostgres(t)
	ctx := context.Background()
	_, provURL, err := testutil.ProvisionLogin(ctx, pg, pool)
	if err != nil {
		t.Fatal(err)
	}
	env := envOf(map[string]string{"CORE_CLOUD_PROVIDERS": "anthropic,openai"})
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := hospitalsModelPolicy(ctx, args, provURL, env, &out)
		return out.String(), err
	}
	if err := seed(ctx, []string{"hospitals", "create", "--name", "Synthetic General", "--short-code", "HAV", "--time-zone", "UTC"},
		provURL, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var hid uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM hospitals WHERE short_code = 'HAV'`).Scan(&hid); err != nil {
		t.Fatal(err)
	}
	type audited struct {
		actor, hospital *string
		payload         map[string]any
	}
	audits := func() []audited {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT actor_id::text, hospital_id::text, payload FROM audit_log
			WHERE action = 'hospital.model_policy_changed' ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []audited
		for rows.Next() {
			var a audited
			var raw []byte
			if err := rows.Scan(&a.actor, &a.hospital, &raw); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &a.payload); err != nil {
				t.Fatal(err)
			}
			out = append(out, a)
		}
		return out
	}
	policy := func() (provider, ref *string, budget int64, by *uuid.UUID) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT cloud_provider, agreement_ref, monthly_token_budget, updated_by
			FROM hospital_model_policy WHERE hospital_id = $1`, hid).Scan(&provider, &ref, &budget, &by); err != nil {
			t.Fatal(err)
		}
		return
	}

	out, err := run("set", "hav", "--provider", "anthropic", "--agreement-ref", "DEMO-SYNTHETIC")
	if err != nil || out != "model policy "+hid.String()+" anthropic\n" {
		t.Fatalf("anthropic: %q %v", out, err)
	}
	if p, ref, b, by := policy(); p == nil || *p != "anthropic" || ref == nil || *ref != "DEMO-SYNTHETIC" || b != 2000000 || by != nil {
		t.Fatalf("anthropic row: %v %v %d %v", p, ref, b, by)
	}
	// By id, with a budget.
	if out, err = run("set", hid.String(), "--provider", "openai", "--agreement-ref", "DPA-2026-0042", "--budget", "750000"); err != nil ||
		out != "model policy "+hid.String()+" openai\n" {
		t.Fatalf("openai: %q %v", out, err)
	}
	// A disabled or unknown provider: refused, nothing written.
	if _, err := run("set", "HAV", "--provider", "xai", "--agreement-ref", "DEMO-SYNTHETIC"); !errors.Is(err, errProviderDisabled) {
		t.Fatalf("xai: %v", err)
	}
	if _, err := run("set", "HAV", "--provider", "mistral", "--agreement-ref", "DEMO-SYNTHETIC"); !errors.Is(err, errUnknownProvider) {
		t.Fatalf("mistral: %v", err)
	}
	if p, _, b, _ := policy(); p == nil || *p != "openai" || b != 750000 {
		t.Fatalf("after refusals: %v %d", p, b)
	}
	// none switches off and keeps the budget.
	if out, err = run("set", "HAV", "--provider", "none"); err != nil || out != "model policy "+hid.String()+" none\n" {
		t.Fatalf("none: %q %v", out, err)
	}
	if p, ref, b, _ := policy(); p != nil || ref != nil || b != 750000 {
		t.Fatalf("none row: %v %v %d", p, ref, b)
	}
	// An unknown hospital.
	if _, err := run("set", "NOPE", "--provider", "none"); !errors.Is(err, account.ErrNoSuchHospital) {
		t.Fatalf("unknown hospital: %v", err)
	}
	if _, err := run("set", uuid.NewString(), "--provider", "anthropic", "--agreement-ref", "DEMO-SYNTHETIC"); !errors.Is(err, account.ErrNoSuchHospital) {
		t.Fatalf("unknown hospital id: %v", err)
	}

	a := audits()
	want := []map[string]any{
		{"hospital_id": hid.String(), "provider": "anthropic", "previous": "none", "agreement_ref": "DEMO-SYNTHETIC",
			"monthly_token_budget": float64(2000000), "via": "cli"},
		{"hospital_id": hid.String(), "provider": "openai", "previous": "anthropic", "agreement_ref": "DPA-2026-0042",
			"monthly_token_budget": float64(750000), "via": "cli"},
		{"hospital_id": hid.String(), "provider": "none", "previous": "openai", "monthly_token_budget": float64(750000), "via": "cli"},
	}
	if len(a) != len(want) {
		t.Fatalf("%d audit rows, want %d", len(a), len(want))
	}
	for i, row := range a {
		if row.actor != nil || row.hospital != nil {
			t.Errorf("row %d: actor %v hospital %v, want none", i, row.actor, row.hospital)
		}
		if len(row.payload) != len(want[i]) {
			t.Errorf("row %d: %v, want %v", i, row.payload, want[i])
		}
		for k, v := range want[i] {
			if row.payload[k] != v {
				t.Errorf("row %d %s = %v, want %v", i, k, row.payload[k], v)
			}
		}
	}
}
