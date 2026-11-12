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
// engine compute: a condition, location or source must be in the period's
// stored filter lists (422 before anything is computed or stored), and a
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

// listed reports whether f's condition, location and source are in the
// period's stored filter lists.
func listed(lists json.RawMessage, f disclose.Filters) (bool, error) {
	var l struct {
		Conditions []struct{ ID string } `json:"conditions"`
		Locations  []struct{ ID string } `json:"locations"`
		Sources    []struct{ ID string } `json:"sources"`
	}
	if err := json.Unmarshal(lists, &l); err != nil {
		return false, analysis.ErrFailed
	}
	in := func(v string, ids []struct{ ID string }) bool {
		if v == "" {
			return true
		}
		for _, x := range ids {
			if x.ID == v {
				return true
			}
		}
		return false
	}
	return in(f[disclose.DimCondition], l.Conditions) && in(f[disclose.DimLocation], l.Locations) &&
		in(f[disclose.DimSource], l.Sources), nil
}

// errListed: an id not in the period's lists (422 validation_failed).
var errListed = errQuery

// errCold: past the person's cold contexts for the period (429).
type coldError struct{}

func (coldError) Error() string { return "routes: cold contexts" }

// admit checks f against p's lists and reserves its context for the
// person: errListed, coldError, or an engine error.
func (h *handlers) admit(ctx context.Context, p analysis.Period, f disclose.Filters) error {
	if f[disclose.DimCondition] != "" || f[disclose.DimLocation] != "" || f[disclose.DimSource] != "" {
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
