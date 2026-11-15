package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/config"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/kek"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// fakeCheck is a readiness check that records whether it had a deadline.
type fakeCheck struct {
	err      error
	deadline bool
}

func (f *fakeCheck) check(ctx context.Context) error {
	_, f.deadline = ctx.Deadline()
	return f.err
}

// Health reports each part honestly and claims nothing not yet built: no
// scanner, no sandbox, so the service never says it can take a document.
func TestHealthHandler(t *testing.T) {
	up, down := &fakeCheck{}, &fakeCheck{err: errors.New("down")}
	for _, tc := range []struct {
		r           readiness
		db, kr, str bool
	}{
		{readiness{db: up.check, keyring: up.check, storage: up.check}, true, true, true},
		{readiness{db: down.check, keyring: up.check, storage: down.check}, false, true, false},
		{readiness{}, false, false, false},
	} {
		rep, err := healthHandler(tc.r)(context.Background(), reqtoken.Claims{}, &docspb.HealthRequest{})
		h, ok := rep.(*docspb.HealthReply)
		if err != nil || !ok || h.GetDatabaseReady() != tc.db || h.GetKeyringReady() != tc.kr || h.GetStorageReady() != tc.str ||
			h.GetScannerReady() || h.GetSandboxReady() {
			t.Errorf("%v: %v %v", tc, rep, err)
		}
	}
	if !up.deadline || !down.deadline {
		t.Error("a readiness check has no deadline")
	}
}

// An upload needs both the keyring and the store.
func TestUploadReady(t *testing.T) {
	up, down := &fakeCheck{}, &fakeCheck{err: errors.New("down")}
	ctx := context.Background()
	if err := uploadReady(readiness{keyring: up.check, storage: up.check})(ctx); err != nil {
		t.Fatal(err)
	}
	for _, r := range []readiness{{keyring: down.check, storage: up.check}, {keyring: up.check, storage: down.check},
		{storage: up.check}, {keyring: up.check}} {
		if uploadReady(r)(ctx) == nil {
			t.Errorf("%v: ready", r)
		}
	}
}

func TestKeyringInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyring.json")
	var out bytes.Buffer
	if err := run([]string{"keyring", "init", path}, func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	if err := keyringInit(path, &out); err == nil || !strings.Contains(err.Error(), "never overwritten") {
		t.Fatalf("second init: %v", err)
	}
	if err := keyringInit(filepath.Join(t.TempDir(), "missing-dir", "k.json"), &out); err == nil ||
		strings.Contains(err.Error(), "missing-dir") {
		t.Fatalf("unwritable: %v", err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o400 {
		t.Fatalf("mode: %v", err)
	}
	if err := run([]string{"keyring", "init", "relative.json"}, func(string) string { return "" }); err != errUsage {
		t.Fatalf("relative path: %v", err)
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"serve", "x"}, {"Serve"}, {""}, {"keyring"}, {"keyring", "init"},
		{"keyring", "rotate", "/k.json"}} {
		if err := run(args, func(string) string { return "" }); err != errUsage {
			t.Errorf("%v: %v", args, err)
		}
	}
	if !strings.HasPrefix(usage, "usage: documents") {
		t.Fatalf("usage %q: CI's image check looks for \"usage: documents\"", usage)
	}
}

// serve fails closed before it touches the network: bad settings, then an
// unreadable trusted-keys file, each named by its variable only.
func TestServeRefusesBadSettings(t *testing.T) {
	if err := serve(func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "DOCUMENTS_NATS_URL") {
		t.Fatalf("empty environment: %v", err)
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "request-keys.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, keys := range []string{filepath.Join(dir, "missing.json"), bad} {
		env := map[string]string{
			"APP_ENV":                     "development",
			"DOCUMENTS_NATS_URL":          "nats://127.0.0.1:1",
			"DOCUMENTS_NATS_PASSWORD":     "syntheticDocumentsPassword0123456789",
			"DOCUMENTS_DATABASE_URL":      "postgres://documents_app@127.0.0.1:1/mhai?sslmode=disable",
			"DOCUMENTS_TRUSTED_KEYS_PATH": keys,
		}
		err := serve(func(k string) string { return env[k] })
		if err == nil || !strings.Contains(err.Error(), "DOCUMENTS_TRUSTED_KEYS_PATH") || strings.Contains(err.Error(), dir) {
			t.Fatalf("keys %s: %v", keys, err)
		}
	}
}

// With good settings and keys but no NATS, serve refuses to run (a
// service that can't hear core isn't up), and says so without the URL or
// the password.
func TestServeRefusesWithoutNATS(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "request-keys.json")
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reqtoken.WriteTrusted(keys, map[string]ed25519.PublicKey{reqtoken.KeyID(pub): pub}); err != nil {
		t.Fatal(err)
	}
	const pw = "syntheticDocumentsPassword0123456789"
	env := map[string]string{
		"APP_ENV":                     "development",
		"DOCUMENTS_NATS_URL":          "nats://127.0.0.1:1",
		"DOCUMENTS_NATS_PASSWORD":     pw,
		"DOCUMENTS_DATABASE_URL":      "postgres://documents_app@127.0.0.1:1/mhai?sslmode=disable",
		"DOCUMENTS_TRUSTED_KEYS_PATH": keys,
	}
	done := make(chan error, 1)
	go func() { done <- serve(func(k string) string { return env[k] }) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "NATS") || strings.Contains(err.Error(), pw) ||
			strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("%v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve didn't give up on NATS")
	}
}

// Every log line the service writes at start is structured JSON with no
// setting's value in it.
func TestStartLogHasNoValues(t *testing.T) {
	var buf bytes.Buffer
	logStart(newLogger(&buf), "development", 4, "file", true, false)
	out := buf.String()
	if !strings.Contains(out, `"msg":"documents serving"`) || !strings.Contains(out, `"subjects":"documents.v1.ctl.>"`) ||
		!strings.Contains(out, `"kek_provider":"file"`) || !strings.Contains(out, `"keyring_ready":true`) {
		t.Fatalf("%s", out)
	}
	for _, v := range []string{"password", "postgres://", "nats://", "/trusted"} {
		if strings.Contains(out, v) {
			t.Errorf("start log carries %s: %s", v, out)
		}
	}
}

// Startup refusals (design §6.2, departure 10): the file keyring is
// announced at every start and a bad keyring leaves the service not
// ready, not stopped; a bad store secret stops it; a too-privileged
// database login stops it, while a database that isn't there yet doesn't.
func TestStartupRefusals(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	log := newLogger(&buf)
	if openKeyring(ctx, config.Settings{}, log) != nil || !strings.Contains(buf.String(), "no key-encryption key provider") {
		t.Fatalf("no provider: %s", buf.String())
	}
	buf.Reset()
	missing := filepath.Join(t.TempDir(), "keyring.json")
	k := openKeyring(ctx, config.Settings{KEKProvider: "file", KeyringPath: missing}, log)
	if k == nil || k.Check(ctx) == nil || !strings.Contains(buf.String(), "pilot and development only") ||
		!strings.Contains(buf.String(), `"reason":"missing"`) || strings.Contains(buf.String(), missing) {
		t.Fatalf("missing keyring: %s", buf.String())
	}
	for err, code := range map[error]string{kek.ErrKeyringMode: "mode", kek.ErrKeyringPlaceholder: "placeholder",
		kek.ErrKeyringCheck: "check", kek.ErrKeyringInvalid: "invalid", errors.New("x"): "error"} {
		if keyringReason(err) != code {
			t.Errorf("%v: %s", err, keyringReason(err))
		}
	}

	if c, err := openStore(config.Settings{}); c != nil || err != nil {
		t.Fatalf("no store: %v %v", c, err)
	}
	dir := t.TempDir()
	st := &config.S3Settings{Endpoint: "http://127.0.0.1:1", Bucket: "mhai-documents-test", Region: "us-east-1",
		AccessKeyID: "TESTONLYDOCS", SecretKeyFile: filepath.Join(dir, "secret")}
	if _, err := openStore(config.Settings{S3: st}); err == nil || !strings.Contains(err.Error(), "DOCUMENTS_S3_SECRET_KEY_FILE") {
		t.Fatalf("no secret file: %v", err)
	}
	if err := os.WriteFile(st.SecretKeyFile, []byte("test-only-store-secret-0123456789"), 0o400); err != nil {
		t.Fatal(err)
	}
	if c, err := openStore(config.Settings{S3: st}); c == nil || err != nil {
		t.Fatalf("store: %v", err)
	}
	bad := *st
	bad.Bucket = "Bad"
	if _, err := openStore(config.Settings{S3: &bad}); err == nil {
		t.Fatal("a bad bucket opened")
	}

	if err := checkLogin(ctx, fakeQuerier{err: errors.New("synthetic: refused")}); err != nil {
		t.Fatalf("a database that isn't there yet: %v", err)
	}
	if err := checkLogin(ctx, fakeQuerier{super: true}); err == nil || !strings.Contains(err.Error(), "superuser") {
		t.Fatalf("superuser: %v", err)
	}
}

// fakeQuerier answers dblogin's one row: a superuser, or an error.
type fakeQuerier struct {
	super bool
	err   error
}

func (f fakeQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return f }

func (f fakeQuerier) Scan(dest ...any) error {
	if f.err != nil {
		return f.err
	}
	*dest[0].(*bool) = f.super
	*dest[len(dest)-1].(*bool) = true
	return nil
}
