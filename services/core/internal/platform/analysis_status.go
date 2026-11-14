package platform

// The platform sidebar's Analysis status (mhai-web Plan 12, contract
// §14.12): GET /platform/analysis/status, platform admins only (the BFF
// keeps it from auditors too).
//
// Core serves no platform Analysis view yet (/platform/analysis/{view} and
// filters are not routes), so aggregates_enabled and patient_views_enabled
// are false whatever the switches say, and the app shows Analysis as off.
// sample is false (no sample data is served here) and my_clearances is
// empty (no clearances in core). The hospitals are split by their effective
// switch, the lower of the global row's and the hospital's (00021): on is
// included, anything else excluded as not_covered. Removed hospitals are
// left out. Not recorded: it carries switch positions and hospital names,
// no analysis figure.

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

func registerAnalysisStatus(m *authz.Mux, d Deps) {
	authz.Route(m, http.MethodGet, "/platform/analysis/status",
		authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin}},
		http.HandlerFunc(d.analysisStatus))
}

// Person is someone (or a hospital) as the platform app names them.
type Person struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// AnalysisExcluded is a hospital left out of Analysis, and why.
type AnalysisExcluded struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// AnalysisHospitals are the hospitals Analysis covers and those it doesn't.
type AnalysisHospitals struct {
	Included int                `json:"included"`
	Total    int                `json:"total"`
	List     []Person           `json:"list"`
	Excluded []AnalysisExcluded `json:"excluded"`
}

// AnalysisStatusOut is the status as mhai-web's readStatus reads it.
type AnalysisStatusOut struct {
	AggregatesEnabled   bool              `json:"aggregates_enabled"`
	PatientViewsEnabled bool              `json:"patient_views_enabled"`
	Sample              bool              `json:"sample"`
	Hospitals           AnalysisHospitals `json:"hospitals"`
	MyClearances        []struct{}        `json:"my_clearances"`
}

func (d Deps) analysisStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	out := AnalysisStatusOut{MyClearances: []struct{}{},
		Hospitals: AnalysisHospitals{List: []Person{}, Excluded: []AnalysisExcluded{}}}
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `SELECT h.id, h.name,
				EXISTS (SELECT 1 FROM analysis_switch g WHERE g.hospital_id IS NULL AND g.position = 'on')
				AND EXISTS (SELECT 1 FROM analysis_switch s WHERE s.hospital_id = h.id AND s.position = 'on')
			FROM hospitals h WHERE h.status <> 'removed' ORDER BY h.name, h.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var name string
			var on bool
			if err := rows.Scan(&id, &name, &on); err != nil {
				return err
			}
			if on {
				out.Hospitals.List = append(out.Hospitals.List, Person{ID: id.String(), Name: name})
			} else {
				out.Hospitals.Excluded = append(out.Hospitals.Excluded, AnalysisExcluded{ID: id.String(), Name: name, Reason: "not_covered"})
			}
		}
		return rows.Err()
	})
	if err != nil {
		d.fail(w, r, p, "analysis status")
		return
	}
	out.Hospitals.Included = len(out.Hospitals.List)
	out.Hospitals.Total = out.Hospitals.Included + len(out.Hospitals.Excluded)
	httpx.JSON(w, http.StatusOK, out)
}
