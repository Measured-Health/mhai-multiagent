// Package routes serves Patient Analysis's aggregate reads (design §9;
// plan Task 10): GET /analysis/filters and GET /analysis/{view}, for a
// hospital admin or an analyst with two-step, only while the hospital's
// analysis switch is on (otherwise 404, before anything else is read).
// Every number it sends is from a stored, guarded body (internal/analysis);
// it never imports the raw reader (internal/analysis/cells), which a test
// pins (I2). Query strings are never logged: the access log and spans name
// the registered pattern only.
package routes

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// Deps are what the routes need. A nil Pool registers the routes for the
// permission table only.
type Deps struct {
	Pool   *pgxpool.Pool
	Engine *analysis.Engine
	Log    *slog.Logger
	// Now is the rate limiter's clock (nil: time.Now).
	Now func() time.Time
}

// Rule is the routes' rule: hospital admins and analysts, two-step.
var Rule = authz.Rule{AnyRole: []string{authz.RoleHospitalAdmin, authz.RoleAnalyst}, NeedsMFA: true}

type handlers struct {
	d   Deps
	lim *limiter
}

// Register adds the aggregate routes to m.
func Register(m *authz.Mux, d Deps) {
	h := &handlers{d: d, lim: newLimiter(AggregateReads, AggregateWindow, d.Now)}
	authz.Route(m, http.MethodGet, "/analysis/filters", Rule, http.HandlerFunc(h.filters))
	authz.Route(m, http.MethodGet, "/analysis/{view}", Rule, http.HandlerFunc(h.views))
}

func (h *handlers) log() *slog.Logger {
	if h.d.Log != nil {
		return h.d.Log
	}
	return slog.New(slog.DiscardHandler)
}

// on reports whether the principal's hospital has analysis on; a failure
// to read it is "off" (fail closed: 404 tells nothing).
func (h *handlers) on(ctx context.Context) bool {
	if h.d.Pool == nil || h.d.Engine == nil {
		return false
	}
	p, ok := authz.FromContext(ctx)
	if !ok {
		return false
	}
	var pos string
	err := authz.Tx(ctx, h.d.Pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mhai_analysis_position($1)`, p.HospitalID).Scan(&pos)
	})
	return err == nil && pos == "on"
}

// gate is the checks every route makes first: switch on (else 404), then
// the session's aggregate budget (else 429).
func (h *handlers) gate(w http.ResponseWriter, r *http.Request) bool {
	if !h.on(r.Context()) {
		httpx.Error(w, r, "not_found")
		return false
	}
	p, _ := authz.FromContext(r.Context())
	if !h.lim.allow(sessionKey(p)) {
		httpx.ErrorRetryAfter(w, r, "rate_limited", int(AggregateWindow.Seconds()))
		return false
	}
	return true
}

func sessionKey(p authz.Principal) string {
	if p.SessionID != "" {
		return p.SessionID
	}
	return p.UserID.String()
}

var (
	sessionShape = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	idShape      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// errQuery is any query string that isn't a period, filters or both as the
// contract has them.
var errQuery = errors.New("routes: query")

// errRange: not exactly one period (422 invalid_range).
var errRange = errors.New("routes: range")

// parse reads a query: the period (period and start, or from and to that
// are exactly one period) and the filters. needPeriod: a view must name
// one; the filters route may name none (zero Period, ok false).
func parse(q url.Values, allowFilters, needPeriod bool) (calendar.Period, bool, disclose.Filters, error) {
	var f disclose.Filters
	known := map[string]bool{"period": true, "start": true, "from": true, "to": true}
	if allowFilters {
		for _, n := range disclose.DimNames {
			known[n] = true
		}
	}
	for k, vs := range q {
		if !known[k] || len(vs) != 1 || vs[0] == "" {
			return calendar.Period{}, false, f, errQuery
		}
	}
	if allowFilters {
		for d, n := range disclose.DimNames {
			f[d] = q.Get(n)
			// This hospital's ids are lowercase UUIDs (the dictionaries').
			if v := f[d]; v != "" && (d == disclose.DimCondition || d == disclose.DimLocation || d == disclose.DimSource) && !idShape.MatchString(v) {
				return calendar.Period{}, false, f, errQuery
			}
		}
	}
	level, start, from, to := q.Get("period"), q.Get("start"), q.Get("from"), q.Get("to")
	var p calendar.Period
	switch {
	case level != "" && start != "" && from == "" && to == "":
		var err error
		if p, err = disclose.PeriodOf(level, start); err != nil {
			return p, false, f, errRange
		}
	case level == "" && start == "" && from != "" && to != "":
		found := false
		for _, k := range []calendar.Kind{calendar.Week, calendar.Month, calendar.Quarter} {
			c, err := disclose.PeriodOf(string(k), from)
			if err == nil && c.To.AddDate(0, 0, -1).Format(time.DateOnly) == to {
				p, found = c, true
				break
			}
		}
		if !found {
			return p, false, f, errRange
		}
	case level == "" && start == "" && from == "" && to == "":
		if needPeriod {
			return p, false, f, errRange
		}
		return p, false, f, nil
	default:
		return p, false, f, errRange
	}
	return p, true, f, nil
}

// problem is the code for an engine or parse error.
func problem(err error) string {
	switch {
	case errors.Is(err, errRange), errors.Is(err, analysis.ErrNoPeriod):
		return "invalid_range"
	case errors.Is(err, disclose.ErrTooManyFilters):
		return "too_many_filters"
	case errors.Is(err, errQuery), errors.Is(err, disclose.ErrInvalid):
		return "validation_failed"
	case errors.As(err, new(coldError)):
		return "rate_limited"
	case errors.Is(err, analysis.ErrPending), errors.Is(err, analysis.ErrNotReady):
		return "not_ready"
	}
	return "unavailable"
}

// audited writes analysis.viewed once per session, filter set and period,
// and analysis.unusual_querying once a day past 100 distinct filter sets
// (D7). Codes only: the view, the period and the filters' codes and ids.
func (h *handlers) audited(ctx context.Context, view string, p analysis.Period, f disclose.Filters) error {
	pr, _ := authz.FromContext(ctx)
	sess := sessionKey(pr)
	if !sessionShape.MatchString(sess) {
		sess = pr.UserID.String()
	}
	rid := httpx.RequestIDFrom(ctx)
	return authz.Tx(ctx, h.d.Pool, func(tx pgx.Tx) error {
		var first, unusual bool
		if err := tx.QueryRow(ctx, `SELECT first, unusual FROM mhai_analysis_viewed($1, $2, $3, $4, $5, $6)`,
			pr.UserID, sess, p.ID, analysis.Hash(f), analysis.FiltersHash(f), h.d.Engine.ClockNow()).
			Scan(&first, &unusual); err != nil {
			return err
		}
		if first {
			filters := map[string]any{}
			for d, v := range f {
				if v != "" {
					filters[disclose.DimNames[d]] = v
				}
			}
			if err := audit.Write(ctx, tx, audit.Event{ActorID: pr.UserID.String(), HospitalID: pr.HospitalID.String(), RequestID: rid,
				Action: "analysis.viewed", Entity: "analysis_period", EntityID: p.ID.String(),
				Payload: map[string]any{"view": view, "period": string(p.Period.Kind) + ":" + p.Period.From.Format(time.DateOnly),
					"filters": filters}}); err != nil {
				return err
			}
		}
		if unusual {
			return audit.Write(ctx, tx, audit.Event{ActorID: pr.UserID.String(), HospitalID: pr.HospitalID.String(), RequestID: rid,
				Action: "analysis.unusual_querying", Entity: "user", EntityID: pr.UserID.String(),
				Payload: map[string]any{"filter_sets": disclose.UnusualFilterSets + 1}})
		}
		return nil
	})
}
