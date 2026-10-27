//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/config"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

const (
	testBFFKey = "test-only-bff-service-key-0123456789abcdef"
	testMFAKey = "test-only-mfa-key-0123456789abcdef!"
	corePW     = "test-only-core-login-password"
	provPW     = "test-only-prov-login-password"
)

func withUser(raw, user, pw string) string {
	u, _ := url.Parse(raw)
	u.User = url.UserPassword(user, pw)
	return u.String()
}

type stack struct {
	pg       *testutil.PG
	nats     *testutil.NATS
	stopNATS func()
	cfg      config.Config
	provURL  string
}

// newStack migrates a fresh Postgres with the compose logins (as the
// core-migrate one-shot does), starts NATS and writes a signing key.
func newStack(t *testing.T) *stack {
	t.Helper()
	ctx := context.Background()
	pg := testutil.StartPostgres(t)
	env := map[string]string{"CORE_DB_USER": "core_app", "CORE_DB_PASSWORD": corePW,
		"PROVISION_DB_USER": "core_provision", "PROVISION_DB_PASSWORD": provPW}
	if err := migrateWithLogins(ctx, []string{"up"}, pg.URL, func(k string) string { return env[k] }, &bytes.Buffer{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	n, stop, err := testutil.NewNATS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	key := filepath.Join(t.TempDir(), "access.pem")
	if err := keys([]string{"new", "--out", key}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	return &stack{pg: pg, nats: n, stopNATS: stop, provURL: withUser(pg.URL, "core_provision", provPW), cfg: config.Config{
		DatabaseURL: withUser(pg.URL, "core_app", corePW), NATSURL: n.GatewayURL, AccessKeyPath: key,
		MFAKey: []byte(testMFAKey), BFFServiceKeys: []string{testBFFKey},
	}}
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func waitReadyz(t *testing.T, h http.Handler, want int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		rec := get(h, "/api/v1/readyz")
		if rec.Code == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("readyz %d, want %d (%s)", rec.Code, want, rec.Body.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// The whole service as serve builds it: the compose logins, readyz, a
// clinician seeded with the CLI on the provisioning login signs in with the
// BFF key, and readyz goes 503 when Postgres or NATS goes away.
func TestServeEndToEnd(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	log := logx.New(&bytes.Buffer{})
	a, err := newApp(ctx, s.cfg, log)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	waitReadyz(t, a.handler, 200)

	var out bytes.Buffer
	if err := seed(ctx, []string{"hospitals", "create", "--name", "Havana General", "--short-code", "HAV", "--time-zone", "Africa/Lagos"},
		s.provURL, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	pw := "tangerine-orbit-77-quill"
	if err := seed(ctx, []string{"users", "create", "--email", "synthetic.clinician@havana.example", "--name", "Synthetic Clinician",
		"--roles", "clinician", "--hospital", "HAV"}, s.provURL, strings.NewReader(pw+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"username": "synthetic.clinician@havana.example", "password": pw})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	r.Header.Set("X-MHAI-Service", testBFFKey)
	r.Header.Set("X-MHAI-Client-IP", "198.51.100.20")
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, r)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || got["access_token"] == nil || got["refresh_token"] == nil {
		t.Fatalf("login %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("login reply is cacheable")
	}

	// Postgres gone (the pool closed) is not ready.
	a.pool.Close()
	waitReadyz(t, a.handler, 503)
	a.close()

	// NATS gone is not ready either.
	b, err := newApp(ctx, s.cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	waitReadyz(t, b.handler, 200)
	s.stopNATS()
	waitReadyz(t, b.handler, 503)
}

// serve refuses the owner/superuser connection and the provisioning login.
func TestServeRefusesWrongLogins(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	for name, u := range map[string]string{"superuser": s.pg.URL, "provisioning": s.provURL} {
		cfg := s.cfg
		cfg.DatabaseURL = u
		a, err := newApp(ctx, cfg, logx.New(&bytes.Buffer{}))
		if err == nil {
			a.close()
			t.Fatalf("%s login accepted", name)
		}
		if !strings.Contains(err.Error(), "mhai_app") {
			t.Fatalf("%s: unclear refusal: %v", name, err)
		}
	}
}

// The sweeper deletes expired Idempotency-Key rows on its ticker, running
// as the core service's own login.
func TestSweeperRuns(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	super, err := db.Open(ctx, s.pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer super.Close()
	if _, err := super.Exec(ctx, `INSERT INTO idempotency_keys (scope, key, request_hash, expires_at)
		VALUES ('sweeper', 'old', 'h', now() - interval '1 hour'), ('sweeper', 'live', 'h', now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	a, err := newApp(ctx, s.cfg, logx.New(&bytes.Buffer{}))
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { runSweeper(sctx, a.pool, a.log, 50*time.Millisecond); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		_ = super.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE scope = 'sweeper'`).Scan(&n)
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sweeper left %d rows", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	var live int
	_ = super.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE scope = 'sweeper' AND key = 'live'`).Scan(&live)
	if live != 1 {
		t.Fatal("sweeper deleted a live row")
	}
}

// migrate up refuses a placeholder login password before touching the
// database.
func TestMigrateRefusesPlaceholderLoginPassword(t *testing.T) {
	pg := testutil.StartPostgres(t)
	env := map[string]string{"CORE_DB_USER": "core_app", "CORE_DB_PASSWORD": "changemeReplaceWithLettersAndDigits"}
	err := migrateWithLogins(context.Background(), []string{"up"}, pg.URL, func(k string) string { return env[k] }, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "CORE_DB_PASSWORD") || strings.Contains(err.Error(), "changeme") {
		t.Fatalf("want a refusal naming CORE_DB_PASSWORD, got %v", err)
	}
}
