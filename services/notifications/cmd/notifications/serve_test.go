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

	"github.com/Measured-Health/mhai-multiagent/services/gokit/notifypb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/config"
)

// devPassword is a synthetic NATS password, built at run time.
func devPassword() string { return "synthetic" + strings.Repeat("Notify", 4) + "0123" }

// Health reports the database honestly and names the enabled channels; a
// reply never shares the settings' slice.
func TestHealthHandler(t *testing.T) {
	var deadline bool
	up := func(ctx context.Context) error { _, deadline = ctx.Deadline(); return nil }
	down := func(context.Context) error { return errors.New("down") }
	chans := []string{"log"}
	for _, tc := range []struct {
		db       checker
		channels []string
		ready    bool
	}{{up, chans, true}, {down, nil, false}, {nil, nil, false}} {
		rep, err := healthHandler(tc.db, tc.channels)(context.Background(), reqtoken.Claims{}, &notifypb.HealthRequest{})
		h, ok := rep.(*notifypb.HealthReply)
		if err != nil || !ok || h.GetDatabaseReady() != tc.ready || len(h.GetChannels()) != len(tc.channels) {
			t.Errorf("%v: %v %v", tc.ready, rep, err)
		}
		if ok && len(h.Channels) > 0 {
			h.Channels[0] = "tampered"
		}
	}
	if chans[0] != "log" {
		t.Fatal("the reply shares the settings' channels")
	}
	if !deadline {
		t.Error("the database check has no deadline")
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"serve", "x"}, {"Serve"}, {""}} {
		if err := run(args, func(string) string { return "" }); err != errUsage {
			t.Errorf("%v: %v", args, err)
		}
	}
	if !strings.HasPrefix(usage, "usage: notifications") {
		t.Fatalf("usage %q: CI's image check looks for \"usage: notifications\"", usage)
	}
}

func devEnv(keys string) map[string]string {
	return map[string]string{
		"APP_ENV":                  "development",
		"NOTIFY_NATS_URL":          "nats://127.0.0.1:1",
		"NOTIFY_NATS_PASSWORD":     devPassword(),
		"NOTIFY_DATABASE_URL":      "postgres://notify_app@127.0.0.1:1/mhai?sslmode=disable&connect_timeout=1",
		"NOTIFY_TRUSTED_KEYS_PATH": keys,
	}
}

// serve fails closed before it touches the network: bad settings, then an
// unreadable trusted-keys file, each named by its variable only.
func TestServeRefusesBadSettings(t *testing.T) {
	if err := serve(func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "NOTIFY_NATS_URL") {
		t.Fatalf("empty environment: %v", err)
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "request-keys.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, keys := range []string{filepath.Join(dir, "missing.json"), bad} {
		env := devEnv(keys)
		err := serve(func(k string) string { return env[k] })
		if err == nil || !strings.Contains(err.Error(), "NOTIFY_TRUSTED_KEYS_PATH") || strings.Contains(err.Error(), dir) {
			t.Fatalf("keys %s: %v", keys, err)
		}
	}
}

func writeKeys(t *testing.T) string {
	t.Helper()
	keys := filepath.Join(t.TempDir(), "request-keys.json")
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reqtoken.WriteTrusted(keys, map[string]ed25519.PublicKey{reqtoken.KeyID(pub): pub}); err != nil {
		t.Fatal(err)
	}
	return keys
}

// With good settings and keys but no NATS, serve refuses to run (a
// service that can't hear core isn't up), and says so without the URL or
// the password.
func TestServeRefusesWithoutNATS(t *testing.T) {
	env := devEnv(writeKeys(t))
	done := make(chan error, 1)
	go func() { done <- serve(func(k string) string { return env[k] }) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "NATS") || strings.Contains(err.Error(), devPassword()) ||
			strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("%v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve didn't give up on NATS")
	}
}

// A missing NKey seed file is refused by its variable, never its path.
func TestNATSOptions(t *testing.T) {
	if opts, err := natsOptions(config.Settings{NATSPassword: devPassword()}); err != nil || len(opts) != 5 {
		t.Fatalf("password: %d %v", len(opts), err)
	}
	missing := filepath.Join(t.TempDir(), "notifications.nk")
	_, err := natsOptions(config.Settings{NATSNKeyFile: missing})
	if err == nil || !strings.Contains(err.Error(), "NOTIFY_NATS_NKEY_FILE") || strings.Contains(err.Error(), missing) {
		t.Fatalf("missing seed: %v", err)
	}
}

// fakeQ answers the login check with a privileged login, or not at all.
type fakeQ struct{ super, fail bool }

type fakeRow struct{ q fakeQ }

func (r fakeRow) Scan(dest ...any) error {
	if r.q.fail {
		return errors.New("synthetic: no answer")
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *bool:
			*p = (i == 0 && r.q.super) || i == len(dest)-1
		case *int64:
			*p = 0
		}
	}
	return nil
}

func (q fakeQ) QueryRow(context.Context, string, ...any) pgx.Row { return fakeRow{q} }

// A superuser login stops the service; one that doesn't answer yet doesn't
// (health says database_ready false); a fit login passes.
func TestCheckLogin(t *testing.T) {
	err := checkLogin(context.Background(), fakeQ{super: true})
	if err == nil || !strings.Contains(err.Error(), "NOTIFY_DATABASE_URL") || !strings.Contains(err.Error(), "superuser") {
		t.Fatalf("superuser: %v", err)
	}
	if err := checkLogin(context.Background(), fakeQ{fail: true}); err != nil {
		t.Fatalf("no answer: %v", err)
	}
	if err := checkLogin(context.Background(), fakeQ{}); err != nil {
		t.Fatalf("fit: %v", err)
	}
}

// The start log is structured JSON with no setting's value in it, and says
// when nothing is sent.
func TestStartLogHasNoValues(t *testing.T) {
	var buf bytes.Buffer
	logStart(newLogger(&buf), "development", 4, nil, false)
	out := buf.String()
	if !strings.Contains(out, `"msg":"notifications serving"`) || !strings.Contains(out, `"subjects":"notify.v1.ctl.>"`) ||
		!strings.Contains(out, `"sends":false`) || !strings.Contains(out, "nothing is sent") {
		t.Fatalf("%s", out)
	}
	for _, v := range []string{"password", "postgres://", "nats://", "/trusted"} {
		if strings.Contains(out, v) {
			t.Errorf("start log carries %s: %s", v, out)
		}
	}
}
