package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
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

// A NATS disconnect flips readiness to not-ready, a reconnect flips it
// back, both through the handlers Connect is given (closes TODO T3 M6).
func TestReconnectFlipsReadiness(t *testing.T) {
	var logs strings.Builder
	link := bus.NewLink(slog.New(slog.NewTextHandler(&logs, nil)))
	var o nats.Options
	for _, opt := range link.Options() {
		if err := opt(&o); err != nil {
			t.Fatal(err)
		}
	}
	if o.DisconnectedErrCB == nil || o.ReconnectedCB == nil || o.ClosedCB == nil {
		t.Fatal("link did not set all three handlers")
	}
	ready := WithLink(func(context.Context) error { return nil }, link)
	if err := ready(context.Background()); err != nil {
		t.Fatalf("up: %v", err)
	}
	o.DisconnectedErrCB(nil, errors.New("read tcp 10.0.0.9:4222: secret detail"))
	if err := ready(context.Background()); err == nil {
		t.Fatal("ready while disconnected")
	}
	o.ReconnectedCB(nil)
	if err := ready(context.Background()); err != nil {
		t.Fatalf("after reconnect: %v", err)
	}
	if link.Reconnects() != 1 {
		t.Fatalf("reconnects: %d", link.Reconnects())
	}
	o.ClosedCB(nil)
	if ready(context.Background()) == nil {
		t.Fatal("ready after the connection closed")
	}
	if strings.Contains(logs.String(), "secret detail") || !strings.Contains(logs.String(), "disconnected") {
		t.Fatalf("log: %q", logs.String())
	}
	if WithLink(nil, nil) != nil {
		t.Fatal("nil link must return the check unchanged")
	}
}
