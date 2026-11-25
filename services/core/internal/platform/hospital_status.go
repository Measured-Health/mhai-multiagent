package platform

// Deactivating and reactivating a hospital from the platform app (mhai-web's
// Deactivate and Reactivate buttons; migration 00060):
//
//   POST /platform/hospitals/{id}/deactivate {reason, note?, mfa_code |
//   backup_code} → 200 and the hospital's page.
//   POST /platform/hospitals/{id}/reactivate {reason, mfa_code |
//   backup_code} → 200 and the hospital's page.
//
// Platform admins only (auditors 403), on a two-step session, with a
// step-up code checked as the other platform step-up writes check it
// (auth.Service.StepUpAudited). Idempotency-Key as every platform write.
//
// Deactivate: reason is one of DeactivateReasons; note is optional text (1
// to 500 characters once trimmed, newlines allowed, no other control
// character, nothing of a pseudonym's shape), blank or null meaning none,
// and required for "other" (mhai-web's sample API's rules). Allowed from
// active and setup_incomplete; any other status is 409 conflict, an
// unknown id 404, both before a code is spent. In one transaction: the
// status deactivated and the version up by one; the deactivation record
// (who, when, the reason and note, the status left); every open session of
// the hospital's accounts revoked (put on the denylist after commit, so
// they stop at once, not at the next refresh); and hospital.deactivated
// {hospital_id, reason, from, sessions_revoked}, never the note. 00009's
// trigger cancels the hospital's queued and running questions.
//
// Reactivate: reason is required text, as the note. Allowed from
// deactivated with an open record (a hospital deactivated by hand has none
// and is 409 conflict); the status goes back to the one the record left
// (active or setup_incomplete), the version up by one, the record closed
// (who, when, why) and kept, and hospital.reactivated {hospital_id, to},
// never the reason. Staff sign in again themselves.
//
// Logs carry ids and codes only: never a note or reason.

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

const (
	// HospitalStatusChangesPerMinute bounds a session's deactivations and
	// reactivations together.
	HospitalStatusChangesPerMinute = 10
	hospitalStatusScope            = "platform.hospital.status_rate"
)

// DeactivateReasons are mhai-web's DeactivateReason codes.
var DeactivateReasons = []string{"contract_ended", "security_concern", "hospital_request", "other"}

// DeactivateReasonWords are the reasons as mhai-web's sample API words them
// in the activity feed (its REASON_WORDS).
var DeactivateReasonWords = map[string]string{
	"contract_ended":   "Contract ended",
	"security_concern": "Security concern",
	"hospital_request": "At the hospital's request",
	"other":            "Other",
}

// Deactivation is a hospital's open deactivation record.
type Deactivation struct {
	By     Person  `json:"by"`
	At     string  `json:"at"`
	Reason string  `json:"reason"`
	Note   *string `json:"note"`
}

// deactivateIn is the deactivation's body.
type deactivateIn struct {
	Reason     *string `json:"reason"`
	Note       *string `json:"note"`
	MFACode    string  `json:"mfa_code"`
	BackupCode string  `json:"backup_code"`
}

// reactivateIn is the reactivation's body.
type reactivateIn struct {
	Reason     *string `json:"reason"`
	MFACode    string  `json:"mfa_code"`
	BackupCode string  `json:"backup_code"`
}

// StatusText trims s; ok is false unless what is left is a note or reason
// the routes take (cancelNoteOK: 1 to 500 characters, newlines allowed, no
// other control character, nothing of a pseudonym's shape).
func StatusText(s string) (string, bool) {
	t := strings.TrimSpace(s)
	return t, cancelNoteOK(t)
}

// CheckDeactivation checks a deactivation's reason and note: the note
// trimmed, nil when absent or blank, required for "other".
func CheckDeactivation(reason string, note *string) (*string, bool) {
	if !slices.Contains(DeactivateReasons, reason) {
		return nil, false
	}
	var out *string
	if note != nil && strings.TrimSpace(*note) != "" {
		t, ok := StatusText(*note)
		if !ok {
			return nil, false
		}
		out = &t
	}
	if reason == "other" && out == nil {
		return nil, false
	}
	return out, true
}

var errWrongState = errors.New("platform: hospital in the wrong state")

// hospitalState is hid's status and its open record's status left (nil
// when it has none), or errNoHospital.
func hospitalState(ctx context.Context, tx pgx.Tx, hid uuid.UUID) (string, *string, error) {
	var status string
	var from *string
	err := tx.QueryRow(ctx, `SELECT h.status, (SELECT d.from_status FROM hospital_deactivations d
			WHERE d.hospital_id = h.id AND d.reactivated_at IS NULL)
		FROM hospitals h WHERE h.id = $1`, hid).Scan(&status, &from)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, errNoHospital
	}
	return status, from, err
}

func canDeactivate(status string) bool { return status == "active" || status == "setup_incomplete" }

// lockHospital locks hid's row for a status change and returns its status:
// errNoHospital for no hospital, errWrongState for a removed one (00059's
// edit policy hides it from FOR UPDATE).
func lockHospital(ctx context.Context, tx pgx.Tx, hid uuid.UUID) (string, error) {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM hospitals WHERE id = $1 FOR UPDATE`, hid).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		exists, err := hospitalExists(ctx, tx, hid)
		switch {
		case err != nil:
			return "", err
		case exists:
			return "", errWrongState
		}
		return "", errNoHospital
	}
	return status, err
}

// revokeHospitalSessions revokes every open session of hid's accounts and
// returns them with their access token ids, for the denylist after commit.
func revokeHospitalSessions(ctx context.Context, tx pgx.Tx, hid uuid.UUID) (map[string][]string, error) {
	rows, err := tx.Query(ctx, `UPDATE sessions SET revoked_at = now()
		WHERE revoked_at IS NULL AND user_id IN (SELECT u.id FROM users u WHERE u.hospital_id = $1)
		RETURNING id, access_jti`, hid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var sid uuid.UUID
		var jti *uuid.UUID
		if err := rows.Scan(&sid, &jti); err != nil {
			return nil, err
		}
		var js []string
		if jti != nil {
			js = []string{jti.String()}
		}
		out[sid.String()] = js
	}
	return out, rows.Err()
}

// deactivateHospital deactivates hid in tx and reads the page back.
func deactivateHospital(ctx context.Context, tx pgx.Tx, hid, by uuid.UUID, reason string, note *string, rid string) (Hospital, map[string][]string, error) {
	status, err := lockHospital(ctx, tx, hid)
	if err != nil {
		return Hospital{}, nil, err
	}
	if !canDeactivate(status) {
		return Hospital{}, nil, errWrongState
	}
	if _, err := tx.Exec(ctx, `UPDATE hospitals SET status = 'deactivated', version = version + 1 WHERE id = $1`, hid); err != nil {
		return Hospital{}, nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO hospital_deactivations (hospital_id, from_status, reason, note, deactivated_by)
		VALUES ($1, $2, $3, $4, $5)`, hid, status, reason, note, by); err != nil {
		return Hospital{}, nil, err
	}
	revoked, err := revokeHospitalSessions(ctx, tx, hid)
	if err != nil {
		return Hospital{}, nil, err
	}
	// Codes and a count only: never the note.
	if err := audit.Write(ctx, tx, audit.Event{ActorID: by.String(), RequestID: rid,
		Action: "hospital.deactivated", Entity: "hospital", EntityID: hid.String(),
		Payload: map[string]any{"hospital_id": hid.String(), "reason": reason, "from": status, "sessions_revoked": len(revoked)}}); err != nil {
		return Hospital{}, nil, err
	}
	out, err := readHospital(ctx, tx, hid)
	return out, revoked, err
}

// reactivateHospital puts hid back as its open record left it, in tx, and
// reads the page back.
func reactivateHospital(ctx context.Context, tx pgx.Tx, hid, by uuid.UUID, reason, rid string) (Hospital, error) {
	status, err := lockHospital(ctx, tx, hid)
	if err != nil {
		return Hospital{}, err
	}
	if status != "deactivated" {
		return Hospital{}, errWrongState
	}
	var rec uuid.UUID
	var to string
	err = tx.QueryRow(ctx, `SELECT id, from_status FROM hospital_deactivations
		WHERE hospital_id = $1 AND reactivated_at IS NULL FOR UPDATE`, hid).Scan(&rec, &to)
	if errors.Is(err, pgx.ErrNoRows) {
		return Hospital{}, errWrongState
	}
	if err != nil {
		return Hospital{}, err
	}
	// The status first: 00060's guard lets it back only while the record
	// is open, and closes the record only once it is back.
	if _, err := tx.Exec(ctx, `UPDATE hospitals SET status = $2, version = version + 1 WHERE id = $1`, hid, to); err != nil {
		return Hospital{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE hospital_deactivations SET reactivated_by = $2, reactivated_at = now(), reactivation_reason = $3
		WHERE id = $1`, rec, by, reason); err != nil {
		return Hospital{}, err
	}
	if err := audit.Write(ctx, tx, audit.Event{ActorID: by.String(), RequestID: rid,
		Action: "hospital.reactivated", Entity: "hospital", EntityID: hid.String(),
		Payload: map[string]any{"hospital_id": hid.String(), "to": to}}); err != nil {
		return Hospital{}, err
	}
	return readHospital(ctx, tx, hid)
}

// readDeactivation is hid's open deactivation record, or nil.
func readDeactivation(ctx context.Context, tx pgx.Tx, hid uuid.UUID) (*Deactivation, error) {
	var d Deactivation
	var by uuid.UUID
	var at time.Time
	err := tx.QueryRow(ctx, `SELECT d.deactivated_by, COALESCE(u.display_name, ''), d.deactivated_at, d.reason, d.note
		FROM hospital_deactivations d LEFT JOIN users u ON u.id = d.deactivated_by
		WHERE d.hospital_id = $1 AND d.reactivated_at IS NULL`, hid).Scan(&by, &d.By.Name, &at, &d.Reason, &d.Note)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.By.ID, d.At = by.String(), stamp(at)
	return &d, nil
}

// statusChange runs a deactivation or reactivation once its body is
// checked. Order: the hospital and its state (404, 409; before a code is
// spent), the step-up check's availability (503), the session's minute
// (429), the code (403 mfa_required with none; mfa_invalid, audited
// auth.mfa.failed; 429 mfa_locked, audited), then the write, checking the
// state again under the row lock.
func (d Deps) statusChange(w http.ResponseWriter, r *http.Request, p authz.Principal, hid uuid.UUID, what string,
	allowed func(status string, from *string) bool, mfa, backup string,
	write func(ctx context.Context, tx pgx.Tx) (Hospital, map[string][]string, error)) {
	ctx := r.Context()
	rid := httpx.RequestIDFrom(ctx)
	err := d.tx(ctx, func(tx pgx.Tx) error {
		status, from, err := hospitalState(ctx, tx, hid)
		if err != nil {
			return err
		}
		if !allowed(status, from) {
			return errWrongState
		}
		return nil
	})
	switch {
	case errors.Is(err, errNoHospital):
		httpx.Error(w, r, "not_found")
		return
	case errors.Is(err, errWrongState):
		httpx.Error(w, r, "conflict")
		return
	case err != nil:
		d.log().Error("platform: hospital "+what+" lookup failed", "request_id", rid, "user_id", p.UserID.String(), "reason", "internal_error")
		httpx.Error(w, r, "internal_error")
		return
	}
	if d.Auth == nil {
		d.log().Error("platform: hospital "+what+" refused: no step-up", "request_id", rid, "reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return
	}
	ok, err := d.reserveRate(r, p, hospitalStatusScope, "platform.hospital.status.v1", HospitalStatusChangesPerMinute)
	switch {
	case err != nil:
		d.log().Error("platform: hospital "+what+" refused: rate counter unavailable", "request_id", rid, "reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return
	case !ok:
		httpx.Error(w, r, "rate_limited")
		return
	}
	if mfa == "" && backup == "" {
		httpx.Error(w, r, "mfa_required")
		return
	}
	// The code is spent here: an Idempotency-Key retry is answered from the
	// stored reply, never checked again.
	if err := d.Auth.StepUpAudited(r, mfa, backup); err != nil {
		d.Auth.WriteStepUpError(w, r, err)
		return
	}
	var out Hospital
	var revoked map[string][]string
	err = d.tx(ctx, func(tx pgx.Tx) error {
		var err error
		out, revoked, err = write(ctx, tx)
		return err
	})
	switch {
	case errors.Is(err, errNoHospital):
		httpx.Error(w, r, "not_found")
		return
	case errors.Is(err, errWrongState):
		httpx.Error(w, r, "conflict")
		return
	case err != nil:
		d.log().Error("platform: hospital "+what+" failed", "request_id", rid, "user_id", p.UserID.String(),
			"hospital_id", hid.String(), "reason", "internal_error")
		httpx.Error(w, r, "internal_error")
		return
	}
	d.denyAfterCommit(r, revoked)
	httpx.JSON(w, http.StatusOK, out)
}

// hospitalDeactivate is POST /platform/hospitals/{id}/deactivate.
func (d Deps) hospitalDeactivate(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	hid, ok := pagePath(w, r)
	if !ok {
		return
	}
	var in deactivateIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Reason == nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	reason := *in.Reason
	note, ok := CheckDeactivation(reason, in.Note)
	if !ok {
		httpx.Error(w, r, "validation_failed")
		return
	}
	d.statusChange(w, r, p, hid, "deactivate",
		func(status string, _ *string) bool { return canDeactivate(status) }, in.MFACode, in.BackupCode,
		func(ctx context.Context, tx pgx.Tx) (Hospital, map[string][]string, error) {
			return deactivateHospital(ctx, tx, hid, p.UserID, reason, note, httpx.RequestIDFrom(ctx))
		})
}

// hospitalReactivate is POST /platform/hospitals/{id}/reactivate.
func (d Deps) hospitalReactivate(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	hid, ok := pagePath(w, r)
	if !ok {
		return
	}
	var in reactivateIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Reason == nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	reason, ok := StatusText(*in.Reason)
	if !ok {
		httpx.Error(w, r, "validation_failed")
		return
	}
	d.statusChange(w, r, p, hid, "reactivate",
		func(status string, from *string) bool { return status == "deactivated" && from != nil }, in.MFACode, in.BackupCode,
		func(ctx context.Context, tx pgx.Tx) (Hospital, map[string][]string, error) {
			out, err := reactivateHospital(ctx, tx, hid, p.UserID, reason, httpx.RequestIDFrom(ctx))
			return out, nil, err
		})
}
