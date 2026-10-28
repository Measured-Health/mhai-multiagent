package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
)

func readyz(t *testing.T, d Deps) *httptest.ResponseRecorder {
	t.Helper()
	m := authz.NewMux(nil)
	Register(m, d)
	rec := httptest.NewRecorder()
	// No BFF key: readyz is a probe, like healthz.
	m.Handler([]string{testKey}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil))
	return rec
}

// /readyz is 200 only when the readiness check passes; a failing or
// missing check is 503 not_ready, with no detail of what failed.
func TestReadyz(t *testing.T) {
	rec := readyz(t, Deps{Ready: func(context.Context) error { return nil }})
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"ready"}` || rec.Header().Get("X-Request-ID") == "" {
		t.Fatalf("ready: %d %q", rec.Code, rec.Body.String())
	}
	rec = readyz(t, Deps{Ready: func(context.Context) error { return errors.New("postgres: secret detail") }})
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), `"not_ready"`) || strings.Contains(rec.Body.String(), "secret detail") {
		t.Fatalf("not ready: %d %q", rec.Code, rec.Body.String())
	}
	if rec := readyz(t, Deps{}); rec.Code != 503 {
		t.Fatalf("no check: %d", rec.Code)
	}
}

// Readiness fails on a missing pool, connection or bucket, without
// touching the network.
func TestReadinessNeedsEverything(t *testing.T) {
	if err := Readiness(nil, nil, nil)(context.Background()); err == nil {
		t.Fatal("nil dependencies reported ready")
	}
}
