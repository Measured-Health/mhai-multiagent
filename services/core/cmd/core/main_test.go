package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/server"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

func TestHealthz(t *testing.T) {
	m := authz.NewMux(nil)
	server.Register(m, server.Deps{})
	rec := httptest.NewRecorder()
	m.Handler(nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("body %q", rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("content type %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing X-Request-ID")
	}
}

// core keys new --out PATH writes a PKCS#8 Ed25519 key that serve loads,
// readable by the owner only; it never overwrites a key, and it prints the
// key id, never the key.
func TestKeysNew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.pem")
	var out bytes.Buffer
	if err := keys([]string{"new", "--out", path}, &out); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	iss, err := token.LoadIssuer(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), iss.KeyID) || strings.Contains(out.String(), "PRIVATE KEY") {
		t.Fatalf("output %q", out.String())
	}
	if err := keys([]string{"new", "--out", path}, &out); err == nil {
		t.Fatal("overwrote an existing key")
	}
	// ensure keeps an existing key (and checks it loads), or makes one.
	out.Reset()
	if err := keys([]string{"ensure", "--out", path}, &out); err != nil || !strings.Contains(out.String(), iss.KeyID) {
		t.Fatalf("ensure existing: %v %q", err, out.String())
	}
	fresh := filepath.Join(t.TempDir(), "fresh.pem")
	if err := keys([]string{"ensure", "--out", fresh}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := token.LoadIssuer(fresh); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(t.TempDir(), "junk.pem")
	_ = os.WriteFile(junk, []byte("not a key"), 0o600)
	if err := keys([]string{"ensure", "--out", junk}, &out); err == nil {
		t.Fatal("ensure accepted a file that is not a key")
	}
	for _, bad := range [][]string{nil, {"new"}, {"old", "--out", path}, {"new", "--out"}} {
		if err := keys(bad, &out); err == nil {
			t.Errorf("keys %v accepted", bad)
		}
	}
}

// Logins requested from the migrate step: both variables or neither, and
// a placeholder password is refused before anything is touched.
func TestLoginRequests(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	reqs, err := loginRequests(env(map[string]string{}))
	if err != nil || len(reqs) != 0 {
		t.Fatalf("none: %v %v", reqs, err)
	}
	reqs, err = loginRequests(env(map[string]string{
		"CORE_DB_USER": "core_app", "CORE_DB_PASSWORD": "test-only-core-db-password",
		"PROVISION_DB_USER": "core_provision", "PROVISION_DB_PASSWORD": "test-only-provision-db-password",
	}))
	if err != nil || len(reqs) != 2 || reqs[0].role != "mhai_app" || reqs[1].role != "mhai_provision" {
		t.Fatalf("both: %+v %v", reqs, err)
	}
	for _, bad := range []map[string]string{
		{"CORE_DB_USER": "core_app"},
		{"CORE_DB_PASSWORD": "test-only-core-db-password"},
		{"CORE_DB_USER": "core_app", "CORE_DB_PASSWORD": "changemeReplaceWithLettersAndDigits"},
		{"PROVISION_DB_USER": "core_provision", "PROVISION_DB_PASSWORD": "short"},
	} {
		_, err := loginRequests(env(bad))
		if err == nil {
			t.Errorf("%v accepted", bad)
			continue
		}
		for _, v := range bad {
			if strings.Contains(v, "change") && strings.Contains(err.Error(), v) {
				t.Errorf("error echoes the password: %v", err)
			}
		}
	}
}
