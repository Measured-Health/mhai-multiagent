//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
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
	reqKey := filepath.Join(t.TempDir(), "request.pem")
	if err := keys([]string{"new", "--kind", "request", "--out", reqKey,
		"--trusted-out", filepath.Join(t.TempDir(), "request-keys.json")}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	return &stack{pg: pg, nats: n, stopNATS: stop, provURL: withUser(pg.URL, "core_provision", provPW), cfg: config.Config{Env: "development",
		DatabaseURL: withUser(pg.URL, "core_app", corePW), NATSURL: n.GatewayURL, AccessKeyPath: key, RequestKeyPath: reqKey,
		MFAKey: []byte(testMFAKey), BFFServiceKeys: []string{testBFFKey}, HTTPAPI: true,
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

	// serve made the JOBS and DEAD streams and their consumers.
	js, err := jetstream.New(a.nc)
	if err != nil {
		t.Fatal(err)
	}
	for stream, consumer := range map[string]string{bus.JobsStream: bus.PipelineConsumer, bus.DeadStream: bus.CoreDeadConsumer} {
		if _, err := js.Consumer(ctx, stream, consumer); err != nil {
			t.Fatalf("serve did not create %s/%s: %v", stream, consumer, err)
		}
	}

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

// serve refuses to start when the request key is the access key copied to
// another file.
func TestServeRefusesSharedSigningKey(t *testing.T) {
	s := newStack(t)
	raw, err := os.ReadFile(s.cfg.AccessKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.cfg
	cfg.RequestKeyPath = filepath.Join(t.TempDir(), "copy.pem")
	if err := os.WriteFile(cfg.RequestKeyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := newApp(context.Background(), cfg, logx.New(&bytes.Buffer{}))
	if err == nil {
		a.close()
		t.Fatal("started with one key for both token kinds")
	}
	if !strings.Contains(err.Error(), "REQUEST_KEY_PATH") {
		t.Fatalf("unclear refusal: %v", err)
	}
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

// CORE_HTTP_API=off (spec §17.1): the HTTP listener serves the probes and
// nothing else, with or without the BFF key, and the API is on NATS: a
// clinician signs in as the clinical BFF on api.v1.clinical.
func TestHTTPAPIOffServesProbesOnly(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	cfg := s.cfg
	cfg.HTTPAPI = false
	cfg.HTTPAddr = "127.0.0.1:8080"
	cfg.BFFServiceKeys = nil
	a, err := newApp(ctx, cfg, logx.New(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer a.close()
	waitReadyz(t, a.handler, 200)
	if rec := get(a.handler, "/api/v1/healthz"); rec.Code != 200 {
		t.Fatalf("healthz %d", rec.Code)
	}
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
	for _, p := range []string{"/api/v1/auth/login", "/api/docs", "/api/openapi.yaml"} {
		r := httptest.NewRequest(http.MethodPost, p, bytes.NewReader(body))
		if p != "/api/v1/auth/login" {
			r = httptest.NewRequest(http.MethodGet, p, nil)
		}
		r.Header.Set("X-MHAI-Service", testBFFKey)
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		a.handler.ServeHTTP(rec, r)
		if rec.Code != 404 {
			t.Fatalf("%s over HTTP with the API off: %d %s", p, rec.Code, rec.Body.String())
		}
	}

	bff, err := nats.Connect(s.nats.BFFClinicalURL, nats.CustomInboxPrefix(bus.BFFInbox("clinical")))
	if err != nil {
		t.Fatal(err)
	}
	defer bff.Close()
	m := nats.NewMsg("api.v1.clinical")
	m.Header.Set("Mhai-Method", "POST")
	m.Header.Set("Mhai-Path", "/api/v1/auth/login")
	m.Header.Set("Content-Type", "application/json")
	m.Header.Set("X-MHAI-Client-IP", "198.51.100.21")
	m.Data = body
	reply, err := bff.RequestMsg(m, 10*time.Second)
	if err != nil {
		t.Fatalf("sign-in over NATS: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(reply.Data, &got)
	if reply.Header.Get("Mhai-Status-Code") != "200" || got["access_token"] == nil || reply.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("sign-in over NATS: %s %s", reply.Header.Get("Mhai-Status-Code"), reply.Data)
	}
}
