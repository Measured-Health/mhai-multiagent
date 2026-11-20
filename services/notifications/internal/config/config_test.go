package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// devPassword is a synthetic NATS password, built at run time so no
// password-like literal sits in the repository.
func devPassword() string { return "synthetic" + strings.Repeat("Notify", 4) + "0123" }

func devEnv() map[string]string {
	return map[string]string{
		"APP_ENV":                  "development",
		"NOTIFY_NATS_URL":          "nats://nats:4222",
		"NOTIFY_NATS_PASSWORD":     devPassword(),
		"NOTIFY_DATABASE_URL":      "postgres://notify_app@postgres:5432/mhai?sslmode=disable",
		"NOTIFY_TRUSTED_KEYS_PATH": "/trusted/request-keys.json",
	}
}

func prodEnv() map[string]string {
	return map[string]string{
		"APP_ENV":                  "production",
		"NOTIFY_NATS_URL":          "tls://nats:4222",
		"NOTIFY_NATS_NKEY_FILE":    "/run/secrets/notifications.nk",
		"NOTIFY_DATABASE_URL":      "postgres://notify_app@db:5432/mhai?sslmode=verify-full",
		"NOTIFY_TRUSTED_KEYS_PATH": "/trusted/request-keys.json",
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
	// No channel by default: the service sends nothing.
	if s, err := FromEnv(env(devEnv())); err != nil || s.Sends() || s.Channels != nil {
		t.Fatalf("default channels: %+v %v", s, err)
	}
	m = devEnv()
	m["NOTIFY_CHANNELS"] = " log "
	if s, err := FromEnv(env(m)); err != nil || !s.Sends() || len(s.Channels) != 1 || s.Channels[0] != ChannelLog {
		t.Fatalf("log in development: %+v %v", s, err)
	}
	// Several NATS servers, each checked.
	m = prodEnv()
	m["NOTIFY_NATS_URL"] = "tls://n1:4222, tls://n2:4222"
	if _, err := FromEnv(env(m)); err != nil {
		t.Fatalf("two servers: %v", err)
	}
}

// Fails closed: every missing, malformed or relaxed setting is refused,
// and the refusal names the variable, never its value.
func TestSettingsRefusals(t *testing.T) {
	secret := devPassword()
	for _, tc := range []struct {
		name string
		base func() map[string]string
		edit func(m map[string]string)
		want string
	}{
		{"nothing set", func() map[string]string { return map[string]string{} }, func(map[string]string) {}, "NOTIFY_NATS_URL"},
		{"no nats url", devEnv, func(m map[string]string) { delete(m, "NOTIFY_NATS_URL") }, "NOTIFY_NATS_URL"},
		{"no database url", devEnv, func(m map[string]string) { delete(m, "NOTIFY_DATABASE_URL") }, "NOTIFY_DATABASE_URL"},
		{"blank database url", devEnv, func(m map[string]string) { m["NOTIFY_DATABASE_URL"] = "   " }, "NOTIFY_DATABASE_URL"},
		{"no trusted keys", devEnv, func(m map[string]string) { delete(m, "NOTIFY_TRUSTED_KEYS_PATH") }, "NOTIFY_TRUSTED_KEYS_PATH"},
		{"relative trusted keys", devEnv, func(m map[string]string) { m["NOTIFY_TRUSTED_KEYS_PATH"] = "keys.json" }, "NOTIFY_TRUSTED_KEYS_PATH"},
		{"no nats credential", devEnv, func(m map[string]string) { delete(m, "NOTIFY_NATS_PASSWORD") }, "NOTIFY_NATS_NKEY_FILE"},
		{"both nats credentials", devEnv, func(m map[string]string) { m["NOTIFY_NATS_NKEY_FILE"] = "/x.nk" }, "NOTIFY_NATS_PASSWORD"},
		{"password outside development", prodEnv, func(m map[string]string) {
			delete(m, "NOTIFY_NATS_NKEY_FILE")
			m["NOTIFY_NATS_PASSWORD"] = secret
		}, "NOTIFY_NATS_PASSWORD"},
		{"placeholder password", devEnv, func(m map[string]string) {
			m["NOTIFY_NATS_PASSWORD"] = "changemeReplaceWithLettersAndDigits"
		}, "NOTIFY_NATS_PASSWORD"},
		{"placeholder password, other spelling", devEnv, func(m map[string]string) {
			m["NOTIFY_NATS_PASSWORD"] = "Change-Me-please"
		}, "NOTIFY_NATS_PASSWORD"},
		{"credentials in the nats url", devEnv, func(m map[string]string) {
			m["NOTIFY_NATS_URL"] = "nats://notifications:" + secret + "@nats:4222"
		}, "NOTIFY_NATS_URL"},
		{"plaintext nats outside development", prodEnv, func(m map[string]string) { m["NOTIFY_NATS_URL"] = "nats://nats:4222" }, "NOTIFY_NATS_URL"},
		{"one plaintext server of two", prodEnv, func(m map[string]string) {
			m["NOTIFY_NATS_URL"] = "tls://n1:4222,nats://n2:4222"
		}, "NOTIFY_NATS_URL"},
		{"not a nats url", devEnv, func(m map[string]string) { m["NOTIFY_NATS_URL"] = "http://nats:4222" }, "NOTIFY_NATS_URL"},
		{"nats url without a host", devEnv, func(m map[string]string) { m["NOTIFY_NATS_URL"] = "nats://" }, "NOTIFY_NATS_URL"},
		{"database without verify-full", prodEnv, func(m map[string]string) {
			m["NOTIFY_DATABASE_URL"] = "postgres://notify_app:" + secret + "@db:5432/mhai?sslmode=require"
		}, "NOTIFY_DATABASE_URL"},
		{"database with two sslmodes", prodEnv, func(m map[string]string) {
			m["NOTIFY_DATABASE_URL"] = "postgres://notify_app@db:5432/mhai?sslmode=verify-full&sslmode=disable"
		}, "NOTIFY_DATABASE_URL"},
		{"database not postgres", prodEnv, func(m map[string]string) {
			m["NOTIFY_DATABASE_URL"] = "mysql://notify_app@db:5432/mhai?sslmode=verify-full"
		}, "NOTIFY_DATABASE_URL"},
		{"database not postgres in development", devEnv, func(m map[string]string) {
			m["NOTIFY_DATABASE_URL"] = "mysql://notify_app@db/mhai"
		}, "NOTIFY_DATABASE_URL"},
		{"unknown APP_ENV", devEnv, func(m map[string]string) { m["APP_ENV"] = "dev" }, "APP_ENV"},
		{"log channel outside development", prodEnv, func(m map[string]string) { m["NOTIFY_CHANNELS"] = "log" }, "NOTIFY_CHANNELS"},
		{"email channel before PR 4", devEnv, func(m map[string]string) { m["NOTIFY_CHANNELS"] = "email" }, "NOTIFY_CHANNELS"},
		{"unknown channel", devEnv, func(m map[string]string) { m["NOTIFY_CHANNELS"] = "sms" }, "NOTIFY_CHANNELS"},
		{"a channel twice", devEnv, func(m map[string]string) { m["NOTIFY_CHANNELS"] = "log, log" }, "NOTIFY_CHANNELS"},
		{"an empty channel name", devEnv, func(m map[string]string) { m["NOTIFY_CHANNELS"] = "log," }, "NOTIFY_CHANNELS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.base()
			tc.edit(m)
			s, err := FromEnv(env(m))
			if err == nil {
				t.Fatal("accepted")
			}
			if s.NATSURL != "" || s.DatabaseURL != "" || s.Channels != nil {
				t.Errorf("returned settings with the error: %+v", s)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q doesn't name %s", err, tc.want)
			}
			for _, v := range []string{secret, "changeme", "Change-Me", "notify_app", "n2:4222"} {
				if strings.Contains(err.Error(), v) {
					t.Errorf("error carries a value (%s): %q", v, err)
				}
			}
		})
	}
}
