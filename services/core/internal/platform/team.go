package platform

// The platform team (mhai-web Plan 7b; platform admin design §9, §13):
//
//   - GET /platform/team: platform admins and auditors; the active
//     platform accounts (no hospital, a platform role), by name. is_you is
//     decided from the token, never a name.
//   - PATCH /platform/team/{id} {role, version, mfa_code | backup_code}:
//     platform admins, with step-up (core's rule: a demotion is as
//     damaging as a removal; mhai-web's contract sent no code); changes a
//     member's role (platform_admin or auditor) and revokes the member's
//     sessions. 409 own_role for one's own, 409 last_admin when it would
//     leave no active platform admin, 409 conflict on a stale version.
//     Audited platform.team_role_changed.
//   - DELETE /platform/team/{id} {mfa_code | backup_code}: platform
//     admins, with step-up; deactivates the account and revokes its
//     sessions, denylisted after commit so its tokens stop at once on every
//     replica. 409 last_admin for the last active platform admin.
//     Audited platform.team_member_removed.
//   - POST /platform/team/invites: 501 not_implemented. Core sends no
//     invitation and keeps no invite: platform accounts are made with
//     "core users create" (the provisioning CLI).
//
// Every team write takes one transaction advisory lock, so the last-admin
// rule holds when two admins act at once. The provisioning CLI's user
// commands don't take it (a residual risk, noted in the PR).

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

const (
	// TeamWritesPerMinute is the team writes one session may make a minute.
	TeamWritesPerMinute = 10
	teamScope           = "platform.team.write_rate"
)

// TeamRoles are the roles a platform member can have.
var TeamRoles = []string{authz.RolePlatformAdmin, authz.RoleAuditor}

var (
	errOwnRole   = errors.New("platform: own role")
	errLastAdmin = errors.New("platform: last platform admin")
	errStale     = errors.New("platform: stale version")
	errNoMember  = errors.New("platform: no such member")
)

func registerTeam(m *authz.Mux, d Deps) {
	read := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: TeamRoles}
	write := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin}, Write: true}
	stepUp := write
	stepUp.StepUp = true
	authz.Route(m, http.MethodGet, "/platform/team", read, http.HandlerFunc(d.teamList))
	authz.Route(m, http.MethodPost, "/platform/team/invites", write, http.HandlerFunc(notImplemented))
	authz.Route(m, http.MethodPatch, "/platform/team/{id}", stepUp, http.HandlerFunc(d.teamPatch))
	authz.Route(m, http.MethodDelete, "/platform/team/{id}", stepUp, http.HandlerFunc(d.teamDelete))
}

// TeamMember is one platform account.
type TeamMember struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Email        string  `json:"email"`
	Role         string  `json:"role"`
	Status       string  `json:"status"`
	MFAEnabled   bool    `json:"mfa_enabled"`
	LastSignInAt *string `json:"last_sign_in_at"`
	IsYou        bool    `json:"is_you"`
	Version      int     `json:"version"`
}

// teamSelect reads active platform members: an account with no hospital
// and a platform role. A member holding both roles shows as platform_admin.
const teamSelect = `SELECT u.id, u.display_name, u.email::text,
		CASE WHEN EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role = 'platform_admin')
		     THEN 'platform_admin' ELSE 'auditor' END,
		mhai_platform_mfa_enabled(u.id),
		u.last_login_at,
		COALESCE((SELECT v.version FROM platform_member_versions v WHERE v.user_id = u.id), 1)
	FROM users u
	WHERE u.hospital_id IS NULL AND u.is_active
	  AND EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role IN ('platform_admin', 'auditor'))`

func scanMember(row pgx.Row, you uuid.UUID) (TeamMember, error) {
	var m TeamMember
	var id uuid.UUID
	var last *time.Time
	if err := row.Scan(&id, &m.Name, &m.Email, &m.Role, &m.MFAEnabled, &last, &m.Version); err != nil {
		return m, err
	}
	m.ID, m.Status, m.IsYou = id.String(), "active", id == you
	if last != nil {
		s := stamp(*last)
		m.LastSignInAt = &s
	}
	return m, nil
}

func readMember(ctx context.Context, tx pgx.Tx, id, you uuid.UUID) (TeamMember, error) {
	m, err := scanMember(tx.QueryRow(ctx, teamSelect+` AND u.id = $1`, id), you)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, errNoMember
	}
	return m, err
}

// teamList is GET /platform/team.
func (d Deps) teamList(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	members := []TeamMember{}
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), teamSelect+` ORDER BY u.display_name, u.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMember(rows, p.UserID)
			if err != nil {
				return err
			}
			members = append(members, m)
		}
		return rows.Err()
	})
	if err != nil {
		d.fail(w, r, p, "team list")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"members": members})
}

// pathMember is the path's member id (a canonical uuid), or false (404).
func pathMember(r *http.Request) (uuid.UUID, bool) {
	s := r.PathValue("id")
	if !canonicalUUID(s) {
		return uuid.Nil, false
	}
	id := uuid.MustParse(s)
	return id, id != uuid.Nil
}

// teamLock serialises the team's writes.
func teamLock(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('mhai.platform_team'))`)
	return err
}

// otherAdmins counts the active platform admins other than id.
func otherAdmins(ctx context.Context, tx pgx.Tx, id uuid.UUID) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM users u
		WHERE u.hospital_id IS NULL AND u.is_active AND u.id <> $1
		  AND EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role = 'platform_admin')`, id).Scan(&n)
	return n, err
}

func bumpMember(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	_, err := tx.Exec(ctx, `INSERT INTO platform_member_versions (user_id, version) VALUES ($1, 2)
		ON CONFLICT (user_id) DO UPDATE SET version = platform_member_versions.version + 1`, id)
	return err
}

func (d Deps) teamAnswer(w http.ResponseWriter, r *http.Request, p authz.Principal, what string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errNoMember):
		httpx.Error(w, r, "not_found")
	case errors.Is(err, errOwnRole):
		httpx.Error(w, r, "own_role")
	case errors.Is(err, errLastAdmin):
		httpx.Error(w, r, "last_admin")
	case errors.Is(err, errStale):
		httpx.Error(w, r, "conflict")
	default:
		d.fail(w, r, p, what)
	}
	return true
}

type rolePatch struct {
	Role       string `json:"role"`
	Version    *int   `json:"version"`
	MFACode    string `json:"mfa_code"`
	BackupCode string `json:"backup_code"`
}

// teamCheck is a team write's rules, read under the lock in the write and
// once before the code is spent: the member (404), its version (409
// conflict; skipped when version is nil) and, when it would lose an admin,
// the last admin (409). It returns the member.
func teamCheck(ctx context.Context, tx pgx.Tx, id, you uuid.UUID, version *int, losesAdmin func(TeamMember) bool) (TeamMember, error) {
	m, err := readMember(ctx, tx, id, you)
	if err != nil {
		return m, err
	}
	if version != nil && m.Version != *version {
		return m, errStale
	}
	if m.Role == authz.RolePlatformAdmin && losesAdmin(m) {
		n, err := otherAdmins(ctx, tx, id)
		if err != nil {
			return m, err
		}
		if n == 0 {
			return m, errLastAdmin
		}
	}
	return m, nil
}

// revokeMember revokes every open session of a platform member and returns
// each one's access token id (session id to token ids, as
// auth.Service.DenySessions takes them), for the denylist after commit.
func revokeMember(ctx context.Context, tx pgx.Tx, id uuid.UUID) (map[string][]string, error) {
	rows, err := tx.Query(ctx, `UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL
		RETURNING id, access_jti`, id)
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

// denyAfterCommit puts the revoked sessions on the denylist. A failure is
// logged by ids only: revoked_at still refuses them within the guard's
// cache time.
func (d Deps) denyAfterCommit(r *http.Request, revoked map[string][]string) {
	if len(revoked) == 0 || d.Auth == nil {
		return
	}
	// After commit, whatever the caller does: the revocation has happened.
	if err := d.Auth.DenySessions(context.WithoutCancel(r.Context()), revoked); err != nil {
		d.log().Error("platform: team write: denylist write failed", "request_id", httpx.RequestIDFrom(r.Context()),
			"reason", "denylist_unavailable")
	}
}

// teamPatch is PATCH /platform/team/{id}, with step-up (a demotion is as
// damaging as a removal). Order: the path (404), the body (422), own role
// (409), the member, version and last admin (404, 409; before a code is
// spent), the session's minute (429), the code, then the same rules again
// under the lock. The member's sessions are revoked: they sign in again
// with the new role.
func (d Deps) teamPatch(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathMember(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var b rolePatch
	if !httpx.Decode(w, r, &b) {
		return
	}
	if !slices.Contains(TeamRoles, b.Role) || b.Version == nil || *b.Version < 1 {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if id == p.UserID {
		httpx.Error(w, r, "own_role")
		return
	}
	if d.Auth == nil {
		httpx.Error(w, r, "not_ready")
		return
	}
	demotes := func(TeamMember) bool { return b.Role != authz.RolePlatformAdmin }
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		_, err := teamCheck(r.Context(), tx, id, p.UserID, b.Version, demotes)
		return err
	})
	if d.teamAnswer(w, r, p, "team role check", err) {
		return
	}
	if !d.teamRate(w, r, p) || !d.stepUp(w, r, b.MFACode, b.BackupCode) {
		return
	}
	rid := httpx.RequestIDFrom(r.Context())
	var out TeamMember
	var sids map[string][]string
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		if err := teamLock(r.Context(), tx); err != nil {
			return err
		}
		m, err := teamCheck(r.Context(), tx, id, p.UserID, b.Version, demotes)
		if err != nil {
			return err
		}
		if m.Role == b.Role {
			out = m
			return nil
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM user_roles WHERE user_id = $1 AND role IN ('platform_admin', 'auditor')`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, id, b.Role); err != nil {
			return err
		}
		if sids, err = revokeMember(r.Context(), tx, id); err != nil {
			return err
		}
		// A demoted admin is no longer one of a lift's two people.
		if m.Role == authz.RolePlatformAdmin {
			if err := cancelMemberLifts(r.Context(), tx, p, rid, id); err != nil {
				return err
			}
		}
		if err := bumpMember(r.Context(), tx, id); err != nil {
			return err
		}
		if err := audit.Write(r.Context(), tx, audit.Event{ActorID: p.UserID.String(), RequestID: rid,
			Action: "platform.team_role_changed", Entity: "user", EntityID: id.String(),
			Payload: map[string]any{"user_id": id.String(), "role": b.Role, "previous": m.Role}}); err != nil {
			return err
		}
		out, err = readMember(r.Context(), tx, id, p.UserID)
		return err
	})
	if d.teamAnswer(w, r, p, "team role change", err) {
		return
	}
	d.denyAfterCommit(r, sids)
	httpx.JSON(w, http.StatusOK, out)
}

type stepUpBody struct {
	MFACode    string `json:"mfa_code"`
	BackupCode string `json:"backup_code"`
}

// teamDelete is DELETE /platform/team/{id}. Order: the path (404), the
// body (422), the member and the last admin (404, 409; before a code is
// spent), the session's minute (429), the code, then the same rules again
// under the lock.
func (d Deps) teamDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathMember(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var b stepUpBody
	if !httpx.Decode(w, r, &b) {
		return
	}
	if d.Auth == nil {
		httpx.Error(w, r, "not_ready")
		return
	}
	always := func(TeamMember) bool { return true }
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		_, err := teamCheck(r.Context(), tx, id, p.UserID, nil, always)
		return err
	})
	if d.teamAnswer(w, r, p, "team member lookup", err) {
		return
	}
	if !d.teamRate(w, r, p) || !d.stepUp(w, r, b.MFACode, b.BackupCode) {
		return
	}
	rid := httpx.RequestIDFrom(r.Context())
	var sids map[string][]string
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		if err := teamLock(r.Context(), tx); err != nil {
			return err
		}
		m, err := teamCheck(r.Context(), tx, id, p.UserID, nil, always)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `UPDATE users SET is_active = false WHERE id = $1 AND hospital_id IS NULL`, id); err != nil {
			return err
		}
		if sids, err = revokeMember(r.Context(), tx, id); err != nil {
			return err
		}
		if err := cancelMemberLifts(r.Context(), tx, p, rid, id); err != nil {
			return err
		}
		if err := bumpMember(r.Context(), tx, id); err != nil {
			return err
		}
		return audit.Write(r.Context(), tx, audit.Event{ActorID: p.UserID.String(), RequestID: rid,
			Action: "platform.team_member_removed", Entity: "user", EntityID: id.String(),
			Payload: map[string]any{"user_id": id.String(), "role": m.Role}})
	})
	if d.teamAnswer(w, r, p, "team member removal", err) {
		return
	}
	d.denyAfterCommit(r, sids)
	w.WriteHeader(http.StatusNoContent)
}

// teamRate counts one write against the session's minute (429 over it,
// 503 when the counter fails).
func (d Deps) teamRate(w http.ResponseWriter, r *http.Request, p authz.Principal) bool {
	allowed, err := d.reserveRate(r, p, teamScope, "platform.team.write.v1", TeamWritesPerMinute)
	switch {
	case err != nil:
		d.log().Error("platform: team write refused: rate counter unavailable", "request_id", httpx.RequestIDFrom(r.Context()),
			"reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return false
	case !allowed:
		httpx.Error(w, r, "rate_limited")
		return false
	}
	return true
}

// notImplemented answers 501 not_implemented: a route the platform app
// calls that core has no backing feature for yet. It is registered with
// its real rule, so the caller is checked (role, audience, two-step)
// before it learns that.
func notImplemented(w http.ResponseWriter, r *http.Request) {
	httpx.Error(w, r, "not_implemented")
}
