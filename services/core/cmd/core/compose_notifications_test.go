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

// The demo's email (deploy/compose.yml, profile notifications; deploy/README.md
// "Turn on email for the demo"): off by default; the service runs locked down
// as uid 65532 on internal networks only (backend and mail), with its own NATS
// NKey file, its own login and the SMTP password as read-only files, never a
// variable, and nothing of core's. Its one way out is mail-relay, pinned by
// digest, which forwards to the SMTP host and port only and holds nothing.
func TestDeployNotificationsProfile(t *testing.T) {
	c := loadDeployCompose(t)
	n, ok := c.Services["notifications"]
	r, ok2 := c.Services["mail-relay"]
	if !ok || !ok2 {
		t.Fatal("deploy/compose.yml has no notifications or mail-relay service")
	}
	for name, s := range c.Services {
		in := slices.Contains(s.Profiles, "notifications")
		if want := name == "notifications" || name == "mail-relay"; in != want {
			t.Errorf("%s in the notifications profile: %v", name, in)
		}
	}
	for name, s := range map[string]deployService{"notifications": n, "mail-relay": r} {
		if !slices.Equal(s.Profiles, []string{"notifications"}) {
			t.Errorf("%s profiles %v", name, s.Profiles)
		}
		if !s.ReadOnly || !slices.Equal(s.CapDrop, []string{"ALL"}) || !slices.Contains(s.SecurityOpt, "no-new-privileges:true") ||
			s.MemLimit == "" {
			t.Errorf("%s isn't locked down: %+v", name, s)
		}
		if s.User == "" || strings.HasPrefix(s.User, "0") || strings.HasPrefix(s.User, "root") {
			t.Errorf("%s user %q", name, s.User)
		}
		if len(s.Ports) != 0 || s.NetworkMode != "" || s.EnvFile.Kind != 0 {
			t.Errorf("%s: ports %v, network_mode %q, env_file set %v", name, s.Ports, s.NetworkMode, s.EnvFile.Kind != 0)
		}
	}
	if n.User != "65532:65532" || !strings.HasPrefix(n.Image, "${NOTIFICATIONS_IMAGE:-ghcr.io/measured-health/mhai-multiagent-notifications:") {
		t.Errorf("notifications user %q image %q", n.User, n.Image)
	}
	if !slices.Equal(n.networkNames(), []string{"backend", "mail"}) || !c.Networks["backend"].Internal || !c.Networks["mail"].Internal {
		t.Errorf("notifications networks %v: want the internal backend and mail only", n.networkNames())
	}
	if !slices.Equal(r.networkNames(), []string{"mail", "mail-egress"}) || c.Networks["mail-egress"].Internal {
		t.Errorf("mail-relay networks %v: want mail and its own way out, mail-egress", r.networkNames())
	}
	for name, s := range c.Services {
		for _, net := range []string{"mail", "mail-egress"} {
			if slices.Contains(s.networkNames(), net) && name != "mail-relay" && !(name == "notifications" && net == "mail") {
				t.Errorf("%s is on %s", name, net)
			}
		}
	}
	want := []string{"trustedkeys:/trusted:ro", "./secrets/nats-notifications.nk:/run/secrets/nats-notifications.nk:ro",
		"./secrets/smtp_password:/run/secrets/smtp_password:ro"}
	if !slices.Equal(n.Volumes, want) {
		t.Errorf("notifications volumes %v, want %v", n.Volumes, want)
	}
	e := n.Environment
	for k, v := range map[string]string{
		"NOTIFY_NATS_NKEY_FILE": "/run/secrets/nats-notifications.nk", "NOTIFY_SMTP_PASSWORD_FILE": "/run/secrets/smtp_password",
		"NOTIFY_CHANNELS": "${NOTIFY_CHANNELS:-}", "NOTIFY_APP_URL": "${CORE_APP_URL:-}",
		"NOTIFY_TRUSTED_KEYS_PATH": "/trusted/request-keys.json", "NOTIFY_SMTP_HOST": "${NOTIFY_SMTP_HOST:-}",
	} {
		if e[k] != v {
			t.Errorf("notifications %s = %q, want %q", k, e[k], v)
		}
	}
	if !strings.HasPrefix(e["NOTIFY_DATABASE_URL"], "postgres://notify_app:${NOTIFY_DB_PASSWORD:-}@postgres:5432/") {
		t.Errorf("notifications NOTIFY_DATABASE_URL %q: the notify_app login only", e["NOTIFY_DATABASE_URL"])
	}
	if _, ok := e["NOTIFY_SMTP_PASSWORD"]; ok {
		t.Error("notifications sets NOTIFY_SMTP_PASSWORD: the password is a file only")
	}
	for k, v := range e {
		for _, bad := range []string{"POSTGRES_PASSWORD", "CORE_DB_PASSWORD", "PROVISION_DB_PASSWORD", "INTELLIGENCE_DB_PASSWORD",
			"MFA_ENCRYPTION_KEY", "BFF_SERVICE_KEYS", "nats-gateway", "corekeys"} {
			if strings.Contains(k+v, bad) {
				t.Errorf("notifications env %s carries %s", k, bad)
			}
		}
	}
	// Only core-migrate (which makes the login) and the service see the
	// login's password; no other service mounts the SMTP password or the NKey.
	for name, s := range c.Services {
		if name == "notifications" {
			continue
		}
		for k, v := range s.Environment {
			if strings.Contains(v, "NOTIFY_DB_PASSWORD") && !(name == "core-migrate" && k == "NOTIFY_DB_PASSWORD") {
				t.Errorf("%s env %s carries NOTIFY_DB_PASSWORD", name, k)
			}
		}
		for _, v := range s.Volumes {
			if strings.Contains(v, "smtp_password") || strings.Contains(v, "nats-notifications") {
				t.Errorf("%s mounts %s", name, v)
			}
		}
	}
	if v := c.Services["core-migrate"].Environment["NOTIFY_DB_PASSWORD"]; v != "${NOTIFY_DB_PASSWORD:-}" {
		t.Errorf("core-migrate NOTIFY_DB_PASSWORD %q, want ${NOTIFY_DB_PASSWORD:-} (the login only when set)", v)
	}
	core := c.Services["core"].Environment
	if core["CORE_NOTIFY_ENABLED"] != "${CORE_NOTIFY_ENABLED:-false}" || core["CORE_DELIVERY"] != "${CORE_DELIVERY:-log}" {
		t.Errorf("core CORE_NOTIFY_ENABLED %q CORE_DELIVERY %q: off by default", core["CORE_NOTIFY_ENABLED"], core["CORE_DELIVERY"])
	}
	// The relay: pinned, no secret, no environment, no mount; it forwards to
	// the SMTP host and port and nothing else.
	if !regexp.MustCompile(`^alpine/socat:[0-9.]+@sha256:[0-9a-f]{64}$`).MatchString(r.Image) {
		t.Errorf("mail-relay image %q is not pinned by digest", r.Image)
	}
	if len(r.Environment) != 0 || len(r.Volumes) != 0 {
		t.Errorf("mail-relay env %v volumes %v: it holds nothing", r.Environment, r.Volumes)
	}
	if len(r.Command) != 2 || !strings.HasPrefix(r.Command[0], "TCP-LISTEN:${NOTIFY_SMTP_PORT:-587},") ||
		!strings.HasPrefix(r.Command[1], "TCP:${NOTIFY_SMTP_HOST:-smtp.invalid}:${NOTIFY_SMTP_PORT:-587},") {
		t.Errorf("mail-relay command %v", r.Command)
	}
	// NATS knows the user from the start under notifications-off.conf's
	// seedless key, until enable-notifications.sh writes the real one.
	off := stripOffKey(t)
	if got := c.Services["nats"].Environment["NATS_NOTIFICATIONS_NKEY"]; got != "${NATS_NOTIFICATIONS_NKEY:-"+off+"}" {
		t.Errorf("nats NATS_NOTIFICATIONS_NKEY = %q, want the seedless default %s", got, off)
	}
	// The deploy workflow publishes the image, ships the script, and starts
	// the profile only with NOTIFY_ENABLED=true.
	wf := readRepoFile(t, filepath.Join(".github", "workflows", "deploy.yml"))
	for _, want := range []string{"file: services/notifications/Dockerfile", "ghcr.io/${{ github.repository }}-notifications",
		"notifications/enable-notifications.sh", `if [ "$notify" = true ]; then`,
		"docker compose --profile notifications up -d mail-relay notifications"} {
		if !strings.Contains(wf, want) {
			t.Errorf("deploy.yml lacks %q", want)
		}
	}
}

// stripOffKey is the public key in services/nats/notifications-off.conf.
func stripOffKey(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^nkey: (U[A-Z2-7]{55})$`).FindStringSubmatch(readRepoFile(t, filepath.Join("services", "nats", "notifications-off.conf")))
	if m == nil {
		t.Fatal("notifications-off.conf has no public key")
	}
	return m[1]
}
