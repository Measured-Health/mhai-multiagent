// Package server assembles the core service's routes: every /api/v1 route
// is registered here, through authz.Route, so the permission table that
// cmd/permgen publishes is the one the service serves.
package server

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/alerts"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/letters"
	analysisroutes "github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/routes"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/emrctl"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/feedback"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intel"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/platform"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/queries"
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
	// SecurityContact is the integrity page's contact (configuration).
	SecurityContact platform.SecurityContact
	// AuditWalk runs a started chain check (nil: in the background with
	// the 10-minute budget); tests hold runs running with it.
	AuditWalk func(runID uuid.UUID)
	// Health is the health bucket (bus.OpenHealthBucket), which GET
	// /platform/health serves; nil answers 503 unavailable.
	Health jetstream.KeyValue
	// CloudProviders are the enabled cloud model providers
	// (CORE_CLOUD_PROVIDERS), for the model policy routes.
	CloudProviders []string
	// The EMR connection routes (EMR plan Task 11): off unless EMREnabled
	// (CORE_EMR_ENABLED); the offered types; the emr client and the
	// sealer; whether addresses may be IP literals (development).
	EMREnabled     bool
	EMRTypes       []string
	EMR            *emrctl.Client
	EMRSealer      *emrctl.Sealer
	EMRDevelopment bool
	// Analysis computes and serves Patient Analysis's stored bodies; nil
	// (or a hospital whose switch is off) answers every /analysis/ route
	// 404.
	Analysis *analysis.Engine
	// AnalysisKeys derive the drill keys; nil: no drill, every list 422.
	AnalysisKeys *analysis.Keys
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
	queries.Register(m, queries.Deps{Pool: d.Pool, Intel: d.Intel, Cap: d.QueryCap, Patients: d.EMREnabled})
	// The feedback routes log through the service's logger, whether or not
	// an intelligence client is set.
	feedback.Register(m, feedback.Deps{Pool: d.Pool, Intel: d.Intel, Log: a.Log})
	// The platform app's audit log reads (Plan 4 spec §5.2), system
	// health (§6.1), the failed jobs (§6.2), the guardrail statistics
	// (§6.3), the hospitals' cloud model policies (cloud model
	// providers plan, Task 2) and the EMR connections (EMR plan Task 11).
	platform.Register(m, platform.Deps{Pool: d.Pool, Log: a.Log, MasterKey: a.MFAKey,
		SecurityContact: d.SecurityContact, Walk: d.AuditWalk, Health: d.Health, Intel: d.Intel,
		Auth: a, QueryCap: d.QueryCap, CloudProviders: d.CloudProviders, EMREnabled: d.EMREnabled, EMRTypes: d.EMRTypes,
		EMR: d.EMR, EMRSealer: d.EMRSealer, EMRDevelopment: d.EMRDevelopment})
	// Patient Analysis's reads and the de-identified list (analysis
	// backend plan Tasks 10 and 13): off unless the hospital's analysis
	// switch is on.
	analysisroutes.Register(m, analysisroutes.Deps{Pool: d.Pool, Engine: d.Analysis, Log: a.Log, Keys: d.AnalysisKeys})
	// Clearance letters, hospital side (analysis backend plan Tasks 18-20):
	// off unless the hospital's analysis switch is on and its named-access
	// position is letters_only or on (none is, by default).
	letters.Register(m, letters.Deps{Pool: d.Pool, Auth: a, Log: a.Log})
	// EMR alerts (EMR plan Tasks 32-34): the hospital admin's alert
	// settings, possible duplicates and exclusions, and the clinicians'
	// alerts and notification settings. They always answer (empty lists,
	// not_connected) whether or not EMR is on; nothing raises an alert
	// until core's finding consumer runs (PR 8b).
	alerts.Register(m, alerts.Deps{Pool: d.Pool, Log: a.Log, Auth: a, MasterKey: a.MFAKey})
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
