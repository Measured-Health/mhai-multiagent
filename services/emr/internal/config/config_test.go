package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func devEnv() map[string]string {
	return map[string]string{
		"APP_ENV":               "development",
		"EMR_NATS_URL":          "nats://nats:4222",
		"EMR_NATS_PASSWORD":     "syntheticEmrPassword0123456789",
		"EMR_DATABASE_URL":      "postgres://emr_app@postgres:5432/mhai?sslmode=disable",
		"EMR_TRUSTED_KEYS_PATH": "/trusted/request-keys.json",
		"EMR_SEAL_KEY_PATH":     "/emr-keys/seal.key",
	}
}

func prodEnv() map[string]string {
	return map[string]string{
		"APP_ENV":               "production",
		"EMR_NATS_URL":          "tls://nats:4222",
		"EMR_NATS_NKEY_FILE":    "/run/secrets/emr.nk",
		"EMR_DATABASE_URL":      "postgres://emr_app@db:5432/mhai?sslmode=verify-full",
		"EMR_TRUSTED_KEYS_PATH": "/trusted/request-keys.json",
		"EMR_SEAL_KEY_PATH":     "/emr-keys/seal.key",
	}
}

func TestSettingsLoad(t *testing.T) {
	s, err := FromEnv(env(devEnv()))
	if err != nil || !s.Development() || s.NATSPassword == "" || s.RuleSetActive != "" {
		t.Fatalf("%+v %v", s, err)
	}
	s, err = FromEnv(env(prodEnv()))
	if err != nil || s.Development() || s.NATSNKeyFile == "" {
		t.Fatalf("%+v %v", s, err)
	}
	// APP_ENV unset is production.
	m := prodEnv()
	delete(m, "APP_ENV")
	if s, err := FromEnv(env(m)); err != nil || s.Env != "production" {
		t.Fatalf("unset APP_ENV: %+v %v", s, err)
	}
}

// Refusals name the variable, never its value.
func TestSettingsRefusals(t *testing.T) {
	const secret = "syntheticEmrPassword0123456789"
	for _, tc := range []struct {
		name string
		base func() map[string]string
		edit func(m map[string]string)
		want string
	}{
		{"no nats url", devEnv, func(m map[string]string) { delete(m, "EMR_NATS_URL") }, "EMR_NATS_URL"},
		{"no database url", devEnv, func(m map[string]string) { delete(m, "EMR_DATABASE_URL") }, "EMR_DATABASE_URL"},
		{"no trusted keys", devEnv, func(m map[string]string) { delete(m, "EMR_TRUSTED_KEYS_PATH") }, "EMR_TRUSTED_KEYS_PATH"},
		{"no seal key", devEnv, func(m map[string]string) { delete(m, "EMR_SEAL_KEY_PATH") }, "EMR_SEAL_KEY_PATH"},
		{"no nats credential", devEnv, func(m map[string]string) { delete(m, "EMR_NATS_PASSWORD") }, "EMR_NATS_NKEY_FILE"},
		{"both nats credentials", devEnv, func(m map[string]string) { m["EMR_NATS_NKEY_FILE"] = "/x.nk" }, "EMR_NATS_PASSWORD"},
		{"password outside development", prodEnv, func(m map[string]string) {
			delete(m, "EMR_NATS_NKEY_FILE")
			m["EMR_NATS_PASSWORD"] = secret
		}, "EMR_NATS_PASSWORD"},
		{"placeholder password", devEnv, func(m map[string]string) { m["EMR_NATS_PASSWORD"] = "changemeReplaceWithLettersAndDigits" }, "EMR_NATS_PASSWORD"},
		{"credentials in the nats url", devEnv, func(m map[string]string) { m["EMR_NATS_URL"] = "nats://emr:" + secret + "@nats:4222" }, "EMR_NATS_URL"},
		{"plaintext nats outside development", prodEnv, func(m map[string]string) { m["EMR_NATS_URL"] = "nats://nats:4222" }, "EMR_NATS_URL"},
		{"database without verify-full", prodEnv, func(m map[string]string) {
			m["EMR_DATABASE_URL"] = "postgres://emr_app:" + secret + "@db:5432/mhai?sslmode=require"
		}, "EMR_DATABASE_URL"},
		{"unknown APP_ENV", devEnv, func(m map[string]string) { m["APP_ENV"] = "dev" }, "APP_ENV"},
		{"bad rule set name", devEnv, func(m map[string]string) { m["EMR_RULE_SET_ACTIVE"] = "../ruleset-1" }, "EMR_RULE_SET_ACTIVE"},
		{"proxy without key", devEnv, func(m map[string]string) { m["EMR_PROXY_URL"] = "http://egress-emr:3128" }, "EMR_PROXY_KEY_PATH"},
		{"proxy with credentials", devEnv, func(m map[string]string) {
			m["EMR_PROXY_URL"] = "http://emr:" + secret + "@egress-emr:3128"
			m["EMR_PROXY_KEY_PATH"] = "/k"
		}, "EMR_PROXY_URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.base()
			tc.edit(m)
			_, err := FromEnv(env(m))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q doesn't name %s", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "changeme") {
				t.Errorf("error carries a value: %q", err)
			}
		})
	}
}
