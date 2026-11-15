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

// documentsService is the documents service in docker-compose.yml, with
// the hardening fields the checks read.
type documentsService struct {
	Build       map[string]any    `yaml:"build"`
	Image       string            `yaml:"image"`
	Environment map[string]string `yaml:"environment"`
	EnvFile     any               `yaml:"env_file"`
	Volumes     []string          `yaml:"volumes"`
	Profiles    []string          `yaml:"profiles"`
	Ports       []string          `yaml:"ports"`
	Networks    any               `yaml:"networks"`
	NetworkMode string            `yaml:"network_mode"`
	ReadOnly    bool              `yaml:"read_only"`
	CapDrop     []string          `yaml:"cap_drop"`
	SecurityOpt []string          `yaml:"security_opt"`
	User        string            `yaml:"user"`
	Privileged  bool              `yaml:"privileged"`
}

// ADR 0016, documents off by default (design §13, §17 departures 2 and 9):
// the documents service exists only under the profile documents, is the
// only service in it, builds from services/, runs locked down (read-only
// root, no capabilities, no-new-privileges) on the internal backend
// network only with no port, gets its own NATS password and nothing of
// core's or the emr service's, and mounts only the public trusted keys,
// read-only.
func TestComposeDocumentsProfile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Services map[string]documentsService `yaml:"services"`
		Networks map[string]struct {
			Internal bool `yaml:"internal"`
		} `yaml:"networks"`
	}
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	d, ok := c.Services["documents"]
	if !ok {
		t.Fatal("no documents service")
	}
	for name, s := range c.Services {
		if name != "documents" && slices.Contains(s.Profiles, "documents") {
			t.Errorf("%s is in the documents profile (PR 1 runs the service alone)", name)
		}
	}
	if !slices.Equal(d.Profiles, []string{"documents"}) {
		t.Errorf("documents profiles %v, want [documents]", d.Profiles)
	}
	if d.Image != "mhai-documents:local" || d.Build["context"] != "./services" || d.Build["dockerfile"] != "documents/Dockerfile" {
		t.Errorf("documents build %v image %q", d.Build, d.Image)
	}
	if d.EnvFile != nil || len(d.Ports) != 0 || d.NetworkMode != "" || d.User != "" || d.Privileged {
		t.Errorf("documents: env_file %v ports %v network_mode %q user %q privileged %v", d.EnvFile, d.Ports,
			d.NetworkMode, d.User, d.Privileged)
	}
	if !d.ReadOnly || !slices.Equal(d.CapDrop, []string{"ALL"}) || !slices.Contains(d.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("documents isn't locked down: read_only %v cap_drop %v security_opt %v", d.ReadOnly, d.CapDrop, d.SecurityOpt)
	}
	nets, _ := d.Networks.([]any)
	if len(nets) != 1 || nets[0] != "backend" || !c.Networks["backend"].Internal {
		t.Errorf("documents networks %v (backend internal %v), want only the internal backend", d.Networks,
			c.Networks["backend"].Internal)
	}
	if !slices.Equal(d.Volumes, []string{"trustedkeys:/trusted:ro"}) {
		t.Errorf("documents volumes %v, want only the trusted keys, read-only", d.Volumes)
	}
	e := d.Environment
	if e["APP_ENV"] != "development" || e["DOCUMENTS_NATS_URL"] != "nats://nats:4222" ||
		e["DOCUMENTS_NATS_PASSWORD"] != "${NATS_DOCUMENTS_PASSWORD:-}" ||
		e["DOCUMENTS_TRUSTED_KEYS_PATH"] != "/trusted/request-keys.json" ||
		!strings.Contains(e["DOCUMENTS_DATABASE_URL"], "${DOCUMENTS_DB_USER:-documents_app}:${DOCUMENTS_DB_PASSWORD:-}@") {
		t.Errorf("documents env: %v", e)
	}
	for k, v := range e {
		for _, bad := range []string{"NATS_GATEWAY_PASSWORD", "NATS_EMR_PASSWORD", "MFA_ENCRYPTION_KEY", "BFF_SERVICE_KEYS",
			"CORE_DB_PASSWORD", "EMR_DB_PASSWORD", "POSTGRES_PASSWORD", "PROVISION_DB_PASSWORD", "INTELLIGENCE_DB_PASSWORD",
			"NATS_JS_KEY", "REQUEST_KEY"} {
			if strings.Contains(k+v, bad) && !strings.Contains(k+v, "DOCUMENTS_DB_PASSWORD") {
				t.Errorf("documents env %s carries %s", k, bad)
			}
		}
	}
	// Only nats and the documents service ever see its NATS password, and
	// no other service mounts anything of the documents service's.
	for name, s := range c.Services {
		if name == "nats" || name == "documents" {
			continue
		}
		for k, v := range s.Environment {
			if strings.Contains(k+v, "NATS_DOCUMENTS_PASSWORD") || strings.Contains(k+v, "DOCUMENTS_DB_PASSWORD") {
				t.Errorf("%s env %s carries a documents secret", name, k)
			}
		}
	}
}

// Today's dev setups keep working: the documents NATS user is off unless
// NATS_DOCUMENTS_CONF=documents.conf, NATS needs no NATS_DOCUMENTS_PASSWORD
// while it is off, and the start-up guard refuses a missing or placeholder
// password when it is on, or any other fragment.
func TestComposeDocumentsNATSUserOffByDefault(t *testing.T) {
	svcs := loadCompose(t)
	n := svcs["nats"]
	if n.Environment["NATS_DOCUMENTS_CONF"] != "${NATS_DOCUMENTS_CONF:-documents-off.conf}" {
		t.Errorf("NATS_DOCUMENTS_CONF = %q, want the documents-off.conf default", n.Environment["NATS_DOCUMENTS_CONF"])
	}
	if pw := n.Environment["NATS_DOCUMENTS_PASSWORD"]; pw != "${NATS_DOCUMENTS_PASSWORD:-}" {
		t.Errorf("NATS_DOCUMENTS_PASSWORD = %q, want optional", pw)
	}
	if !slices.Contains(n.Volumes, "./services/nats/${NATS_DOCUMENTS_CONF:-documents-off.conf}:/etc/nats/documents.conf:ro") {
		t.Errorf("nats doesn't mount the documents fragment as /etc/nats/documents.conf: %v", n.Volumes)
	}

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
		{map[string]string{}, true}, // unset is off
		{map[string]string{"NATS_DOCUMENTS_CONF": "documents-off.conf"}, true},
		{map[string]string{"NATS_DOCUMENTS_CONF": "documents-off.conf", "NATS_DOCUMENTS_PASSWORD": ""}, true},
		{map[string]string{"NATS_DOCUMENTS_CONF": "documents.conf", "NATS_DOCUMENTS_PASSWORD": "syntheticDocsPassword0123456789"}, true},
		{map[string]string{"NATS_DOCUMENTS_CONF": "documents.conf"}, false},
		{map[string]string{"NATS_DOCUMENTS_CONF": "documents.conf", "NATS_DOCUMENTS_PASSWORD": "changemeReplaceWithLettersAndDigits"}, false},
		{map[string]string{"NATS_DOCUMENTS_CONF": "documents.conf", "NATS_DOCUMENTS_PASSWORD": "Placeholder0123456789"}, false},
		{map[string]string{"NATS_DOCUMENTS_CONF": "../../etc/passwd"}, false},
		{map[string]string{"NATS_DOCUMENTS_CONF": "emr.conf"}, false},
		// Both users on together.
		{map[string]string{"NATS_DOCUMENTS_CONF": "documents.conf", "NATS_DOCUMENTS_PASSWORD": "syntheticDocsPassword0123456789",
			"NATS_EMR_CONF": "emr.conf", "NATS_EMR_PASSWORD": "syntheticEmrPassword0123456789"}, true},
	} {
		if got := run(tc.env); got != tc.start {
			t.Errorf("%v: started %v, want %v", tc.env, got, tc.start)
		}
	}
	// .env.example leaves the documents user off.
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "NATS_DOCUMENTS_") || strings.HasPrefix(line, "DOCUMENTS_") {
			t.Errorf(".env.example sets %s", line)
		}
	}
}

// The documents image is built in CI from services/, like core's and the
// emr service's, and runs.
func TestDocumentsImageBuiltInCI(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	ci := string(raw)
	for _, want := range []string{"docker build -t mhai-documents:ci -f services/documents/Dockerfile services",
		`"usage: documents"*`, "- services/documents/**", "working-directory: services/documents",
		"--profile documents config -q"} {
		if !strings.Contains(ci, want) {
			t.Errorf("ci.yml lacks %q", want)
		}
	}
}
