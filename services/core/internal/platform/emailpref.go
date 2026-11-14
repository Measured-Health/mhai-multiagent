package platform

// The analytics e-mail setting (mhai-web Plan 7d; analytics design §7):
//
//   - GET /platform/me/analytics-email: platform admins (an auditor is
//     403: e-mail is for platform admins only); the signed-in person's
//     mode, off by default, and its version.
//   - PUT /platform/me/analytics-email {mode, version}: platform admins;
//     409 conflict on a stale version. Audited
//     platform.analytics_email_changed.
//
// The person is the token's, never the body's. Core stores the choice; it
// sends no e-mail yet (no mail service): a gap recorded in the PR.
//
// The notifications bell (GET /platform/notifications, POST
// /platform/notifications/seen), the platform settings (Plan 8c) and the
// named-access switch (Plan 11b) have no backing feature in core: they are
// registered with their rules and answer 501 not_implemented.

import (
	"errors"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// EmailModes are platform_email_prefs.mode's values.
var EmailModes = []string{"off", "critical", "critical_and_digest"}

func registerEmailPref(m *authz.Mux, d Deps) {
	admin := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin}}
	write := admin
	write.Write = true
	authz.Route(m, http.MethodGet, "/platform/me/analytics-email", admin, http.HandlerFunc(d.emailGet))
	authz.Route(m, http.MethodPut, "/platform/me/analytics-email", write, http.HandlerFunc(d.emailPut))
}

// registerNotImplemented registers the routes core has no feature for.
func registerNotImplemented(m *authz.Mux) {
	read := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: TeamRoles}
	write := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin}, Write: true}
	stepUp := write
	stepUp.StepUp = true
	h := http.HandlerFunc(notImplemented)
	for _, r := range []struct {
		method, path string
		rule         authz.Rule
	}{
		{http.MethodGet, "/platform/notifications", read},
		{http.MethodPost, "/platform/notifications/seen", write},
		{http.MethodGet, "/platform/settings", read},
		{http.MethodGet, "/platform/settings/retention/{id}/preview", read},
		{http.MethodPut, "/platform/settings/retention/{id}", stepUp},
		{http.MethodPut, "/platform/settings/retention-pause", stepUp},
		{http.MethodPut, "/platform/settings/triage", stepUp},
		{http.MethodPost, "/platform/settings/requests/{id}/approve", stepUp},
		{http.MethodPost, "/platform/settings/requests/{id}/cancel", write},
		{http.MethodPost, "/platform/settings/feedback-buffer/reset", stepUp},
		{http.MethodGet, "/platform/named-access", read},
		{http.MethodPost, "/platform/named-access/requests", stepUp},
		{http.MethodPost, "/platform/named-access/requests/{id}/approve", stepUp},
		{http.MethodPost, "/platform/named-access/requests/{id}/cancel", write},
		{http.MethodPost, "/platform/named-access/lower", stepUp},
	} {
		authz.Route(m, r.method, r.path, r.rule, h)
	}
}

// EmailPref is a person's analytics e-mail mode.
type EmailPref struct {
	Mode    string `json:"mode"`
	Version int    `json:"version"`
}

func readEmailPref(r *http.Request, tx pgx.Tx, p authz.Principal) (EmailPref, error) {
	out := EmailPref{Mode: "off", Version: 1}
	err := tx.QueryRow(r.Context(), `SELECT mode, version FROM platform_email_prefs WHERE user_id = $1`, p.UserID).
		Scan(&out.Mode, &out.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	return out, err
}

func (d Deps) emailGet(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var out EmailPref
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		out, err = readEmailPref(r, tx, p)
		return err
	})
	if err != nil {
		d.fail(w, r, p, "analytics email read")
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (d Deps) emailPut(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var b struct {
		Mode    string `json:"mode"`
		Version *int   `json:"version"`
	}
	if !httpx.Decode(w, r, &b) {
		return
	}
	if !slices.Contains(EmailModes, b.Mode) || b.Version == nil || *b.Version < 1 {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var out EmailPref
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtext('mhai.platform_email'), hashtext($1::text))`,
			p.UserID); err != nil {
			return err
		}
		cur, err := readEmailPref(r, tx, p)
		if err != nil {
			return err
		}
		if cur.Version != *b.Version {
			return errStale
		}
		if cur.Mode == b.Mode {
			out = cur
			return nil
		}
		if _, err := tx.Exec(r.Context(), `INSERT INTO platform_email_prefs (user_id, mode, version) VALUES ($1, $2, $3)
			ON CONFLICT (user_id) DO UPDATE SET mode = EXCLUDED.mode, version = EXCLUDED.version`,
			p.UserID, b.Mode, cur.Version+1); err != nil {
			return err
		}
		if err := audit.Write(r.Context(), tx, audit.Event{ActorID: p.UserID.String(), RequestID: httpx.RequestIDFrom(r.Context()),
			Action: "platform.analytics_email_changed", Entity: "user", EntityID: p.UserID.String(),
			Payload: map[string]any{"mode": b.Mode, "previous": cur.Mode}}); err != nil {
			return err
		}
		out = EmailPref{Mode: b.Mode, Version: cur.Version + 1}
		return nil
	})
	switch {
	case errors.Is(err, errStale):
		httpx.Error(w, r, "conflict")
	case err != nil:
		d.fail(w, r, p, "analytics email write")
	default:
		httpx.JSON(w, http.StatusOK, out)
	}
}
