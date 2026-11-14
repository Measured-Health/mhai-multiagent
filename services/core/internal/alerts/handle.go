package alerts

// POST /alerts/{id}/acknowledge and /dismiss {reason, note?} (design §12.5;
// plan Task 33), and GET and PATCH /me/notification-settings. The first
// handler wins at the database (UPDATE … WHERE status = 'new', and 00029's
// restrictive policy); a second gets 409 already_handled with who and when.
// An alert the caller doesn't see, or another hospital's, is 404. The
// dismissal note is stored on the alert only: never logged, audited or
// traced.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// DismissReasons are the closed dismissal reasons.
var DismissReasons = []string{"not_relevant", "already_known", "wrong_patient", "other"}

// maxNote bounds the dismissal note (characters).
const maxNote = 500

type dismissIn struct {
	Reason *string `json:"reason"`
	Note   *string `json:"note"`
}

// errHandled: someone handled the alert first; the reply names who.
type errHandled struct {
	status string
	by     *string
	at     *time.Time
}

func (e errHandled) Error() string { return "alerts: already handled" }

func (d Deps) handle(kind string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := caller(w, r)
		if !ok {
			return
		}
		id, ok := canonicalID(r.PathValue("id"))
		if !ok || r.URL.RawQuery != "" {
			httpx.Error(w, r, "not_found")
			return
		}
		status, reason := "handled", ""
		var note *string
		if kind == "dismiss" {
			var in dismissIn
			if !httpx.Decode(w, r, &in) {
				return
			}
			if in.Reason == nil || !slices.Contains(DismissReasons, *in.Reason) {
				httpx.Error(w, r, "validation_failed")
				return
			}
			if in.Note != nil {
				n := strings.TrimSpace(*in.Note)
				if !utf8.ValidString(n) || utf8.RuneCountInString(n) > maxNote {
					httpx.Error(w, r, "validation_failed")
					return
				}
				if n != "" {
					note = &n
				}
			}
			status, reason = "dismissed", *in.Reason
		} else if !emptyBody(r) {
			httpx.Error(w, r, "validation_failed")
			return
		}
		var out AlertOut
		err := d.tx(r.Context(), func(tx pgx.Tx) error {
			ctx := r.Context()
			var rs any
			if reason != "" {
				rs = reason
			}
			tag, err := tx.Exec(ctx, `UPDATE alerts a SET status = $3, handled_by = $2, handled_at = now(),
				dismiss_reason = $4, dismiss_note = $5, updated_at = now() WHERE `+visible+` AND a.id = $6 AND a.status = 'new'`,
				p.HospitalID, p.UserID, status, rs, note, id)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				var h errHandled
				err := tx.QueryRow(ctx, `SELECT a.status, u.display_name, a.handled_at FROM alerts a
					LEFT JOIN users u ON u.id = a.handled_by AND u.hospital_id = a.hospital_id
					WHERE `+visible+` AND a.id = $3`, p.HospitalID, p.UserID, id).Scan(&h.status, &h.by, &h.at)
				if errors.Is(err, pgx.ErrNoRows) {
					return errNotFound
				}
				if err != nil {
					return err
				}
				return h
			}
			e := audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
				RequestID: httpx.RequestIDFrom(ctx), Action: "alert.acknowledged", Entity: "alert", EntityID: id.String(),
				Payload: map[string]any{"alert_id": id.String()}}
			if kind == "dismiss" {
				e.Action = "alert.dismissed"
				e.Payload["reason"] = reason
			}
			if err := audit.Write(ctx, tx, e); err != nil {
				return err
			}
			out, err = loadAlert(ctx, tx, p.HospitalID, p.UserID, id, d.now())
			return err
		})
		var h errHandled
		switch {
		case errors.As(err, &h):
			var by any
			if h.by != nil {
				by = map[string]any{"display_name": *h.by}
			}
			httpx.ErrorFields(w, r, "already_handled", map[string]any{"status": h.status, "handled_by": by,
				"handled_at": stampPtr(h.at)})
		case err != nil:
			d.fail(w, r, p, kind, err)
		default:
			httpx.JSON(w, http.StatusOK, out)
		}
	})
}

// emptyBody reports whether the request has no body, or {}.
func emptyBody(r *http.Request) bool {
	if r.Body == nil {
		return true
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, httpx.MaxBody+1))
	if err != nil || len(b) > httpx.MaxBody {
		return false
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return true
	}
	var m map[string]json.RawMessage
	return json.Unmarshal(b, &m) == nil && m != nil && len(m) == 0
}

// NotifyOut is the caller's notification switches.
type NotifyOut struct {
	AlertPopups      bool `json:"alert_popups"`
	CorrectionPopups bool `json:"correction_popups"`
}

type notifyPatch struct {
	AlertPopups      *bool `json:"alert_popups"`
	CorrectionPopups *bool `json:"correction_popups"`
}

func loadNotify(r *http.Request, tx pgx.Tx, me uuid.UUID) (NotifyOut, error) {
	out := NotifyOut{AlertPopups: true, CorrectionPopups: true}
	err := tx.QueryRow(r.Context(), `SELECT alert_popups, correction_popups FROM user_notification_settings
		WHERE user_id = $1`, me).Scan(&out.AlertPopups, &out.CorrectionPopups)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return out, err
}

// notifyGet is GET /me/notification-settings (on by default).
func (d Deps) notifyGet(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var out NotifyOut
	if err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		out, err = loadNotify(r, tx, p.UserID)
		return err
	}); err != nil {
		d.fail(w, r, p, "notification settings read", err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// notifyPatch is PATCH /me/notification-settings: one switch or both;
// audited with the new values. Critical alerts pop up whatever the switch
// (design §12.3): it mutes pop-ups only.
func (d Deps) notifyPatch(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	var in notifyPatch
	if !httpx.Decode(w, r, &in) {
		return
	}
	if r.URL.RawQuery != "" || (in.AlertPopups == nil && in.CorrectionPopups == nil) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var out NotifyOut
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		ctx := r.Context()
		cur, err := loadNotify(r, tx, p.UserID)
		if err != nil {
			return err
		}
		if in.AlertPopups != nil {
			cur.AlertPopups = *in.AlertPopups
		}
		if in.CorrectionPopups != nil {
			cur.CorrectionPopups = *in.CorrectionPopups
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_notification_settings (user_id, hospital_id, alert_popups, correction_popups, updated_at)
			VALUES ($1, $2, $3, $4, now()) ON CONFLICT (user_id) DO UPDATE SET alert_popups = EXCLUDED.alert_popups,
			correction_popups = EXCLUDED.correction_popups, updated_at = EXCLUDED.updated_at`,
			p.UserID, p.HospitalID, cur.AlertPopups, cur.CorrectionPopups); err != nil {
			return err
		}
		out = cur
		return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Action: "notification_settings.changed", Entity: "user",
			EntityID: p.UserID.String(),
			Payload:  map[string]any{"alert_popups": cur.AlertPopups, "correction_popups": cur.CorrectionPopups}})
	})
	if err != nil {
		d.fail(w, r, p, "notification settings save", err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}
