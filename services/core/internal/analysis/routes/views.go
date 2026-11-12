package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

func knownView(v string) bool {
	for _, x := range disclose.Views {
		if string(x) == v {
			return true
		}
	}
	return false
}

// views is GET /analysis/{view}: the stored body of one frozen period and
// filter set, with its history (up to 7 earlier periods of the level, each
// copied from its own stored body for the same filters).
func (h *handlers) views(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	view := r.PathValue("view")
	if !knownView(view) {
		httpx.Error(w, r, "not_found")
		return
	}
	per, _, f, err := parse(r.URL.Query(), true, true)
	if err == nil {
		err = disclose.CheckFilters(f)
	}
	if err != nil {
		httpx.Error(w, r, problem(err))
		return
	}
	// One budget for the body and its history.
	ctx, cancel := context.WithTimeout(r.Context(), h.d.Engine.ColdBudget())
	defer cancel()
	p, err := h.d.Engine.Lookup(ctx, per)
	if err != nil {
		httpx.Error(w, r, problem(err))
		return
	}
	s, err := h.d.Engine.Body(ctx, p, f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	hist, err := h.history(ctx, per, f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := analysis.Served(s.Views[view], s, hist)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.audited(r.Context(), view, p, f); err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// history is the points of the earlier periods of per's level that are
// answerable, newest last, at most disclose.HistoryPoints; each from its
// stored body (computed first if it isn't stored, within the budget).
func (h *handlers) history(ctx context.Context, per calendar.Period, f disclose.Filters) ([]disclose.HistoryPoint, error) {
	var bodies []disclose.Body
	prev := per
	for i := 0; i < disclose.HistoryPoints; i++ {
		prev = calendar.Prev(prev)
		p, err := h.d.Engine.Lookup(ctx, prev)
		if err != nil {
			if err == analysis.ErrNoPeriod {
				break // older than 24 months: nothing earlier is answerable
			}
			return nil, err
		}
		s, err := h.d.Engine.Body(ctx, p, f)
		if err != nil {
			return nil, err
		}
		bodies = append(bodies, s.Record)
	}
	return disclose.History(bodies), nil
}

// fail answers an engine failure with its fixed code; a failure that isn't
// the client's is logged with ids and a fixed reason only.
func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	code := problem(err)
	if code == "unavailable" {
		p, _ := authz.FromContext(r.Context())
		h.log().Error("analysis: read failed", "request_id", httpx.RequestIDFrom(r.Context()),
			"hospital_id", p.HospitalID.String(), "reason", "unavailable")
	}
	httpx.Error(w, r, code)
}

// filters is GET /analysis/filters: the answerable periods, and the lists
// of one period (the latest frozen month when none is named; departure 15).
func (h *handlers) filters(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	per, named, _, err := parse(r.URL.Query(), false, false)
	if err != nil {
		httpx.Error(w, r, problem(err))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.d.Engine.ColdBudget())
	defer cancel()
	all, err := h.d.Engine.Periods(ctx)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	periods := map[string][]string{"week": {}, "month": {}, "quarter": {}}
	var target *analysis.Period
	for i, p := range all {
		periods[string(p.Period.Kind)] = append(periods[string(p.Period.Kind)], p.Period.From.Format(time.DateOnly))
		if !named && p.Period.Kind == calendar.Month {
			target = &all[i] // ordered by start: the last is the latest
		}
	}
	if named {
		p, err := h.d.Engine.Lookup(ctx, per)
		if err != nil {
			httpx.Error(w, r, problem(err))
			return
		}
		target = &p
	}
	out := map[string]any{"periods": periods, "updated_at": nil, "conditions": []any{}, "locations": []any{},
		"specialties": []any{}, "sources": []any{}}
	if target != nil {
		lists, at, err := h.d.Engine.Filters(ctx, *target)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		var l map[string]json.RawMessage
		if err := json.Unmarshal(lists, &l); err != nil {
			h.fail(w, r, analysis.ErrFailed)
			return
		}
		for _, k := range []string{"conditions", "locations", "specialties", "sources"} {
			if v, ok := l[k]; ok {
				out[k] = v
			}
		}
		out["updated_at"] = at.UTC().Format("2006-01-02T15:04:05Z")
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}
