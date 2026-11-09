package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
				// The emr service's own password, in its own variable.
				if name == "emr" && k == "EMR_NATS_PASSWORD" && s == "NATS_EMR_PASSWORD" {
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

// Plan departure 5: core requires services/gokit (replace ../gokit), so
// its image builds from services/ with core/Dockerfile, the same way in
// dev compose, CI, the deploy workflow and the Makefile. A build from
// services/core would fail at go mod download, so a stale context anywhere
// is a broken image there.
func TestCoreBuildsFromServices(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	read := func(rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	var c struct {
		Services map[string]struct {
			Build any    `yaml:"build"`
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(read("docker-compose.yml")), &c); err != nil {
		t.Fatal(err)
	}
	builds := 0
	for name, s := range c.Services {
		if s.Image != "mhai-core:local" || s.Build == nil {
			continue
		}
		builds++
		b, ok := s.Build.(map[string]any)
		if !ok || b["context"] != "./services" || b["dockerfile"] != "core/Dockerfile" {
			t.Errorf("%s builds core with %v, want context ./services and dockerfile core/Dockerfile", name, s.Build)
		}
	}
	if builds != 1 {
		t.Errorf("%d services build mhai-core:local, want 1", builds)
	}
	// Every docker build of core in CI and the Makefile.
	for _, rel := range []string{".github/workflows/ci.yml", "Makefile"} {
		n := 0
		for _, line := range strings.Split(read(rel), "\n") {
			if !strings.Contains(line, "docker build") || !strings.Contains(line, "mhai-core:") {
				continue
			}
			n++
			if !strings.HasSuffix(strings.TrimSpace(line), "-f services/core/Dockerfile services") {
				t.Errorf("%s: %q must build with -f services/core/Dockerfile services", rel, strings.TrimSpace(line))
			}
		}
		if n == 0 {
			t.Errorf("%s builds no core image", rel)
		}
	}
	deploy := read(".github/workflows/deploy.yml")
	if !strings.Contains(deploy, "context: services\n") || !strings.Contains(deploy, "file: services/core/Dockerfile\n") ||
		strings.Contains(deploy, "context: services/core") {
		t.Error("deploy.yml must build core with context services and file services/core/Dockerfile")
	}
	// The context is all of services/: the Python project and test files
	// stay out of it.
	ignore := read("services/.dockerignore")
	for _, want := range []string{"intelligence", "**/*_test.go"} {
		if !strings.Contains(ignore, want+"\n") {
			t.Errorf("services/.dockerignore lacks %s", want)
		}
	}
	if !strings.Contains(read("services/core/Dockerfile"), "COPY gokit/") {
		t.Error("services/core/Dockerfile doesn't copy gokit")
	}
}

// EMR off by default (plan Task 5): the emr service, its key one-shot and
// their volumes exist only under the profile emr; only they mount the emr
// volumes (core never sees a private key or a secret); they publish no
// port (any port would be on 127.0.0.1); the emr service gets its own NATS
// password and nothing of core's.
func TestComposeEMRProfile(t *testing.T) {
	svcs := loadCompose(t)
	emrNames := []string{"emr", "emr-keys", "emr-proxy-key", "egress-emr"}
	for _, name := range emrNames {
		s, ok := svcs[name]
		if !ok {
			t.Fatalf("no %s service", name)
		}
		if !slices.Equal(s.Profiles, []string{"emr"}) {
			t.Errorf("%s profiles %v, want [emr]", name, s.Profiles)
		}
		for _, p := range s.Ports {
			if !strings.HasPrefix(p, "127.0.0.1:") {
				t.Errorf("%s publishes %s", name, p)
			}
		}
		if s.EnvFile != nil {
			t.Errorf("%s uses env_file", name)
		}
	}
	for name, s := range svcs {
		for _, v := range s.Volumes {
			vol := strings.SplitN(v, ":", 2)[0]
			if (vol == "emr-secrets" || vol == "emr-keys") && name != "emr" && name != "emr-keys" {
				t.Errorf("%s mounts %s", name, vol)
			}
			if vol == "emr-secrets" && name != "emr" {
				t.Errorf("%s mounts emr-secrets", name)
			}
		}
		if !slices.Contains(emrNames, name) && slices.Contains(s.Profiles, "emr") {
			t.Errorf("%s is in the emr profile", name)
		}
	}
	e := svcs["emr"]
	if !strings.Contains(e.Environment["EMR_NATS_PASSWORD"], "NATS_EMR_PASSWORD") || strings.Contains(e.Environment["EMR_NATS_URL"], "@") {
		t.Errorf("emr NATS settings: %v", e.Environment)
	}
	for k, v := range e.Environment {
		for _, bad := range []string{"NATS_GATEWAY_PASSWORD", "MFA_ENCRYPTION_KEY", "BFF_SERVICE_KEYS", "CORE_DB_PASSWORD",
			"POSTGRES_PASSWORD", "PROVISION_DB_PASSWORD", "INTELLIGENCE_DB_PASSWORD", "NATS_JS_KEY"} {
			if strings.Contains(v, bad) {
				t.Errorf("emr env %s carries %s", k, bad)
			}
		}
	}
	if svcs["core"].Environment["CORE_EMR_ENABLED"] != "" && !strings.Contains(svcs["core"].Environment["CORE_EMR_ENABLED"], "false") {
		t.Errorf("core CORE_EMR_ENABLED defaults on: %q", svcs["core"].Environment["CORE_EMR_ENABLED"])
	}
	var c struct {
		Volumes map[string]any `yaml:"volumes"`
	}
	raw, _ := os.ReadFile(filepath.Join("..", "..", "..", "..", "docker-compose.yml"))
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"emr-secrets", "emr-keys"} {
		if _, ok := c.Volumes[v]; !ok {
			t.Errorf("no volume %s", v)
		}
	}
}

// Today's dev setups keep working: the emr NATS user is off unless
// NATS_EMR_CONF=emr.conf, NATS needs no NATS_EMR_PASSWORD while it is off,
// and only nats and the emr service ever see that password.
func TestComposeEMRNATSUserOffByDefault(t *testing.T) {
	svcs := loadCompose(t)
	n := svcs["nats"]
	if n.Environment["NATS_EMR_CONF"] != "${NATS_EMR_CONF:-emr-off.conf}" {
		t.Errorf("NATS_EMR_CONF = %q, want the emr-off.conf default", n.Environment["NATS_EMR_CONF"])
	}
	if pw := n.Environment["NATS_EMR_PASSWORD"]; pw != "${NATS_EMR_PASSWORD:-}" {
		t.Errorf("NATS_EMR_PASSWORD = %q, want optional", pw)
	}
	if !slices.Contains(n.Volumes, "./services/nats/${NATS_EMR_CONF:-emr-off.conf}:/etc/nats/emr.conf:ro") {
		t.Errorf("nats doesn't mount the emr fragment as /etc/nats/emr.conf: %v", n.Volumes)
	}
	for name, s := range svcs {
		if name == "nats" || name == "emr" {
			continue
		}
		for k, v := range s.Environment {
			if strings.Contains(k+v, "NATS_EMR_PASSWORD") {
				t.Errorf("%s env %s carries NATS_EMR_PASSWORD", name, k)
			}
		}
	}

	// The guard, as compose runs it: off needs nothing; on needs a real
	// password; nothing else is a valid fragment.
	const execLine = "exec nats-server -c /etc/nats/nats.conf"
	script := strings.Replace(strings.ReplaceAll(n.Command[0], "$$", "$"), execLine, "echo started", 1)
	base := map[string]string{}
	for _, k := range natsUserSecrets {
		base[k] = "synthetic" + strings.ReplaceAll(strings.ToLower(k), "_", "") + "0123456789"
	}
	base["NATS_JS_KEY"] = "ksynthetic0123456789abcdef0123456789abcdef"
	run := func(extra map[string]string) bool {
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		for k, v := range base {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		for k, v := range extra {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		return err == nil && strings.Contains(string(out), "started")
	}
	for _, tc := range []struct {
		env   map[string]string
		start bool
	}{
		{map[string]string{"NATS_EMR_CONF": "emr-off.conf"}, true},
		{map[string]string{"NATS_EMR_CONF": "emr-off.conf", "NATS_EMR_PASSWORD": ""}, true},
		{map[string]string{"NATS_EMR_CONF": "emr.conf", "NATS_EMR_PASSWORD": "syntheticEmrPassword0123456789"}, true},
		{map[string]string{"NATS_EMR_CONF": "emr.conf"}, false},
		{map[string]string{"NATS_EMR_CONF": "emr.conf", "NATS_EMR_PASSWORD": "changemeReplaceWithLettersAndDigits"}, false},
		{map[string]string{"NATS_EMR_CONF": "../../etc/passwd"}, false},
		{map[string]string{}, true}, // unset is off
	} {
		if got := run(tc.env); got != tc.start {
			t.Errorf("%v: started %v, want %v", tc.env, got, tc.start)
		}
	}
	// .env.example leaves the emr user off.
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "NATS_EMR_") {
			t.Errorf(".env.example sets %s", line)
		}
	}
}

// deployService is a service in deploy/compose.yml with the fields the
// cloud egress checks read. networks is a list or a map, so it stays a node.
type deployService struct {
	Image       string            `yaml:"image"`
	Environment map[string]string `yaml:"environment"`
	Networks    yaml.Node         `yaml:"networks"`
	NetworkMode string            `yaml:"network_mode"`
	Profiles    []string          `yaml:"profiles"`
	Ports       []string          `yaml:"ports"`
	Volumes     []string          `yaml:"volumes"`
	ReadOnly    bool              `yaml:"read_only"`
	MemLimit    string            `yaml:"mem_limit"`
	CapDrop     []string          `yaml:"cap_drop"`
	User        string            `yaml:"user"`
}

type deployCompose struct {
	Services map[string]deployService `yaml:"services"`
	Networks map[string]struct {
		Internal bool `yaml:"internal"`
		External bool `yaml:"external"`
	} `yaml:"networks"`
}

func (s deployService) networkNames() []string {
	var out []string
	switch s.Networks.Kind {
	case yaml.SequenceNode:
		for _, n := range s.Networks.Content {
			out = append(out, n.Value)
		}
	case yaml.MappingNode:
		for i := 0; i < len(s.Networks.Content); i += 2 {
			out = append(out, s.Networks.Content[i].Value)
		}
	}
	slices.Sort(out)
	return out
}

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func loadDeployCompose(t *testing.T) deployCompose {
	t.Helper()
	return loadComposeAs(t, filepath.Join("deploy", "compose.yml"))
}

// loadComposeAs reads a compose file with the fields the network checks
// read.
func loadComposeAs(t *testing.T, name string) deployCompose {
	t.Helper()
	var c deployCompose
	if err := yaml.Unmarshal([]byte(readRepoFile(t, name)), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// commentedPipeline parses deploy/compose.yml's commented-out pipeline
// block as an operator would uncomment it.
func commentedPipeline(t *testing.T) deployService {
	t.Helper()
	return commentedService(t, "pipeline")
}

// commentedService parses deploy/compose.yml's commented-out block for
// service name: every line from "  # <name>:" to the first line that
// isn't a comment loses its "  # " prefix. Lines commented twice stay
// comments.
func commentedService(t *testing.T, name string) deployService {
	t.Helper()
	var block []string
	in := false
	for _, line := range strings.Split(readRepoFile(t, filepath.Join("deploy", "compose.yml")), "\n") {
		if line == "  # "+name+":" {
			in = true
		}
		if !in {
			continue
		}
		if !strings.HasPrefix(line, "  #") {
			break
		}
		block = append(block, strings.TrimPrefix(strings.TrimPrefix(line, "  #"), " "))
	}
	if len(block) == 0 {
		t.Fatalf("deploy/compose.yml has no commented %s block", name)
	}
	var svcs map[string]deployService
	if err := yaml.Unmarshal([]byte(strings.Join(block, "\n")), &svcs); err != nil {
		t.Fatalf("the commented %s block isn't valid YAML once uncommented: %v", name, err)
	}
	p, ok := svcs[name]
	if !ok {
		t.Fatalf("the commented block has no %s service", name)
	}
	return p
}

// Cloud plan Task 14 (A15): only the egress proxy joins the egress network,
// which is the only one with a route out besides edge (core's and NATS's
// loopback ports). The proxy is off by default (profile cloud), publishes
// no port, runs read-only as a non-root user with no capabilities and a
// memory cap, and mounts its config and the allowed-hosts list read-only.
func TestEgressProxyOnlyServiceOnEgressNetwork(t *testing.T) {
	c := loadDeployCompose(t)
	p, ok := c.Services["egress-proxy"]
	if !ok {
		t.Fatal("no egress-proxy service in deploy/compose.yml")
	}
	if !slices.Equal(p.networkNames(), []string{"backend", "egress"}) {
		t.Errorf("egress-proxy networks %v, want [backend egress]", p.networkNames())
	}
	if !slices.Equal(p.Profiles, []string{"cloud"}) {
		t.Errorf("egress-proxy profiles %v, want [cloud]: off by default", p.Profiles)
	}
	if len(p.Ports) != 0 {
		t.Errorf("egress-proxy publishes %v", p.Ports)
	}
	if !p.ReadOnly || p.MemLimit == "" || !slices.Equal(p.CapDrop, []string{"ALL"}) {
		t.Errorf("egress-proxy: read_only %v, mem_limit %q, cap_drop %v", p.ReadOnly, p.MemLimit, p.CapDrop)
	}
	if p.User == "" || strings.HasPrefix(p.User, "0") || strings.HasPrefix(p.User, "root") {
		t.Errorf("egress-proxy user %q: run squid as its own user", p.User)
	}
	mounts := map[string]string{}
	for _, v := range p.Volumes {
		// The source may hold a ${VAR:-default}, so read from the right.
		parts := strings.Split(v, ":")
		n := len(parts)
		if n < 3 || parts[n-1] != "ro" {
			t.Errorf("egress-proxy mount %q must be read-only", v)
			continue
		}
		mounts[parts[n-2]] = strings.Join(parts[:n-2], ":")
	}
	for _, target := range []string{"/etc/squid/squid.conf", "/etc/squid/allowed-hosts"} {
		if mounts[target] == "" {
			t.Errorf("egress-proxy doesn't mount %s", target)
		}
	}
	if !strings.HasSuffix(mounts["/etc/squid/squid.conf"], "egress-proxy/squid.conf") {
		t.Errorf("squid.conf comes from %q", mounts["/etc/squid/squid.conf"])
	}
	e, ok := c.Networks["egress"]
	if !ok || e.Internal || e.External {
		t.Errorf("networks.egress = %+v (present %v): a project network with a route out", e, ok)
	}
	if !c.Networks["backend"].Internal {
		t.Error("networks.backend must stay internal")
	}
	all := map[string]deployService{"pipeline (commented)": commentedPipeline(t)}
	for name, s := range c.Services {
		all[name] = s
	}
	for name, s := range all {
		if name != "egress-proxy" && slices.Contains(s.networkNames(), "egress") {
			t.Errorf("%s is on the egress network", name)
		}
		if name != "egress-proxy" && slices.Contains(s.Profiles, "cloud") {
			t.Errorf("%s is in the cloud profile", name)
		}
	}
}

// The pipeline, once uncommented, stays on backend only (no route out) and
// reaches the providers only through the proxy, named by its own setting:
// HTTPS_PROXY would send every library's HTTPS through it (plan Task 14
// check M5), so it is never set. The key is a read-only file mount.
func TestPipelineNeverOnEgress(t *testing.T) {
	p := commentedPipeline(t)
	if !slices.Equal(p.networkNames(), []string{"backend"}) {
		t.Errorf("pipeline networks %v, want [backend] only", p.networkNames())
	}
	if p.NetworkMode != "" || len(p.Ports) != 0 {
		t.Errorf("pipeline network_mode %q, ports %v", p.NetworkMode, p.Ports)
	}
	if got := p.Environment["INTELLIGENCE_CLOUD_PROXY"]; got != "http://egress-proxy:3128" {
		t.Errorf("INTELLIGENCE_CLOUD_PROXY = %q, want http://egress-proxy:3128", got)
	}
	for k := range p.Environment {
		if l := strings.ToLower(k); l == "https_proxy" || l == "http_proxy" || l == "all_proxy" {
			t.Errorf("pipeline sets %s: a process-wide proxy reaches every library", k)
		}
	}
	key := false
	for _, v := range p.Volumes {
		parts := strings.Split(v, ":")
		if len(parts) >= 2 && strings.HasPrefix(parts[1], "/run/secrets/") && strings.HasSuffix(parts[1], "-api-key") {
			key = true
			if len(parts) != 3 || parts[2] != "ro" {
				t.Errorf("key mount %q must be read-only", v)
			}
		}
	}
	if !key {
		t.Error("the pipeline block shows no key-file mount (/run/secrets/<provider>-api-key)")
	}
	raw := readRepoFile(t, filepath.Join("deploy", "compose.yml"))
	for _, v := range []string{"INTELLIGENCE_CLOUD_PROVIDERS", "INTELLIGENCE_CLOUD_ANTHROPIC_MODEL",
		"INTELLIGENCE_CLOUD_ANTHROPIC_KEY_FILE", "INTELLIGENCE_CLOUD_ANTHROPIC_ZDR_REF"} {
		if !strings.Contains(raw, "# "+v+":") {
			t.Errorf("the pipeline block doesn't show %s, commented out", v)
		}
		if _, ok := p.Environment[v]; ok {
			t.Errorf("%s is set by uncommenting the block once: it must stay commented (off by default)", v)
		}
	}
}

// The proxy's image is pinned by digest, so a re-pushed tag can't change
// what holds the only route out.
func TestEgressProxyPinnedByDigest(t *testing.T) {
	img := loadDeployCompose(t).Services["egress-proxy"].Image
	if !regexp.MustCompile(`^ubuntu/squid(:[A-Za-z0-9._-]+)?@sha256:[0-9a-f]{64}$`).MatchString(img) {
		t.Errorf("egress-proxy image %q is not ubuntu/squid pinned by digest", img)
	}
}

// Provider keys are files on the pipeline host only (design §7): no
// service in either compose file (nor the commented pipeline) has a
// provider key in its environment, and core has no worker cloud setting.
func TestNoProviderKeyInAnyEnvironment(t *testing.T) {
	providerVar := regexp.MustCompile(`(?i)(OPENAI|ANTHROPIC|XAI|INTELLIGENCE_CLOUD_[A-Z]+)_(API_)?KEY$|^(OPENAI|ANTHROPIC|XAI|AZURE_OPENAI)_`)
	keyValue := regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{8,}|sk-ant-[A-Za-z0-9_-]{8,}|xai-[A-Za-z0-9_-]{8,})`)
	check := func(file, name string, env map[string]string) {
		for k, v := range env {
			if providerVar.MatchString(k) {
				t.Errorf("%s: %s env sets %s", file, name, k)
			}
			if keyValue.MatchString(v) {
				t.Errorf("%s: %s env %s holds a key-shaped value", file, name, k)
			}
			if name == "core" && strings.HasPrefix(k, "INTELLIGENCE_CLOUD_") {
				t.Errorf("%s: core has the worker's %s", file, k)
			}
		}
	}
	for name, s := range loadCompose(t) {
		check("docker-compose.yml", name, s.Environment)
	}
	for name, s := range loadDeployCompose(t).Services {
		check("deploy/compose.yml", name, s.Environment)
	}
	check("deploy/compose.yml", "pipeline (commented)", commentedPipeline(t).Environment)
	// The negative case, so the pattern can't silently match nothing.
	if !providerVar.MatchString("ANTHROPIC_API_KEY") || !providerVar.MatchString("INTELLIGENCE_CLOUD_XAI_KEY") ||
		providerVar.MatchString("INTELLIGENCE_CLOUD_XAI_KEY_FILE") || providerVar.MatchString("NATS_JS_KEY") {
		t.Fatal("providerVar pattern is wrong")
	}
}

// squid.conf (plan Task 14): CONNECT to 443 of the listed hosts only,
// matched by name with no reverse lookup (-n), IP literals refused first,
// no caching, no TLS interception, no forwarding headers, and an access
// log of method, host, port and status (no path, no client details).
func TestSquidConfAllowsOnlyConnect443ToListedHosts(t *testing.T) {
	conf := readRepoFile(t, filepath.Join("deploy", "egress-proxy", "squid.conf"))
	var lines []string
	for _, l := range strings.Split(conf, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			lines = append(lines, strings.Join(strings.Fields(l), " "))
		}
	}
	for _, want := range []string{
		"http_port 3128",
		`acl providers dstdomain -n "/etc/squid/allowed-hosts"`,
		`acl ip_literal dstdom_regex -n ^[0-9.]+$ ^\[?[0-9a-fA-F:]+\]?$`,
		"acl SSL_port port 443",
		"acl connect_method method CONNECT",
		"acl private_dst dst 0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 172.16.0.0/12 192.168.0.0/16 ::/128 ::1/128 fc00::/7 fe80::/10",
		"cache deny all",
		"forwarded_for delete",
		"via off",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("squid.conf lacks %q", want)
		}
	}
	var access []string
	for _, l := range lines {
		if strings.HasPrefix(l, "http_access ") {
			access = append(access, l)
		}
	}
	// Review of PR E: every name-only refusal comes first, so a name off
	// the list is refused with no DNS lookup (no exfiltration through
	// lookups of <data>.attacker.example); only then the address denies
	// (dst ACLs resolve the name), then the one allow, then deny all.
	want := []string{
		"http_access deny !connect_method",
		"http_access deny !SSL_port",
		"http_access deny ip_literal",
		"http_access deny !providers",
		"http_access deny private_dst",
		"http_access allow connect_method SSL_port providers",
		"http_access deny all",
	}
	if !slices.Equal(access, want) {
		t.Errorf("http_access rules\n%q\nwant\n%q", access, want)
	}
	dstACLs := map[string]bool{"to_localhost": true, "to_linklocal": true}
	for _, l := range lines {
		if f := strings.Fields(l); len(f) >= 3 && f[0] == "acl" && (f[2] == "dst" || f[2] == "dstdomain" && !slices.Contains(f, "-n")) {
			dstACLs[f[1]] = true
		}
	}
	firstDst := len(access)
	for i, l := range access {
		for _, w := range strings.Fields(l)[2:] {
			if dstACLs[strings.TrimPrefix(w, "!")] && i < firstDst {
				firstDst = i
			}
		}
	}
	if firstDst <= slices.Index(access, "http_access deny !providers") {
		t.Errorf("a resolving acl runs before the name allow-list: %q", access[firstDst])
	}
	for _, l := range lines {
		for _, bad := range []string{"ssl_bump", "cache_dir", "sslcrtd", "https_port", "cache_peer", "never_direct", "always_direct", "include "} {
			if strings.HasPrefix(l, bad) {
				t.Errorf("squid.conf has %q", l)
			}
		}
		if strings.HasPrefix(l, "acl ") && strings.Contains(l, " dst ") && strings.Contains(strings.Join(access, " "), "allow "+strings.Fields(l)[1]) {
			t.Errorf("an address acl (resolved from the client's host name) allows: %q", l)
		}
		if strings.HasPrefix(l, "logformat ") {
			for _, code := range []string{"%>ru", "%ru", "%rp", "%>rp", "%>a", "%un", "%>h", "%<h", "%{"} {
				if strings.Contains(l, code) {
					t.Errorf("logformat logs %s: %q", code, l)
				}
			}
		}
	}
	// The example lists no host: a provider is added when it is enabled.
	for _, l := range strings.Split(readRepoFile(t, filepath.Join("deploy", "egress-proxy", "allowed-hosts.example")), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			t.Errorf("allowed-hosts.example lists %q", l)
		}
	}
}

// Plan Task 15 (A6, A29, A37; design §6.4): the emr service has no route
// out. It is on backend (internal: Postgres, NATS) and emr-proxy
// (internal: egress-emr only); egress-emr is the only service on the
// egress-emr network, the one with a route out, and on no other network
// but emr-proxy, so it reaches neither Postgres nor NATS. Both are in the
// profile emr only (off by default), publish no port, and share only the
// allow-list (written by emr, read-only for the proxy) and K_proxy
// (read-only in both). The proxy runs read-only, unprivileged, with no
// capabilities and no secret of anyone else's. In both compose files; in
// deploy/compose.yml the emr service is a commented block until the demo
// has its NATS user and login.
func TestEMRNeverOnEgressNetwork(t *testing.T) {
	type file struct {
		name string
		c    deployCompose
		emr  deployService
	}
	dev := loadComposeAs(t, "docker-compose.yml")
	dep := loadDeployCompose(t)
	for _, f := range []file{{"docker-compose.yml", dev, dev.Services["emr"]}, {"deploy/compose.yml", dep, commentedService(t, "emr")}} {
		p, ok := f.c.Services["egress-emr"]
		if !ok {
			t.Errorf("%s: no egress-emr service", f.name)
			continue
		}
		if got := p.networkNames(); !slices.Equal(got, []string{"egress-emr", "emr-proxy"}) {
			t.Errorf("%s: egress-emr networks %v, want [egress-emr emr-proxy]", f.name, got)
		}
		if got := f.emr.networkNames(); !slices.Equal(got, []string{"backend", "emr-proxy"}) {
			t.Errorf("%s: emr networks %v, want [backend emr-proxy]: no route out", f.name, got)
		}
		if f.emr.NetworkMode != "" || len(f.emr.Ports) != 0 || len(p.Ports) != 0 || p.NetworkMode != "" {
			t.Errorf("%s: emr or egress-emr publishes a port or sets network_mode", f.name)
		}
		if !slices.Equal(p.Profiles, []string{"emr"}) || !slices.Equal(f.emr.Profiles, []string{"emr"}) {
			t.Errorf("%s: profiles egress-emr %v, emr %v: want [emr], off by default", f.name, p.Profiles, f.emr.Profiles)
		}
		if !p.ReadOnly || !slices.Equal(p.CapDrop, []string{"ALL"}) || strings.HasPrefix(p.User, "0") || strings.HasPrefix(p.User, "root") {
			t.Errorf("%s: egress-emr read_only %v, cap_drop %v, user %q", f.name, p.ReadOnly, p.CapDrop, p.User)
		}
		for _, n := range []string{"backend", "emr-proxy"} {
			if !f.c.Networks[n].Internal {
				t.Errorf("%s: network %s must be internal", f.name, n)
			}
		}
		if e, ok := f.c.Networks["egress-emr"]; !ok || e.Internal || e.External {
			t.Errorf("%s: networks.egress-emr = %+v (present %v): a project network with a route out", f.name, e, ok)
		}
		all := map[string]deployService{"emr": f.emr}
		for name, s := range f.c.Services {
			all[name] = s
		}
		for name, s := range all {
			nets := s.networkNames()
			if name != "egress-emr" && slices.Contains(nets, "egress-emr") {
				t.Errorf("%s: %s is on the egress-emr network", f.name, name)
			}
			if name != "egress-emr" && name != "emr" && slices.Contains(nets, "emr-proxy") {
				t.Errorf("%s: %s is on the emr-proxy network", f.name, name)
			}
		}
		// The proxy's mounts: the allow-list and K_proxy, both read-only;
		// nothing else (never a sealing key, the secret store or core's keys).
		for _, v := range p.Volumes {
			parts := strings.Split(v, ":")
			n := len(parts)
			if n < 3 || parts[n-1] != "ro" {
				t.Errorf("%s: egress-emr mount %q must be read-only", f.name, v)
			}
			if src := parts[0]; src != "emr-egress-conf" && !strings.Contains(v, "emr-proxy") {
				t.Errorf("%s: egress-emr mounts %q", f.name, v)
			}
		}
		allowRW := false
		for _, v := range f.emr.Volumes {
			if strings.HasPrefix(v, "emr-egress-conf:") && !strings.HasSuffix(v, ":ro") {
				allowRW = true
			}
			if strings.Contains(v, "emr-proxy") && !strings.HasSuffix(v, ":ro") {
				t.Errorf("%s: emr mounts K_proxy writable: %q", f.name, v)
			}
		}
		if !allowRW {
			t.Errorf("%s: emr doesn't mount emr-egress-conf to write the allow-list", f.name)
		}
		for k := range p.Environment {
			if !strings.HasPrefix(k, "EGRESS_EMR_") {
				t.Errorf("%s: egress-emr has %s in its environment", f.name, k)
			}
		}
		for k, v := range map[string]string{"EMR_PROXY_URL": "http://egress-emr:3128", "EMR_EGRESS_DIR": "/emr-egress"} {
			if f.emr.Environment[k] != v {
				t.Errorf("%s: emr %s = %q, want %q", f.name, k, f.emr.Environment[k], v)
			}
		}
		for k := range f.emr.Environment {
			if l := strings.ToLower(k); l == "https_proxy" || l == "http_proxy" || l == "all_proxy" {
				t.Errorf("%s: emr sets %s: only its egress clients use the proxy", f.name, k)
			}
		}
		// Only the emr services ever mount the egress volumes.
		for name, s := range f.c.Services {
			for _, v := range s.Volumes {
				if (strings.HasPrefix(v, "emr-egress-conf:") || strings.HasPrefix(v, "emr-proxy-key:")) &&
					name != "emr" && name != "egress-emr" && name != "emr-proxy-key" {
					t.Errorf("%s: %s mounts %s", f.name, name, v)
				}
			}
		}
	}
	// The demo's services keep the networks they had: nothing outside the
	// profile emr changes in deploy/compose.yml.
	want := map[string][]string{"postgres": {"backend"}, "nats": {"backend", "bff", "edge"}, "core-migrate": {"backend"},
		"core": {"backend", "edge"}, "core-cli": {"backend"}, "egress-proxy": {"backend", "egress"}}
	for name, nets := range want {
		if got := dep.Services[name].networkNames(); !slices.Equal(got, nets) {
			t.Errorf("deploy/compose.yml: %s networks %v, want %v", name, got, nets)
		}
	}
}

// EMR PR 4 review item 1: egress-emr is told the backend subnet
// explicitly. In docker-compose.yml the backend network has a fixed
// subnet and egress-emr's EGRESS_EMR_INTERNAL_CIDRS defaults to it; in
// deploy/compose.yml (whose backend can't change under the running demo)
// the operator sets it, and egress-emr runs as production there, so it
// refuses to start without it.
func TestEgressEMRToldBackendSubnet(t *testing.T) {
	var dev struct {
		Services map[string]deployService `yaml:"services"`
		Networks map[string]struct {
			IPAM struct {
				Config []struct {
					Subnet string `yaml:"subnet"`
				} `yaml:"config"`
			} `yaml:"ipam"`
		} `yaml:"networks"`
	}
	if err := yaml.Unmarshal([]byte(readRepoFile(t, "docker-compose.yml")), &dev); err != nil {
		t.Fatal(err)
	}
	cfg := dev.Networks["backend"].IPAM.Config
	if len(cfg) != 1 || cfg[0].Subnet == "" {
		t.Fatalf("docker-compose.yml: backend has no fixed subnet: %+v", cfg)
	}
	env := dev.Services["egress-emr"].Environment
	if got := env["EGRESS_EMR_INTERNAL_CIDRS"]; !strings.Contains(got, strings.TrimPrefix(strings.TrimSuffix(cfg[0].Subnet, "}"), "${EMR_BACKEND_SUBNET:-")) || got == "" {
		t.Errorf("docker-compose.yml: EGRESS_EMR_INTERNAL_CIDRS %q doesn't name the backend subnet %q", got, cfg[0].Subnet)
	}
	dep := loadDeployCompose(t).Services["egress-emr"].Environment
	if _, ok := dep["EGRESS_EMR_INTERNAL_CIDRS"]; !ok {
		t.Error("deploy/compose.yml: egress-emr doesn't take EGRESS_EMR_INTERNAL_CIDRS")
	}
	if e := dep["EGRESS_EMR_ENV"]; e != "" && e != "production" && e != "staging" {
		t.Errorf("deploy/compose.yml: EGRESS_EMR_ENV %q: the demo's proxy must require the internal networks", e)
	}
}
