package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type composeService struct {
	Image       string            `yaml:"image"`
	Environment map[string]string `yaml:"environment"`
	EnvFile     any               `yaml:"env_file"`
	Command     []string          `yaml:"command"`
	Volumes     []string          `yaml:"volumes"`
	Profiles    []string          `yaml:"profiles"`
	Ports       []string          `yaml:"ports"`
	DependsOn   map[string]any    `yaml:"depends_on"`
}

// workerPasswords are the intelligence workers' NATS passwords: each
// worker sees its own, inside its NATS_URL, and nothing else.
var workerPasswords = map[string]string{"pipeline": "NATS_PIPELINE_PASSWORD", "echo": "NATS_ECHO_PASSWORD"}

// natsUserSecrets are the NATS server's secrets: one password per user
// and the JetStream encryption key. Only the nats service holds them all;
// core holds its own (gateway) password, inside NATS_URL, and each worker
// its own (workerPasswords).
var natsUserSecrets = []string{"NATS_GATEWAY_PASSWORD", "NATS_PIPELINE_PASSWORD", "NATS_ECHO_PASSWORD",
	"NATS_BFF_CLINICAL_PASSWORD", "NATS_BFF_PLATFORM_PASSWORD", "NATS_JS_KEY"}

func loadCompose(t *testing.T) map[string]composeService {
	t.Helper()
	return loadComposeFile(t, "docker-compose.yml")
}

func loadComposeFile(t *testing.T, name string) map[string]composeService {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", name))
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
			"NATS_BFF_CLINICAL_PASSWORD", "NATS_BFF_PLATFORM_PASSWORD", "NATS_JS_KEY", "POSTGRES_PASSWORD", "PROVISION_DB_PASSWORD",
			"INTELLIGENCE_DB_PASSWORD"} {
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
	if _, ok := svcs["intelligence"]; ok {
		t.Error("the one-shot intelligence service is back: the workers are pipeline and echo")
	}
	for w := range workerPasswords {
		if _, ok := svcs[w]; !ok {
			t.Fatalf("no %s worker in docker-compose.yml", w)
		}
	}
	// The pipeline's database password goes to it and to core-migrate
	// (which makes the login), nowhere else.
	for name, svc := range svcs {
		if name == "pipeline" || name == "core-migrate" {
			continue
		}
		for k, v := range svc.Environment {
			if k == "INTELLIGENCE_DB_PASSWORD" || strings.Contains(v, "INTELLIGENCE_DB_PASSWORD") {
				t.Errorf("%s env %s carries INTELLIGENCE_DB_PASSWORD", name, k)
			}
		}
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
				// A worker's own NATS_URL names its own user (TestComposeWorkers),
				// never .env's NATS_URL, which holds the gateway's password.
				if _, worker := workerPasswords[name]; worker && bad == "NATS_URL" && k == "NATS_URL" &&
					strings.HasPrefix(v, "nats://"+name+":") {
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
				if k == "NATS_URL" && workerPasswords[name] == s {
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

type composeFile struct {
	Services map[string]struct {
		Environment map[string]string `yaml:"environment"`
		Command     []string          `yaml:"command"`
		Volumes     []string          `yaml:"volumes"`
		DependsOn   map[string]any    `yaml:"depends_on"`
	} `yaml:"services"`
	Volumes map[string]any `yaml:"volumes"`
}

// core serve needs REQUEST_KEY_PATH (spec §7.2): both compose files set it
// to a file apart from the access key, a one-shot makes it with
// `keys ensure --kind request` before core starts, and the public
// trusted-keys file goes to its own volume (workers will mount it; it holds
// no private key). INTELLIGENCE_QUERY_CAP is set (decision 8).
func TestComposeMakesTheRequestKey(t *testing.T) {
	for _, file := range []string{"docker-compose.yml", filepath.Join("deploy", "compose.yml")} {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", file))
			if err != nil {
				t.Fatal(err)
			}
			var c composeFile
			if err := yaml.Unmarshal(raw, &c); err != nil {
				t.Fatal(err)
			}
			core, ok := c.Services["core"]
			if !ok {
				t.Fatal("no core service")
			}
			req, acc := core.Environment["REQUEST_KEY_PATH"], core.Environment["ACCESS_KEY_PATH"]
			if req == "" || req == acc || !strings.HasPrefix(req, "/keys/") {
				t.Fatalf("REQUEST_KEY_PATH %q, ACCESS_KEY_PATH %q", req, acc)
			}
			if core.Environment["INTELLIGENCE_QUERY_CAP"] == "" {
				t.Error("core lacks INTELLIGENCE_QUERY_CAP")
			}
			var maker string
			var trusted string
			for name, s := range c.Services {
				cmd := s.Command
				if len(cmd) >= 2 && cmd[0] == "keys" && cmd[1] == "ensure" && slices.Contains(cmd, "request") {
					maker = name
					for i, a := range cmd {
						if a == "--out" && i+1 < len(cmd) && cmd[i+1] != req {
							t.Errorf("%s writes %s, core reads %s", name, cmd[i+1], req)
						}
						if a == "--trusted-out" && i+1 < len(cmd) {
							trusted = cmd[i+1]
						}
					}
					if !slices.Contains(s.Volumes, "corekeys:/keys") {
						t.Errorf("%s doesn't write to the corekeys volume: %v", name, s.Volumes)
					}
					if trusted == "" || strings.HasPrefix(trusted, "/keys/") {
						t.Errorf("%s: trusted-keys file %q must be outside the private key volume", name, trusted)
					}
					var vol string
					for _, v := range s.Volumes {
						if parts := strings.Split(v, ":"); len(parts) >= 2 && strings.HasPrefix(trusted, parts[1]+"/") {
							vol = parts[0]
						}
					}
					if vol == "" || vol == "corekeys" {
						t.Errorf("%s: the trusted-keys file is on volume %q", name, vol)
					} else if _, ok := c.Volumes[vol]; !ok {
						t.Errorf("volume %s isn't declared", vol)
					}
				}
			}
			if maker == "" {
				t.Fatal("no one-shot runs keys ensure --kind request")
			}
			if _, ok := core.DependsOn[maker]; !ok {
				t.Errorf("core doesn't wait for %s", maker)
			}
		})
	}
}

// Redis and the Python queue stubs are gone (Plan 3 T13): NATS carries the
// jobs. No compose file, service, volume, environment entry or
// .env.example line names Redis or the QUEUE_* settings.
func TestComposeHasNoRedis(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	for _, file := range []string{"docker-compose.yml", filepath.Join("deploy", "compose.yml"), ".env.example"} {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, file))
			if err != nil {
				t.Fatal(err)
			}
			text := strings.ToLower(string(raw))
			for _, bad := range []string{"redis", "queue_backend", "queue_url", "queue_dlq_url", "queue_workers"} {
				if strings.Contains(text, bad) {
					t.Errorf("%s still mentions %q", file, bad)
				}
			}
		})
	}
	var c composeFile
	raw, err := os.ReadFile(filepath.Join(root, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Services["redis"]; ok {
		t.Error("compose still has a redis service")
	}
	if _, ok := c.Volumes["redisdata"]; ok {
		t.Error("compose still has the redisdata volume")
	}
	for name, s := range c.Services {
		if _, ok := s.DependsOn["redis"]; ok {
			t.Errorf("%s depends on redis", name)
		}
	}
}

// F18 / spec §4, §8.2, §12: each worker service runs `intelligence worker
// --service <name>` in development, signs in to NATS as its own user with
// its own password, mounts the trusted public keys read-only and never the
// private-key volume, publishes no port, and gets only its own settings.
// Only the pipeline has a database, as its own login with the password
// outside the URL. echo is test-only (profile test).
func TestComposeWorkers(t *testing.T) {
	svcs := loadCompose(t)
	for name, pw := range workerPasswords {
		t.Run(name, func(t *testing.T) {
			w := svcs[name]
			if w.EnvFile != nil {
				t.Error("env_file brings in every secret in .env")
			}
			if !slices.Equal(w.Command, []string{"worker", "--service", name}) {
				t.Errorf("command = %q", w.Command)
			}
			if !strings.HasPrefix(w.Image, "mhai-intelligence:") {
				t.Errorf("image = %q", w.Image)
			}
			e := w.Environment
			if e["APP_ENV"] != "development" {
				t.Errorf("APP_ENV = %q: an unset APP_ENV is production, where the worker needs NKeys and TLS", e["APP_ENV"])
			}
			if want := "nats://" + name + ":${" + pw + ":?"; !strings.HasPrefix(e["NATS_URL"], want) {
				t.Errorf("NATS_URL = %q, want its own user (%s...)", e["NATS_URL"], want)
			}
			if e["INTELLIGENCE_TRUSTED_KEYS_PATH"] != "/trusted/request-keys.json" {
				t.Errorf("INTELLIGENCE_TRUSTED_KEYS_PATH = %q", e["INTELLIGENCE_TRUSTED_KEYS_PATH"])
			}
			if _, ok := e["OTEL_EXPORTER_OTLP_ENDPOINT"]; !ok {
				t.Error("no OTEL_EXPORTER_OTLP_ENDPOINT")
			}
			if !slices.Contains(w.Volumes, "trustedkeys:/trusted:ro") {
				t.Errorf("volumes %v: the trusted keys must be mounted read-only", w.Volumes)
			}
			for _, v := range w.Volumes {
				if strings.HasPrefix(v, "corekeys:") {
					t.Errorf("%s mounts the private-key volume", name)
				}
			}
			if len(w.Ports) != 0 {
				t.Errorf("ports %v: the workers are reached over NATS only", w.Ports)
			}
			if _, ok := w.DependsOn["core-request-key"]; !ok {
				t.Error("doesn't wait for the trusted-keys file (core-request-key)")
			}
			for k, v := range e {
				for other, opw := range workerPasswords {
					if other != name && (k == opw || strings.Contains(v, opw)) {
						t.Errorf("env %s carries %s", k, opw)
					}
				}
				for _, bad := range []string{"POSTGRES_PASSWORD", "POSTGRES_USER", "CORE_DB_", "PROVISION_DB_", "MFA_ENCRYPTION_KEY",
					"BFF_SERVICE_KEYS", "NATS_BFF_", "NATS_GATEWAY_PASSWORD", "NATS_JS_KEY", "DATABASE_URL"} {
					if strings.Contains(k, bad) || strings.Contains(v, bad) {
						t.Errorf("env %s carries %s", k, bad)
					}
				}
			}
			_, hasDB := e["INTELLIGENCE_DB_URL"]
			switch name {
			case "pipeline":
				u := e["INTELLIGENCE_DB_URL"]
				if !strings.HasPrefix(u, "postgres://postgres:5432/") || strings.Contains(u, "@") {
					t.Errorf("INTELLIGENCE_DB_URL = %q: no credentials in the URL", u)
				}
				if !strings.Contains(e["INTELLIGENCE_DB_USER"], "intelligence_app") ||
					!strings.Contains(e["INTELLIGENCE_DB_PASSWORD"], "${INTELLIGENCE_DB_PASSWORD:?") {
					t.Errorf("the pipeline must use its own login: user %q", e["INTELLIGENCE_DB_USER"])
				}
				if len(w.Profiles) != 0 {
					t.Errorf("pipeline profiles %v", w.Profiles)
				}
			case "echo":
				if hasDB || e["INTELLIGENCE_DB_PASSWORD"] != "" {
					t.Error("echo has no database")
				}
				if !slices.Equal(w.Profiles, []string{"test"}) {
					t.Errorf("echo profiles = %v, want [test]: it is test-only", w.Profiles)
				}
			}
		})
	}
	mig := svcs["core-migrate"].Environment
	if !strings.Contains(mig["INTELLIGENCE_DB_PASSWORD"], "INTELLIGENCE_DB_PASSWORD") || mig["INTELLIGENCE_DB_USER"] != svcs["pipeline"].Environment["INTELLIGENCE_DB_USER"] {
		t.Error("core-migrate must make the pipeline's login")
	}
	core := svcs["core"].Environment
	for _, k := range []string{"REQUEST_KEY_PATH", "REQUEST_TRUSTED_KEYS_PATH", "INTELLIGENCE_QUERY_CAP"} {
		if core[k] == "" {
			t.Errorf("core lacks %s", k)
		}
	}
	if !strings.Contains(core["CORE_HTTP_API"], "on") {
		t.Errorf("core CORE_HTTP_API = %q, want on until mhai-web's BFF uses NATS", core["CORE_HTTP_API"])
	}
}

// The smoke override: stub models (never in production: the worker refuses
// stub outside development), echo on, no Ollama.
func TestComposeSmokeOverride(t *testing.T) {
	svcs := loadComposeFile(t, "docker-compose.smoke.yml")
	if svcs["pipeline"].Environment["INTELLIGENCE_MODEL_CLIENT"] != "stub" {
		t.Error("the smoke pipeline must use the stub models")
	}
	if _, ok := svcs["pipeline"].DependsOn["ollama-init"]; ok {
		t.Error("the smoke pipeline waits for a model pull")
	}
	if _, ok := svcs["echo"]; !ok || len(svcs["echo"].Profiles) != 0 {
		t.Error("the smoke stack must turn echo on")
	}
	for _, s := range []string{"ollama", "ollama-init"} {
		if !slices.Equal(svcs[s].Profiles, []string{"models"}) {
			t.Errorf("%s runs in the smoke stack", s)
		}
	}
	for name, s := range svcs {
		for k := range s.Environment {
			if strings.Contains(k, "PASSWORD") || strings.Contains(k, "KEY") {
				t.Errorf("%s: the smoke override sets %s; secrets come from the generated env file", name, k)
			}
		}
	}
}
