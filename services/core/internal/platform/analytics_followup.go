package platform

// Follow-ups on What changed (Plan 7c spec §7.4, §10; A15, A18–A22).
//
//   - PUT /platform/analytics/changes/{id}/follow-up {note?, version}:
//     marks an open change followed up, or edits the note of the caller's
//     own follow-up. version must be the change's current version.
//   - DELETE /platform/analytics/changes/{id}/follow-up {version}: undoes
//     the follow-up, whoever wrote it.
//
// Platform admins only (auditors 403, hospital audiences not_platform), with
// Idempotency-Key as every core write; no step-up (a follow-up is a note
// between staff and changes no figure). Each write is one transaction as
// mhai_platform: a conditional UPDATE of the change's version (WHERE
// version = $v on an open change, so of two writes with one version one
// wins and the other is 409 conflict), the follow-up row, and the audit row
// (analytics.followed_up, analytics.follow_up_edited,
// analytics.follow_up_undone: the change id, and whether there is a note,
// never the note). The note stays in analytics_follow_ups: it is in no log
// line, error, span or audit payload; the reply (the change, as the changes
// list sends it) carries it to the caller.
//
// Ten writes a minute per session, PUT and DELETE together (429
// rate_limited), counted after the body is checked.
//
// Order: the path (404), the body (422), the session's minute (429), then in
// the transaction the change
// (404 unknown; 409 closed or a stale version), then the follow-up (403 when
// another admin's is in place and this is a PUT; 409 when a DELETE finds
// none).

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

const (
	// FollowUpNoteMax is a follow-up note's length in characters (as the
	// cancel note's).
	FollowUpNoteMax = CancelNoteMax
	// FollowUpWritesPerMinute is the follow-up writes (PUT and DELETE
	// together) one session may make a minute, as the job retries'.
	FollowUpWritesPerMinute = 10
	// followUpScope is the idempotency_keys scope of the write counter (as
	// retryScope).
	followUpScope = "platform.analytics.follow_up_rate"
)

// followUpRate counts one follow-up write of the session; false when the
// answer (429 rate_limited or 503 not_ready) is already written.
func (d Deps) followUpRate(w http.ResponseWriter, r *http.Request, p authz.Principal) bool {
	allowed, err := d.reserveRate(r, p, followUpScope, "platform.analytics.follow_up.v1", FollowUpWritesPerMinute)
	switch {
	case err != nil:
		d.log().Error("platform: follow-up refused: rate counter unavailable", "request_id", httpx.RequestIDFrom(r.Context()),
			"reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return false
	case !allowed:
		httpx.Error(w, r, "rate_limited")
		return false
	}
	return true
}

// The follow-up refusals, each answered with its code.
var (
	errNoChange     = errors.New("platform: no such change")      // not_found
	errChangeMoved  = errors.New("platform: change conflict")     // conflict
	errNotTheAuthor = errors.New("platform: another's follow-up") // forbidden
)

func registerFollowUps(m *authz.Mux, d Deps) {
	write := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin}, Write: true}
	authz.Route(m, http.MethodPut, "/platform/analytics/changes/{id}/follow-up", write, http.HandlerFunc(d.followUpPut))
	authz.Route(m, http.MethodDelete, "/platform/analytics/changes/{id}/follow-up", write, http.HandlerFunc(d.followUpDelete))
}

// followUpNoteOK is the note's rule (R11, as the cancel note): 1 to 500
// characters, newlines allowed, no other control or format character, no
// line or paragraph separator, nothing of a pseudonym's shape.
func followUpNoteOK(s string) bool { return cancelNoteOK(s) }

// followUpBody is the PUT's body; DELETE's has version only.
type followUpBody struct {
	Note    optString `json:"note"`
	Version *int      `json:"version"`
}

// pathChange is the path's change id: a canonical uuid, or false (404).
func pathChange(r *http.Request) (uuid.UUID, bool) {
	s := r.PathValue("id")
	if !canonicalUUID(s) {
		return uuid.Nil, false
	}
	return uuid.MustParse(s), true
}

// takeVersion moves an open change's version on from v, after checking
// that it exists: errNoChange, or errChangeMoved when it is closed or v is
// stale. The row stays locked until commit.
func takeVersion(ctx context.Context, tx pgx.Tx, id uuid.UUID, v int) error {
	var open bool
	err := tx.QueryRow(ctx, `SELECT closed_week IS NULL FROM analytics_changes WHERE id = $1`, id).Scan(&open)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNoChange
	}
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE analytics_changes SET version = version + 1
		WHERE id = $1 AND version = $2 AND closed_week IS NULL`, id, v)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errChangeMoved
	}
	return nil
}

// followUpOf is the change's follow-up: author and version, if any.
func followUpOf(ctx context.Context, tx pgx.Tx, id uuid.UUID) (by uuid.UUID, version int, ok bool, err error) {
	err = tx.QueryRow(ctx, `SELECT by_user, version FROM analytics_follow_ups WHERE change_id = $1 FOR UPDATE`, id).Scan(&by, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, 0, false, nil
	}
	return by, version, err == nil, err
}

// readChange is the change as the caller sees it.
func readChange(ctx context.Context, tx pgx.Tx, id uuid.UUID, caller uuid.UUID) (analytics.ChangeOut, error) {
	row, err := scanChange(tx.QueryRow(ctx, changeSelect+` WHERE c.id = $1`, id))
	if err != nil {
		return analytics.ChangeOut{}, err
	}
	return analytics.ChangeOutOf(row, caller.String())
}

// followUpAudit appends the write's audit row (ids, and whether there is a
// note) in the write's transaction.
func (d Deps) followUpAudit(ctx context.Context, tx pgx.Tx, p authz.Principal, rid, action string, id uuid.UUID, payload map[string]any) error {
	payload["change_id"] = id.String()
	if err := audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), RequestID: rid, Action: action,
		Entity: "analytics_change", EntityID: id.String(), Payload: payload}); err != nil {
		return err
	}
	if d.AfterFollowUpAudit != nil {
		return d.AfterFollowUpAudit()
	}
	return nil
}

// followUpAnswer writes the outcome of a follow-up write.
func (d Deps) followUpAnswer(w http.ResponseWriter, r *http.Request, p authz.Principal, out analytics.ChangeOut, err error) {
	switch {
	case errors.Is(err, errNoChange):
		httpx.Error(w, r, "not_found")
	case errors.Is(err, errChangeMoved):
		httpx.Error(w, r, "conflict")
	case errors.Is(err, errNotTheAuthor):
		httpx.Error(w, r, "forbidden")
	case err != nil:
		// Ids and a fixed reason; never the note.
		d.log().Error("platform: follow-up write failed", "request_id", httpx.RequestIDFrom(r.Context()),
			"user_id", p.UserID.String(), "reason", "internal_error")
		httpx.Error(w, r, "internal_error")
	default:
		httpx.JSON(w, http.StatusOK, out)
	}
}

// followUpPut is PUT /platform/analytics/changes/{id}/follow-up.
func (d Deps) followUpPut(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathChange(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var body followUpBody
	if !httpx.Decode(w, r, &body) {
		return
	}
	if body.Version == nil || *body.Version < 1 || (body.Note.v != nil && !followUpNoteOK(*body.Note.v)) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !d.followUpRate(w, r, p) {
		return
	}
	note := body.Note.v
	rid := httpx.RequestIDFrom(r.Context())
	var out analytics.ChangeOut
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		ctx := r.Context()
		if err := takeVersion(ctx, tx, id, *body.Version); err != nil {
			return err
		}
		by, _, has, err := followUpOf(ctx, tx, id)
		if err != nil {
			return err
		}
		switch {
		case has && by != p.UserID:
			// They undo first (§7.4).
			return errNotTheAuthor
		case has:
			if _, err := tx.Exec(ctx, `UPDATE analytics_follow_ups SET note = $2, version = version + 1 WHERE change_id = $1`,
				id, note); err != nil {
				return err
			}
			if err := d.followUpAudit(ctx, tx, p, rid, "analytics.follow_up_edited", id, map[string]any{}); err != nil {
				return err
			}
		default:
			if _, err := tx.Exec(ctx, `INSERT INTO analytics_follow_ups (change_id, by_user, note) VALUES ($1, $2, $3)`,
				id, p.UserID, note); err != nil {
				return err
			}
			if err := d.followUpAudit(ctx, tx, p, rid, "analytics.followed_up", id, map[string]any{"has_note": note != nil}); err != nil {
				return err
			}
		}
		var err2 error
		out, err2 = readChange(ctx, tx, id, p.UserID)
		return err2
	})
	d.followUpAnswer(w, r, p, out, err)
}

// followUpDelete is DELETE /platform/analytics/changes/{id}/follow-up.
func (d Deps) followUpDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathChange(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var body struct {
		Version *int `json:"version"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	if body.Version == nil || *body.Version < 1 {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !d.followUpRate(w, r, p) {
		return
	}
	rid := httpx.RequestIDFrom(r.Context())
	var out analytics.ChangeOut
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		ctx := r.Context()
		if err := takeVersion(ctx, tx, id, *body.Version); err != nil {
			return err
		}
		_, _, has, err := followUpOf(ctx, tx, id)
		if err != nil {
			return err
		}
		if !has {
			return errChangeMoved // nothing to undo
		}
		if _, err := tx.Exec(ctx, `DELETE FROM analytics_follow_ups WHERE change_id = $1`, id); err != nil {
			return err
		}
		if err := d.followUpAudit(ctx, tx, p, rid, "analytics.follow_up_undone", id, map[string]any{}); err != nil {
			return err
		}
		var err2 error
		out, err2 = readChange(ctx, tx, id, p.UserID)
		return err2
	})
	d.followUpAnswer(w, r, p, out, err)
}
