package config

import (
	"strings"
	"testing"
)

const goodKey = "0123456789abcdef0123456789abcdef" // 32 bytes, synthetic

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{"GATEWAY_ENV", "HTTP_ADDR", "DATABASE_URL", "NATS_URL", "NATS_CREDS",
		"ACCESS_KEY_PATH", "MFA_ENCRYPTION_KEY", "BFF_SERVICE_KEYS", "BREACHED_LIST_PATH"} {
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
