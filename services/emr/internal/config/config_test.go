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
		"APP_ENV":                "production",
		"EMR_NATS_URL":           "tls://nats:4222",
		"EMR_NATS_NKEY_FILE":     "/run/secrets/emr.nk",
		"EMR_DATABASE_URL":       "postgres://emr_app@db:5432/mhai?sslmode=verify-full",
		"EMR_TRUSTED_KEYS_PATH":  "/trusted/request-keys.json",
		"EMR_SEAL_KEY_PATH":      "/emr-keys/seal.key",
		"EMR_NATS_TLS_CA_FILE":   "/run/tls/ca.pem",
		"EMR_NATS_TLS_CERT_FILE": "/run/tls/emr.pem",
		"EMR_NATS_TLS_KEY_FILE":  "/run/tls/emr-key.pem",
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
		{"proxy without egress dir", devEnv, func(m map[string]string) {
			m["EMR_PROXY_URL"] = "http://egress-emr:3128"
			m["EMR_PROXY_KEY_PATH"] = "/k"
		}, "EMR_EGRESS_DIR"},
		{"egress dir without proxy", devEnv, func(m map[string]string) { m["EMR_EGRESS_DIR"] = "/emr-egress-conf" }, "EMR_PROXY_URL"},
		{"proxy with credentials", devEnv, func(m map[string]string) {
			m["EMR_PROXY_URL"] = "http://emr:" + secret + "@egress-emr:3128"
			m["EMR_PROXY_KEY_PATH"] = "/k"
			m["EMR_EGRESS_DIR"] = "/d"
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

// The normaliser's code tables and maps: off by default, both or neither.
func TestCodesAndMapsTogether(t *testing.T) {
	s, err := FromEnv(env(devEnv()))
	if err != nil || s.CodesDir != "" || s.MapsDir != "" {
		t.Fatalf("default: %+v %v", s, err)
	}
	for _, k := range []string{"EMR_CODES_DIR", "EMR_MAPS_DIR"} {
		e := devEnv()
		e[k] = "/app/x"
		if _, err := FromEnv(env(e)); err == nil || !strings.Contains(err.Error(), "EMR_CODES_DIR and EMR_MAPS_DIR") {
			t.Errorf("only %s: %v", k, err)
		}
	}
	e := devEnv()
	e["EMR_CODES_DIR"], e["EMR_MAPS_DIR"] = "/app/codes/testdata", "/app/maps"
	if s, err := FromEnv(env(e)); err != nil || s.CodesDir != "/app/codes/testdata" || s.MapsDir != "/app/maps" {
		t.Fatalf("both: %+v %v", s, err)
	}
}

// EMR_DEV_CA_FILE (the synthetic FHIR server's throwaway CA, plan Task 25)
// is accepted in development only: everywhere else TLS uses the system
// roots and nothing else.
func TestDevCAOnlyInDevelopment(t *testing.T) {
	e := devEnv()
	e["EMR_DEV_CA_FILE"] = " /synth/ca.pem "
	if s, err := FromEnv(env(e)); err != nil || s.DevCAFile != "/synth/ca.pem" {
		t.Fatalf("development: %+v %v", s, err)
	}
	for _, envName := range []string{"staging", "production"} {
		p := prodEnv()
		p["APP_ENV"] = envName
		p["EMR_DEV_CA_FILE"] = "/synth/ca.pem"
		if _, err := FromEnv(env(p)); err == nil || !strings.Contains(err.Error(), "EMR_DEV_CA_FILE") || strings.Contains(err.Error(), "/synth") {
			t.Errorf("%s: %v", envName, err)
		}
	}
}

// The rules (PR 7) are off by default: no rule set file, nothing active,
// the recommended breaker values. A rule set needs the normaliser's tables;
// the breaker values are bounded.
func TestRulesSettings(t *testing.T) {
	s, err := FromEnv(env(devEnv()))
	if err != nil || s.RuleSetFile != "" || s.RuleSetActive != "" || s.BreakerMultiple != 5 || s.BreakerFloorCritical != 10 {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	e := devEnv()
	e["EMR_RULE_SET_FILE"] = "/app/rules/ruleset-1.yaml"
	if _, err := FromEnv(env(e)); err == nil || !strings.Contains(err.Error(), "EMR_RULE_SET_FILE needs EMR_CODES_DIR") {
		t.Fatalf("rules without codes: %v", err)
	}
	e["EMR_CODES_DIR"], e["EMR_MAPS_DIR"] = "/app/codes", "/app/maps"
	e["EMR_BREAKER_MULTIPLE"], e["EMR_BREAKER_FLOOR_CRITICAL"] = "3.5", "25"
	if s, err := FromEnv(env(e)); err != nil || s.RuleSetFile == "" || s.BreakerMultiple != 3.5 || s.BreakerFloorCritical != 25 {
		t.Fatalf("set: %+v %v", s, err)
	}
	for k, v := range map[string]string{"EMR_BREAKER_MULTIPLE": "0", "EMR_BREAKER_FLOOR_CRITICAL": "0"} {
		e := devEnv()
		e[k] = v
		if _, err := FromEnv(env(e)); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%s: %v", k, v, err)
		}
	}
	for _, v := range []string{"NaN", "x", "1e9", "-1"} {
		e := devEnv()
		e["EMR_BREAKER_MULTIPLE"] = v
		if _, err := FromEnv(env(e)); err == nil {
			t.Errorf("EMR_BREAKER_MULTIPLE=%s accepted", v)
		}
	}
}

// Outside development NATS verifies a client certificate (as for core), so
// the three EMR_NATS_TLS_* files are required there, all or none
// everywhere; staging is held to the same rule. In development they are
// optional (the dev stacks run NATS without TLS).
func TestNATSTLSFiles(t *testing.T) {
	s, err := FromEnv(env(prodEnv()))
	if err != nil || !s.NATSTLS.Complete() || s.NATSTLS.Key != "/run/tls/emr-key.pem" {
		t.Fatalf("production with the files: %+v %v", s, err)
	}
	for _, envName := range []string{"staging", "production", ""} {
		for _, drop := range []string{"EMR_NATS_TLS_CA_FILE", "EMR_NATS_TLS_CERT_FILE", "EMR_NATS_TLS_KEY_FILE"} {
			p := prodEnv()
			p["APP_ENV"] = envName
			delete(p, drop)
			_, err := FromEnv(env(p))
			if err == nil || !strings.Contains(err.Error(), "EMR_NATS_TLS_") || strings.Contains(err.Error(), "/run/tls") {
				t.Errorf("%q without %s: %v", envName, drop, err)
			}
		}
	}
	d := devEnv()
	d["EMR_NATS_TLS_CA_FILE"] = "/run/tls/ca.pem"
	if _, err := FromEnv(env(d)); err == nil || !strings.Contains(err.Error(), "set together") {
		t.Fatalf("development, one file: %v", err)
	}
	d["EMR_NATS_TLS_CERT_FILE"], d["EMR_NATS_TLS_KEY_FILE"] = "/run/tls/emr.pem", "/run/tls/emr-key.pem"
	if s, err := FromEnv(env(d)); err != nil || !s.NATSTLS.Complete() {
		t.Fatalf("development, all three: %+v %v", s, err)
	}
}

// The secret store (plan Task 12, H3): production takes OpenBao only;
// development and staging either. The errors name the variable only.
func TestSecretStoreSettings(t *testing.T) {
	bao := func(m map[string]string) map[string]string {
		m["EMR_SECRET_STORE"] = "openbao"
		m["EMR_OPENBAO_ADDR"] = "https://openbao:8200"
		m["EMR_OPENBAO_CA_FILE"] = "/run/tls/ca.pem"
		m["EMR_OPENBAO_CLIENT_CERT_FILE"] = "/run/tls/emr.pem"
		m["EMR_OPENBAO_CLIENT_KEY_FILE"] = "/run/tls/emr-key.pem"
		m["EMR_OPENBAO_CREDENTIALS_FILE"] = "/run/secrets/openbao-emr.json"
		return m
	}
	if s, err := FromEnv(env(bao(prodEnv()))); err != nil || s.SecretStore != "openbao" {
		t.Fatalf("production with openbao: %+v %v", s, err)
	}
	m := prodEnv()
	if s, err := FromEnv(env(m)); err != nil || s.SecretStore != "none" {
		t.Fatalf("production with no store: %+v %v", s, err)
	}
	for _, e := range []func() map[string]string{devEnv, func() map[string]string { m := prodEnv(); m["APP_ENV"] = "staging"; return m }} {
		m := e()
		m["EMR_SECRETS_DIR"] = "/emr-secrets"
		if s, err := FromEnv(env(m)); err != nil || s.SecretStore != "file" {
			t.Fatalf("%s with the file store: %+v %v", m["APP_ENV"], s, err)
		}
		if s, err := FromEnv(env(bao(e()))); err != nil || s.SecretStore != "openbao" {
			t.Fatalf("%s with openbao: %+v %v", m["APP_ENV"], s, err)
		}
	}
	// Development may leave out the client certificate.
	d := bao(devEnv())
	delete(d, "EMR_OPENBAO_CLIENT_CERT_FILE")
	delete(d, "EMR_OPENBAO_CLIENT_KEY_FILE")
	if _, err := FromEnv(env(d)); err != nil {
		t.Fatalf("development without a client certificate: %v", err)
	}
	const secret = "syntheticCredentialInTheUrl0123"
	for _, tc := range []struct {
		name string
		edit func(m map[string]string)
		want string
	}{
		{"file store in production", func(m map[string]string) {
			for k := range bao(map[string]string{}) {
				delete(m, k)
			}
			m["EMR_SECRETS_DIR"] = "/emr-secrets"
		}, "EMR_SECRET_STORE=openbao"},
		{"file named in production", func(m map[string]string) {
			for k := range bao(map[string]string{}) {
				delete(m, k)
			}
			m["EMR_SECRET_STORE"] = "file"
			m["EMR_SECRETS_DIR"] = "/emr-secrets"
		}, "EMR_SECRET_STORE=openbao"},
		{"both stores", func(m map[string]string) { m["EMR_SECRETS_DIR"] = "/emr-secrets" }, "EMR_SECRETS_DIR"},
		{"plain http", func(m map[string]string) { m["EMR_OPENBAO_ADDR"] = "http://openbao:8200" }, "EMR_OPENBAO_ADDR"},
		{"credentials in the address", func(m map[string]string) {
			m["EMR_OPENBAO_ADDR"] = "https://emr:" + secret + "@openbao:8200"
		}, "EMR_OPENBAO_ADDR"},
		{"a path", func(m map[string]string) { m["EMR_OPENBAO_ADDR"] = "https://openbao:8200/v1" }, "EMR_OPENBAO_ADDR"},
		{"no CA", func(m map[string]string) { delete(m, "EMR_OPENBAO_CA_FILE") }, "EMR_OPENBAO_CA_FILE"},
		{"no credentials", func(m map[string]string) { delete(m, "EMR_OPENBAO_CREDENTIALS_FILE") }, "EMR_OPENBAO_CREDENTIALS_FILE"},
		{"no client certificate", func(m map[string]string) {
			delete(m, "EMR_OPENBAO_CLIENT_CERT_FILE")
			delete(m, "EMR_OPENBAO_CLIENT_KEY_FILE")
		}, "EMR_OPENBAO_CLIENT_CERT_FILE"},
		{"half a client certificate", func(m map[string]string) { delete(m, "EMR_OPENBAO_CLIENT_KEY_FILE") }, "EMR_OPENBAO_CLIENT_KEY_FILE"},
		{"unknown store", func(m map[string]string) { m["EMR_SECRET_STORE"] = "vault" }, "EMR_SECRET_STORE"},
		{"openbao settings without the store", func(m map[string]string) { delete(m, "EMR_SECRET_STORE") }, "EMR_OPENBAO_"},
	} {
		m := bao(prodEnv())
		tc.edit(m)
		_, err := FromEnv(env(m))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (want %s)", tc.name, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "/run/") {
			t.Errorf("%s: the error names a value: %v", tc.name, err)
		}
	}
}
