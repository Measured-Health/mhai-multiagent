package platform

// Guardrail statistics (Plan 4 spec §6.3; Plan 7c spec §9, rulings R41a,
// R41a-2, R41a-3 and R41a-4).
//
// GET /platform/guardrails/periods?period=week|month|quarter (new, 7c-1):
// the HGA decision counts of the latest published period of that kind
// (month-aligned weeks, months, quarters, UTC), per hospital and across all
// of them, rendered from the QG family of the one stored snapshot
// (analytics_snapshots) that Answer quality reads, so the two show the
// same cells with the same hidden pattern. The route reads that row and
// nothing else: no request computes a figure or touches a clinical table. A
// period not published yet, or stored unavailable, is 503 not_ready.
//
// GET /platform/guardrails?period=7d|30d|90d (Plan 4c) is deprecated (ADR
// 0012; R41a-4 keeps its schema and enum exactly as they were, so the
// calendar periods live on the route above): from the 7c-1 release every
// response carries Deprecation, Sunset and Link headers, the rolling
// windows are served in the same shape with every count and share hidden,
// all included (R41a-3), and after the sunset the values are 422
// validation_failed. Nothing is read from or written to guardrail_snapshots
// any more.
//
// Platform admins and auditors, two-step always; 6 reads a minute per
// session across both routes (429 rate_limited, C31). Not recorded in the
// audit log (aggregates about no person).

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

const (
	// GuardrailsPerMinute is the reads one session may make a minute (C31).
	GuardrailsPerMinute = 6
	// guardrailsScope is the idempotency_keys scope of the read rate
	// counter; no Idempotency-Key scope (a user id) takes this form.
	guardrailsScope = "platform.guardrails.rate"
	// guardrailsTimeout bounds one read.
	guardrailsTimeout = 10 * time.Second
)

// GuardrailsGrace is how long after a period ends its figures wait (R43):
// the calendar's grace, one constant.
const GuardrailsGrace = calendar.Grace

// The deprecated rolling windows (R41a-3), in one place: the release that
// hid them, and the sunset 90 days on (ADR 0012).
//
// RELEASE: RollingDeprecatedAt is a placeholder; the 7c-1 release sets it to
// its own date (the sunset follows).
var (
	RollingDeprecatedAt = time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	RollingSunset       = RollingDeprecatedAt.AddDate(0, 0, 90)
)

// RollingNotice is the deprecation notice the Link header names.
const RollingNotice = "/docs/notices/guardrails-rolling-windows"

// rollingPeriods are the deprecated values and their length in days.
var rollingPeriods = map[string]int{"7d": 7, "30d": 30, "90d": 90}

var rollingDeprecated = httpx.Deprecated(RollingDeprecatedAt, RollingSunset, RollingNotice)

// Rates is one row's guarded figures (spec §6.3): answers = pass +
// regenerate + escalate, degraded on its own, and the three shares of
// answers.
type Rates struct {
	Answers         smallgroup.Guarded      `json:"answers"`
	Pass            smallgroup.Guarded      `json:"pass"`
	Regenerate      smallgroup.Guarded      `json:"regenerate"`
	Escalate        smallgroup.Guarded      `json:"escalate"`
	Degraded        smallgroup.Guarded      `json:"degraded"`
	PassShare       smallgroup.GuardedShare `json:"pass_share"`
	RegenerateShare smallgroup.GuardedShare `json:"regenerate_share"`
	EscalateShare   smallgroup.GuardedShare `json:"escalate_share"`
}

// GuardrailHospital is one hospital's row: {hospital, ...Rates}.
type GuardrailHospital struct {
	Hospital Ref `json:"hospital"`
	Rates
}

// GuardrailsOut is the response.
type GuardrailsOut struct {
	Period    string              `json:"period"`
	From      string              `json:"from"`
	To        string              `json:"to"`
	UpdatedAt string              `json:"updated_at"`
	All       Rates               `json:"all"`
	Hospitals []GuardrailHospital `json:"hospitals"`
}

// guardClock replaces the route's clock in tests (nil: Deps.now).
var guardClock func() time.Time

// guardWindow is a rolling period's window (R42, R43): whole UTC days,
// ending at the start of the UTC day GuardrailsGrace before now.
func guardWindow(now time.Time, days int) (from, to time.Time) {
	to = now.UTC().Add(-GuardrailsGrace).Truncate(24 * time.Hour)
	return to.AddDate(0, 0, -days), to
}

var errSnapshot = errors.New("platform: guardrail snapshot not usable")

// errNotPublished is a calendar period with no usable stored snapshot yet.
var errNotPublished = errors.New("platform: no published period")

func registerGuardrails(m *authz.Mux, d Deps) {
	read := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin, authz.RoleAuditor}}
	authz.Route(m, http.MethodGet, "/platform/guardrails", read, rollingDeprecated(http.HandlerFunc(d.guardrails)))
	authz.Route(m, http.MethodGet, "/platform/guardrails/periods", read, http.HandlerFunc(d.guardrailPeriods))
}

// guardrails is GET /platform/guardrails?period=7d|30d|90d (deprecated) →
// GuardrailsOut, every figure hidden.
func (d Deps) guardrails(w http.ResponseWriter, r *http.Request) {
	v, err := params(r, []string{"period"})
	if _, rolling := rollingPeriods[v["period"]]; err != nil || !rolling {
		httpx.Error(w, r, "validation_failed")
		return
	}
	d.guardrailsRead(w, r, v["period"])
}

// guardrailPeriods is GET /platform/guardrails/periods?period=week|month|quarter
// → GuardrailsOut of the latest published period.
func (d Deps) guardrailPeriods(w http.ResponseWriter, r *http.Request) {
	v, err := params(r, []string{"period"})
	if err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if _, err := calendar.ParseKind(v["period"]); err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	d.guardrailsRead(w, r, v["period"])
}

func (d Deps) guardrailsRead(w http.ResponseWriter, r *http.Request, period string) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	now := d.now()
	if guardClock != nil {
		now = guardClock().UTC()
	}
	days, rolling := rollingPeriods[period]
	if rolling && !now.Before(RollingSunset) {
		httpx.Error(w, r, "validation_failed") // retired (ADR 0012)
		return
	}
	allowed, err := d.reserveRate(r, p, guardrailsScope, "platform.guardrails.rate.v1", GuardrailsPerMinute)
	switch {
	case err != nil:
		d.log().Error("platform: guardrails refused: rate counter unavailable", "request_id", httpx.RequestIDFrom(r.Context()),
			"reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return
	case !allowed:
		httpx.Error(w, r, "rate_limited")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), guardrailsTimeout)
	defer cancel()
	var out GuardrailsOut
	if rolling {
		from, to := guardWindow(now, days)
		out, err = d.rollingHidden(ctx, period, from, to)
	} else {
		out, err = d.calendarGuardrails(ctx, calendar.Kind(period))
	}
	switch {
	case errors.Is(err, errNotPublished):
		httpx.Error(w, r, "not_ready")
		return
	case err != nil:
		// No count is ever in the log: ids and a fixed reason only.
		d.log().Error("platform: guardrails failed", "request_id", httpx.RequestIDFrom(r.Context()),
			"user_id", p.UserID.String(), "reason", "internal_error")
		httpx.Error(w, r, "internal_error")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

// calendarGuardrails reads the latest stored snapshot of kind k and renders
// its QG family.
func (d Deps) calendarGuardrails(ctx context.Context, k calendar.Kind) (GuardrailsOut, error) {
	var from time.Time
	var state string
	var body *string
	var computed time.Time
	err := authz.Tx(ctx, d.Pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT period_from, state, body::text, computed_at FROM analytics_snapshots
			WHERE kind = $1 ORDER BY period_from DESC LIMIT 1`, string(k)).Scan(&from, &state, &body, &computed)
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return GuardrailsOut{}, errNotPublished
	case err != nil:
		return GuardrailsOut{}, err
	case state != "ok" || body == nil:
		return GuardrailsOut{}, errNotPublished
	}
	p := calendar.Of(k, from.UTC())
	if !p.From.Equal(from.UTC()) {
		return GuardrailsOut{}, errSnapshot
	}
	b, err := analytics.DecodeBody([]byte(*body), p)
	if err != nil {
		return GuardrailsOut{}, errSnapshot
	}
	return GuardrailsFromBody(b, p, computed), nil
}

// GuardrailsFromBody renders the guardrail statistics of a stored body: its
// QG cells as stored (the same cells, shown and hidden, as Answer quality),
// the hospitals whose names can be shown, in the body's order.
func GuardrailsFromBody(b analytics.Body, p calendar.Period, computed time.Time) GuardrailsOut {
	rates := func(r analytics.Row) Rates {
		return Rates{Answers: r.Answers, Pass: r.Pass, Regenerate: r.Regenerate, Escalate: r.Escalate, Degraded: r.Degraded,
			PassShare: r.PassShare, RegenerateShare: r.RegenerateShare, EscalateShare: r.EscalateShare}
	}
	out := GuardrailsOut{Period: string(p.Kind), From: stamp(p.From), To: stamp(p.To), UpdatedAt: stamp(computed),
		All: rates(b.All), Hospitals: make([]GuardrailHospital, 0, len(b.Hospitals))}
	for _, h := range b.Hospitals {
		if h.Hospital.Name == "" {
			continue // a name that can't be shown: every figure of it is hidden
		}
		out.Hospitals = append(out.Hospitals, GuardrailHospital{Hospital: Ref{ID: h.Hospital.ID, Name: h.Hospital.Name}, Rates: rates(h.Row)})
	}
	return out
}

// HiddenRates is a row with every count and share hidden.
func HiddenRates() Rates {
	h := smallgroup.Guarded{Suppressed: true}
	s := smallgroup.GuardedShare{Suppressed: true}
	return Rates{Answers: h, Pass: h, Regenerate: h, Escalate: h, Degraded: h, PassShare: s, RegenerateShare: s, EscalateShare: s}
}

// RollingGuardrails is a deprecated rolling period's response (R41a-3):
// the hospitals listed, every figure hidden, all included.
func RollingGuardrails(period string, from, to time.Time, hospitals []Ref) GuardrailsOut {
	out := GuardrailsOut{Period: period, From: stamp(from), To: stamp(to), UpdatedAt: stamp(to), All: HiddenRates(),
		Hospitals: make([]GuardrailHospital, 0, len(hospitals))}
	for _, h := range hospitals {
		out.Hospitals = append(out.Hospitals, GuardrailHospital{Hospital: h, Rates: HiddenRates()})
	}
	return out
}

// rollingHidden reads the active hospitals (and nothing else) for a
// deprecated rolling period.
func (d Deps) rollingHidden(ctx context.Context, period string, from, to time.Time) (GuardrailsOut, error) {
	var refs []Ref
	err := authz.Tx(ctx, d.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text, name FROM hospitals WHERE status = 'active' ORDER BY name, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ref Ref
			if err := rows.Scan(&ref.ID, &ref.Name); err != nil {
				return err
			}
			if audit.ShowableName(ref.Name) {
				refs = append(refs, ref)
			}
		}
		return rows.Err()
	})
	return RollingGuardrails(period, from, to, refs), err
}
