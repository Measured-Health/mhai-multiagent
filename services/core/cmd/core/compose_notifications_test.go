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

// ADR 0018, notifications off by default (design §15): the notifications
// service exists only under the profile notifications, alone; it builds
// from services/, runs locked down (read-only root, no capabilities,
// no-new-privileges) on the internal backend network only with no port,
// gets its own NATS password and database login and nothing of core's or
// another service's, mounts only the public trusted keys, read-only, and
// sends nothing unless NOTIFY_CHANNELS names a channel.
func TestComposeNotificationsProfile(t *testing.T) {
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
	d, ok := c.Services["notifications"]
	if !ok {
		t.Fatal("no notifications service")
	}
	for name, s := range c.Services {
		if name != "notifications" && slices.Contains(s.Profiles, "notifications") {
			t.Errorf("%s is in the notifications profile", name)
		}
	}
	if !slices.Equal(d.Profiles, []string{"notifications"}) {
		t.Errorf("notifications profiles %v", d.Profiles)
	}
	if d.Image != "mhai-notifications:local" || d.Build["context"] != "./services" ||
		d.Build["dockerfile"] != "notifications/Dockerfile" {
		t.Errorf("notifications build %v image %q", d.Build, d.Image)
	}
	if d.EnvFile != nil || len(d.Ports) != 0 || d.NetworkMode != "" || d.User != "" || d.Privileged || len(d.CapAdd) != 0 {
		t.Errorf("notifications: %+v", d)
	}
	if !d.ReadOnly || !slices.Equal(d.CapDrop, []string{"ALL"}) || !slices.Contains(d.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("notifications isn't locked down: %v %v %v", d.ReadOnly, d.CapDrop, d.SecurityOpt)
	}
	if nets, _ := d.Networks.([]any); len(nets) != 1 || nets[0] != "backend" || !c.Networks["backend"].Internal {
		t.Errorf("notifications networks %v, want only the internal backend", d.Networks)
	}
	if !slices.Equal(d.Volumes, []string{"trustedkeys:/trusted:ro"}) {
		t.Errorf("notifications volumes %v, want only the trusted keys, read-only", d.Volumes)
	}
	e := d.Environment
	if e["APP_ENV"] != "development" || e["NOTIFY_NATS_URL"] != "nats://nats:4222" ||
		e["NOTIFY_NATS_PASSWORD"] != "${NATS_NOTIFICATIONS_PASSWORD:-}" ||
		e["NOTIFY_TRUSTED_KEYS_PATH"] != "/trusted/request-keys.json" || e["NOTIFY_CHANNELS"] != "${NOTIFY_CHANNELS:-}" ||
		!strings.Contains(e["NOTIFY_DATABASE_URL"], "${NOTIFY_DB_USER:-notify_app}:${NOTIFY_DB_PASSWORD:-}@") {
		t.Errorf("notifications env: %v", e)
	}
	for k, v := range e {
		for _, bad := range []string{"NATS_GATEWAY_PASSWORD", "NATS_EMR_PASSWORD", "NATS_DOCUMENTS_PASSWORD", "MFA_ENCRYPTION_KEY",
			"BFF_SERVICE_KEYS", "CORE_DB_PASSWORD", "EMR_DB_PASSWORD", "DOCUMENTS_DB_PASSWORD", "POSTGRES_PASSWORD",
			"PROVISION_DB_PASSWORD", "INTELLIGENCE_DB_PASSWORD", "NATS_JS_KEY", "REQUEST_KEY"} {
			if strings.Contains(k+v, bad) {
				t.Errorf("notifications env %s carries %s", k, bad)
			}
		}
	}
	// Only nats and the notifications service see its NATS password;
	// core-migrate (which makes the login) and the service see its database
	// password.
	if v := c.Services["core-migrate"].Environment["NOTIFY_DB_PASSWORD"]; v != "${NOTIFY_DB_PASSWORD:-}" {
		t.Errorf("core-migrate NOTIFY_DB_PASSWORD %q, want ${NOTIFY_DB_PASSWORD:-} (optional)", v)
	}
	for name, s := range c.Services {
		if name == "nats" || name == "notifications" {
			continue
		}
		for k, v := range s.Environment {
			if strings.Contains(k+v, "NATS_NOTIFICATIONS_PASSWORD") ||
				(strings.Contains(k+v, "NOTIFY_DB_PASSWORD") && !(name == "core-migrate" && k == "NOTIFY_DB_PASSWORD")) {
				t.Errorf("%s env %s carries a notifications secret", name, k)
			}
		}
	}
}

// Today's dev setups keep working: the notifications NATS user is off
// unless NATS_NOTIFICATIONS_CONF=notifications.conf, NATS needs no
// NATS_NOTIFICATIONS_PASSWORD while it is off, and the start-up guard
// refuses a missing or placeholder password when it is on, or any other
// fragment.
func TestComposeNotificationsNATSUserOffByDefault(t *testing.T) {
	n := loadCompose(t)["nats"]
	if n.Environment["NATS_NOTIFICATIONS_CONF"] != "${NATS_NOTIFICATIONS_CONF:-notifications-off.conf}" {
		t.Errorf("NATS_NOTIFICATIONS_CONF = %q", n.Environment["NATS_NOTIFICATIONS_CONF"])
	}
	if pw := n.Environment["NATS_NOTIFICATIONS_PASSWORD"]; pw != "${NATS_NOTIFICATIONS_PASSWORD:-}" {
		t.Errorf("NATS_NOTIFICATIONS_PASSWORD = %q, want optional", pw)
	}
	if !slices.Contains(n.Volumes,
		"./services/nats/${NATS_NOTIFICATIONS_CONF:-notifications-off.conf}:/etc/nats/notifications.conf:ro") {
		t.Errorf("nats doesn't mount the notifications fragment: %v", n.Volumes)
	}
	const execLine = "exec nats-server -c /etc/nats/nats.conf"
	script := strings.Replace(strings.ReplaceAll(n.Command[0], "$$", "$"), execLine, "echo started", 1)
	base := map[string]string{}
	for _, k := range natsUserSecrets {
		base[k] = "synthetic" + strings.ReplaceAll(strings.ToLower(k), "_", "") + "0123456789"
	}
	base["NATS_JS_KEY"] = "ksynthetic0123456789abcdef0123456789abcdef"
	good := "synthetic" + strings.Repeat("Notify", 3) + "0123"
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
		{map[string]string{}, true},
		{map[string]string{"NATS_NOTIFICATIONS_CONF": "notifications-off.conf"}, true},
		{map[string]string{"NATS_NOTIFICATIONS_CONF": "notifications.conf", "NATS_NOTIFICATIONS_PASSWORD": good}, true},
		{map[string]string{"NATS_NOTIFICATIONS_CONF": "notifications.conf"}, false},
		{map[string]string{"NATS_NOTIFICATIONS_CONF": "notifications.conf",
			"NATS_NOTIFICATIONS_PASSWORD": "changemeReplaceWithLettersAndDigits"}, false},
		{map[string]string{"NATS_NOTIFICATIONS_CONF": "../../etc/passwd"}, false},
		{map[string]string{"NATS_NOTIFICATIONS_CONF": "documents.conf"}, false},
	} {
		if got := run(tc.env); got != tc.start {
			t.Errorf("%v: started %v, want %v", tc.env, got, tc.start)
		}
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "NATS_NOTIFICATIONS_") || strings.HasPrefix(line, "NOTIFY_") {
			t.Errorf(".env.example sets %s", line)
		}
	}
}

// The notifications image is built in CI from services/, and runs.
func TestNotificationsImageBuiltInCI(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	ci := string(raw)
	for _, want := range []string{"docker build -t mhai-notifications:ci -f services/notifications/Dockerfile services",
		`"usage: notifications"*`, "- services/notifications/**", "working-directory: services/notifications",
		"--profile notifications config -q"} {
		if !strings.Contains(ci, want) {
			t.Errorf("ci.yml lacks %q", want)
		}
	}
}
