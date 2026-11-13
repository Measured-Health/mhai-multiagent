package platform

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// errNoList: the entry isn't a drill-down (404).
var errNoList = errors.New("platform: not a drill-down entry")

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
	case err != nil:
		d.fail(w, r, p, "patients")
	default:
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, http.StatusOK, struct {
			Pseudonyms []string `json:"pseudonyms"`
		}{pids})
	}
}
