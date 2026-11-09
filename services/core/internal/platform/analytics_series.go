package platform

// The analytics series (Plan 7c spec §7.2): a hospital's stored cell for
// each of the latest 12 weeks, 12 months or 8 quarters, with the median of
// the other hospitals' shown values; and an open change's series (Plan
// 7c-3). Read as analytics.go's routes are: stored rows only, the same read
// counter.

import (
	"errors"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// hospitalSeries is GET /platform/analytics/hospitals/{id}/series.
func (d Deps) hospitalSeries(w http.ResponseWriter, r *http.Request) {
	k, v, ok := analyticsKind(r, []string{"measure", "period"})
	if !ok || !slices.Contains(analytics.SeriesMeasures, v["measure"]) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !d.analyticsStart(w, r) {
		return
	}
	id := r.PathValue("id")
	if !canonicalUUID(id) {
		httpx.Error(w, r, "not_found")
		return
	}
	rows, _, err := d.storedSnapshots(r.Context(), k)
	if err != nil {
		d.analyticsFail(w, r, "series")
		return
	}
	out, err := analytics.HospitalSeries(k, rows, id, v["measure"])
	switch {
	case errors.Is(err, analytics.ErrNoHospital):
		httpx.Error(w, r, "not_found")
		return
	case err != nil:
		d.analyticsFail(w, r, "series")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

// changeSeries is GET /platform/analytics/changes/{id}/series (spec §7.2,
// F14): the open change's measure for its hospital, or the all cells and no
// median for a change across all hospitals. An unknown or closed change is
// 404 not_found (a closed change leaves the list).
func (d Deps) changeSeries(w http.ResponseWriter, r *http.Request) {
	k, _, ok := analyticsKind(r, []string{"period"})
	if !ok {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !d.analyticsStart(w, r) {
		return
	}
	id := r.PathValue("id")
	if !canonicalUUID(id) {
		httpx.Error(w, r, "not_found")
		return
	}
	var c analytics.ChangeRow
	found := false
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		c, err = scanChange(tx.QueryRow(r.Context(), changeSelect+` WHERE c.id = $1 AND c.closed_week IS NULL`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		d.analyticsFail(w, r, "change series")
		return
	}
	if !found {
		httpx.Error(w, r, "not_found")
		return
	}
	if _, err := analytics.ChangeOutOf(c, ""); err != nil {
		// A stored change that can't be sent is no change to show.
		httpx.Error(w, r, "not_found")
		return
	}
	rows, _, err := d.storedSnapshots(r.Context(), k)
	if err != nil {
		d.analyticsFail(w, r, "change series")
		return
	}
	out, err := analytics.ChangeSeries(k, rows, c.Hospital, c.Measure)
	if err != nil {
		d.analyticsFail(w, r, "change series")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}
