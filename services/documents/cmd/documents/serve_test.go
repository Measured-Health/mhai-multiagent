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

	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

type fakeDB struct {
	err      error
	deadline bool
}

func (f *fakeDB) Ping(ctx context.Context) error {
	_, f.deadline = ctx.Deadline()
	return f.err
}

// Health reports the database honestly and claims nothing PR 1 hasn't
// built: no keyring, no scanner, no sandbox, so the service never says it
// can take a document.
func TestHealthHandler(t *testing.T) {
	up, down := &fakeDB{}, &fakeDB{err: errors.New("down")}
	for _, tc := range []struct {
		db    pinger
		ready bool
	}{{up, true}, {down, false}, {nil, false}} {
		rep, err := healthHandler(tc.db)(context.Background(), reqtoken.Claims{}, &docspb.HealthRequest{})
		h, ok := rep.(*docspb.HealthReply)
		if err != nil || !ok || h.GetDatabaseReady() != tc.ready || h.GetKeyringReady() || h.GetScannerReady() ||
			h.GetSandboxReady() {
			t.Errorf("%v: %v %v", tc.db, rep, err)
		}
	}
	if !up.deadline {
		t.Error("the database ping has no deadline")
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"serve", "x"}, {"Serve"}, {""}} {
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
	logStart(newLogger(&buf), "development", 4)
	out := buf.String()
	if !strings.Contains(out, `"msg":"documents serving"`) || !strings.Contains(out, `"subjects":"documents.v1.ctl.>"`) {
		t.Fatalf("%s", out)
	}
	for _, v := range []string{"password", "postgres://", "nats://", "/trusted"} {
		if strings.Contains(out, v) {
			t.Errorf("start log carries %s: %s", v, out)
		}
	}
}
