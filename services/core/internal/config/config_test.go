package config

import (
	"strings"
	"testing"
)

const goodKey = "0123456789abcdef0123456789abcdef" // 32 bytes, synthetic

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{"CORE_ENV", "HTTP_ADDR", "DATABASE_URL", "NATS_URL", "NATS_NKEY_SEED_PATH", "NATS_GATEWAY_PASSWORD", "NATS_WORKER_PASSWORD",
		"ACCESS_KEY_PATH", "REQUEST_KEY_PATH", "MFA_ENCRYPTION_KEY", "BFF_SERVICE_KEYS", "BREACHED_LIST_PATH", "CORE_API_DOCS",
		"NATS_TLS_CA_PATH", "NATS_TLS_CERT_PATH", "NATS_TLS_KEY_PATH", "NATS_JS_REPLICAS", "JOBS_PIPELINE_MAX_ACK_PENDING",
		"INTELLIGENCE_QUERY_CAP", "CORE_HTTP_API", "CORE_NATS_API_CONCURRENCY", "CORE_ALLOW_SINGLE_NODE"} {
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
	if c.HTTPAddr != ":8080" || c.Env != "production" {
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
	ok := Config{Env: "development", DatabaseURL: "postgres://u:p@h/db", NATSURL: "nats://h:4222", AccessKeyPath: "/k.pem", RequestKeyPath: "/r.pem"}
	if err := ok.ValidateServe(); err != nil {
		t.Fatal(err)
	}
}

// serve needs the request-token key too (spec §7.2), in its own file: one
// key must never sign both token kinds.
func TestServeNeedsRequestKey(t *testing.T) {
	c := Config{Env: "development", DatabaseURL: "postgres://u:p@h/db", NATSURL: "nats://h:4222", AccessKeyPath: "/keys/access.pem"}
	if err := c.ValidateServe(); err == nil || !strings.Contains(err.Error(), "REQUEST_KEY_PATH") {
		t.Fatalf("want REQUEST_KEY_PATH named, got %v", err)
	}
	c.RequestKeyPath = c.AccessKeyPath
	err := c.ValidateServe()
	if err == nil || !strings.Contains(err.Error(), "REQUEST_KEY_PATH") || strings.Contains(err.Error(), "/keys/") {
		t.Fatalf("same file for both keys: %v", err)
	}
	setEnv(t, map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "BFF_SERVICE_KEYS": goodKey, "REQUEST_KEY_PATH": "/keys/request.pem"})
	got, err := Load()
	if err != nil || got.RequestKeyPath != "/keys/request.pem" {
		t.Fatalf("Load: %q %v", got.RequestKeyPath, err)
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
		{"", "", false, false}, // unset env is production: docs off
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

// The NATS TLS files, the stream replica count and the pipeline consumer's
// ack window come from the environment; bad values are refused by name.
func TestLoadNATSTLSAndStreams(t *testing.T) {
	base := map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "BFF_SERVICE_KEYS": goodKey}
	setEnv(t, base)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.NATSJSReplicas != 1 || c.JobsPipelineMaxAckPending != 8 || c.NATSTLS != (NATSTLS{}) {
		t.Fatalf("defaults: %d %d %+v", c.NATSJSReplicas, c.JobsPipelineMaxAckPending, c.NATSTLS)
	}
	env := map[string]string{"NATS_TLS_CA_PATH": "/tls/ca.pem", "NATS_TLS_CERT_PATH": "/tls/core.pem",
		"NATS_TLS_KEY_PATH": "/tls/core-key.pem", "NATS_JS_REPLICAS": "3", "JOBS_PIPELINE_MAX_ACK_PENDING": "16"}
	for k, v := range base {
		env[k] = v
	}
	setEnv(t, env)
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	if c.NATSTLS != (NATSTLS{CAPath: "/tls/ca.pem", CertPath: "/tls/core.pem", KeyPath: "/tls/core-key.pem"}) ||
		c.NATSJSReplicas != 3 || c.JobsPipelineMaxAckPending != 16 {
		t.Fatalf("got %+v %d %d", c.NATSTLS, c.NATSJSReplicas, c.JobsPipelineMaxAckPending)
	}
	for name, bad := range map[string][]string{"NATS_JS_REPLICAS": {"0", "2", "4", "-1", "x", "7"},
		"JOBS_PIPELINE_MAX_ACK_PENDING": {"0", "-3", "abc", "100001"}} {
		for _, v := range bad {
			e := map[string]string{name: v}
			for k, v := range base {
				e[k] = v
			}
			setEnv(t, e)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("%s=%q: %v", name, v, err)
			}
		}
	}
}

// TLS (with a client certificate: verify is on in production) is required
// on the NATS connection everywhere but development; an unknown or empty
// environment fails closed. A partial TLS setting is refused everywhere.
func TestValidateServeRequiresNATSTLSOutsideDevelopment(t *testing.T) {
	full := NATSTLS{CAPath: "/tls/ca.pem", CertPath: "/tls/core.pem", KeyPath: "/tls/core-key.pem"}
	base := Config{DatabaseURL: "postgres://u:p@h/db?sslmode=verify-full", NATSURL: "nats://h:4222", AccessKeyPath: "/k.pem", RequestKeyPath: "/r.pem",
		NATSJSReplicas: 3}
	for _, env := range []string{"production", "Production", "staging", ""} {
		c := base
		c.Env = env
		if err := c.ValidateServe(); err == nil || !strings.Contains(err.Error(), "NATS_TLS") {
			t.Fatalf("%q without TLS: %v", env, err)
		}
		c.NATSTLS = full
		if err := c.ValidateServe(); err != nil {
			t.Fatalf("%q with TLS: %v", env, err)
		}
	}
	c := base
	c.Env = "development"
	if err := c.ValidateServe(); err != nil {
		t.Fatalf("development without TLS: %v", err)
	}
	for _, partial := range []NATSTLS{{CAPath: "/tls/ca.pem"}, {CertPath: "/c", KeyPath: "/k"}, {CAPath: "/a", CertPath: "/c"}} {
		c.NATSTLS = partial
		if err := c.ValidateServe(); err == nil || !strings.Contains(err.Error(), "NATS_TLS") || strings.Contains(err.Error(), "/tls/") {
			t.Fatalf("partial %+v: %v", partial, err)
		}
	}
}

// INTELLIGENCE_QUERY_CAP is the questions in flight per hospital (decision
// 8): 10 by default, 1 to 1000.
func TestLoadQueryCap(t *testing.T) {
	base := map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "BFF_SERVICE_KEYS": goodKey}
	setEnv(t, base)
	c, err := Load()
	if err != nil || c.IntelligenceQueryCap != 10 {
		t.Fatalf("default: %d %v", c.IntelligenceQueryCap, err)
	}
	e := map[string]string{"INTELLIGENCE_QUERY_CAP": "25"}
	for k, v := range base {
		e[k] = v
	}
	setEnv(t, e)
	if c, err = Load(); err != nil || c.IntelligenceQueryCap != 25 {
		t.Fatalf("25: %d %v", c.IntelligenceQueryCap, err)
	}
	for _, bad := range []string{"0", "-1", "1001", "ten"} {
		e["INTELLIGENCE_QUERY_CAP"] = bad
		setEnv(t, e)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "INTELLIGENCE_QUERY_CAP") {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}

// CORE_HTTP_API (spec §17.1): on by default until mhai-web's BFF switches to
// NATS. Off, the HTTP listener serves only the probes, on loopback: the
// default address is 127.0.0.1:8080, any other host is refused, and the
// BFF key is no longer needed.
func TestHTTPAPISwitch(t *testing.T) {
	setEnv(t, map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "BFF_SERVICE_KEYS": goodKey})
	c, err := Load()
	if err != nil || !c.HTTPAPI || c.HTTPAddr != ":8080" {
		t.Fatalf("default: %+v %v", c, err)
	}
	for _, on := range []string{"on", "true", "ON"} {
		setEnv(t, map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "BFF_SERVICE_KEYS": goodKey, "CORE_HTTP_API": on})
		if c, err := Load(); err != nil || !c.HTTPAPI {
			t.Fatalf("%q: %+v %v", on, c, err)
		}
	}
	// On, the BFF key is still required.
	setEnv(t, map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "CORE_HTTP_API": "on"})
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "BFF_SERVICE_KEYS") {
		t.Fatalf("on without a BFF key: %v", err)
	}
	for _, off := range []string{"off", "false", "Off"} {
		setEnv(t, map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "CORE_HTTP_API": off})
		c, err := Load()
		if err != nil || c.HTTPAPI || c.HTTPAddr != "127.0.0.1:8080" || len(c.BFFServiceKeys) != 0 {
			t.Fatalf("%q: %+v %v", off, c, err)
		}
	}
	for _, addr := range []string{"127.0.0.1:9090", "[::1]:8080", "127.0.0.2:8080"} {
		setEnv(t, map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "CORE_HTTP_API": "off", "HTTP_ADDR": addr})
		if c, err := Load(); err != nil || c.HTTPAddr != addr {
			t.Fatalf("%q: %+v %v", addr, c, err)
		}
	}
	for _, addr := range []string{":8080", "0.0.0.0:8080", "10.0.0.5:8080", "[::]:8080", "core:8080", "localhost:8080", "nonsense"} {
		setEnv(t, map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "CORE_HTTP_API": "off", "HTTP_ADDR": addr})
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "HTTP_ADDR") {
			t.Fatalf("off with %q: %v", addr, err)
		}
	}
	setEnv(t, map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "BFF_SERVICE_KEYS": goodKey, "CORE_HTTP_API": "maybe"})
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CORE_HTTP_API") {
		t.Fatalf("bad value: %v", err)
	}
}

// CORE_NATS_API_CONCURRENCY bounds the api.v1 messages handled at once.
func TestLoadNATSAPIConcurrency(t *testing.T) {
	base := map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "BFF_SERVICE_KEYS": goodKey}
	setEnv(t, base)
	c, err := Load()
	if err != nil || c.NATSAPIConcurrency != 256 {
		t.Fatalf("default: %d %v", c.NATSAPIConcurrency, err)
	}
	e := map[string]string{"CORE_NATS_API_CONCURRENCY": "32"}
	for k, v := range base {
		e[k] = v
	}
	setEnv(t, e)
	if c, err = Load(); err != nil || c.NATSAPIConcurrency != 32 {
		t.Fatalf("32: %d %v", c.NATSAPIConcurrency, err)
	}
	for _, bad := range []string{"0", "-1", "100001", "many"} {
		e["CORE_NATS_API_CONCURRENCY"] = bad
		setEnv(t, e)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CORE_NATS_API_CONCURRENCY") {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}

// TestUnsetEnvIsProduction (Task 7 review): an unset CORE_ENV fails
// closed: production, no API docs, NATS TLS required, and no development
// relaxation (test-only intelligence operations are granted only when
// Development is true).
func TestUnsetEnvIsProduction(t *testing.T) {
	setEnv(t, map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "BFF_SERVICE_KEYS": goodKey})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != "production" || c.Development() || c.APIDocs {
		t.Fatalf("unset CORE_ENV: env %q development %v docs %v", c.Env, c.Development(), c.APIDocs)
	}
	c.DatabaseURL, c.NATSURL, c.AccessKeyPath, c.RequestKeyPath = "postgres://u:p@h/db", "nats://h:4222", "/k.pem", "/r.pem"
	if err := c.ValidateServe(); err == nil || !strings.Contains(err.Error(), "NATS_TLS") {
		t.Fatalf("unset CORE_ENV without NATS TLS: %v", err)
	}
	for env, want := range map[string]bool{"development": true, " Development ": true, "production": false, "dev": false, "": false} {
		if got := (Config{Env: env}).Development(); got != want {
			t.Errorf("Development(%q) = %v", env, got)
		}
	}
}

// productionServe is a production config ValidateServe accepts.
func productionServe() Config {
	return Config{Env: "production", DatabaseURL: "postgres://u:p@db.internal/mhai?sslmode=verify-full", NATSURL: "tls://h:4222",
		AccessKeyPath: "/k.pem", RequestKeyPath: "/r.pem", NATSJSReplicas: 3,
		NATSTLS: NATSTLS{CAPath: "/tls/ca.pem", CertPath: "/tls/core.pem", KeyPath: "/tls/core-key.pem"}}
}

// Final review I-2: outside development core's Postgres connection must be
// sslmode=verify-full (no escape hatch), whatever else the URL says; an
// unknown or empty environment fails closed. Errors never carry the URL.
func TestValidateServeRequiresVerifyFullOutsideDevelopment(t *testing.T) {
	if err := productionServe().ValidateServe(); err != nil {
		t.Fatalf("production with verify-full: %v", err)
	}
	for _, env := range []string{"production", "staging", ""} {
		for _, u := range []string{
			"postgres://u:secret-pw@db.internal/mhai",
			"postgres://u:secret-pw@db.internal/mhai?sslmode=disable",
			"postgres://u:secret-pw@db.internal/mhai?sslmode=require",
			"postgres://u:secret-pw@db.internal/mhai?sslmode=verify-ca",
			"postgres://u:secret-pw@db.internal/mhai?sslmode=prefer",
			"postgres://u:secret-pw@db.internal/mhai?sslmode=verify-full&sslmode=disable",
			"host=db.internal user=u password=secret-pw sslmode=verify-full",
		} {
			c := productionServe()
			c.Env, c.DatabaseURL = env, u
			err := c.ValidateServe()
			if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") || !strings.Contains(err.Error(), "sslmode=verify-full") {
				t.Fatalf("%q %q: %v", env, u, err)
			}
			if strings.Contains(err.Error(), "secret-pw") || strings.Contains(err.Error(), "db.internal") {
				t.Fatalf("error carries the URL: %v", err)
			}
		}
	}
	// The single-node escape hatch is for replicas only, never TLS.
	c := productionServe()
	c.DatabaseURL, c.AllowSingleNode = "postgres://u:p@h/db?sslmode=disable", true
	if err := c.ValidateServe(); err == nil || !strings.Contains(err.Error(), "sslmode=verify-full") {
		t.Fatalf("single node without TLS: %v", err)
	}
	// Development (the demo and every test stack) is unchanged.
	d := Config{Env: "development", DatabaseURL: "postgres://u:p@h/db?sslmode=disable", NATSURL: "nats://h:4222",
		AccessKeyPath: "/k.pem", RequestKeyPath: "/r.pem", NATSJSReplicas: 1}
	if err := d.ValidateServe(); err != nil {
		t.Fatalf("development: %v", err)
	}
}

// Final review I-2: outside development JetStream needs 3 or more replicas,
// unless CORE_ALLOW_SINGLE_NODE=true says this is a single-node install on
// purpose (serve then logs a warning: SingleNode).
func TestValidateServeRequiresReplicasOutsideDevelopment(t *testing.T) {
	for _, n := range []int{0, 1} {
		c := productionServe()
		c.NATSJSReplicas = n
		if err := c.ValidateServe(); err == nil || !strings.Contains(err.Error(), "NATS_JS_REPLICAS") ||
			!strings.Contains(err.Error(), "CORE_ALLOW_SINGLE_NODE") {
			t.Fatalf("%d replicas: %v", n, err)
		}
		c.AllowSingleNode = true
		if err := c.ValidateServe(); err != nil {
			t.Fatalf("%d replicas, single node allowed: %v", n, err)
		}
		if !c.SingleNode() {
			t.Fatalf("%d replicas allowed: no warning", n)
		}
	}
	for _, n := range []int{3, 5} {
		c := productionServe()
		c.NATSJSReplicas = n
		if err := c.ValidateServe(); err != nil || c.SingleNode() {
			t.Fatalf("%d replicas: %v %v", n, err, c.SingleNode())
		}
	}
	d := Config{Env: "development", DatabaseURL: "postgres://u:p@h/db", NATSURL: "nats://h:4222",
		AccessKeyPath: "/k.pem", RequestKeyPath: "/r.pem", NATSJSReplicas: 1}
	if err := d.ValidateServe(); err != nil || d.SingleNode() {
		t.Fatalf("development, 1 replica: %v %v", err, d.SingleNode())
	}
}

// CORE_ALLOW_SINGLE_NODE is true or false; anything else is refused.
func TestLoadAllowSingleNode(t *testing.T) {
	base := map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "BFF_SERVICE_KEYS": goodKey}
	for v, want := range map[string]bool{"": false, "true": true, "TRUE": true, "false": false} {
		e := map[string]string{"CORE_ALLOW_SINGLE_NODE": v}
		for k, x := range base {
			e[k] = x
		}
		setEnv(t, e)
		c, err := Load()
		if err != nil || c.AllowSingleNode != want {
			t.Fatalf("%q: %v %v", v, c.AllowSingleNode, err)
		}
	}
	e := map[string]string{"CORE_ALLOW_SINGLE_NODE": "yes-please"}
	for k, x := range base {
		e[k] = x
	}
	setEnv(t, e)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CORE_ALLOW_SINGLE_NODE") {
		t.Fatalf("bad value: %v", err)
	}
}
