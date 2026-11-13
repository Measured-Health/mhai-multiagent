package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
)

// envOf is a getenv over m.
func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// K6: "hospitals model-policy set" checks everything before connecting (url
// is empty: connecting would say DATABASE_URL is required). An unknown
// provider, or one the deployment hasn't enabled (CORE_CLOUD_PROVIDERS),
// is refused; none is always allowed. Errors never echo the reference.
func TestCLIModelPolicyArgsCheckedFirst(t *testing.T) {
	ctx := context.Background()
	enabled := envOf(map[string]string{"CORE_CLOUD_PROVIDERS": "anthropic"})
	run := func(env func(string) string, args ...string) error {
		return hospitalsModelPolicy(ctx, args, "", env, &bytes.Buffer{})
	}
	for _, args := range [][]string{
		{},
		{"set"},
		{"set", "HAV"},
		{"get", "HAV", "--provider", "none"},
		{"set", "--provider", "none", "HAV"},
		{"set", "HAV", "--provider", "none", "EXTRA"},
		{"set", "HAV", "--provider", "none", "--model", "x"},
		{"set", "HAV", "-h"},
	} {
		if err := run(enabled, args...); !errors.Is(err, errModelPolicyArgs) {
			t.Errorf("%v: %v", args, err)
		}
	}
	if err := run(enabled, "set", "not a code", "--provider", "none"); !errors.Is(err, account.ErrBadHospitalRef) {
		t.Errorf("bad ref: %v", err)
	}
	for name, c := range map[string]struct {
		env  func(string) string
		args []string
		want error
	}{
		"unknown provider":     {enabled, []string{"set", "HAV", "--provider", "mistral", "--agreement-ref", "DEMO-SYNTHETIC"}, errUnknownProvider},
		"gemini not enabled":   {enabled, []string{"set", "HAV", "--provider", "gemini", "--agreement-ref", "DEMO-SYNTHETIC"}, errProviderDisabled},
		"local":                {enabled, []string{"set", "HAV", "--provider", "local"}, errUnknownProvider},
		"disabled provider":    {enabled, []string{"set", "HAV", "--provider", "xai", "--agreement-ref", "DEMO-SYNTHETIC"}, errProviderDisabled},
		"nothing enabled":      {envOf(nil), []string{"set", "HAV", "--provider", "anthropic", "--agreement-ref", "DEMO-SYNTHETIC"}, errProviderDisabled},
		"provider without ref": {enabled, []string{"set", "HAV", "--provider", "anthropic"}, errNeedsAgreementRef},
		"bad ref":              {enabled, []string{"set", "HAV", "--provider", "anthropic", "--agreement-ref", "signed-by-dr-synthetic"}, errBadAgreementRef},
		"budget 0":             {enabled, []string{"set", "HAV", "--provider", "none", "--budget", "0"}, errBadBudget},
		"pseudonym ref":        {enabled, []string{"set", "HAV", "--provider", "anthropic", "--agreement-ref", "P-AB12CD34"}, errBadAgreementRef},
		"name ref":             {enabled, []string{"set", "HAV", "--provider", "anthropic", "--agreement-ref", "ADAEZE-OKAFOR"}, errBadAgreementRef},
		"ref while off":        {enabled, []string{"set", "HAV", "--provider", "none", "--agreement-ref", "AGR-2026-01"}, errRefWithoutProvider},
		"budget too big":       {enabled, []string{"set", "HAV", "--provider", "none", "--budget", "1000000001"}, errBadBudget},
	} {
		err := run(c.env, c.args...)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
		if err != nil && (strings.Contains(err.Error(), "signed-by-dr-synthetic") || strings.Contains(err.Error(), "OKAFOR") ||
			strings.Contains(err.Error(), "P-AB12CD34")) {
			t.Errorf("%s: the error echoes the reference: %v", name, err)
		}
	}
	// A bad CORE_CLOUD_PROVIDERS is refused, naming the variable only.
	err := run(envOf(map[string]string{"CORE_CLOUD_PROVIDERS": "anthropic,anthropic"}), "set", "HAV", "--provider", "none")
	if err == nil || !strings.Contains(err.Error(), "CORE_CLOUD_PROVIDERS") || strings.Contains(err.Error(), "anthropic,anthropic") {
		t.Errorf("bad env: %v", err)
	}
	// Good arguments reach the connection.
	for _, args := range [][]string{
		{"set", "HAV", "--provider", "none"},
		{"set", "hav", "--provider", "anthropic", "--agreement-ref", "DEMO-SYNTHETIC", "--budget", "5000"},
	} {
		if err := run(enabled, args...); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
			t.Errorf("%v, no url: %v", args, err)
		}
	}
	// seed routes "hospitals model-policy" here.
	t.Setenv("CORE_CLOUD_PROVIDERS", "")
	err = seed(ctx, []string{"hospitals", "model-policy", "set", "HAV", "--provider", "xai", "--agreement-ref", "DEMO-SYNTHETIC"}, "",
		strings.NewReader(""), &bytes.Buffer{})
	if !errors.Is(err, errProviderDisabled) {
		t.Errorf("through seed: %v", err)
	}
}
