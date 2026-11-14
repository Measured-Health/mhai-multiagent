// Package alerts serves EMR alerts and their settings (EMR design §12,
// §13.1, §16.2, §16.3; plan Tasks 32-34):
//
//   - hospital admins: GET and PATCH /hospital/alert-settings (units,
//     levels, recipients, by section with a version), the possible
//     duplicates (GET /hospital/patient-matches, confirm or reject with
//     step-up) and exclusions (POST /hospital/patient-exclusions, step-up);
//   - clinicians: GET /alerts, GET /alerts/unread-count, POST
//     /alerts/{id}/acknowledge and /dismiss, GET and PATCH
//     /me/notification-settings.
//
// Identity is the verified principal only. Every query runs as the
// caller's hospital (and user: authz.TxUser), so row-level security and
// 00029's policies see the same caller; who sees an alert is decided in
// SQL (on the queue, or delivered to the caller), and anything else,
// another hospital's included, is 404. Logs carry ids and codes only: never
// a name, an MRN, a value or a dismissal note. Every change and every page
// of alerts or matches served is audited with a closed payload.
//
// Nothing here raises an alert: core's finding consumer (PR 8b) does. With
// no consumer running the lists are empty and the settings answer
// not_connected until a connection exists.
package alerts

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

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// Deps are what the routes need.
type Deps struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	// Auth checks the step-up code of the match and exclusion writes; nil
	// answers them 503 not_ready.
	Auth *auth.Service
	// MasterKey (MFA_ENCRYPTION_KEY) derives the list cursor's HMAC key;
	// without it a cursor is refused (the first page still answers, with
	// no next_cursor).
	MasterKey []byte
	// Now is the clock (ages); defaults to time.Now.
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

// Register adds the routes.
func Register(m *authz.Mux, d Deps) {
	admin := []string{authz.RoleHospitalAdmin}
	clinician := []string{authz.RoleClinician}
	adminRead := authz.Rule{AnyRole: admin, NeedsMFA: true}
	adminWrite := authz.Rule{AnyRole: admin, NeedsMFA: true, Write: true}
	adminStepUp := authz.Rule{AnyRole: admin, NeedsMFA: true, Write: true, StepUp: true}
	read := authz.Rule{AnyRole: clinician}
	write := authz.Rule{AnyRole: clinician, Write: true}

	authz.Route(m, http.MethodGet, "/hospital/alert-settings", adminRead, http.HandlerFunc(d.settingsGet))
	authz.Route(m, http.MethodPatch, "/hospital/alert-settings", adminWrite, http.HandlerFunc(d.settingsPatch))
	authz.Route(m, http.MethodGet, "/hospital/patient-matches", adminRead, http.HandlerFunc(d.matchesList))
	authz.Route(m, http.MethodPost, "/hospital/patient-matches/{id}/confirm", adminStepUp, http.HandlerFunc(d.matchConfirm))
	authz.Route(m, http.MethodPost, "/hospital/patient-matches/{id}/reject", adminStepUp, http.HandlerFunc(d.matchReject))
	authz.Route(m, http.MethodPost, "/hospital/patient-exclusions", adminStepUp, http.HandlerFunc(d.exclude))

	authz.Route(m, http.MethodGet, "/alerts", read, http.HandlerFunc(d.list))
	authz.Route(m, http.MethodGet, "/alerts/unread-count", read, http.HandlerFunc(d.unreadCount))
	authz.Route(m, http.MethodPost, "/alerts/{id}/acknowledge", write, d.handle("acknowledge"))
	authz.Route(m, http.MethodPost, "/alerts/{id}/dismiss", write, d.handle("dismiss"))
	authz.Route(m, http.MethodGet, "/me/notification-settings", read, http.HandlerFunc(d.notifyGet))
	authz.Route(m, http.MethodPatch, "/me/notification-settings", write, http.HandlerFunc(d.notifyPatch))
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
	errNotFound = errors.New("alerts: not found")
	errInvalid  = errors.New("alerts: invalid")
	errConflict = errors.New("alerts: version conflict")
	errNoRecips = errors.New("alerts: no recipients")
	errBusy     = errors.New("alerts: busy")
)

// codeFor maps an error to its problem code. A driver error is never sent
// or logged: only the code (and a database error's SQLSTATE).
func codeFor(err error) string {
	var pe *pgconn.PgError
	switch {
	case errors.Is(err, errNotFound):
		return "not_found"
	case errors.Is(err, errInvalid):
		return "validation_failed"
	case errors.Is(err, errConflict):
		return "conflict"
	case errors.Is(err, errNoRecips):
		return "no_recipients"
	case errors.Is(err, errBusy):
		return "not_ready"
	case errors.As(err, &pe) && pe.Code == "P0002": // no_data_found from a definer
		return "not_found"
	case errors.As(err, &pe) && pe.Code == "23514": // check_violation (A15's trigger)
		return "validation_failed"
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
		attrs = append(attrs, "sqlstate", pe.Code, "constraint", pe.ConstraintName)
	}
	if code == "internal_error" {
		d.log().Error("alerts: "+what+" failed", attrs...)
	} else {
		d.log().Info("alerts: "+what+" refused", attrs...)
	}
	httpx.Error(w, r, code)
}

// tx runs fn as the caller's hospital and user.
func (d Deps) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	if d.Pool == nil {
		return errBusy
	}
	return authz.TxUser(ctx, d.Pool, fn)
}

// retryAttempts bounds retryTx.
const retryAttempts = 3

// retryTx is tx, retried when the database chose this transaction as a
// deadlock victim (40P01) or a serialization failure (40001): the merge
// and the exclusion take the emr service's link locks and the patient rows,
// so one racing a normaliser batch on the same patients can lose (TODO.md,
// PR 5 follow-ups). Nothing is left half-done by a rollback. After the
// last attempt the caller answers 503 not_ready (errBusy).
func (d Deps) retryTx(ctx context.Context, fn func(pgx.Tx) error) error {
	var err error
	for i := 0; i < retryAttempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return errBusy
			case <-time.After(time.Duration(i*50) * time.Millisecond):
			}
		}
		err = d.tx(ctx, fn)
		if !retryable(err) {
			return err
		}
	}
	return errBusy
}

func retryable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && (pe.Code == "40P01" || pe.Code == "40001")
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
