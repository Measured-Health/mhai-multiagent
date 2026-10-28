//go:build test

package server

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// FixturesBuilt reports whether the /_test/ fixture routes are compiled in.
const FixturesBuilt = true

// registerFixtures adds the routes that prove the authorization rules.
// They exist only under the test build tag.
func registerFixtures(m *authz.Mux, d Deps) {
	whoami := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := authz.FromContext(r.Context())
		if !ok {
			httpx.Error(w, r, "internal_error")
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"user_id": p.UserID.String(), "roles": p.Roles, "platform": p.Platform})
	})
	clinical := authz.Rule{AnyRole: []string{authz.RoleClinician}}
	platform := authz.Rule{Platform: true, AnyRole: []string{authz.RolePlatformAdmin, authz.RoleAuditor}}
	authz.Route(m, http.MethodGet, "/_test/clinical", clinical, whoami)
	authz.Route(m, http.MethodGet, "/_test/admin", authz.Rule{AnyRole: []string{authz.RoleHospitalAdmin}}, whoami)
	authz.Route(m, http.MethodGet, "/_test/platform", platform, whoami)
	platformWrite := platform
	platformWrite.Write = true
	authz.Route(m, http.MethodPost, "/_test/platform", platformWrite, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	// Reads a clinical query by id with no hospital filter of its own: row-
	// level security, set by authz.Tx, is what hides other hospitals' rows.
	authz.Route(m, http.MethodGet, "/_test/clinical-queries/{id}", clinical, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			httpx.Error(w, r, "not_found")
			return
		}
		var got uuid.UUID
		err = authz.Tx(r.Context(), d.Pool, func(tx pgx.Tx) error {
			return tx.QueryRow(r.Context(), `SELECT id FROM clinical_queries WHERE id = $1`, id).Scan(&got)
		})
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, r, "not_found")
			return
		}
		if err != nil {
			httpx.Error(w, r, "internal_error")
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"id": got.String()})
	}))
}
