package routes

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// drillShape is a drill key: 43 characters of base64url.
var drillShape = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// The list's own refusals.
var (
	errStepUp = errors.New("routes: two-step no longer on")
	errRole   = errors.New("routes: role no longer held")
)

// limitError is a list limit that refused (429 and one flagged entry a
// day): window "minute" or "day".
type limitError struct{ window string }

func (limitError) Error() string { return "routes: list limit" }

// patientsReply is the contract's de-identified list.
type patientsReply struct {
	Level    string                `json:"level"`
	Count    int64                 `json:"count"`
	Patients []analysis.PatientRow `json:"patients"`
}

// patients is GET /analysis/patients: the de-identified list behind one
// shown count of a stored body (design §5.6, §9; plan Task 13). Nothing is
// computed for it: a body that isn't stored, a drill that isn't one of its
// drillable counts' keys for this exact context, or a group whose count
// isn't that cell is 422 too_small. In one transaction as the person: the
// role, two-step and active account re-read; the list limits; the list
// opened (the patients reserved against the person's quarter); its audit
// entry appended last. Nothing in the query, the reply or the counts is
// logged.
func (h *handlers) patients(w http.ResponseWriter, r *http.Request) {
	if !h.on(r.Context()) {
		httpx.Error(w, r, "not_found")
		return
	}
	q := r.URL.Query()
	if lv := q["level"]; len(lv) != 1 || lv[0] != "deidentified" {
		httpx.Error(w, r, "level_not_supported")
		return
	}
	dr := q["drill"]
	if len(dr) != 1 || !drillShape.MatchString(dr[0]) {
		httpx.Error(w, r, "too_small")
		return
	}
	drill := dr[0]
	q.Del("level")
	q.Del("drill")
	per, _, f, err := parse(q, true, true)
	if err == nil {
		err = disclose.CheckFilters(f)
	}
	if err != nil {
		httpx.Error(w, r, problem(err))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*disclose.StatementTimeout)
	defer cancel()
	p, err := h.d.Engine.Lookup(ctx, per)
	if err != nil {
		httpx.Error(w, r, problem(err))
		return
	}
	pr, _ := authz.FromContext(ctx)
	s, ok, err := h.d.Engine.Stored(ctx, p, f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	g, found := analysis.DrillGroup{}, false
	if ok {
		g, found = analysis.FindDrill(h.d.Keys, pr.HospitalID, p.ID, f, s.Record, drill)
	}
	if !found {
		httpx.Error(w, r, "too_small")
		return
	}
	view := uuid.New()
	var rows []analysis.PatientRow
	err = db.WithHospitalUser(ctx, h.d.Pool, pr.HospitalID, pr.UserID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '`+disclose.StatementTimeout.String()+`'`); err != nil {
			return err
		}
		if err := stillAllowed(ctx, tx, pr); err != nil {
			return err
		}
		var admit string
		if err := tx.QueryRow(ctx, `SELECT mhai_analysis_list_admit($1, $2, $3)`, ListsPerMinute, ListsPerDay,
			h.d.Engine.ClockNow()).Scan(&admit); err != nil {
			return err
		}
		if admit != "ok" {
			return limitError{admit}
		}
		var err error
		rows, err = analysis.Open(ctx, tx, analysis.List{View: view, Period: p, F: f, G: g, Drill: drill, Now: h.d.Engine.ClockNow()})
		if err != nil {
			return err
		}
		if h.afterOpen != nil {
			if err := h.afterOpen(); err != nil {
				return err
			}
		}
		// Last: the audit chain's lock is held until commit.
		return audit.Write(ctx, tx, audit.Event{ActorID: pr.UserID.String(), HospitalID: pr.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Action: "analysis.patients_viewed", Entity: "analysis_view", EntityID: view.String(),
			Payload: map[string]any{"view": string(g.View), "period": string(p.Period.Kind) + ":" + p.Period.From.Format(time.DateOnly),
				"filters": analysis.FiltersCodes(f), "patient_count": g.Count, "rows": len(rows)}})
	})
	var lim limitError
	switch {
	case err == nil:
	case errors.Is(err, errStepUp):
		httpx.Error(w, r, "mfa_required")
		return
	case errors.Is(err, errRole):
		httpx.Error(w, r, "forbidden")
		return
	case errors.As(err, &lim):
		h.flag(r.Context(), pr, "list_"+lim.window, "analysis.list_limit_reached", map[string]any{"window": lim.window})
		httpx.ErrorRetryAfter(w, r, "rate_limited", 60)
		return
	case errors.Is(err, analysis.ErrPatientCap):
		h.flag(r.Context(), pr, "patient_cap", "analysis.patient_cap_reached", map[string]any{"cap": disclose.PatientCap})
		httpx.Error(w, r, "rate_limited")
		return
	case errors.Is(err, analysis.ErrTooSmall):
		httpx.Error(w, r, "too_small")
		return
	default:
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, patientsReply{Level: "deidentified", Count: g.Count, Patients: rows})
}

// stillAllowed re-reads, inside the list's transaction, what the guard's
// cache may hold for up to 30 seconds (design §6.4): the account active
// and in this hospital, two-step on, and one of the routes' roles still
// held (T13, T14).
func stillAllowed(ctx context.Context, tx pgx.Tx, pr authz.Principal) error {
	var active, mfa bool
	var roles []string
	err := tx.QueryRow(ctx, `SELECT u.is_active AND u.hospital_id = $2, COALESCE(m.enabled, false),
		ARRAY(SELECT r.role FROM user_roles r WHERE r.user_id = u.id)
		FROM users u LEFT JOIN user_mfa m ON m.user_id = u.id WHERE u.id = $1`, pr.UserID, pr.HospitalID).Scan(&active, &mfa, &roles)
	if errors.Is(err, pgx.ErrNoRows) {
		return errRole
	}
	if err != nil {
		return err
	}
	if !active {
		return errRole
	}
	if !mfa {
		return errStepUp
	}
	for _, want := range Rule.AnyRole {
		if slices.Contains(roles, want) && slices.Contains(pr.Roles, want) {
			return nil
		}
	}
	return errRole
}

// flag records a refused list once a day per person and kind, with a
// flagged audit entry, in its own transaction (the request's rolled back).
// A failure is logged with ids and a fixed reason only; the reply is the
// refusal either way.
func (h *handlers) flag(ctx context.Context, pr authz.Principal, kind, action string, payload map[string]any) {
	err := db.WithHospitalUser(ctx, h.d.Pool, pr.HospitalID, pr.UserID, func(tx pgx.Tx) error {
		var first bool
		if err := tx.QueryRow(ctx, `SELECT mhai_analysis_drill_flag($1, $2)`, kind, h.d.Engine.ClockNow()).Scan(&first); err != nil {
			return err
		}
		if !first {
			return nil
		}
		return audit.Write(ctx, tx, audit.Event{ActorID: pr.UserID.String(), HospitalID: pr.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Action: action, Entity: "user", EntityID: pr.UserID.String(), Payload: payload})
	})
	if err != nil {
		h.log().Error("analysis: flag failed", "request_id", httpx.RequestIDFrom(ctx), "hospital_id", pr.HospitalID.String(),
			"reason", "unavailable")
	}
}
