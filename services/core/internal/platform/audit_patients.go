package platform

import (
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// errNoList: the entry isn't a drill-down (404).
var errNoList = errors.New("platform: not a drill-down entry")

// errNotReader: the account no longer holds a reading role, or is no
// longer active (403).
var errNotReader = errors.New("platform: no longer an admin or auditor")

// auditPatients is GET /platform/audit/{id}/patients (design §8.8,
// contract §14.9): the pids a drill-down entry's list sent, in order, for
// that list's period. The only route outside Analysis that sends a pid.
// Every reveal is recorded (audit.patients_viewed, the entry revealed as
// its entity, how many pids), in the same transaction: no pid is sent
// unrecorded, and none is kept anywhere else or logged. An entry that
// isn't a drill-down, or no such entry, is 404.
func (d Deps) auditPatients(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	seq, ok := audit.EventID(r.PathValue("id"))
	if !ok || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	var pids []string
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		// What the guard may have cached for up to 30 seconds, re-read
		// before any pid is read: the account active, a platform account,
		// and still a platform admin or auditor.
		var usable bool
		var roles []string
		err := tx.QueryRow(r.Context(), `SELECT u.is_active AND u.hospital_id IS NULL,
			ARRAY(SELECT x.role FROM user_roles x WHERE x.user_id = u.id) FROM users u WHERE u.id = $1`, p.UserID).Scan(&usable, &roles)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotReader
		}
		if err != nil {
			return err
		}
		held := false
		for _, want := range []string{authz.RolePlatformAdmin, authz.RoleAuditor} {
			held = held || (slices.Contains(roles, want) && slices.Contains(p.Roles, want))
		}
		if !usable || !held {
			return errNotReader
		}
		if err := tx.QueryRow(r.Context(), `SELECT mhai_audit_view_pseudonyms($1)`, seq).Scan(&pids); err != nil {
			return err
		}
		if len(pids) == 0 {
			return errNoList
		}
		return audit.Write(r.Context(), tx, audit.Event{ActorID: p.UserID.String(), RequestID: httpx.RequestIDFrom(r.Context()),
			Action: "audit.patients_viewed", Entity: "audit_log", EntityID: strconv.FormatInt(seq, 10),
			Payload: map[string]any{"rows": len(pids)}})
	})
	switch {
	case errors.Is(err, errNoList):
		httpx.Error(w, r, "not_found")
	case errors.Is(err, errNotReader):
		httpx.Error(w, r, "forbidden")
	case err != nil:
		d.fail(w, r, p, "patients")
	default:
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, http.StatusOK, struct {
			Pseudonyms []string `json:"pseudonyms"`
		}{pids})
	}
}
