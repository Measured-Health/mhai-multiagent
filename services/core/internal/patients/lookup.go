// Package patients serves a clinician's patient lookup by MRN (POST
// /patients/lookup; TODO.md's design note, approved 14 Nov 2026).
package patients

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// Limits are the lookup's rate limits.
type Limits struct {
	PerMinute, PerDay, HospitalPerDay int
}

// Deps are what the route needs.
type Deps struct {
	Pool   *pgxpool.Pool
	Log    *slog.Logger
	On     bool
	Limits Limits
	Now    func() time.Time
}

// Register adds the route.
func Register(m *authz.Mux, d Deps) {
	authz.Route(m, http.MethodPost, "/patients/lookup",
		authz.Rule{AnyRole: []string{authz.RoleClinician}, NeedsMFA: true, Write: true, NoIdempotency: true},
		httpx.NoStore(http.HandlerFunc(d.lookup)))
}

func (d Deps) lookup(w http.ResponseWriter, r *http.Request) { httpx.Error(w, r, "not_ready") }

// NormaliseMRN is not built yet.
func NormaliseMRN(string) (string, bool) { return "", false }
