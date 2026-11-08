package platform

// The analytics series (Plan 7c spec §7.2): a hospital's stored cell for
// each of the latest 12 weeks, 12 months or 8 quarters, with the median of
// the other hospitals' shown values; and a change's series, which waits for
// What changed (Plan 7c-3). Read as analytics.go's routes are: stored
// snapshots only, the same read counter.

import (
	"errors"
	"net/http"
	"slices"

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

// changeSeries is GET /platform/analytics/changes/{id}/series. What
// changed comes with Plan 7c-3: until then there is no change, so every id
// is 404 not_found (after the query check and the read count).
func (d Deps) changeSeries(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := analyticsKind(r, []string{"period"}); !ok {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !d.analyticsStart(w, r) {
		return
	}
	httpx.Error(w, r, "not_found")
}
