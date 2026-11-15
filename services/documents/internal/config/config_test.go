package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func devEnv() map[string]string {
	return map[string]string{
		"APP_ENV":                     "development",
		"DOCUMENTS_NATS_URL":          "nats://nats:4222",
		"DOCUMENTS_NATS_PASSWORD":     "syntheticDocumentsPassword0123456789",
		"DOCUMENTS_DATABASE_URL":      "postgres://documents_app@postgres:5432/mhai?sslmode=disable",
		"DOCUMENTS_TRUSTED_KEYS_PATH": "/trusted/request-keys.json",
	}
}

func prodEnv() map[string]string {
	return map[string]string{
		"APP_ENV":                     "production",
		"DOCUMENTS_NATS_URL":          "tls://nats:4222",
		"DOCUMENTS_NATS_NKEY_FILE":    "/run/secrets/documents.nk",
		"DOCUMENTS_DATABASE_URL":      "postgres://documents_app@db:5432/mhai?sslmode=verify-full",
		"DOCUMENTS_TRUSTED_KEYS_PATH": "/trusted/request-keys.json",
	}
}

func TestSettingsLoad(t *testing.T) {
	s, err := FromEnv(env(devEnv()))
	if err != nil || !s.Development() || s.NATSPassword == "" || s.NATSNKeyFile != "" {
		t.Fatalf("%+v %v", s, err)
	}
	s, err = FromEnv(env(prodEnv()))
	if err != nil || s.Development() || s.NATSNKeyFile == "" || s.Env != "production" {
		t.Fatalf("%+v %v", s, err)
	}
	// Staging is held to production's rules.
	m := prodEnv()
	m["APP_ENV"] = "staging"
	if s, err := FromEnv(env(m)); err != nil || s.Development() {
		t.Fatalf("staging: %+v %v", s, err)
	}
	// APP_ENV unset is production: what development relaxes stays off
	// unless it is named.
	m = prodEnv()
	delete(m, "APP_ENV")
	if s, err := FromEnv(env(m)); err != nil || s.Env != "production" {
		t.Fatalf("unset APP_ENV: %+v %v", s, err)
	}
	// Several NATS servers, each checked.
	m = prodEnv()
	m["DOCUMENTS_NATS_URL"] = "tls://n1:4222, tls://n2:4222"
	if _, err := FromEnv(env(m)); err != nil {
		t.Fatalf("two servers: %v", err)
	}
}

// Fails closed: every missing, malformed or relaxed setting is refused,
// and the refusal names the variable, never its value.
func TestSettingsRefusals(t *testing.T) {
	const secret = "syntheticDocumentsPassword0123456789"
	for _, tc := range []struct {
		name string
		base func() map[string]string
		edit func(m map[string]string)
		want string
	}{
		{"nothing set", func() map[string]string { return map[string]string{} }, func(map[string]string) {}, "DOCUMENTS_NATS_URL"},
		{"no nats url", devEnv, func(m map[string]string) { delete(m, "DOCUMENTS_NATS_URL") }, "DOCUMENTS_NATS_URL"},
		{"no database url", devEnv, func(m map[string]string) { delete(m, "DOCUMENTS_DATABASE_URL") }, "DOCUMENTS_DATABASE_URL"},
		{"blank database url", devEnv, func(m map[string]string) { m["DOCUMENTS_DATABASE_URL"] = "   " }, "DOCUMENTS_DATABASE_URL"},
		{"no trusted keys", devEnv, func(m map[string]string) { delete(m, "DOCUMENTS_TRUSTED_KEYS_PATH") }, "DOCUMENTS_TRUSTED_KEYS_PATH"},
		{"relative trusted keys", devEnv, func(m map[string]string) { m["DOCUMENTS_TRUSTED_KEYS_PATH"] = "keys.json" }, "DOCUMENTS_TRUSTED_KEYS_PATH"},
		{"no nats credential", devEnv, func(m map[string]string) { delete(m, "DOCUMENTS_NATS_PASSWORD") }, "DOCUMENTS_NATS_NKEY_FILE"},
		{"both nats credentials", devEnv, func(m map[string]string) { m["DOCUMENTS_NATS_NKEY_FILE"] = "/x.nk" }, "DOCUMENTS_NATS_PASSWORD"},
		{"password outside development", prodEnv, func(m map[string]string) {
			delete(m, "DOCUMENTS_NATS_NKEY_FILE")
			m["DOCUMENTS_NATS_PASSWORD"] = secret
		}, "DOCUMENTS_NATS_PASSWORD"},
		{"placeholder password", devEnv, func(m map[string]string) {
			m["DOCUMENTS_NATS_PASSWORD"] = "changemeReplaceWithLettersAndDigits"
		}, "DOCUMENTS_NATS_PASSWORD"},
		{"placeholder password, other spelling", devEnv, func(m map[string]string) {
			m["DOCUMENTS_NATS_PASSWORD"] = "Change-Me-please"
		}, "DOCUMENTS_NATS_PASSWORD"},
		{"credentials in the nats url", devEnv, func(m map[string]string) {
			m["DOCUMENTS_NATS_URL"] = "nats://documents:" + secret + "@nats:4222"
		}, "DOCUMENTS_NATS_URL"},
		{"plaintext nats outside development", prodEnv, func(m map[string]string) { m["DOCUMENTS_NATS_URL"] = "nats://nats:4222" }, "DOCUMENTS_NATS_URL"},
		{"one plaintext server of two", prodEnv, func(m map[string]string) {
			m["DOCUMENTS_NATS_URL"] = "tls://n1:4222,nats://n2:4222"
		}, "DOCUMENTS_NATS_URL"},
		{"not a nats url", devEnv, func(m map[string]string) { m["DOCUMENTS_NATS_URL"] = "http://nats:4222" }, "DOCUMENTS_NATS_URL"},
		{"nats url without a host", devEnv, func(m map[string]string) { m["DOCUMENTS_NATS_URL"] = "nats://" }, "DOCUMENTS_NATS_URL"},
		{"database without verify-full", prodEnv, func(m map[string]string) {
			m["DOCUMENTS_DATABASE_URL"] = "postgres://documents_app:" + secret + "@db:5432/mhai?sslmode=require"
		}, "DOCUMENTS_DATABASE_URL"},
		{"database with two sslmodes", prodEnv, func(m map[string]string) {
			m["DOCUMENTS_DATABASE_URL"] = "postgres://documents_app@db:5432/mhai?sslmode=verify-full&sslmode=disable"
		}, "DOCUMENTS_DATABASE_URL"},
		{"database not postgres", prodEnv, func(m map[string]string) {
			m["DOCUMENTS_DATABASE_URL"] = "mysql://documents_app@db:5432/mhai?sslmode=verify-full"
		}, "DOCUMENTS_DATABASE_URL"},
		{"database not postgres in development", devEnv, func(m map[string]string) {
			m["DOCUMENTS_DATABASE_URL"] = "mysql://documents_app@db/mhai"
		}, "DOCUMENTS_DATABASE_URL"},
		{"unknown APP_ENV", devEnv, func(m map[string]string) { m["APP_ENV"] = "dev" }, "APP_ENV"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.base()
			tc.edit(m)
			s, err := FromEnv(env(m))
			if err == nil {
				t.Fatal("accepted")
			}
			if s != (Settings{}) {
				t.Errorf("returned settings with the error: %+v", s)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q doesn't name %s", err, tc.want)
			}
			for _, v := range []string{secret, "changeme", "Change-Me", "documents_app", "n2:4222"} {
				if strings.Contains(err.Error(), v) {
					t.Errorf("error carries a value (%s): %q", v, err)
				}
			}
		})
	}
}
