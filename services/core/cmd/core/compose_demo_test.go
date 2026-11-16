package main

import (
	"slices"
	"strings"
	"testing"
)

// The public demo's sample answers and seed (deploy/README.md, "Public demo:
// sample answers"): both only under the profile demo, the worker as the
// demo, never production, on backend only, with read-only secret files.
func TestDemoSampleAIService(t *testing.T) {
	c := loadDeployCompose(t)
	s, ok := c.Services["sample-ai"]
	if !ok {
		t.Fatal("deploy/compose.yml has no sample-ai service")
	}
	if !slices.Equal(s.Profiles, []string{"demo"}) {
		t.Errorf("sample-ai profiles %v, want [demo]", s.Profiles)
	}
	if !s.ReadOnly || !slices.Equal(s.CapDrop, []string{"ALL"}) || s.MemLimit == "" || s.Memswap != s.MemLimit {
		t.Errorf("sample-ai: read_only %v, cap_drop %v, mem_limit %q, memswap %q", s.ReadOnly, s.CapDrop, s.MemLimit, s.Memswap)
	}
	if s.EnvFile.Kind != 0 {
		t.Error("sample-ai has an env_file: it must not see the core service's secrets")
	}
	if got := s.networkNames(); !slices.Equal(got, []string{"backend"}) {
		t.Errorf("sample-ai networks %v, want [backend] (no route out)", got)
	}
	if len(s.Ports) != 0 {
		t.Errorf("sample-ai publishes %v", s.Ports)
	}
	want := map[string]string{
		"APP_ENV":                       "demo",
		"INTELLIGENCE_MODEL_CLIENT":     "sample",
		"INTELLIGENCE_SAMPLE_HOSPITALS": "${DEMO_SAMPLE_HOSPITALS:-}",
		"INTELLIGENCE_SINGLE_HOST":      "on",
		"NATS_NKEY_SEED_PATH":           "/run/secrets/nats-pipeline.nk",
		"INTELLIGENCE_DB_USER":          "intelligence_app",
		"INTELLIGENCE_DB_PASSWORD_FILE": "/run/secrets/intelligence-db-password",
	}
	for k, v := range want {
		if got := s.Environment[k]; got != v {
			t.Errorf("sample-ai %s = %q, want %q", k, got, v)
		}
	}
	for k, v := range s.Environment {
		if strings.Contains(v, ":?") {
			t.Errorf("sample-ai %s = %q: a required variable would break the default deploy's compose config", k, v)
		}
		if strings.HasPrefix(k, "INTELLIGENCE_CLOUD") || k == "INTELLIGENCE_MODEL_ROLES" || strings.Contains(k, "PASSWORD") && k != "INTELLIGENCE_DB_PASSWORD_FILE" {
			t.Errorf("sample-ai sets %s", k)
		}
	}
	for _, v := range s.Volumes {
		if !strings.HasSuffix(v, ":ro") || strings.HasPrefix(v, "corekeys") {
			t.Errorf("sample-ai mount %q: read-only, and never the core keys", v)
		}
	}
}

func TestDemoSeedService(t *testing.T) {
	c := loadDeployCompose(t)
	s, ok := c.Services["core-demo-seed"]
	if !ok {
		t.Fatal("deploy/compose.yml has no core-demo-seed service")
	}
	if !slices.Equal(s.Profiles, []string{"demo"}) || !slices.Equal(s.Command, []string{"demo", "seed"}) {
		t.Errorf("core-demo-seed profiles %v command %v", s.Profiles, s.Command)
	}
	if s.Environment["MHAI_DEMO_SEED"] != "on" || s.Environment["APP_ENV"] != "demo" {
		t.Error("core-demo-seed must set MHAI_DEMO_SEED=on and APP_ENV=demo")
	}
	if got := s.networkNames(); !slices.Equal(got, []string{"backend"}) {
		t.Errorf("core-demo-seed networks %v, want [backend]", got)
	}
	if s.Restart != "no" || !s.ReadOnly {
		t.Errorf("core-demo-seed restart %q read_only %v", s.Restart, s.ReadOnly)
	}
}

// Only the demo's two services carry the demo switches; the pilot's
// pipeline and the dev stack never do.
func TestDemoSwitchesOnlyOnDemoServices(t *testing.T) {
	for name, s := range loadDeployCompose(t).Services {
		if name != "core-demo-seed" {
			if _, ok := s.Environment["MHAI_DEMO_SEED"]; ok {
				t.Errorf("%s sets MHAI_DEMO_SEED", name)
			}
		}
		if name != "sample-ai" {
			if s.Environment["INTELLIGENCE_MODEL_CLIENT"] == "sample" || (s.Environment["APP_ENV"] == "demo" && name != "core-demo-seed") {
				t.Errorf("%s runs the sample client or APP_ENV=demo", name)
			}
			if slices.Contains(s.Profiles, "demo") && name != "core-demo-seed" {
				t.Errorf("%s is in the demo profile", name)
			}
		}
	}
	for name, s := range loadCompose(t) {
		if s.Environment["INTELLIGENCE_MODEL_CLIENT"] == "sample" || s.Environment["MHAI_DEMO_SEED"] != "" {
			t.Errorf("docker-compose.yml %s carries a demo switch", name)
		}
	}
}
