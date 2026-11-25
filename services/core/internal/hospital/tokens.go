package hospital

// Invites and password resets by a hospital admin (frontend requirements
// §2.5, §5.1; migration 00050):
//
//   - POST /hospital/staff/invites (step-up) {emails, roles, specialty}:
//     one account per new address, `invited` (no password) until the
//     person chooses one through the invite's link (POST
//     /auth/password/reset, internal/auth); the link lasts 3 days.
//   - POST /hospital/staff/{id}/resend-invite (step-up): a fresh link for
//     an invited person; the old one stops working.
//   - POST /hospital/staff/{id}/password-reset (step-up): a reset link
//     (30 minutes) for an active person who isn't you; every session and
//     trusted device is revoked at once. Their password keeps working
//     until they choose a new one.
//
// The link carries a token of 32 random bytes; only its SHA-256 is stored
// (account_tokens), it works once, and a person has one live token per
// purpose (a new one revokes the old). The link goes to the sender only
// (internal/delivery); it is never logged or audited, and never stored in
// an idempotency reply (the routes are NoStore). On development with the
// log sender (which sends nothing) the reply carries it once, so the admin
// can pass it on; everywhere else `link` is null. With no sender these
// routes fail closed (503 no_delivery) before changing anything; when the
// sender can't take a committed link the reply is 424 not_delivered (the
// invite or reset stands).

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/delivery"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// Token lifetimes and the limits on issuing them.
const (
	InviteTTL = 3 * 24 * time.Hour
	ResetTTL  = 30 * time.Minute
	// maxInviteBatch is one request's addresses; maxInvitesPerDay a
	// hospital's invites (new and resent) in any 24 hours.
	maxInviteBatch    = 200
	maxInvitesPerDay  = 500
	maxResendsPerDay  = 5
	maxResetsPerHour  = 3
	maxInviteEmailLen = 320
	// MaxUnavailablePerDay caps a hospital's invited addresses that turn
	// out to be held elsewhere (not_available) in any 24 hours: past it an
	// invite is refused whole (429), so probing which addresses have an
	// account somewhere is bounded.
	MaxUnavailablePerDay = 50
	resetLinkPath        = "/reset-password"
	tokenPurposeInvite   = "invite"
	tokenPurposeReset    = "password_reset"
)

// LinkOut is the link shown to the admin once (development only).
type LinkOut struct {
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

// StaffWithLinkOut is a person and, on development only, the link they
// were sent.
type StaffWithLinkOut struct {
	StaffOut
	Link *LinkOut `json:"link"`
}

// issueToken revokes person id's live token of purpose and stores a new
// one; it returns the plain token (for the link, never stored) and when
// it expires (the database's clock).
func issueToken(ctx context.Context, tx pgx.Tx, p authz.Principal, id uuid.UUID, purpose string, ttl time.Duration) (string, time.Time, error) {
	if _, err := tx.Exec(ctx, `UPDATE account_tokens SET revoked_at = now()
		WHERE user_id = $1 AND purpose = $2 AND used_at IS NULL AND revoked_at IS NULL`, id, purpose); err != nil {
		return "", time.Time{}, err
	}
	plain, hash := token.NewRefresh()
	var exp time.Time
	err := tx.QueryRow(ctx, `INSERT INTO account_tokens (hospital_id, user_id, purpose, token_hash, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6)) RETURNING expires_at`,
		p.HospitalID, id, purpose, hash, p.UserID, ttl.Seconds()).Scan(&exp)
	return plain, exp, err
}

// revokeTokens ends every live token of person id (removal, deactivation).
func revokeTokens(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE account_tokens SET revoked_at = now() WHERE user_id = $1 AND used_at IS NULL AND revoked_at IS NULL`, id)
	return err
}

// linkFor is the link a token goes out in: the clinical app's reset page
// (relative when no app URL is configured).
func (d Deps) linkFor(plain string) string {
	return strings.TrimRight(d.AppURL, "/") + resetLinkPath + "?token=" + url.QueryEscape(plain)
}

// shown is the link to put in the reply: only on development with the log
// sender (ShowLinks), else nil.
func (d Deps) shown(m delivery.Message) *LinkOut {
	if !d.ShowLinks {
		return nil
	}
	return &LinkOut{URL: m.Link, ExpiresAt: stamp(m.ExpiresAt)}
}

// tokenMessage issues a token for person id and builds its message.
func (d Deps) tokenMessage(ctx context.Context, tx pgx.Tx, p authz.Principal, id uuid.UUID, kind delivery.Kind,
	purpose string, ttl time.Duration) (delivery.Message, error) {
	plain, exp, err := issueToken(ctx, tx, p, id, purpose, ttl)
	if err != nil {
		return delivery.Message{}, err
	}
	return delivery.Message{Kind: kind, HospitalID: p.HospitalID, Recipient: id, By: p.UserID,
		Link: d.linkFor(plain), ExpiresAt: exp}, nil
}

// staffPasswordReset is POST /hospital/staff/{id}/password-reset {mfa_code | backup_code}.
func (d Deps) staffPasswordReset() http.Handler {
	return d.run(staffAction{what: "staff password reset", stepUp: true, withLink: true,
		check: func(ctx context.Context, tx pgx.Tx, p authz.Principal, s StaffOut) error {
			if s.IsYou {
				return errSelf
			}
			// A single sign-on account has no password here to reset (a
			// local one would sidestep the identity provider).
			if !s.active || s.invited || (s.SignIn != nil && *s.SignIn == "sso") {
				return errConflict
			}
			if err := d.needSender(); err != nil {
				return err
			}
			n, err := messagesWithin(ctx, tx, p.HospitalID, s.uid, uuid.Nil, delivery.PasswordReset, time.Hour)
			if err != nil {
				return err
			}
			if n >= maxResetsPerHour {
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
			if _, err := tx.Exec(ctx, `DELETE FROM trusted_devices WHERE user_id = $1`, id); err != nil {
				return done{}, err
			}
			m, err := d.tokenMessage(ctx, tx, p, id, delivery.PasswordReset, tokenPurposeReset, ResetTTL)
			if err != nil {
				return done{}, err
			}
			return done{revoked: revoked, msgs: []delivery.Message{m}, link: d.shown(m)},
				d.auditStaff(ctx, tx, p, id, "staff.password_reset_sent", map[string]any{"sessions_revoked": len(revoked)})
		},
	})
}

// staffResendInvite is POST /hospital/staff/{id}/resend-invite {mfa_code | backup_code}.
func (d Deps) staffResendInvite() http.Handler {
	return d.run(staffAction{what: "staff resend invite", stepUp: true, withLink: true,
		check: func(ctx context.Context, tx pgx.Tx, p authz.Principal, s StaffOut) error {
			if !s.active || !s.invited {
				return errConflict
			}
			if err := d.needSender(); err != nil {
				return err
			}
			n, err := messagesWithin(ctx, tx, p.HospitalID, s.uid, uuid.Nil, delivery.Invite, 24*time.Hour)
			if err != nil {
				return err
			}
			if n >= maxResendsPerDay {
				return errRateLimited
			}
			return hospitalInvitesLeft(ctx, tx, p.HospitalID, 1)
		},
		act: func(ctx context.Context, tx pgx.Tx, p authz.Principal, s StaffOut) (done, error) {
			m, err := d.tokenMessage(ctx, tx, p, s.uid, delivery.Invite, tokenPurposeInvite, InviteTTL)
			if err != nil {
				return done{}, err
			}
			if _, err := tx.Exec(ctx, `UPDATE users SET staff_version = staff_version + 1 WHERE id = $1`, s.uid); err != nil {
				return done{}, err
			}
			return done{msgs: []delivery.Message{m}, link: d.shown(m)},
				d.auditStaff(ctx, tx, p, s.uid, "staff.invite_resent", map[string]any{})
		},
	})
}

// hospitalInvitesLeft refuses n more invites past the hospital's daily
// limit.
func hospitalInvitesLeft(ctx context.Context, tx pgx.Tx, hid uuid.UUID, n int) error {
	var sent int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM message_outbox WHERE hospital_id = $1 AND kind = 'invite'
		AND created_at >= now() - interval '24 hours'`, hid).Scan(&sent); err != nil {
		return err
	}
	if sent+n > maxInvitesPerDay {
		return errRateLimited
	}
	return nil
}

type inviteIn struct {
	Emails    []string  `json:"emails"`
	Roles     []string  `json:"roles"`
	Specialty optString `json:"specialty"`
	stepUpIn
}

// SkippedOut is an address not invited, and why.
type SkippedOut struct {
	Email  string `json:"email"`
	Reason string `json:"reason"`
}

// InviteOut is the invite reply.
type InviteOut struct {
	Invited []StaffWithLinkOut `json:"invited"`
	Skipped []SkippedOut       `json:"skipped"`
}

// inviteRoles are the roles an invite may give: Analyst is granted after
// the person has joined (it needs their two-step).
var inviteRoles = []string{authz.RoleClinician, authz.RoleHospitalAdmin}

// validInvite checks the body on its own, and sorts the addresses: the
// ones to invite (normalised, in order) and the ones skipped as malformed
// or repeated.
func validInvite(in inviteIn) (emails []string, skipped []SkippedOut, err error) {
	if len(in.Emails) == 0 || len(in.Emails) > maxInviteBatch || len(in.Roles) == 0 || len(in.Roles) > len(inviteRoles) {
		return nil, nil, errInvalid
	}
	seenRole := map[string]bool{}
	for _, r := range in.Roles {
		if r == authz.RoleAnalyst {
			return nil, nil, errNotEnabled
		}
		if !slices.Contains(inviteRoles, r) || seenRole[r] {
			return nil, nil, errInvalid
		}
		seenRole[r] = true
	}
	if in.Specialty.set && in.Specialty.val != nil && !specialtyRe.MatchString(*in.Specialty.val) {
		return nil, nil, errInvalid
	}
	skipped = []SkippedOut{}
	seen := map[string]bool{}
	for _, e := range in.Emails {
		if len(e) > maxInviteEmailLen {
			return nil, nil, errInvalid
		}
		n := account.NormaliseEmail(e)
		switch {
		case !account.ValidEmail(n):
			skipped = append(skipped, SkippedOut{Email: e, Reason: "invalid_email"})
		case seen[n]:
			skipped = append(skipped, SkippedOut{Email: e, Reason: "duplicate"})
		default:
			seen[n] = true
			emails = append(emails, n)
		}
	}
	return emails, skipped, nil
}

// staffInvite is POST /hospital/staff/invites {emails, roles, specialty?, mfa_code | backup_code}.
func (d Deps) staffInvite(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var in inviteIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	emails, skipped, err := validInvite(in)
	if err == nil {
		err = d.needSender()
	}
	if err != nil {
		d.fail(w, r, p, "staff invite", err)
		return
	}
	ctx := r.Context()
	if err := d.tx(ctx, func(tx pgx.Tx) error { return hospitalInvitesLeft(ctx, tx, p.HospitalID, len(emails)) }); err != nil {
		d.fail(w, r, p, "staff invite", err)
		return
	}
	if !d.stepUp(w, r, in.stepUpIn) {
		return
	}
	var specialty *string
	if in.Specialty.set && in.Specialty.val != nil {
		t := strings.TrimSpace(*in.Specialty.val)
		specialty = &t
	}
	out := InviteOut{Invited: []StaffWithLinkOut{}, Skipped: skipped}
	var msgs []delivery.Message
	err = d.tx(ctx, func(tx pgx.Tx) error {
		if err := lockStaff(ctx, tx, p.HospitalID); err != nil {
			return err
		}
		if err := hospitalInvitesLeft(ctx, tx, p.HospitalID, len(emails)); err != nil {
			return err
		}
		out.Invited, out.Skipped, msgs = out.Invited[:0], skipped, nil
		var ids []uuid.UUID
		links := map[uuid.UUID]*LinkOut{}
		unavailable := 0
		// The day's addresses already found held elsewhere (the audit's
		// counts, under RLS: this hospital's only).
		var probed int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum((payload->>'count')::int), 0) FROM audit_log
			WHERE hospital_id = $1 AND action = 'staff.invite_unavailable' AND ts >= now() - interval '24 hours'`,
			p.HospitalID).Scan(&probed); err != nil {
			return err
		}
		for _, e := range emails {
			// already_staff only for an account of the caller's hospital
			// (RLS: the gateway sees no other). An address is unique across
			// the platform, so one held anywhere else can't be invited; it
			// is skipped as not_available, the same as an address no
			// hospital may use (a platform account's), so the reply never
			// says another hospital has it. Nothing of that account is read
			// or changed, and nothing is sent.
			var mine bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, e).Scan(&mine); err != nil {
				return err
			}
			if mine {
				out.Skipped = append(out.Skipped, SkippedOut{Email: e, Reason: "already_staff"})
				continue
			}
			local, _, _ := strings.Cut(e, "@")
			var id uuid.UUID
			err := tx.QueryRow(ctx, `INSERT INTO users (hospital_id, email, display_name, specialty, invited_at)
				VALUES ($1, $2, $3, $4, now()) ON CONFLICT (email) DO NOTHING RETURNING id`,
				p.HospitalID, e, local, specialty).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				unavailable++
				if probed+unavailable > MaxUnavailablePerDay {
					return errRateLimited // the whole request rolls back
				}
				out.Skipped = append(out.Skipped, SkippedOut{Email: e, Reason: "not_available"})
				continue
			}
			if err != nil {
				return err
			}
			for _, role := range inviteRoles {
				if slices.Contains(in.Roles, role) {
					if _, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, id, role); err != nil {
						return err
					}
				}
			}
			m, err := d.tokenMessage(ctx, tx, p, id, delivery.Invite, tokenPurposeInvite, InviteTTL)
			if err != nil {
				return err
			}
			if err := delivery.Record(ctx, tx, d.Sender, m); err != nil {
				return err
			}
			if err := d.auditStaff(ctx, tx, p, id, "staff.invited", map[string]any{"roles": sortStaffRoles(in.Roles)}); err != nil {
				return err
			}
			msgs = append(msgs, m)
			links[id] = d.shown(m)
			ids = append(ids, id)
		}
		if unavailable > 0 {
			// Counted, never named: probing other hospitals' addresses is
			// visible to Measured Health.
			if err := audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
				RequestID: httpx.RequestIDFrom(ctx), Action: "staff.invite_unavailable", Entity: "hospital",
				EntityID: p.HospitalID.String(), Payload: map[string]any{"count": unavailable}}); err != nil {
				return err
			}
		}
		for _, id := range ids {
			s, err := loadPerson(ctx, tx, p, id, false)
			if err != nil {
				return err
			}
			out.Invited = append(out.Invited, StaffWithLinkOut{StaffOut: s, Link: links[id]})
		}
		return nil
	})
	if err != nil {
		d.fail(w, r, p, "staff invite", err)
		return
	}
	if !d.sendAll(w, r, p, "staff invite", msgs) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}
