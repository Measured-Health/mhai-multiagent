package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// ReadyTimeout bounds one readiness check.
const ReadyTimeout = 2 * time.Second

var errNotReady = errors.New("server: not ready")

// Readiness checks the core service's dependencies: Postgres answers a
// ping, the NATS connection is up, and every KV bucket answers a status
// call. Any failure is reported as one fixed error (the probe never says
// which dependency, or why).
func Readiness(pool *pgxpool.Pool, nc *nats.Conn, buckets map[string]jetstream.KeyValue) func(context.Context) error {
	return func(ctx context.Context) error {
		if pool == nil || nc == nil || len(buckets) == 0 {
			return errNotReady
		}
		if err := pool.Ping(ctx); err != nil {
			return errNotReady
		}
		if !nc.IsConnected() {
			return errNotReady
		}
		for _, kv := range buckets {
			if kv == nil {
				return errNotReady
			}
			if _, err := kv.Status(ctx); err != nil {
				return errNotReady
			}
		}
		return nil
	}
}

// registerReady adds GET /api/v1/readyz, a probe like healthz: 200 when
// ready, else 503 not_ready. A nil check is never ready.
func registerReady(m *authz.Mux, ready func(context.Context) error) {
	authz.Route(m, http.MethodGet, "/readyz", authz.Rule{Public: true, Probe: true},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ready == nil {
				httpx.Error(w, r, "not_ready")
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), ReadyTimeout)
			defer cancel()
			if err := ready(ctx); err != nil {
				httpx.Error(w, r, "not_ready")
				return
			}
			httpx.JSON(w, http.StatusOK, map[string]string{"status": "ready"})
		}))
}
