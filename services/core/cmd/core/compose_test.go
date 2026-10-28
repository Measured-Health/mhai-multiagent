package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type composeService struct {
	Environment map[string]string `yaml:"environment"`
	EnvFile     any               `yaml:"env_file"`
	Command     []string          `yaml:"command"`
	Volumes     []string          `yaml:"volumes"`
}

// natsUserSecrets are the NATS server's secrets: one password per user
// and the JetStream encryption key. Only the nats service holds them all;
// core holds its own (gateway) password, inside NATS_URL. Each worker
// service's own password is checked in Task 15, when those services exist.
var natsUserSecrets = []string{"NATS_GATEWAY_PASSWORD", "NATS_PIPELINE_PASSWORD", "NATS_ECHO_PASSWORD",
	"NATS_BFF_CLINICAL_PASSWORD", "NATS_BFF_PLATFORM_PASSWORD", "NATS_JS_KEY"}

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
		for _, bad := range []string{"NATS_WORKER_PASSWORD", "NATS_PIPELINE_PASSWORD", "NATS_ECHO_PASSWORD",
			"NATS_BFF_CLINICAL_PASSWORD", "NATS_BFF_PLATFORM_PASSWORD", "NATS_JS_KEY", "POSTGRES_PASSWORD", "PROVISION_DB_PASSWORD"} {
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
	for _, k := range append([]string{"MFA_ENCRYPTION_KEY", "BFF_SERVICE_KEYS"}, natsUserSecrets...) {
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

// F18: the nats service gets every user's password and the JetStream key
// (an undefined variable stops nats-server), refuses placeholders for each
// of them, and mounts the test include next to nats.conf. No other
// service, core aside, sees any of them; core sees only the gateway's.
func TestComposeNATSGetsEveryUserSecret(t *testing.T) {
	svcs := loadCompose(t)
	n, ok := svcs["nats"]
	if !ok {
		t.Fatal("no nats service")
	}
	guard := strings.Join(n.Command, "\n")
	for _, k := range natsUserSecrets {
		if !strings.Contains(n.Environment[k], k) {
			t.Errorf("nats env lacks %s", k)
		}
		if !strings.Contains(guard, "$$"+k) && !strings.Contains(guard, "$"+k) {
			t.Errorf("nats start-up guard doesn't check %s", k)
		}
	}
	if _, ok := n.Environment["NATS_WORKER_PASSWORD"]; ok {
		t.Error("nats still has the shared worker password")
	}
	var mounted bool
	for _, v := range n.Volumes {
		if strings.HasPrefix(v, "./services/nats/bff-clinical.test.conf:/etc/nats/bff-clinical.test.conf") {
			mounted = true
		}
	}
	if !mounted {
		t.Error("nats doesn't mount bff-clinical.test.conf beside nats.conf")
	}
	for name, svc := range svcs {
		if name == "nats" {
			continue
		}
		for k, v := range svc.Environment {
			for _, s := range natsUserSecrets {
				if name == "core" && k == "NATS_URL" && s == "NATS_GATEWAY_PASSWORD" {
					continue
				}
				if k == s || strings.Contains(v, s) {
					t.Errorf("%s env %s carries %s", name, k, s)
				}
			}
		}
	}
	if r := svcs["core"].Environment["NATS_JS_REPLICAS"]; !strings.Contains(r, "1") {
		t.Errorf("core NATS_JS_REPLICAS = %q, want 1 in dev compose", r)
	}
}

// The nats service's start-up guard, run as compose would run it (with
// nats-server replaced by an echo): good values start, and a missing or
// placeholder secret, or a short or digit-led JetStream key, does not.
func TestComposeNATSGuardRefusesPlaceholders(t *testing.T) {
	n := loadCompose(t)["nats"]
	if len(n.Command) != 1 {
		t.Fatalf("nats command = %q", n.Command)
	}
	const execLine = "exec nats-server -c /etc/nats/nats.conf"
	script := strings.ReplaceAll(n.Command[0], "$$", "$")
	if !strings.Contains(script, execLine) {
		t.Fatal("the guard no longer ends by starting nats-server")
	}
	script = strings.Replace(script, execLine, "echo started", 1)
	good := map[string]string{}
	for _, k := range natsUserSecrets {
		good[k] = "synthetic" + strings.ReplaceAll(strings.ToLower(k), "_", "") + "0123456789"
	}
	good["NATS_JS_KEY"] = "ksynthetic0123456789abcdef0123456789abcdef"
	run := func(env map[string]string) (string, error) {
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run(good); err != nil || !strings.Contains(out, "started") {
		t.Fatalf("good values refused: %v %s", err, out)
	}
	for _, k := range natsUserSecrets {
		for _, bad := range []string{"", "changemeReplaceWithLettersAndDigits", "xxCHANGE-MExx0123456789abcdef0123456789", "aPlaceHolder0123456789abcdef0123456789"} {
			env := map[string]string{}
			for k2, v := range good {
				env[k2] = v
			}
			env[k] = bad
			if out, err := run(env); err == nil || strings.Contains(out, "started") {
				t.Errorf("%s=%q started nats", k, bad)
			}
		}
	}
	for _, bad := range []string{"kshort", "0starts0with0a0digit0123456789abcdef0123"} {
		env := map[string]string{}
		for k, v := range good {
			env[k] = v
		}
		env["NATS_JS_KEY"] = bad
		if out, err := run(env); err == nil || strings.Contains(out, "started") {
			t.Errorf("NATS_JS_KEY=%q started nats", bad)
		}
	}
}

// .env.example lists every NATS secret compose needs, and no shared worker
// password.
func TestEnvExampleListsNATSSecrets(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range natsUserSecrets {
		if !strings.Contains(string(raw), "\n"+k+"=") {
			t.Errorf(".env.example lacks %s", k)
		}
	}
	if strings.Contains(string(raw), "NATS_WORKER_PASSWORD") {
		t.Error(".env.example still has NATS_WORKER_PASSWORD")
	}
}
