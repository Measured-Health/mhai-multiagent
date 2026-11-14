package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// Named access (design §8.2-§8.6; plan Task 22, departures 45-57): the
// position, the person's own cover, and the named list. Off by default:
// analysis off is 404, the effective named-access position off is 403
// named_access_off, and a named list needs the position on and a letter
// that covers the person now. A name or an MRN is never logged, traced,
// stored or kept by the idempotency store (httpx.NoStore): it is read from
// the identity source after the list and its entry have committed, put in
// this one reply, and dropped.

// NamedRule is the named list's rule: the routes' roles, two-step, and a
// write (auditors are refused). The named list has no step-up (design
// §8.3); the role, two-step and account are re-read in its transaction.
var NamedRule = authz.Rule{AnyRole: Rule.AnyRole, NeedsMFA: true, Write: true}

func (h *handlers) resolver() analysis.Resolver {
	if h.d.Identity != nil {
		return h.d.Identity
	}
	return analysis.Unavailable{}
}

// namedAccessReply is GET /analysis/named-access.
type namedAccessReply struct {
	Position string     `json:"position"`
	Since    *time.Time `json:"since"`
}

// namedAccess is GET /analysis/named-access: the hospital's effective
// position and since (design §8.4).
func (h *handlers) namedAccess(w http.ResponseWriter, r *http.Request) {
	if !h.on(r.Context()) {
		httpx.Error(w, r, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	var out namedAccessReply
	err := analysis.PersonTx(ctx, h.d.Pool, func(tx pgx.Tx) error {
		var err error
		out.Position, out.Since, err = analysis.NamedState(ctx, tx)
		return err
	})
	if err != nil {
		h.namedFail(w, r, err)
		return
	}
	if out.Position == "off" {
		httpx.Error(w, r, "named_access_off")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

// clearanceMeReply is GET /analysis/clearance/me (design §8.4).
type clearanceMeReply struct {
	Named       bool       `json:"named"`
	Until       *time.Time `json:"until"`
	ClearanceID *uuid.UUID `json:"clearance_id"`
	Version     *int       `json:"version"`
	Reason      *string    `json:"reason"`
}

// clearanceMe is GET /analysis/clearance/me: the person's own cover, read
// again on every call (nothing cached), at most ClearanceMePerMinute a
// session; not audited.
func (h *handlers) clearanceMe(w http.ResponseWriter, r *http.Request) {
	if !h.on(r.Context()) {
		httpx.Error(w, r, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	p, _ := authz.FromContext(ctx)
	if !h.meLim.allow(sessionKey(p)) {
		httpx.ErrorRetryAfter(w, r, "rate_limited", int(time.Minute.Seconds()))
		return
	}
	var out clearanceMeReply
	err := analysis.PersonTx(ctx, h.d.Pool, func(tx pgx.Tx) error {
		pos, _, err := analysis.NamedState(ctx, tx)
		if err != nil {
			return err
		}
		if pos == "off" {
			return analysis.ErrNamedOff
		}
		c, err := analysis.NamedCover(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		out = clearanceMeReply{Named: c.Named, Until: c.Until, ClearanceID: c.Clearance, Version: c.Version, Reason: c.Reason}
		return nil
	})
	if err != nil {
		h.namedFail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

// namedBody is the named list's request: exactly these fields.
type namedBody struct {
	Drill   string            `json:"drill"`
	Purpose string            `json:"purpose"`
	Note    *string           `json:"note"`
	Filters map[string]string `json:"filters"`
	Period  *struct {
		Level string `json:"level"`
		Start string `json:"start"`
	} `json:"period"`
}

var errBody = errors.New("routes: named body")

// noteOK: 1-200 characters, none a control character (departure 53).
func noteOK(s string) bool {
	n := utf8.RuneCountInString(s)
	if !utf8.ValidString(s) || n < 1 || n > 200 {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// idemKeyOK: one Idempotency-Key of 1-255 printable characters.
func idemKeyOK(keys []string) bool {
	if len(keys) != 1 || keys[0] == "" || len(keys[0]) > 255 {
		return false
	}
	for i := 0; i < len(keys[0]); i++ {
		if keys[0][i] < 0x21 || keys[0][i] > 0x7e {
			return false
		}
	}
	return true
}

// parseNamed checks the body and turns its period and filters into the
// query form the views take (the same parser and filter rules).
func parseNamed(b namedBody) (url.Values, error) {
	if !drillShape.MatchString(b.Drill) || !slices.Contains(analysis.Purposes, b.Purpose) || b.Period == nil {
		return nil, errBody
	}
	if (b.Note != nil && !noteOK(*b.Note)) || (b.Purpose == "other" && b.Note == nil) {
		return nil, errBody
	}
	q := url.Values{"period": {b.Period.Level}, "start": {b.Period.Start}}
	for k, v := range b.Filters {
		if k == "period" || k == "start" || k == "from" || k == "to" {
			return nil, errBody
		}
		q.Set(k, v)
	}
	return q, nil
}

// canonical is the body as the key binds it: the parsed values only, in a
// fixed order (filters by name, sorted by encoding/json).
func canonical(b namedBody, f disclose.Filters) []byte {
	var note any
	if b.Note != nil {
		note = *b.Note
	}
	out, _ := json.Marshal([]any{b.Drill, b.Purpose, note, analysis.FiltersCodes(f), b.Period.Level, b.Period.Start})
	return out
}

// namedLimit: the person's named views today are used up (429).
type namedLimit struct{}

func (namedLimit) Error() string { return "routes: named limit" }

// namedRow is a row with its name and MRN (design §8.3).
type namedRow struct {
	analysis.PatientRow
	Name string  `json:"name"`
	MRN  *string `json:"mrn"`
}

type namedReply struct {
	Level      string     `json:"level"`
	Count      int64      `json:"count"`
	Patients   []namedRow `json:"patients"`
	NamedUntil time.Time  `json:"named_until"`
	Clearance  struct {
		ID      uuid.UUID `json:"id"`
		EndDate string    `json:"end_date"`
	} `json:"clearance"`
}

// named is POST /analysis/patients/named (design §8.3, §8.5): one named
// list, or a replay of one, under the letter that covers the person now.
// In one transaction as the person, in order: the Idempotency-Key; the
// role, two-step and account re-read; the named budget; the cover; the
// drill; the list opened (or replayed) and analysis.named_viewed appended
// last; commit. Only then is identity resolved, for exactly the list's
// subjects; a failure there is 503 and a flagged analysis.named_view_failed.
func (h *handlers) named(w http.ResponseWriter, r *http.Request) {
	if !h.on(r.Context()) {
		httpx.Error(w, r, "not_found")
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	if !idemKeyOK(keys) || r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var b namedBody
	if !httpx.Decode(w, r, &b) {
		return
	}
	q, err := parseNamed(b)
	if err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
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
	pr, _ := authz.FromContext(ctx)
	if h.d.Keys == nil {
		httpx.Error(w, r, "too_small") // no keys: no count has a drill
		return
	}
	p, err := h.d.Engine.Lookup(ctx, per)
	if err != nil {
		httpx.Error(w, r, problem(err))
		return
	}
	s, ok, err := h.d.Engine.Stored(ctx, p, f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	g, found := analysis.DrillGroup{}, false
	if ok {
		g, found = analysis.FindDrill(h.d.Keys, pr.HospitalID, p.ID, f, s.Record, b.Drill)
	}
	var note []byte
	if b.Note != nil {
		if note, err = h.d.Keys.SealNote(pr.HospitalID, *b.Note); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	mac := h.d.Keys.BodyMAC(canonical(b, f))
	if mac == nil {
		h.fail(w, r, analysis.ErrFailed)
		return
	}
	l := analysis.NamedList{View: uuid.New(), Period: p, F: f, G: g, Drill: b.Drill, Purpose: b.Purpose, Note: note,
		Key: analysis.KeyHash(keys[0]), Body: mac, Now: h.d.Engine.ClockNow()}
	var res analysis.Named
	err = analysis.PersonTx(ctx, h.d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '`+disclose.StatementTimeout.String()+`'`); err != nil {
			return err
		}
		state, prior, err := analysis.KeyState(ctx, tx, l.Key, l.Body)
		if err != nil {
			return err
		}
		switch state {
		case "mismatch":
			return analysis.ErrIdemMismatch
		case "expired":
			return analysis.ErrExpired
		case "replay":
			l.View = prior
		case "new":
		default:
			return analysis.ErrFailed
		}
		if err := stillAllowed(ctx, tx, pr); err != nil {
			return err
		}
		admitted, err := analysis.NamedAdmit(ctx, tx, NamedPerDay, l.Now)
		if err != nil {
			return err
		}
		if !admitted {
			return namedLimit{}
		}
		c, err := analysis.NamedCover(ctx, tx, pr.UserID)
		if err != nil {
			return err
		}
		if err := analysis.CoverRefusal(c); err != nil {
			return err
		}
		if !found {
			return analysis.ErrTooSmall
		}
		if state == "replay" {
			res, err = analysis.ReplayNamed(ctx, tx, l)
		} else {
			res, err = analysis.OpenNamed(ctx, tx, l)
		}
		if err != nil {
			return err
		}
		if h.afterOpen != nil {
			if err := h.afterOpen(); err != nil {
				return err
			}
		}
		// Last: the audit chain's lock is held until commit. No entry, no
		// names (T18).
		return audit.Write(ctx, tx, audit.Event{ActorID: pr.UserID.String(), HospitalID: pr.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Action: "analysis.named_viewed", Entity: "analysis_view", EntityID: l.View.String(),
			Payload: map[string]any{"view": string(g.View), "period": string(p.Period.Kind) + ":" + p.Period.From.Format(time.DateOnly),
				"filters": analysis.FiltersCodes(f), "patient_count": g.Count, "rows": len(res.Rows), "purpose": b.Purpose,
				"replays": res.Replays, "clearance_id": res.Clearance.String()}})
	})
	switch {
	case err == nil:
	case errors.Is(err, errStepUp):
		httpx.Error(w, r, "mfa_required")
		return
	case errors.Is(err, errRole):
		httpx.Error(w, r, "forbidden")
		return
	case errors.As(err, new(namedLimit)):
		h.flag(r.Context(), pr, "named_day", "analysis.named_limit_reached", map[string]any{"limit": NamedPerDay})
		httpx.Error(w, r, "rate_limited")
		return
	case errors.Is(err, analysis.ErrPatientCap):
		h.flag(r.Context(), pr, "patient_cap", "analysis.patient_cap_reached", map[string]any{"cap": disclose.PatientCap})
		httpx.Error(w, r, "rate_limited")
		return
	default:
		h.namedFail(w, r, err)
		return
	}

	// Committed and audited: now, and only now, the names.
	people, err := h.identify(ctx, pr, l.View, res)
	if err != nil {
		h.namedViewFailed(r.Context(), pr, l.View, res.Clearance)
		httpx.Error(w, r, "unavailable")
		return
	}
	out := namedReply{Level: "named", Count: g.Count, Patients: people, NamedUntil: res.Until}
	out.Clearance.ID, out.Clearance.EndDate = res.Clearance, res.ClearanceEnd.Format(time.DateOnly)
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

// identify resolves exactly the list's subjects, each reference opened
// for this call only, and checks the reply (T20).
func (h *handlers) identify(ctx context.Context, pr authz.Principal, view uuid.UUID, res analysis.Named) ([]namedRow, error) {
	refs := make([]string, len(res.Refs))
	for i, ct := range res.Refs {
		if len(ct) == 0 {
			return nil, analysis.ErrIdentityUnavailable
		}
		ref, err := h.d.Keys.OpenRef(pr.HospitalID, ct)
		if err != nil || ref == "" {
			return nil, analysis.ErrIdentityUnavailable
		}
		refs[i] = ref
	}
	ids, err := h.resolver().Resolve(ctx, analysis.IdentityRequest{Hospital: pr.HospitalID, User: pr.UserID, View: view,
		Clearance: res.Clearance, Demo: res.Demo, Refs: slices.Clone(refs)})
	if err == nil {
		err = analysis.CheckIdentities(refs, ids)
	}
	if err != nil {
		return nil, err
	}
	out := make([]namedRow, len(res.Rows))
	for i, row := range res.Rows {
		id := ids[refs[i]]
		out[i] = namedRow{PatientRow: row, Name: id.Name()}
		if m := id.MRN(); m != "" {
			out[i].MRN = &m
		}
	}
	return out, nil
}

// namedViewFailed records, in its own transaction, that a committed list
// or replay sent no names (flagged; the letter only). Logged with ids and
// a fixed reason only.
func (h *handlers) namedViewFailed(parent context.Context, pr authz.Principal, view, clearance uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 2*disclose.StatementTimeout)
	defer cancel()
	err := analysis.PersonTx(authz.WithPrincipal(ctx, pr), h.d.Pool, func(tx pgx.Tx) error {
		return audit.Write(ctx, tx, audit.Event{ActorID: pr.UserID.String(), HospitalID: pr.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Action: "analysis.named_view_failed", Entity: "analysis_view", EntityID: view.String(),
			Payload: map[string]any{"clearance_id": clearance.String()}})
	})
	h.log().Error("analysis: named list sent no names", "request_id", httpx.RequestIDFrom(ctx),
		"hospital_id", pr.HospitalID.String(), "reason", "unavailable", "recorded", err == nil)
}

// namedFail answers a named route's refusal with its code, and anything
// else as h.fail does.
func (h *handlers) namedFail(w http.ResponseWriter, r *http.Request, err error) {
	for _, c := range []struct {
		err  error
		code string
	}{
		{analysis.ErrAnalysisOff, "not_found"},
		{analysis.ErrNamedOff, "named_access_off"},
		{analysis.ErrNotCleared, "not_cleared"},
		{analysis.ErrClearanceEnded, "clearance_ended"},
		{analysis.ErrNotAllowed, "forbidden"},
		{analysis.ErrIdemMismatch, "idempotency_mismatch"},
		{analysis.ErrExpired, "expired"},
		{analysis.ErrTooSmall, "too_small"},
	} {
		if errors.Is(err, c.err) {
			httpx.Error(w, r, c.code)
			return
		}
	}
	h.fail(w, r, err)
}
