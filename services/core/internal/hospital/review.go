package hospital

// Reviewer assignment and reminders (frontend requirements §5.4):
//
//   - POST /hospital/corrections/{id}/assign {staff_id}: the waiting
//     correction's reviewer, an active clinician of the hospital other
//     than its submitter (else 422 not_eligible), or null to clear it.
//     Assigning someone sends them a notice (no_delivery without a
//     sender). Assignment points a reviewer at it; any other eligible
//     clinician may still decide it (the two-person rule is unchanged).
//     Migration 00049's trigger holds the same rules in the database.
//   - POST /hospital/corrections/{id}/remind: a reminder to the assigned
//     reviewer, or to every eligible reviewer when none is (or the
//     assignee no longer is); 422 no_recipients when there is nobody; at
//     most once an hour per correction (rate_limited).
//
// Both lock the correction's row, so two admins' requests serialise, and
// answer 404 for an unknown or another hospital's correction and 409
// already_decided once it is no longer waiting. No clinical content is
// read, logged or audited: ids and counts only.

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/delivery"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// waitingCorrection locks a correction of hid for an admin's change and
// returns its submitter and assignee.
func waitingCorrection(ctx context.Context, tx pgx.Tx, hid, id uuid.UUID) (by uuid.UUID, assignee *uuid.UUID, err error) {
	var status string
	err = tx.QueryRow(ctx, `SELECT submitted_by, assigned_to, status FROM feedback_events
		WHERE hospital_id = $1 AND id = $2 AND action = 'correct' FOR UPDATE`, hid, id).Scan(&by, &assignee, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return by, nil, errNotFound
	}
	if err != nil {
		return by, nil, err
	}
	if status != "pending_review" {
		return by, nil, errDecided
	}
	return by, assignee, nil
}

// eligibleReviewer reports whether u may review a correction of hid
// submitted by by: an active clinician of hid other than by who can sign
// in (a password or single sign-on: not an invite still unanswered).
func eligibleReviewer(ctx context.Context, tx pgx.Tx, hid, by, u uuid.UUID) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users c WHERE c.id = $3 AND c.hospital_id = $1 AND c.is_active
		AND (c.password_hash IS NOT NULL OR c.oidc_subject IS NOT NULL) AND c.id <> $2 AND EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = c.id AND r.role = 'clinician'))`, hid, by, u).Scan(&ok)
	return ok, err
}

func (d Deps) auditCorrection(ctx context.Context, tx pgx.Tx, p authz.Principal, id uuid.UUID, action string, payload map[string]any) error {
	return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
		RequestID: httpx.RequestIDFrom(ctx), Action: action, Entity: "correction", EntityID: id.String(), Payload: payload})
}

type assignIn struct {
	StaffID optString `json:"staff_id"`
}

// reviewAssign is POST /hospital/corrections/{id}/assign {staff_id}.
func (d Deps) reviewAssign(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := canonicalID(r.PathValue("id"))
	if !ok || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	var in assignIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	var to *uuid.UUID
	if !in.StaffID.set {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if in.StaffID.val != nil {
		u, ok := canonicalID(*in.StaffID.val)
		if !ok {
			httpx.Error(w, r, "validation_failed")
			return
		}
		to = &u
	}
	ctx := r.Context()
	var msgs []delivery.Message
	err := d.tx(ctx, func(tx pgx.Tx) error {
		by, cur, err := waitingCorrection(ctx, tx, p.HospitalID, id)
		if err != nil {
			return err
		}
		if to != nil {
			ok, err := eligibleReviewer(ctx, tx, p.HospitalID, by, *to)
			if err != nil {
				return err
			}
			if !ok {
				return errNotEnabled
			}
		}
		if eqID(cur, to) {
			return nil // already so: nothing to write or send
		}
		payload := map[string]any{}
		if to != nil {
			if err := d.needSender(); err != nil {
				return err
			}
			payload["assignee"] = to.String()
			m := delivery.Message{Kind: delivery.ReviewAssigned, HospitalID: p.HospitalID, Recipient: *to, Subject: id, By: p.UserID}
			if err := delivery.Record(ctx, tx, d.Sender, m); err != nil {
				return err
			}
			msgs = append(msgs, m)
		}
		if _, err := tx.Exec(ctx, `UPDATE feedback_events SET assigned_to = $2 WHERE id = $1`, id, to); err != nil {
			return err
		}
		return d.auditCorrection(ctx, tx, p, id, "correction.reviewer_assigned", payload)
	})
	if err != nil {
		d.fail(w, r, p, "reviewer assign", err)
		return
	}
	if !d.sendAll(w, r, p, "reviewer assign", msgs) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func eqID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// reviewRemind is POST /hospital/corrections/{id}/remind.
func (d Deps) reviewRemind(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := canonicalID(r.PathValue("id"))
	if !ok || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	var none struct{}
	if !decodeOptional(w, r, &none) {
		return
	}
	ctx := r.Context()
	var msgs []delivery.Message
	err := d.tx(ctx, func(tx pgx.Tx) error {
		by, assignee, err := waitingCorrection(ctx, tx, p.HospitalID, id)
		if err != nil {
			return err
		}
		if err := d.needSender(); err != nil {
			return err
		}
		n, err := messagesWithin(ctx, tx, p.HospitalID, uuid.Nil, id, delivery.ReviewReminder, reviewRemindEvery)
		if err != nil {
			return err
		}
		if n > 0 {
			return errRateLimited
		}
		var to []uuid.UUID
		if assignee != nil {
			ok, err := eligibleReviewer(ctx, tx, p.HospitalID, by, *assignee)
			if err != nil {
				return err
			}
			if ok {
				to = []uuid.UUID{*assignee}
			}
		}
		if to == nil {
			rows, err := tx.Query(ctx, `SELECT u.id FROM users u WHERE u.hospital_id = $1 AND u.is_active AND u.id <> $2
				AND (u.password_hash IS NOT NULL OR u.oidc_subject IS NOT NULL)
				AND EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role = 'clinician')
				ORDER BY u.id LIMIT $3`, p.HospitalID, by, maxReviewers)
			if err != nil {
				return err
			}
			for rows.Next() {
				var u uuid.UUID
				if err := rows.Scan(&u); err != nil {
					rows.Close()
					return err
				}
				to = append(to, u)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		if len(to) == 0 {
			return errNoRecipients
		}
		for _, u := range to {
			m := delivery.Message{Kind: delivery.ReviewReminder, HospitalID: p.HospitalID, Recipient: u, Subject: id, By: p.UserID}
			if err := delivery.Record(ctx, tx, d.Sender, m); err != nil {
				return err
			}
			msgs = append(msgs, m)
		}
		return d.auditCorrection(ctx, tx, p, id, "correction.reviewers_reminded", map[string]any{"recipients": len(to)})
	})
	if err != nil {
		d.fail(w, r, p, "reviewer remind", err)
		return
	}
	if !d.sendAll(w, r, p, "reviewer remind", msgs) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
