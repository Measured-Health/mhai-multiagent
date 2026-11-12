package routes

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
)

// The security review's two controls on what a request may make the
// engine compute: every opaque filter (condition, location, specialty,
// source: any dimension that isn't a fixed code list) must be in the
// period's stored filter lists (422 before anything is computed or stored), and a
// person may have at most ColdContexts distinct contexts computed cold per
// period (reserved before the engine runs; 429 past it).

// reserveCold reserves the context hash of period for user
// (mhai_analysis_reserve_cold: the counter row locked, then checked).
// false is past the cap.
func reserveCold(ctx context.Context, pool *pgxpool.Pool, user, period uuid.UUID, hash []byte) (bool, error) {
	var ok bool
	err := authz.Tx(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mhai_analysis_reserve_cold($1, $2, $3, $4)`, user, period, hash, ColdContexts).Scan(&ok)
	})
	return ok, err
}

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

// admit checks f against p's lists and reserves its context for the
// person: errListed, coldError, or an engine error.
func (h *handlers) admit(ctx context.Context, p analysis.Period, f disclose.Filters) error {
	if opaque(f) {
		lists, _, err := h.d.Engine.Filters(ctx, p)
		if err != nil {
			return err
		}
		ok, err := listed(lists, f)
		if err != nil {
			return err
		}
		if !ok {
			return errListed
		}
	}
	pr, _ := authz.FromContext(ctx)
	ok, err := reserveCold(ctx, h.d.Pool, pr.UserID, p.ID, analysis.Hash(f))
	if err != nil {
		return analysis.ErrFailed
	}
	if !ok {
		return coldError{}
	}
	return nil
}
