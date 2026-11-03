// Package feedback serves feedback and corrections (Plan 4 spec §4.2,
// §4.4, §9): POST /feedback, the corrections queue and its decisions
// (approve, reject, withdraw, with the two-person rule) and the caller's
// own counts.
//
// Identity comes only from the verified token (authz.Principal); the store
// runs every query in db.WithHospitalUser with that hospital and user, so
// RLS and 00011's triggers see the same caller. The free text (reason,
// corrected diagnosis, rejection note) is stored on the row and shown to
// the hospital's clinicians only: it is never logged, audited, traced or
// sent to intelligence. A reviewer reads the masked consultation only.
package feedback

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// ApproveCorrection is the job an approval feeds (published by Task 6).
const ApproveCorrection = "pipeline.approve_correction"

// Deps are what the routes need.
type Deps struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (d Deps) store() Store { return Store{Pool: d.Pool} }

// Register adds the routes. Every one is for clinicians only (a hospital
// admin who isn't a clinician gets 403); every write takes Idempotency-Key
// (the mux's store); none is bff_direct.
func Register(m *authz.Mux, d Deps) {
	clinician := []string{authz.RoleClinician}
	read := authz.Rule{AnyRole: clinician}
	write := authz.Rule{AnyRole: clinician, Write: true}
	authz.Route(m, http.MethodPost, "/feedback", write, http.HandlerFunc(d.submit))
	authz.Route(m, http.MethodGet, "/feedback/stats/mine", read, http.HandlerFunc(d.stats))
	authz.Route(m, http.MethodGet, "/corrections", read, http.HandlerFunc(d.pending))
	authz.Route(m, http.MethodGet, "/corrections/mine", read, http.HandlerFunc(d.mine))
	authz.Route(m, http.MethodGet, "/corrections/reviewed", read, http.HandlerFunc(d.reviewed))
	authz.Route(m, http.MethodPost, "/corrections/{id}/approve",
		authz.Rule{AnyRole: clinician, Write: true, Calls: []string{ApproveCorrection}}, d.decide("approve"))
	authz.Route(m, http.MethodPost, "/corrections/{id}/reject", write, d.decide("reject"))
	authz.Route(m, http.MethodPost, "/corrections/{id}/withdraw", write, d.decide("withdraw"))
}

// caller is the verified principal, or a 500 (the guard always sets one).
func caller(w http.ResponseWriter, r *http.Request) (authz.Principal, bool) {
	p, ok := authz.FromContext(r.Context())
	if !ok || p.HospitalID == uuid.Nil || p.Platform {
		httpx.Error(w, r, "internal_error")
		return p, false
	}
	return p, true
}

// codeFor maps a store error to its problem code. Only the code leaves:
// a driver error can carry row values, so it is never logged or sent.
func codeFor(err error) string {
	switch {
	case errors.Is(err, ErrUnusable):
		return "unauthenticated"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrSelf):
		return "self_approval"
	case errors.Is(err, ErrForbidden):
		return "forbidden"
	case errors.Is(err, ErrDecided):
		return "already_decided"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, ErrInvalid):
		return "validation_failed"
	}
	return "internal_error"
}

// fail answers err's code and logs it by ids and the code only.
func (d Deps) fail(w http.ResponseWriter, r *http.Request, p authz.Principal, what string, id uuid.UUID, err error) {
	code := codeFor(err)
	attrs := []any{"request_id", httpx.RequestIDFrom(r.Context()), "hospital_id", p.HospitalID.String(),
		"user_id", p.UserID.String(), "reason", code}
	if id != uuid.Nil {
		attrs = append(attrs, "id", id.String())
	}
	if code == "internal_error" {
		d.log().Error("feedback: "+what+" failed", attrs...)
	} else {
		d.log().Info("feedback: "+what+" refused", attrs...)
	}
	httpx.Error(w, r, code)
}

// submit is POST /feedback: 200 {status: recorded} for a validation, 200
// {status: pending_review, correction_id} for a correction.
func (d Deps) submit(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	var in FeedbackIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	// The question id names what the guards look up; its shape is checked
	// first. Everything else in the body is checked after the guards.
	qid, ok := canonicalID(in.QueryID)
	if !ok {
		httpx.Error(w, r, "validation_failed")
		return
	}
	out, err := d.store().Submit(r.Context(), p.HospitalID, p.UserID, httpx.RequestIDFrom(r.Context()), qid, in)
	if err != nil {
		d.fail(w, r, p, "submit", qid, err)
		return
	}
	res := FeedbackOut{Status: out.Status}
	if out.CorrectionID != uuid.Nil {
		res.CorrectionID = out.CorrectionID.String()
	}
	httpx.JSON(w, http.StatusOK, res)
}

// decide is POST /corrections/{id}/{approve|reject|withdraw}: 204.
func (d Deps) decide(kind string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := caller(w, r)
		if !ok {
			return
		}
		id, ok := canonicalID(r.PathValue("id"))
		if !ok {
			httpx.Error(w, r, "not_found")
			return
		}
		// Read the body now, check it only after the guards.
		body, err := io.ReadAll(io.LimitReader(r.Body, httpx.MaxBody+1))
		if err != nil || len(body) > httpx.MaxBody {
			httpx.Error(w, r, "validation_failed")
			return
		}
		if _, err := d.store().Decide(r.Context(), p.HospitalID, p.UserID, id, httpx.RequestIDFrom(r.Context()),
			Decision{Kind: kind, Body: body}); err != nil {
			d.fail(w, r, p, kind, id, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// pending is GET /corrections?status=pending (no status means pending).
func (d Deps) pending(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if st, has := r.URL.Query()["status"]; has && (len(st) != 1 || st[0] != "pending") {
		httpx.Error(w, r, "validation_failed")
		return
	}
	d.list(w, r, p, d.store().ListPending)
}

func (d Deps) mine(w http.ResponseWriter, r *http.Request) {
	if p, ok := caller(w, r); ok {
		d.list(w, r, p, d.store().ListMine)
	}
}

func (d Deps) reviewed(w http.ResponseWriter, r *http.Request) {
	if p, ok := caller(w, r); ok {
		d.list(w, r, p, d.store().ListReviewed)
	}
}

func (d Deps) list(w http.ResponseWriter, r *http.Request, p authz.Principal,
	fn func(ctx context.Context, hid, me uuid.UUID) ([]CorrectionOut, error)) {
	out, err := fn(r.Context(), p.HospitalID, p.UserID)
	if err != nil {
		d.fail(w, r, p, "list", uuid.Nil, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// stats is GET /feedback/stats/mine.
func (d Deps) stats(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	st, err := d.store().Stats(r.Context(), p.HospitalID, p.UserID)
	if err != nil {
		d.fail(w, r, p, "stats", uuid.Nil, err)
		return
	}
	httpx.JSON(w, http.StatusOK, st)
}
