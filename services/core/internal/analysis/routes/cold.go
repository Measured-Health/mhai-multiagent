package routes

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
)

// The security review's two controls on what a request may make the
// engine compute: every opaque filter (condition, location, specialty,
// source: any dimension that isn't a fixed code list) must be in the
// period's stored filter lists (422 before anything is computed or
// stored), and every context a request would newly store (its family
// included) is reserved against the person's ColdContexts per period, and
// at most NewContextsMax per request, before the engine runs (429 past
// either).

// codeLists are the dimensions whose values are a fixed code list the
// contract closes (disclose.CheckFilters refuses anything else). Every
// other dimension is opaque, and is checked against the period's lists.
var codeLists = map[int][]string{disclose.DimAgeBand: disclose.AgeBands, disclose.DimSex: disclose.Sexes, disclose.DimSetting: disclose.Settings}

func fixedCodes(d int) bool { _, ok := codeLists[d]; return ok }

// listKeys name each opaque dimension's list in the stored filter lists.
var listKeys = map[int]string{disclose.DimCondition: "conditions", disclose.DimLocation: "locations",
	disclose.DimSpecialty: "specialties", disclose.DimSource: "sources"}

// listed reports whether every opaque filter of f is in the period's
// stored filter lists. A list entry is an object with an id, or a plain
// string (specialties). A dimension with no list is refused: fail closed
// for one added later.
func listed(lists json.RawMessage, f disclose.Filters) (bool, error) {
	var all map[string][]json.RawMessage
	if err := json.Unmarshal(lists, &all); err != nil {
		return false, analysis.ErrFailed
	}
	for d, v := range f {
		if v == "" || fixedCodes(d) {
			continue
		}
		entries, ok := all[listKeys[d]]
		if !ok {
			return false, nil
		}
		found := false
		for _, e := range entries {
			var obj struct{ ID string }
			var str string
			if json.Unmarshal(e, &str) == nil {
				found = found || str == v
			} else if json.Unmarshal(e, &obj) == nil {
				found = found || obj.ID == v
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

// opaque reports whether f sets any dimension that isn't a fixed code list.
func opaque(f disclose.Filters) bool {
	for d, v := range f {
		if v != "" && !fixedCodes(d) {
			return true
		}
	}
	return false
}

// errListed: an id not in the period's lists (422 validation_failed).
var errListed = errQuery

// errCold: past the person's cold contexts for the period (429).
type coldError struct{}

func (coldError) Error() string { return "routes: cold contexts" }

// admit checks f against p's lists and reserves, against the person's
// cold budget, every context the engine would newly store for it (at most
// left): errListed, coldError, or an engine error. It returns how many it
// reserved.
func (h *handlers) admit(ctx context.Context, p analysis.Period, f disclose.Filters, left int) (int, error) {
	if opaque(f) {
		lists, _, err := h.d.Engine.Filters(ctx, p)
		if err != nil {
			return 0, err
		}
		ok, err := listed(lists, f)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, errListed
		}
	}
	n, err := h.d.Engine.Reserve(ctx, p, f, ColdContexts, left)
	if errors.Is(err, analysis.ErrOverBudget) || errors.Is(err, analysis.ErrTooMany) {
		return 0, coldError{}
	}
	return n, err
}
