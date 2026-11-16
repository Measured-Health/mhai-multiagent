package config

import (
	"os"
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

// storeEnv adds a complete object store and file keyring to m.
func storeEnv(m map[string]string) map[string]string {
	m["DOCUMENTS_KEK_PROVIDER"] = "file"
	m["DOCUMENTS_KEYRING_PATH"] = "/keyring/keyring.json"
	m["DOCUMENTS_S3_ENDPOINT"] = "https://store.example.test"
	m["DOCUMENTS_S3_BUCKET"] = "mhai-documents-dev"
	m["DOCUMENTS_S3_REGION"] = "eu-west-1"
	m["DOCUMENTS_S3_ACCESS_KEY_ID"] = "TESTONLYDOCS"
	m["DOCUMENTS_S3_SECRET_KEY_FILE"] = "/run/secrets/documents-s3"
	return m
}

// PR 3: the keyring and the object store are optional (the service then
// stays not ready); when named they are checked in full.
func TestStorageSettings(t *testing.T) {
	s, err := FromEnv(env(devEnv()))
	if err != nil || s.KEKProvider != "" || s.S3 != nil {
		t.Fatalf("unset: %+v %v", s, err)
	}
	m := storeEnv(devEnv())
	m["DOCUMENTS_S3_ENDPOINT"] = "http://minio:9000" // plain http in development only
	m["DOCUMENTS_S3_UPLOAD_ENDPOINT"] = "http://localhost:9000"
	s, err = FromEnv(env(m))
	if err != nil || s.KEKProvider != "file" || s.KeyringPath != "/keyring/keyring.json" || s.S3 == nil ||
		s.S3.Endpoint != "http://minio:9000" || s.S3.UploadEndpoint != "http://localhost:9000" || s.S3.Bucket != "mhai-documents-dev" ||
		s.S3.Region != "eu-west-1" || s.S3.AccessKeyID != "TESTONLYDOCS" || s.S3.SecretKeyFile != "/run/secrets/documents-s3" {
		t.Fatalf("dev store: %+v %v", s, err)
	}
	// Production takes the file keyring only with the pilot's written
	// acceptance (design §6.2, D5).
	m = storeEnv(prodEnv())
	m["DOCUMENTS_ALLOW_FILE_KEYRING"] = "pilot"
	if s, err := FromEnv(env(m)); err != nil || !s.FileKeyringAccepted() {
		t.Fatalf("production pilot: %+v %v", s, err)
	}
	m = storeEnv(prodEnv())
	m["APP_ENV"] = "staging"
	if _, err := FromEnv(env(m)); err != nil {
		t.Fatalf("staging file keyring: %v", err)
	}
}

func TestStorageRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		base func() map[string]string
		edit func(m map[string]string)
		want string
	}{
		{"file keyring in production", func() map[string]string { return storeEnv(prodEnv()) }, func(map[string]string) {},
			"DOCUMENTS_ALLOW_FILE_KEYRING"},
		{"production, other acceptance word", func() map[string]string { return storeEnv(prodEnv()) },
			func(m map[string]string) { m["DOCUMENTS_ALLOW_FILE_KEYRING"] = "yes" }, "DOCUMENTS_ALLOW_FILE_KEYRING"},
		{"unset APP_ENV is production", func() map[string]string { return storeEnv(prodEnv()) },
			func(m map[string]string) { delete(m, "APP_ENV") }, "DOCUMENTS_ALLOW_FILE_KEYRING"},
		{"kms not built", func() map[string]string { return storeEnv(devEnv()) },
			func(m map[string]string) { m["DOCUMENTS_KEK_PROVIDER"] = "kms" }, "DOCUMENTS_KEK_PROVIDER"},
		{"unknown provider", func() map[string]string { return storeEnv(devEnv()) },
			func(m map[string]string) { m["DOCUMENTS_KEK_PROVIDER"] = "vault" }, "DOCUMENTS_KEK_PROVIDER"},
		{"file without a path", func() map[string]string { return storeEnv(devEnv()) },
			func(m map[string]string) { delete(m, "DOCUMENTS_KEYRING_PATH") }, "DOCUMENTS_KEYRING_PATH"},
		{"relative keyring", func() map[string]string { return storeEnv(devEnv()) },
			func(m map[string]string) { m["DOCUMENTS_KEYRING_PATH"] = "keyring.json" }, "DOCUMENTS_KEYRING_PATH"},
		{"keyring path without provider", devEnv,
			func(m map[string]string) { m["DOCUMENTS_KEYRING_PATH"] = "/keyring/keyring.json" }, "DOCUMENTS_KEYRING_PATH"},
		{"store half set", devEnv,
			func(m map[string]string) { m["DOCUMENTS_S3_BUCKET"] = "mhai-documents-dev" }, "DOCUMENTS_S3_ENDPOINT"},
		{"store without the secret file", func() map[string]string { return storeEnv(devEnv()) },
			func(m map[string]string) { delete(m, "DOCUMENTS_S3_SECRET_KEY_FILE") }, "DOCUMENTS_S3_SECRET_KEY_FILE"},
		{"relative secret file", func() map[string]string { return storeEnv(devEnv()) },
			func(m map[string]string) { m["DOCUMENTS_S3_SECRET_KEY_FILE"] = "secret" }, "DOCUMENTS_S3_SECRET_KEY_FILE"},
		{"plain http outside development", func() map[string]string { return storeEnv(prodEnv()) },
			func(m map[string]string) {
				m["DOCUMENTS_ALLOW_FILE_KEYRING"] = "pilot"
				m["DOCUMENTS_S3_ENDPOINT"] = "http://store.example.test"
			}, "DOCUMENTS_S3_ENDPOINT"},
		{"plain http upload outside development", func() map[string]string { return storeEnv(prodEnv()) },
			func(m map[string]string) {
				m["DOCUMENTS_ALLOW_FILE_KEYRING"] = "pilot"
				m["DOCUMENTS_S3_UPLOAD_ENDPOINT"] = "http://uploads.example.test"
			}, "DOCUMENTS_S3_UPLOAD_ENDPOINT"},
		{"credentials in the endpoint", func() map[string]string { return storeEnv(devEnv()) },
			func(m map[string]string) {
				m["DOCUMENTS_S3_ENDPOINT"] = "https://x:y@store.example.test"
			},
			"DOCUMENTS_S3_ENDPOINT"},
		{"upload endpoint without a store", devEnv,
			func(m map[string]string) { m["DOCUMENTS_S3_UPLOAD_ENDPOINT"] = "https://uploads.example.test" }, "DOCUMENTS_S3_ENDPOINT"},
		{"bad bucket", func() map[string]string { return storeEnv(devEnv()) },
			func(m map[string]string) { m["DOCUMENTS_S3_BUCKET"] = "Bad_Bucket" }, "DOCUMENTS_S3_BUCKET"},
		{"bad region", func() map[string]string { return storeEnv(devEnv()) },
			func(m map[string]string) { m["DOCUMENTS_S3_REGION"] = "eu west" }, "DOCUMENTS_S3_REGION"},
		{"bad access key id", func() map[string]string { return storeEnv(devEnv()) },
			func(m map[string]string) { m["DOCUMENTS_S3_ACCESS_KEY_ID"] = "a:b" }, "DOCUMENTS_S3_ACCESS_KEY_ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.base()
			tc.edit(m)
			s, err := FromEnv(env(m))
			if err == nil {
				t.Fatal("accepted")
			}
			if s.S3 != nil || s.KEKProvider != "" {
				t.Errorf("returned settings with the error: %+v", s)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q doesn't name %s", err, tc.want)
			}
			for _, v := range []string{"x:y@", "Bad_Bucket", "TESTONLYDOCS", "/run/secrets", "keyring.json"} {
				if strings.Contains(err.Error(), v) {
					t.Errorf("error carries a value (%s): %q", v, err)
				}
			}
		})
	}
}

func TestReadSecretFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, v string) string {
		p := dir + "/" + name
		if err := os.WriteFile(p, []byte(v), 0o400); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good", "  test-only-store-secret-0123456789\n")
	if v, err := ReadSecretFile("DOCUMENTS_S3_SECRET_KEY_FILE", good); err != nil || v != "test-only-store-secret-0123456789" {
		t.Fatalf("good: %q %v", v, err)
	}
	link := dir + "/link"
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{
		"missing":     dir + "/none",
		"directory":   dir,
		"symlink":     link,
		"short":       write("short", "short-secret"),
		"placeholder": write("ph", "change-me-change-me-change-me"),
		"too large":   write("big", strings.Repeat("s", MaxSecretFileBytes+1)),
	} {
		v, err := ReadSecretFile("DOCUMENTS_S3_SECRET_KEY_FILE", p)
		if err == nil || v != "" || !strings.Contains(err.Error(), "DOCUMENTS_S3_SECRET_KEY_FILE") ||
			strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "change-me") {
			t.Errorf("%s: %q %v", name, v, err)
		}
	}
}

// The keyring mode (TODO.md "One keyring shared by several documents
// servers"): provisioned by default, create in development only, and
// anything else stops the service.
func TestKeyringMode(t *testing.T) {
	s, err := FromEnv(env(storeEnv(devEnv())))
	if err != nil || s.KeyringMode != KeyringProvisioned || s.KeyringCreates() {
		t.Fatalf("default: %q %v", s.KeyringMode, err)
	}
	m := storeEnv(devEnv())
	m["DOCUMENTS_KEYRING_MODE"] = "create"
	if s, err := FromEnv(env(m)); err != nil || s.KeyringMode != KeyringCreate || !s.KeyringCreates() {
		t.Fatalf("development create: %q %v", s.KeyringMode, err)
	}
	for _, e := range []string{"staging", "production"} {
		m := storeEnv(prodEnv())
		m["APP_ENV"] = e
		m["DOCUMENTS_ALLOW_FILE_KEYRING"] = "pilot"
		m["DOCUMENTS_KEYRING_MODE"] = "provisioned"
		if s, err := FromEnv(env(m)); err != nil || s.KeyringMode != KeyringProvisioned {
			t.Fatalf("%s provisioned: %q %v", e, s.KeyringMode, err)
		}
	}
	if s, err := FromEnv(env(devEnv())); err != nil || s.KeyringMode != "" {
		t.Fatalf("no provider: %q %v", s.KeyringMode, err)
	}
	withMode := func(base map[string]string, mode string) map[string]string {
		m := storeEnv(base)
		m["DOCUMENTS_KEYRING_MODE"] = mode
		return m
	}
	pilot := func(m map[string]string) map[string]string { m["DOCUMENTS_ALLOW_FILE_KEYRING"] = "pilot"; return m }
	unsetEnv := pilot(withMode(prodEnv(), "create"))
	delete(unsetEnv, "APP_ENV")
	staging := withMode(prodEnv(), "create")
	staging["APP_ENV"] = "staging"
	noProvider := devEnv()
	noProvider["DOCUMENTS_KEYRING_MODE"] = "provisioned"
	for name, m := range map[string]map[string]string{
		"create in production":           pilot(withMode(prodEnv(), "create")),
		"create with APP_ENV unset":      unsetEnv,
		"create in staging":              staging,
		"unknown mode":                   withMode(devEnv(), "shared"),
		"upper-case mode":                withMode(devEnv(), "CREATE"),
		"mode without the file provider": noProvider,
	} {
		s, err := FromEnv(env(m))
		if err == nil || !strings.Contains(err.Error(), "DOCUMENTS_KEYRING_MODE") || s.KeyringMode != "" {
			t.Errorf("%s: accepted or unnamed: %q %v", name, s.KeyringMode, err)
		}
		if err != nil && strings.Contains(err.Error(), "shared") {
			t.Errorf("%s: the error carries the value", name)
		}
	}
}

// PR 4: the scanner and the sandbox are optional (without them every scan
// waits); set, they must be well formed.
func TestScanningSettings(t *testing.T) {
	m := devEnv()
	m["DOCUMENTS_CLAMD_ADDR"] = "clamav:3310"
	m["DOCUMENTS_SANDBOX_SOCKET"] = "/jobs/sandbox.sock"
	m["DOCUMENTS_SANDBOX_JOBS"] = "/jobs"
	s, err := FromEnv(env(m))
	if err != nil || s.ClamdAddr != "clamav:3310" || s.SandboxSocket != "/jobs/sandbox.sock" || s.SandboxJobs != "/jobs" {
		t.Fatalf("%+v %v", s, err)
	}
	if s, err := FromEnv(env(devEnv())); err != nil || s.ClamdAddr != "" || s.SandboxSocket != "" {
		t.Fatalf("unset: %+v %v", s, err)
	}
	for name, kv := range map[string][2]string{
		"clamd without port": {"DOCUMENTS_CLAMD_ADDR", "clamav"},
		"clamd bad port":     {"DOCUMENTS_CLAMD_ADDR", "clamav:99999"},
		"clamd no host":      {"DOCUMENTS_CLAMD_ADDR", ":3310"},
		"socket only":        {"DOCUMENTS_SANDBOX_SOCKET", "/jobs/sandbox.sock"},
		"jobs only":          {"DOCUMENTS_SANDBOX_JOBS", "/jobs"},
	} {
		m := devEnv()
		m[kv[0]] = kv[1]
		if _, err := FromEnv(env(m)); err == nil || !strings.Contains(err.Error(), "DOCUMENTS_") {
			t.Errorf("%s: %v", name, err)
		}
	}
	m = devEnv()
	m["DOCUMENTS_SANDBOX_SOCKET"], m["DOCUMENTS_SANDBOX_JOBS"] = "jobs/sandbox.sock", "/jobs"
	if _, err := FromEnv(env(m)); err == nil {
		t.Fatal("a relative socket path")
	}
}
