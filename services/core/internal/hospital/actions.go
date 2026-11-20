package hospital

// The staff actions that answered 501 (frontend requirements §5.1, §6):
//
//   - DELETE /hospital/staff/{id} (step-up): removes the person for good.
//     The account is deactivated and marked removed; its roles (Analyst
//     through 00030's function), two-step, sessions, trusted devices and
//     sign-in password and any live invite or reset link go, its email is
//     replaced by a tombstone, and a
//     correction waiting on them as its reviewer is unassigned. Its names
//     stay, so past consultations, feedback and audit entries stay
//     attributed (the retention design keeps rows, not the person's
//     access). 204. Not yourself (self_change); not the last active admin
//     (last_admin).
//   - POST /hospital/staff/{id}/reset-mfa (step-up): removes the person's
//     two-step and backup codes, revokes every session and trusted device;
//     they set two-step up again at their next sign-in. Not yourself; only
//     an active account with two-step on (else conflict). The person is
//     sent a notice, so it fails closed (no_delivery) without a sender.
//   - POST /hospital/staff/{id}/remind: a reminder to set up two-step, for
//     an active account with the mfa_not_set_up issue (else conflict), at
//     most once a day per person (rate_limited); no_delivery without a
//     sender.
//
// Each runs the refusals that need no code first (person, self, status,
// last admin, sender, rate), then the code, then everything again under
// the hospital's staff lock, with its audit entry, in one transaction. The
// denylist and the messages go after commit.

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/delivery"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// The limits on messages to one person (message_outbox, counted under
// the staff lock).
const (
	mfaReminderEvery  = 24 * time.Hour
	mfaResetsPerHour  = 3
	reviewRemindEvery = time.Hour
)

// done is what an action leaves for after its commit.
type done struct {
	revoked map[string][]string
	msgs    []delivery.Message
	// link is the link to show the admin once (development's log sender
	// only), for the actions that issue one.
	link *LinkOut
}

// staffAction is one action on one person.
type staffAction struct {
	what   string
	stepUp bool
	// check refuses the action for person s (run before the code, and
	// again under the lock).
	check func(ctx context.Context, tx pgx.Tx, p authz.Principal, s StaffOut) error
	// act makes it, with its audit entry.
	act func(ctx context.Context, tx pgx.Tx, p authz.Principal, s StaffOut) (done, error)
	// noContent answers 204 rather than the person; withLink answers
	// the person with the link to show once (StaffWithLinkOut).
	noContent, withLink bool
}

func (d Deps) run(a staffAction) http.Handler {
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
		var in stepUpIn
		if !decodeOptional(w, r, &in) {
			return
		}
		ctx := r.Context()
		check := func(tx pgx.Tx, lock bool) (StaffOut, error) {
			s, err := loadPerson(ctx, tx, p, id, lock)
			if err != nil {
				return s, err
			}
			return s, a.check(ctx, tx, p, s)
		}
		if err := d.tx(ctx, func(tx pgx.Tx) error { _, err := check(tx, false); return err }); err != nil {
			d.fail(w, r, p, a.what, err)
			return
		}
		if a.stepUp && !d.stepUp(w, r, in) {
			return
		}
		var out done
		var person StaffOut
		err := d.tx(ctx, func(tx pgx.Tx) error {
			if err := lockStaff(ctx, tx, p.HospitalID); err != nil {
				return err
			}
			s, err := check(tx, true)
			if err != nil {
				return err
			}
			if out, err = a.act(ctx, tx, p, s); err != nil {
				return err
			}
			for _, m := range out.msgs {
				if err := delivery.Record(ctx, tx, d.Sender, m); err != nil {
					return err
				}
			}
			if !a.noContent {
				person, err = loadPerson(ctx, tx, p, id, false)
			}
			return err
		})
		if err != nil {
			d.fail(w, r, p, a.what, err)
			return
		}
		d.afterCommit(ctx, p, id, out)
		if !d.sendAll(w, r, p, a.what, out.msgs) {
			return
		}
		if a.noContent {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if a.withLink {
			httpx.JSON(w, http.StatusOK, StaffWithLinkOut{StaffOut: person, Link: out.link})
			return
		}
		httpx.JSON(w, http.StatusOK, person)
	})
}

// afterCommit denylists the revoked sessions. The database's revocation
// stands either way; the denylist only makes it immediate.
func (d Deps) afterCommit(ctx context.Context, p authz.Principal, id uuid.UUID, out done) {
	if len(out.revoked) == 0 || d.Auth == nil {
		return
	}
	if err := d.Auth.DenySessions(context.WithoutCancel(ctx), out.revoked); err != nil {
		d.log().Error("hospital: denylist after an account action failed; sessions end when the guard's cache expires",
			"request_id", httpx.RequestIDFrom(ctx), "hospital_id", p.HospitalID.String(), "user_id", id.String())
	}
}

// sendAll hands the committed messages to the sender; false means the
// reply (503 no_delivery) is written: the action stands, the message may
// not have gone.
func (d Deps) sendAll(w http.ResponseWriter, r *http.Request, p authz.Principal, what string, msgs []delivery.Message) bool {
	failed := 0
	for _, m := range msgs {
		if err := d.Sender.Send(context.WithoutCancel(r.Context()), m); err != nil {
			failed++
		}
	}
	if failed == 0 {
		return true
	}
	d.log().Error("hospital: "+what+" committed but a message was not handed over", "request_id", httpx.RequestIDFrom(r.Context()),
		"hospital_id", p.HospitalID.String(), "user_id", p.UserID.String(), "failed", failed)
	httpx.Error(w, r, "no_delivery")
	return false
}

// needSender refuses an action that must send a message when there is no
// sender.
func (d Deps) needSender() error {
	if d.Sender == nil {
		return errNoDelivery
	}
	return nil
}

// messagesWithin counts the messages of kind to person id (or, when
// subject is set, about that correction) in the last window, on the
// database's clock (the rows' created_at is its now()).
func messagesWithin(ctx context.Context, tx pgx.Tx, hid, id, subject uuid.UUID, kind delivery.Kind, window time.Duration) (int, error) {
	var n int
	secs := window.Seconds()
	var err error
	if subject != uuid.Nil {
		err = tx.QueryRow(ctx, `SELECT count(*) FROM message_outbox WHERE hospital_id = $1 AND subject_id = $2 AND kind = $3
			AND created_at >= now() - make_interval(secs => $4)`, hid, subject, string(kind), secs).Scan(&n)
	} else {
		err = tx.QueryRow(ctx, `SELECT count(*) FROM message_outbox WHERE hospital_id = $1 AND recipient_id = $2 AND kind = $3
			AND created_at >= now() - make_interval(secs => $4)`, hid, id, string(kind), secs).Scan(&n)
	}
	return n, err
}

func (d Deps) auditStaff(ctx context.Context, tx pgx.Tx, p authz.Principal, id uuid.UUID, action string, payload map[string]any) error {
	return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
		RequestID: httpx.RequestIDFrom(ctx), Action: action, Entity: "user", EntityID: id.String(), Payload: payload})
}

// staffRemove is DELETE /hospital/staff/{id} {mfa_code | backup_code}.
func (d Deps) staffRemove() http.Handler {
	return d.run(staffAction{what: "staff remove", stepUp: true, noContent: true,
		check: func(ctx context.Context, tx pgx.Tx, p authz.Principal, s StaffOut) error {
			if s.IsYou {
				return errSelf
			}
			if s.active && s.has(authz.RoleHospitalAdmin) {
				n, err := otherActiveAdmins(ctx, tx, p.HospitalID, s.uid)
				if err != nil {
					return err
				}
				if n == 0 {
					return errLastAdmin
				}
			}
			return nil
		},
		act: func(ctx context.Context, tx pgx.Tx, p authz.Principal, s StaffOut) (done, error) {
			id, rid := s.uid, httpx.RequestIDFrom(ctx)
			revoked, err := revokeSessions(ctx, tx, id)
			if err != nil {
				return done{}, err
			}
			if err := revokeTokens(ctx, tx, id); err != nil {
				return done{}, err
			}
			if _, err := analysis.RemoveAnalyst(ctx, tx, id, rid); err != nil {
				return done{}, err
			}
			for _, q := range []string{
				`DELETE FROM trusted_devices WHERE user_id = $1`,
				`DELETE FROM user_mfa WHERE user_id = $1`,
				`DELETE FROM user_roles WHERE user_id = $1 AND role IN ('hospital_admin', 'clinician')`,
				`UPDATE feedback_events SET assigned_to = NULL WHERE assigned_to = $1 AND status = 'pending_review'`,
				// The tombstone: a reserved domain that can't receive mail,
				// unique by the account's id.
				`UPDATE users SET is_active = false, removed_at = now(), password_hash = NULL, must_change_password = false,
					email = 'removed-' || id::text || '@removed.invalid', staff_version = staff_version + 1 WHERE id = $1`,
			} {
				if _, err := tx.Exec(ctx, q, id); err != nil {
					return done{}, err
				}
			}
			if _, err := analysis.AuditAnalystChanges(ctx, tx, rid); err != nil {
				return done{}, err
			}
			return done{revoked: revoked}, d.auditStaff(ctx, tx, p, id, "staff.removed", map[string]any{"sessions_revoked": len(revoked)})
		},
	})
}

// staffResetMFA is POST /hospital/staff/{id}/reset-mfa {mfa_code | backup_code}.
func (d Deps) staffResetMFA() http.Handler {
	return d.run(staffAction{what: "staff reset two-step", stepUp: true,
		check: func(ctx context.Context, tx pgx.Tx, p authz.Principal, s StaffOut) error {
			if s.IsYou {
				return errSelf
			}
			if !s.active || !s.mfa {
				return errConflict
			}
			if err := d.needSender(); err != nil {
				return err
			}
			n, err := messagesWithin(ctx, tx, p.HospitalID, s.uid, uuid.Nil, delivery.MFAReset, time.Hour)
			if err != nil {
				return err
			}
			if n >= mfaResetsPerHour {
				return errRateLimited
			}
			return nil
		},
		act: func(ctx context.Context, tx pgx.Tx, p authz.Principal, s StaffOut) (done, error) {
			id := s.uid
			revoked, err := revokeSessions(ctx, tx, id)
			if err != nil {
				return done{}, err
			}
			for _, q := range []string{`DELETE FROM user_mfa WHERE user_id = $1`, `DELETE FROM trusted_devices WHERE user_id = $1`,
				`UPDATE users SET staff_version = staff_version + 1 WHERE id = $1`} {
				if _, err := tx.Exec(ctx, q, id); err != nil {
					return done{}, err
				}
			}
			msg := delivery.Message{Kind: delivery.MFAReset, HospitalID: p.HospitalID, Recipient: id, By: p.UserID}
			return done{revoked: revoked, msgs: []delivery.Message{msg}},
				d.auditStaff(ctx, tx, p, id, "staff.mfa_reset", map[string]any{"sessions_revoked": len(revoked)})
		},
	})
}

// staffRemind is POST /hospital/staff/{id}/remind: set up two-step.
func (d Deps) staffRemind() http.Handler {
	return d.run(staffAction{what: "staff remind",
		check: func(ctx context.Context, tx pgx.Tx, p authz.Principal, s StaffOut) error {
			if !s.active || !slices.Contains(s.Issues, "mfa_not_set_up") {
				return errConflict
			}
			if err := d.needSender(); err != nil {
				return err
			}
			n, err := messagesWithin(ctx, tx, p.HospitalID, s.uid, uuid.Nil, delivery.MFAReminder, mfaReminderEvery)
			if err != nil {
				return err
			}
			if n > 0 {
				return errRateLimited
			}
			return nil
		},
		act: func(ctx context.Context, tx pgx.Tx, p authz.Principal, s StaffOut) (done, error) {
			msg := delivery.Message{Kind: delivery.MFAReminder, HospitalID: p.HospitalID, Recipient: s.uid, By: p.UserID}
			return done{msgs: []delivery.Message{msg}}, d.auditStaff(ctx, tx, p, s.uid, "staff.mfa_reminded", map[string]any{})
		},
	})
}
