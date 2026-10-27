package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type composeService struct {
	Environment map[string]string `yaml:"environment"`
	EnvFile     any               `yaml:"env_file"`
	Command     []string          `yaml:"command"`
}

func loadCompose(t *testing.T) map[string]composeService {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Services map[string]composeService `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c.Services
}

// The core service's compose environment holds only its own settings: no
// env_file (which would bring in every secret in .env), no worker NATS
// password, no owner database password; it connects as its own login.
// The migrate one-shot keeps the owner URL and creates the logins.
func TestComposeSplitsSecretsPerService(t *testing.T) {
	svcs := loadCompose(t)
	core, ok := svcs["core"]
	if !ok {
		t.Fatal("no core service in docker-compose.yml")
	}
	if core.EnvFile != nil {
		t.Error("core must not use env_file")
	}
	for k, v := range core.Environment {
		for _, bad := range []string{"NATS_WORKER_PASSWORD", "POSTGRES_PASSWORD", "PROVISION_DB_PASSWORD"} {
			if k == bad || strings.Contains(v, bad) {
				t.Errorf("core env %s carries %s", k, bad)
			}
		}
	}
	if u := core.Environment["DATABASE_URL"]; !strings.Contains(u, "CORE_DB_USER") || !strings.Contains(u, "CORE_DB_PASSWORD") {
		t.Errorf("core DATABASE_URL must use the core login: %q", u)
	}
	if !strings.HasPrefix(core.Environment["NATS_URL"], "nats://gateway:") {
		t.Errorf("core NATS_URL must use the gateway NATS user")
	}
	mig := svcs["core-migrate"]
	if u := mig.Environment["DATABASE_URL"]; !strings.Contains(u, "POSTGRES_USER") {
		t.Errorf("core-migrate must keep the owner URL: %q", u)
	}
	for _, k := range []string{"CORE_DB_USER", "CORE_DB_PASSWORD", "PROVISION_DB_USER", "PROVISION_DB_PASSWORD"} {
		if _, ok := mig.Environment[k]; !ok {
			t.Errorf("core-migrate lacks %s", k)
		}
	}
	for _, k := range []string{"NATS_WORKER_PASSWORD", "MFA_ENCRYPTION_KEY", "BFF_SERVICE_KEYS"} {
		if _, ok := mig.Environment[k]; ok {
			t.Errorf("core-migrate carries %s", k)
		}
	}
	cli, ok := svcs["core-cli"]
	if !ok || !strings.Contains(cli.Environment["DATABASE_URL"], "PROVISION_DB_USER") {
		t.Errorf("core-cli must connect as the provisioning login")
	}

	// I3: no service outside core gets a core secret, by name or inside a
	// value, and none uses env_file (which would bring in all of .env).
	// nats needs the gateway NATS password for its own config.
	coreSecrets := []string{"MFA_ENCRYPTION_KEY", "BFF_SERVICE_KEYS", "CORE_DB_PASSWORD",
		"PROVISION_DB_PASSWORD", "NATS_GATEWAY_PASSWORD", "NATS_URL"}
	if _, ok := svcs["intelligence"]; !ok {
		t.Fatal("no intelligence service in docker-compose.yml")
	}
	for name, svc := range svcs {
		if strings.HasPrefix(name, "core") {
			continue
		}
		if svc.EnvFile != nil {
			t.Errorf("%s must not use env_file", name)
		}
		for k, v := range svc.Environment {
			for _, bad := range coreSecrets {
				if name == "nats" && bad == "NATS_GATEWAY_PASSWORD" {
					continue
				}
				if k == bad || strings.Contains(v, bad) {
					t.Errorf("%s env %s carries %s", name, k, bad)
				}
			}
		}
	}
}
