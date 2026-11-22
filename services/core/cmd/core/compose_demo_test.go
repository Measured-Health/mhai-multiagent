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
	for name, cmd := range map[string]string{"core-demo-seed": "seed", "core-demo-reset": "reset"} {
		s, ok := c.Services[name]
		if !ok {
			t.Fatalf("deploy/compose.yml has no %s service", name)
		}
		if !slices.Equal(s.Profiles, []string{"demo"}) || !slices.Equal(s.Command, []string{"demo", cmd}) {
			t.Errorf("%s profiles %v command %v", name, s.Profiles, s.Command)
		}
		if s.Environment["MHAI_DEMO_SEED"] != "on" || s.Environment["APP_ENV"] != "demo" {
			t.Errorf("%s must set MHAI_DEMO_SEED=on and APP_ENV=demo", name)
		}
		if got := s.networkNames(); !slices.Equal(got, []string{"backend"}) {
			t.Errorf("%s networks %v, want [backend]", name, got)
		}
		if s.Restart != "no" || !s.ReadOnly || len(s.Ports) != 0 {
			t.Errorf("%s restart %q read_only %v ports %v", name, s.Restart, s.ReadOnly, s.Ports)
		}
		for _, v := range s.Volumes {
			if v != "./secrets/nats-gateway.nk:/run/secrets/nats-gateway.nk:ro" {
				t.Errorf("%s mounts %q: only core's NATS seed, read-only", name, v)
			}
		}
	}
}

// Only the demo's services (the seed, the reset, sample-ai) carry the demo switches; the pilot's
// pipeline and the dev stack never do.
func TestDemoSwitchesOnlyOnDemoServices(t *testing.T) {
	for name, s := range loadDeployCompose(t).Services {
		oneShot := name == "core-demo-seed" || name == "core-demo-reset"
		if !oneShot {
			if _, ok := s.Environment["MHAI_DEMO_SEED"]; ok {
				t.Errorf("%s sets MHAI_DEMO_SEED", name)
			}
		}
		if name != "sample-ai" {
			if s.Environment["INTELLIGENCE_MODEL_CLIENT"] == "sample" || (s.Environment["APP_ENV"] == "demo" && !oneShot) {
				t.Errorf("%s runs the sample client or APP_ENV=demo", name)
			}
			if slices.Contains(s.Profiles, "demo") && !oneShot {
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

// The live site (deploy/README.md, "Live site"): with SITE_MODE=live the
// deploy runs live-site.sh and never the demo seed, refuses a host marked
// live while SITE_MODE is not live, and the scheduled demo reset never
// runs (a run by hand refuses).
func TestLiveSiteModeSkipsTheDemo(t *testing.T) {
	wf := readRepoFile(t, ".github/workflows/deploy.yml")
	for _, want := range []string{
		"SITE_MODE: ${{ vars.SITE_MODE }}",
		"''|demo) echo \"value=demo\"",
		"live) echo \"value=live\"",
		"SITE_MODE: ${{ steps.mode.outputs.value }}",
		"envs: GHCR_TOKEN,GHCR_USER,DEPLOY_BUNDLE,DEMO_SAMPLE_AI,SITE_MODE",
		"premigrate-check.sh live-site.sh demo/enable-demo.sh",
		"if [ \"$SITE_MODE\" = live ]; then\n              bash live-site.sh precheck\n              live_guard=1\n            elif [ -e live-site ]; then",
		"if [ \"$SITE_MODE\" = live ]; then\n              bash live-site.sh\n              live_guard=0\n            fi",
		"trap on_exit EXIT",
		"if [ \"$SITE_MODE\" != live ]; then\n              DEMO_SAMPLE_AI=\"$DEMO_SAMPLE_AI\" bash demo/enable-demo.sh",
	} {
		if !strings.Contains(wf, want) {
			t.Errorf("deploy.yml lacks %q", want)
		}
	}
	// The pre-check comes before core starts, the post-check before the
	// demo seed's branch, and the seed runs once.
	pre := strings.Index(wf, "bash live-site.sh precheck")
	up := strings.Index(wf, "docker compose up -d --remove-orphans core")
	post := strings.Index(wf, "bash live-site.sh\n")
	seed := strings.Index(wf, "bash demo/enable-demo.sh")
	if pre < 0 || up < 0 || post < 0 || seed < 0 || pre > up || post < up || seed < post ||
		strings.Count(wf, "demo/enable-demo.sh\n") != 1 {
		t.Error("deploy.yml: live-site.sh precheck before core starts, the check after, the demo seed once and last")
	}
	reset := readRepoFile(t, ".github/workflows/demo-reset.yml")
	for _, want := range []string{
		"vars.SITE_MODE != 'live' &&",
		"if: vars.SITE_MODE == 'live' && github.event_name == 'workflow_dispatch'",
	} {
		if !strings.Contains(reset, want) {
			t.Errorf("demo-reset.yml lacks %q", want)
		}
	}
	for _, f := range []string{"deploy/demo/enable-demo.sh", "deploy/demo/reset-demo.sh"} {
		if !strings.Contains(readRepoFile(t, f), "is a live site (marker") {
			t.Errorf("%s does not refuse on a live site", f)
		}
	}
}
