package platform

// Inviting a Measured Health staff member to the platform team (mhai-web
// Plan 7b; frontend requirements §14.5; migration 00062):
//
//   POST /platform/team/invites {name, email, role, mfa_code | backup_code}
//   → 201 and the member (status invite_pending), as GET /platform/team
//   then lists them.
//
// Platform admins only (auditors 403), two-step session, step-up (an
// invite can make a platform admin: as damaging as a promotion), at most
// TeamWritesPerMinute team writes a session a minute. role is
// platform_admin or auditor. The name follows the first admin's rule (1 to
// 120 characters), the email any account's; an email any account already
// has is 409 duplicate (mhai-web's invite dialog words it).
//
// The mechanics are the hospital admin's invite's: with no sender
// (CORE_DELIVERY unset) the route answers 503 no_delivery before anything
// changes. In one transaction, under the team lock, as mhai_platform: the
// account (no hospital, no password, invited now), its role, an invite
// link valid 3 days (32 random bytes, only the SHA-256 stored; no
// hospital, 00062) and the audit row platform.team_invited {user_id, role}
// by ids only. After commit the link goes to the sender, which hands it to
// the notifications service by send_platform_link; if it isn't accepted
// the member stays invited and the reply is 424 not_delivered (as adding a
// hospital answers). The link opens the clinical app's set-password page
// (the same page every invite uses); the person then signs in to the
// platform app and sets up two-step there. No outbox row is written: the
// outbox (message_outbox) is per hospital, and an invite's row is only a
// record that it was due, which the audit row already is.
//
// Logs carry ids and codes only: never the name, the email or the link.

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/delivery"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// TeamInviteTTL is a team invite's link's life (the hospital admin's).
const TeamInviteTTL = 3 * 24 * time.Hour

var errEmailTaken = errors.New("platform: email taken")

type teamInviteBody struct {
	Name  *string `json:"name"`
	Email *string `json:"email"`
	Role  string  `json:"role"`
	stepUpBody
}

// teamInvite is POST /platform/team/invites. Order: the query and body
// (422), the sender (503 no_delivery), step-up's readiness (503), the
// email (409 duplicate, before a code is spent), the session's minute
// (429), the code, the write (409 again on a race), then the link to the
// sender (424 not_delivered, the member kept).
func (d Deps) teamInvite(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var b teamInviteBody
	if !httpx.Decode(w, r, &b) {
		return
	}
	if b.Name == nil || b.Email == nil || !slices.Contains(TeamRoles, b.Role) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	name, okName := account.NormaliseHospitalName(*b.Name)
	email := account.NormaliseEmail(*b.Email)
	if !okName || !account.ValidEmail(email) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	rid := httpx.RequestIDFrom(ctx)
	if d.Sender == nil {
		d.log().Error("platform: team invite refused: no sender", "request_id", rid, "reason", "no_delivery")
		httpx.Error(w, r, "no_delivery")
		return
	}
	if !d.stepUpReady(w, r) {
		return
	}
	taken := func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, email).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return errEmailTaken
		}
		return nil
	}
	answer := func(err error, what string) bool {
		switch {
		case err == nil:
			return false
		case errors.Is(err, errEmailTaken):
			httpx.Error(w, r, "duplicate")
		default:
			d.fail(w, r, p, what)
		}
		return true
	}
	if answer(d.tx(ctx, taken), "team invite lookup") {
		return
	}
	if !d.teamRate(w, r, p) || !d.stepUp(w, r, b.MFACode, b.BackupCode) {
		return
	}
	var out TeamMember
	var msg delivery.Message
	err := d.tx(ctx, func(tx pgx.Tx) error {
		if err := teamLock(ctx, tx); err != nil {
			return err
		}
		id := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO users (id, hospital_id, email, display_name, invited_at) VALUES ($1, NULL, $2, $3, now())`,
			id, email, name); err != nil {
			if uniqueOn(err, "users_email_key") {
				return errEmailTaken
			}
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, id, b.Role); err != nil {
			return err
		}
		plain, hash := token.NewRefresh()
		var exp time.Time
		if err := tx.QueryRow(ctx, `INSERT INTO account_tokens (hospital_id, user_id, purpose, token_hash, created_by, expires_at)
			VALUES (NULL, $1, 'invite', $2, $3, now() + make_interval(secs => $4)) RETURNING expires_at`,
			id, hash, p.UserID, TeamInviteTTL.Seconds()).Scan(&exp); err != nil {
			return err
		}
		msg = delivery.Message{Kind: delivery.Invite, Recipient: id, By: p.UserID, Link: d.inviteLink(plain), ExpiresAt: exp}
		if err := audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), RequestID: rid,
			Action: "platform.team_invited", Entity: "user", EntityID: id.String(),
			Payload: map[string]any{"user_id": id.String(), "role": b.Role}}); err != nil {
			return err
		}
		var err error
		out, err = readMember(ctx, tx, id, p.UserID)
		return err
	})
	if answer(err, "team invite") {
		return
	}
	if err := d.Sender.Send(context.WithoutCancel(ctx), msg); err != nil {
		d.log().Error("platform: team member invited but the invite was not handed over", "request_id", rid,
			"member_id", out.ID, "user_id", p.UserID.String(), "reason", "not_delivered")
		httpx.Error(w, r, "not_delivered")
		return
	}
	httpx.JSON(w, http.StatusCreated, out)
}
