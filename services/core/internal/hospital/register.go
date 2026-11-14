// Package hospital serves the hospital admin's pages of the clinical app
// (frontend requirements §5; mhai-web apps/clinical/lib/api/admin-contract.ts):
//
//   - staff: GET /hospital/staff; PATCH /hospital/staff/{id} (roles and
//     specialty, Analyst through migration 00030's functions);
//     POST /hospital/staff/{id}/deactivate and /reactivate;
//   - settings: GET and PATCH /hospital/settings;
//   - usage: GET /hospital/usage?period=, with the EMR alert block
//     (alerts.UsageBlock);
//   - activity: GET /hospital/activity, the hospital's own audit entries
//     as structured events;
//   - review oversight: GET /hospital/review-oversight and GET
//     /hospital/corrections/{id}/eligible-reviewers.
//
// What core has no feature behind (email delivery, invites, password and
// two-step resets by an admin, account removal, reviewer reminders and
// assignment) is registered and answers 501 not_implemented, so the app
// shows a clear refusal rather than a 404.
//
// Every route is a hospital admin's with a two-step session. Every staff
// write also takes a fresh step-up code (mfa_code or backup_code in the
// body), checked after the request's own refusals (not_found,
// self_change, last_admin, conflict) so a refused request doesn't spend
// it. A settings edit takes one only when it loosens a sign-in protection.
//
// Identity is the verified principal only; every query runs as the
// caller's hospital and user (authz.TxUser), so row-level security scopes
// it, and another hospital's person or correction is 404. Logs carry ids
// and codes only: never a name, an email or a specialty. Every change is
// audited in its own transaction with a closed payload.
package hospital

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// Deps are what the routes need.
type Deps struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	// Auth checks the staff writes' step-up codes and denylists the
	// sessions a deactivation revokes; nil answers the writes 503
	// not_ready.
	Auth *auth.Service
	// Notify tells the hospital's admins of an Analyst grant, after
	// commit; nil uses analysis.NotifyAnalystGranted.
	Notify analysis.Notifier
	// Now is the clock (usage periods, activity window); defaults to
	// time.Now.
	Now func() time.Time
	// stepUpCheck, when set (tests only), replaces Auth's step-up check.
	stepUpCheck func(r *http.Request, mfaCode, backupCode string) error
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) notify() analysis.Notifier {
	if d.Notify != nil {
		return d.Notify
	}
	return analysis.NotifyAnalystGranted(d.Log)
}

// Rules of the routes.
var (
	admin      = []string{authz.RoleHospitalAdmin}
	ReadRule   = authz.Rule{AnyRole: admin, NeedsMFA: true}
	WriteRule  = authz.Rule{AnyRole: admin, NeedsMFA: true, Write: true}
	StepUpRule = authz.Rule{AnyRole: admin, NeedsMFA: true, Write: true, StepUp: true}
)

// Unsupported are the routes the app calls that core has no feature
// behind: each answers 501 not_implemented.
var Unsupported = []struct{ Method, Pattern string }{
	{http.MethodPost, "/hospital/staff/invites"},
	{http.MethodDelete, "/hospital/staff/{id}"},
	{http.MethodPost, "/hospital/staff/{id}/password-reset"},
	{http.MethodPost, "/hospital/staff/{id}/reset-mfa"},
	{http.MethodPost, "/hospital/staff/{id}/remind"},
	{http.MethodPost, "/hospital/staff/{id}/resend-invite"},
	{http.MethodPost, "/hospital/corrections/{id}/remind"},
	{http.MethodPost, "/hospital/corrections/{id}/assign"},
}

// Register adds the routes.
func Register(m *authz.Mux, d Deps) {
	for _, rt := range routes(d) {
		authz.Route(m, rt.Method, rt.Pattern, rt.Rule, rt.Handler)
	}
}

// Route is one route: its method, pattern, rule and handler.
type Route struct {
	Method, Pattern string
	Rule            authz.Rule
	Handler         http.Handler
}

func routes(d Deps) []Route {
	out := []Route{
		{http.MethodGet, "/hospital/staff", ReadRule, http.HandlerFunc(d.staffList)},
		{http.MethodPatch, "/hospital/staff/{id}", StepUpRule, http.HandlerFunc(d.staffPatch)},
		{http.MethodPost, "/hospital/staff/{id}/deactivate", StepUpRule, d.staffStatus(false)},
		{http.MethodPost, "/hospital/staff/{id}/reactivate", StepUpRule, d.staffStatus(true)},
		{http.MethodGet, "/hospital/settings", ReadRule, http.HandlerFunc(d.settingsGet)},
		{http.MethodPatch, "/hospital/settings", StepUpRule, http.HandlerFunc(d.settingsPatch)},
		{http.MethodGet, "/hospital/usage", ReadRule, http.HandlerFunc(d.usage)},
		{http.MethodGet, "/hospital/activity", ReadRule, http.HandlerFunc(d.activity)},
		{http.MethodGet, "/hospital/review-oversight", ReadRule, http.HandlerFunc(d.oversight)},
		{http.MethodGet, "/hospital/corrections/{id}/eligible-reviewers", ReadRule, http.HandlerFunc(d.eligibleReviewers)},
	}
	for _, u := range Unsupported {
		out = append(out, Route{u.Method, u.Pattern, WriteRule, http.HandlerFunc(notImplemented)})
	}
	return out
}

// notImplemented answers a route core has no feature behind. The caller
// is checked first, so the guard's refusals still come before it.
func notImplemented(w http.ResponseWriter, r *http.Request) {
	if _, ok := caller(w, r); !ok {
		return
	}
	httpx.Error(w, r, "not_implemented")
}

// caller is the verified clinical principal, or a 500 (the guard always
// sets one; a platform account never reaches a clinical route).
func caller(w http.ResponseWriter, r *http.Request) (authz.Principal, bool) {
	p, ok := authz.FromContext(r.Context())
	if !ok || p.HospitalID == uuid.Nil || p.Platform {
		httpx.Error(w, r, "internal_error")
		return p, false
	}
	return p, true
}

// The closed errors the handlers map to codes.
var (
	errNotFound   = errors.New("hospital: not found")
	errInvalid    = errors.New("hospital: invalid")
	errConflict   = errors.New("hospital: conflict")
	errSelf       = errors.New("hospital: self change")
	errLastAdmin  = errors.New("hospital: last admin")
	errDecided    = errors.New("hospital: already decided")
	errBusy       = errors.New("hospital: busy")
	errNotEnabled = errors.New("hospital: not eligible")
)

// codeFor maps an error to its problem code. A driver error is never sent
// or logged: only the code (and a database error's SQLSTATE).
func codeFor(err error) string {
	switch {
	case errors.Is(err, errNotFound), errors.Is(err, analysis.ErrAnalystNotFound):
		return "not_found"
	case errors.Is(err, errInvalid):
		return "validation_failed"
	case errors.Is(err, errConflict):
		return "conflict"
	case errors.Is(err, errSelf), errors.Is(err, analysis.ErrAnalystSelf):
		return "self_change"
	case errors.Is(err, errLastAdmin):
		return "last_admin"
	case errors.Is(err, errDecided):
		return "already_decided"
	case errors.Is(err, errBusy):
		return "not_ready"
	case errors.Is(err, analysis.ErrAnalystForbidden):
		return "forbidden"
	case errors.Is(err, analysis.ErrAnalystOff):
		return "analysis_off"
	case errors.Is(err, analysis.ErrAnalystCap):
		return "analyst_cap"
	case errors.Is(err, analysis.ErrAnalystNotEligible), errors.Is(err, errNotEnabled):
		return "not_eligible"
	}
	return "internal_error"
}

// fail answers err's code and logs it by ids and codes only.
func (d Deps) fail(w http.ResponseWriter, r *http.Request, p authz.Principal, what string, err error) {
	code := codeFor(err)
	attrs := []any{"request_id", httpx.RequestIDFrom(r.Context()), "hospital_id", p.HospitalID.String(),
		"user_id", p.UserID.String(), "reason", code}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		attrs = append(attrs, "sqlstate", pe.Code)
	}
	if code == "internal_error" {
		d.log().Error("hospital: "+what+" failed", attrs...)
	} else {
		d.log().Info("hospital: "+what+" refused", attrs...)
	}
	httpx.Error(w, r, code)
}

// tx runs fn as the caller's hospital and user (READ COMMITTED: the audit
// chain and 00030's rules need it).
func (d Deps) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	if d.Pool == nil {
		return errBusy
	}
	return authz.TxUser(ctx, d.Pool, fn)
}

// stepUpIn is the step-up code a staff write carries.
type stepUpIn struct {
	MFACode    string `json:"mfa_code,omitempty"`
	BackupCode string `json:"backup_code,omitempty"`
}

func (s stepUpIn) given() bool { return s.MFACode != "" || s.BackupCode != "" }

// stepUp checks a fresh code (a wrong one is audited by auth); false means
// the reply is written. No code at all is 403 mfa_required, unaudited.
func (d Deps) stepUp(w http.ResponseWriter, r *http.Request, in stepUpIn) bool {
	if !in.given() {
		httpx.Error(w, r, "mfa_required")
		return false
	}
	if d.stepUpCheck != nil {
		if d.stepUpCheck(r, in.MFACode, in.BackupCode) != nil {
			httpx.Error(w, r, "mfa_invalid")
			return false
		}
		return true
	}
	if d.Auth == nil {
		httpx.Error(w, r, "not_ready")
		return false
	}
	if err := d.Auth.StepUpAudited(r, in.MFACode, in.BackupCode); err != nil {
		d.Auth.WriteStepUpError(w, r, err)
		return false
	}
	return true
}

// stamp is the API's time format (UTC, milliseconds).
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func stampPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := stamp(*t)
	return &s
}

// canonicalID parses a canonical uuid (lower-case, hyphenated).
func canonicalID(s string) (uuid.UUID, bool) {
	u, err := uuid.Parse(s)
	if err != nil || u.String() != s || u == uuid.Nil {
		return uuid.Nil, false
	}
	return u, true
}
