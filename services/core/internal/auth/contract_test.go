package auth_test

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/server"
)

type oaOperation struct {
	OperationID string                 `yaml:"operationId"`
	Security    *[]map[string][]string `yaml:"security"`
	Responses   map[string]any         `yaml:"responses"`
	ErrorCodes  []string               `yaml:"x-error-codes"`
}

type oaSpec struct {
	OpenAPI string                            `yaml:"openapi"`
	Servers []struct{ URL string }            `yaml:"servers"`
	Paths   map[string]map[string]oaOperation `yaml:"paths"`
	Comps   struct {
		Schemas map[string]struct {
			Properties map[string]struct {
				Enum []string `yaml:"enum"`
			} `yaml:"properties"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

var oaMethods = []string{"get", "put", "post", "delete", "patch", "head", "options"}

func loadSpec(t *testing.T) oaSpec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var s oaSpec
	if err := yaml.Unmarshal(raw, &s); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	if !strings.HasPrefix(s.OpenAPI, "3.") || len(s.Servers) != 1 || s.Servers[0].URL != httpx.BasePath {
		t.Fatalf("openapi %q servers %v: want 3.x with the single server %s", s.OpenAPI, s.Servers, httpx.BasePath)
	}
	return s
}

// TestOpenAPIMatchesHandlers: the committed contract (openapi.yaml) and
// the permission table the service serves (server.Table) list the same
// routes, both ways. Each operation's security matches its rule (probes:
// none; public: the BFF key; the rest: the BFF key and a bearer token), and
// every error code it documents is in httpx.Codes, under that code's status.
func TestOpenAPIMatchesHandlers(t *testing.T) {
	spec := loadSpec(t)
	rules := map[string]authz.Rule{}
	var served []string
	for _, rr := range server.Table() {
		if strings.HasPrefix(rr.Pattern, authz.FixturePrefix) {
			continue // test-build fixtures, never in a release
		}
		k := rr.Method + " " + rr.Pattern
		served = append(served, k)
		rules[k] = rr.Rule
	}
	var documented []string
	for p, ops := range spec.Paths {
		for m, op := range ops {
			if !slices.Contains(oaMethods, m) {
				continue // path-level fields such as parameters
			}
			k := strings.ToUpper(m) + " " + httpx.BasePath + p
			documented = append(documented, k)
			rule, ok := rules[k]
			if !ok {
				continue
			}
			checkSecurity(t, k, rule, op)
			checkErrors(t, k, rule, op)
		}
	}
	sort.Strings(served)
	sort.Strings(documented)
	for _, k := range served {
		if !slices.Contains(documented, k) {
			t.Errorf("served but not in openapi.yaml: %s", k)
		}
	}
	for _, k := range documented {
		if !slices.Contains(served, k) {
			t.Errorf("in openapi.yaml but not served: %s", k)
		}
	}
}

func checkSecurity(t *testing.T, k string, rule authz.Rule, op oaOperation) {
	t.Helper()
	if op.Security == nil {
		t.Errorf("%s: no security stated", k)
		return
	}
	var schemes []string
	for _, req := range *op.Security {
		for name := range req {
			schemes = append(schemes, name)
		}
	}
	sort.Strings(schemes)
	want := []string{"Bearer", "ServiceKey"}
	switch {
	case rule.Probe:
		want = nil
	case rule.Public:
		want = []string{"ServiceKey"}
	}
	if !slices.Equal(schemes, want) {
		t.Errorf("%s: security %v, want %v", k, schemes, want)
	}
}

func checkErrors(t *testing.T, k string, rule authz.Rule, op oaOperation) {
	t.Helper()
	if len(op.ErrorCodes) == 0 && !rule.Probe {
		t.Errorf("%s: no x-error-codes", k)
	}
	for _, c := range op.ErrorCodes {
		st, ok := httpx.Codes[c]
		if !ok {
			t.Errorf("%s: error code %q is not in httpx.Codes", k, c)
			continue
		}
		if _, ok := op.Responses[strconv.Itoa(st)]; !ok {
			t.Errorf("%s: code %q needs a %d response", k, c, st)
		}
	}
	// Every documented 4xx/5xx status has a code that produces it.
	for status := range op.Responses {
		n, err := strconv.Atoi(status)
		if err != nil || n < 400 {
			continue
		}
		if !slices.ContainsFunc(op.ErrorCodes, func(c string) bool { return httpx.Codes[c] == n }) {
			t.Errorf("%s: %s response with no error code for it", k, status)
		}
	}
}

// The Problem schema's code enum is exactly httpx.Codes.
func TestOpenAPIProblemCodes(t *testing.T) {
	spec := loadSpec(t)
	got := spec.Comps.Schemas["Problem"].Properties["code"].Enum
	var want []string
	for c := range httpx.Codes {
		want = append(want, c)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("Problem.code enum %v, want httpx.Codes %v", got, want)
	}
}
