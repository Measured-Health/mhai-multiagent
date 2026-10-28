package config

import (
	"strings"
	"testing"
)

const goodKey = "0123456789abcdef0123456789abcdef" // 32 bytes, synthetic

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{"CORE_ENV", "HTTP_ADDR", "DATABASE_URL", "NATS_URL", "NATS_NKEY_SEED_PATH", "NATS_GATEWAY_PASSWORD", "NATS_WORKER_PASSWORD",
		"ACCESS_KEY_PATH", "MFA_ENCRYPTION_KEY", "BFF_SERVICE_KEYS", "BREACHED_LIST_PATH", "CORE_API_DOCS"} {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadFailsOnMissingSecret(t *testing.T) {
	setEnv(t, map[string]string{"BFF_SERVICE_KEYS": goodKey})
	_, err := Load()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "MFA_ENCRYPTION_KEY") {
		t.Fatalf("error should name the variable, got %q", err)
	}
}

func TestLoadRejectsPlaceholder(t *testing.T) {
	for _, bad := range []string{"change-me", "changeme", "placeholder", "short-key-under-32-bytes",
		"change-me-change-me-change-me-xx", "Prefix-ChangeMe-0123456789abcdefghij", "0123456789abcdef-PLACEHOLDER-0123456789"} {
		setEnv(t, map[string]string{"MFA_ENCRYPTION_KEY": bad, "BFF_SERVICE_KEYS": goodKey})
		_, err := Load()
		if err == nil {
			t.Fatalf("%q should be rejected", bad)
		}
		if !strings.Contains(err.Error(), "MFA_ENCRYPTION_KEY") {
			t.Fatalf("error should name the variable, got %q", err)
		}
		if !strings.Contains(strings.ToLower(bad), "placeholder") && strings.Contains(err.Error(), bad) {
			t.Fatalf("error must not echo the value, got %q", err)
		}
	}
	setEnv(t, map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "BFF_SERVICE_KEYS": "changeme"})
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "BFF_SERVICE_KEYS") {
		t.Fatalf("placeholder BFF key should be rejected, got %v", err)
	}
}

func TestLoadOK(t *testing.T) {
	setEnv(t, map[string]string{
		"MFA_ENCRYPTION_KEY": goodKey,
		"BFF_SERVICE_KEYS":   goodKey + "," + strings.ToUpper(goodKey),
		"HTTP_ADDR":          ":9999",
	})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":9999" || string(c.MFAKey) != goodKey || len(c.BFFServiceKeys) != 2 {
		t.Fatalf("unexpected config: %+v", c)
	}
	setEnv(t, map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "BFF_SERVICE_KEYS": goodKey})
	c, _ = Load()
	if c.HTTPAddr != ":8080" || c.Env != "development" {
		t.Fatalf("defaults wrong: %+v", c)
	}
}

func TestLoadRejectsPlaceholderNATSCredentials(t *testing.T) {
	base := map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "BFF_SERVICE_KEYS": goodKey}
	cases := []struct{ name, key, val string }{
		{"url password", "NATS_URL", "nats://gateway:changemeLettersAndDigitsOnly@nats:4222"},
		{"url hyphen", "NATS_URL", "nats://gateway:change-me@nats:4222"},
		{"database password", "DATABASE_URL", "postgres://core_app:changemeLettersAndDigitsOnly@postgres:5432/mhai"},
	}
	for _, c := range cases {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		env[c.key] = c.val
		setEnv(t, env)
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), c.key) {
			t.Fatalf("%s: want an error naming %s, got %v", c.name, c.key, err)
		}
		if strings.Contains(err.Error(), "LettersAndDigits") || strings.Contains(err.Error(), "change-me") {
			t.Fatalf("%s: error echoes the value: %q", c.name, err)
		}
	}
}

func TestLoadRejectsMalformedNATSURLWithoutEchoing(t *testing.T) {
	setEnv(t, map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "BFF_SERVICE_KEYS": goodKey,
		"NATS_URL": "nats://gateway:s3cretpw@host:abc"})
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "NATS_URL") {
		t.Fatalf("want NATS_URL error, got %v", err)
	}
	if strings.Contains(err.Error(), "s3cretpw") {
		t.Fatalf("error leaks the password: %q", err)
	}
}

func TestLoadAcceptsRealNATSCredentialsAndSeedPath(t *testing.T) {
	setEnv(t, map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "BFF_SERVICE_KEYS": goodKey,
		"NATS_URL": "nats://gateway:k7Qw93hZ@nats:4222", "NATS_NKEY_SEED_PATH": "/run/secrets/seed"})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.NATSNkeySeedPath != "/run/secrets/seed" {
		t.Fatalf("seed path = %q", c.NATSNkeySeedPath)
	}
}

// The core service reads only its own NATS credentials (in NATS_URL or the
// NKey seed): the worker's and the standalone server passwords are not its
// business, so it neither needs nor checks them.
func TestLoadIgnoresOtherServicesSecrets(t *testing.T) {
	setEnv(t, map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "BFF_SERVICE_KEYS": goodKey,
		"NATS_WORKER_PASSWORD": "PLACEHOLDER", "NATS_GATEWAY_PASSWORD": "changeme"})
	if _, err := Load(); err != nil {
		t.Fatalf("core config read another service's secret: %v", err)
	}
}

// serve needs the database, NATS and the signing key; the errors name the
// variables only.
func TestValidateServe(t *testing.T) {
	if err := (Config{}).ValidateServe(); err == nil ||
		!strings.Contains(err.Error(), "DATABASE_URL") || !strings.Contains(err.Error(), "NATS_URL") ||
		!strings.Contains(err.Error(), "ACCESS_KEY_PATH") {
		t.Fatalf("want all three named, got %v", err)
	}
	ok := Config{DatabaseURL: "postgres://u:p@h/db", NATSURL: "nats://h:4222", AccessKeyPath: "/k.pem"}
	if err := ok.ValidateServe(); err != nil {
		t.Fatal(err)
	}
}

// Login passwords for the compose init step: placeholders and short values
// are refused, and the error names the variable, not the value.
func TestCheckPassword(t *testing.T) {
	for _, bad := range []string{"", "short", "changemeReplaceWithLettersAndDigits", "PLACEHOLDERxxxxxxxxxxxx"} {
		err := CheckPassword("CORE_DB_PASSWORD", bad)
		if err == nil || !strings.Contains(err.Error(), "CORE_DB_PASSWORD") {
			t.Fatalf("%q: %v", bad, err)
		}
		if bad != "" && strings.Contains(err.Error(), bad) {
			t.Fatalf("error echoes the value: %v", err)
		}
	}
	if err := CheckPassword("CORE_DB_PASSWORD", "test-only-core-db-password"); err != nil {
		t.Fatal(err)
	}
}

func TestAPIDocsSwitch(t *testing.T) {
	base := map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "BFF_SERVICE_KEYS": goodKey}
	for _, c := range []struct {
		env, flag string
		want      bool
		bad       bool
	}{
		{"", "", true, false}, // unset env is development
		{"development", "", true, false},
		{"production", "", false, false},
		{"preview", "", false, false},
		{"Production", "", false, false},
		{"production", "true", true, false},
		{"development", "false", false, false},
		{"preview", "1", true, false},
		{"development", "yes please", false, true},
	} {
		kv := map[string]string{"CORE_ENV": c.env, "CORE_API_DOCS": c.flag}
		for k, v := range base {
			kv[k] = v
		}
		setEnv(t, kv)
		got, err := Load()
		if c.bad {
			if err == nil || !strings.Contains(err.Error(), "CORE_API_DOCS") || strings.Contains(err.Error(), c.flag) {
				t.Errorf("flag %q: error %v", c.flag, err)
			}
			continue
		}
		if err != nil || got.APIDocs != c.want {
			t.Errorf("env %q flag %q: docs %v err %v, want %v", c.env, c.flag, got.APIDocs, err, c.want)
		}
	}
}
