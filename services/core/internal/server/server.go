// Package server assembles the core service's routes: every /api/v1 route
// is registered here, through authz.Route, so the permission table that
// cmd/permgen publishes is the one the service serves.
package server

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intel"
)

// Deps are what the routes need. Zero Deps build the table only.
type Deps struct {
	Auth *auth.Service
	// Pool runs request transactions through authz.Tx.
	Pool *pgxpool.Pool
	// Ready is the /readyz check (Readiness); nil answers 503.
	Ready func(context.Context) error
	// Intel calls the intelligence workers (request/reply and jobs).
	Intel *intel.Client
	// QueryCap is the questions in flight per hospital
	// (INTELLIGENCE_QUERY_CAP), for intel.ReserveInFlight.
	QueryCap int
}

// Register adds every route to m.
func Register(m *authz.Mux, d Deps) {
	RegisterHealth(m)
	registerReady(m, d.Ready)
	a := d.Auth
	if a == nil {
		a = &auth.Service{}
	}
	a.Register(m)
	registerFixtures(m, d)
}

// RegisterHealth adds GET /api/v1/healthz, served before the BFF check so
// load balancers can probe it.
func RegisterHealth(m *authz.Mux) {
	authz.Route(m, http.MethodGet, "/healthz", authz.Rule{Public: true, Probe: true},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
		}))
}

// Table is the permission table of every route the service serves.
func Table() []authz.RouteRule {
	m := authz.NewMux(nil)
	Register(m, Deps{})
	return m.Table()
}
