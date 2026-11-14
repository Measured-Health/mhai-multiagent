package hospital

// The staff routes (frontend requirements §5.1, §6; Analysis plan Task 17):
// the list, role and specialty edits, deactivation and reactivation.
//
// The rules, in the order a write checks them:
//   - the person in the caller's hospital (else 404 not_found);
//   - the body (422 validation_failed), the version (409 conflict);
//   - an admin never drops their own hospital_admin, nor deactivates
//     themselves (403 self_change); adding a role to yourself is allowed,
//     audited and flagged (self_role_change), except Analyst, which the
//     database refuses (self_change);
//   - an action that leaves no active hospital admin is 409 last_admin;
//   - only then the step-up code (403 mfa_required without one), so a
//     refused request doesn't spend it;
//   - then the change, under a per-hospital advisory lock that re-checks
//     everything above, in one transaction with its audit entries. Analyst
//     goes through 00030's functions (analysis.GrantAnalyst and
//     RemoveAnalyst), whose refusals map to forbidden, not_found,
//     self_change, analysis_off, analyst_cap and not_eligible; any removal
//     of clinician is followed by analysis.AuditAnalystChanges, or the
//     database refuses the commit.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// StaffRoles are the roles a hospital admin gives and takes, in the
// contract's order.
var StaffRoles = []string{authz.RoleClinician, authz.RoleAnalyst, authz.RoleHospitalAdmin}

// specialtyRe is a specialty's shape: a word or a few (letters, spaces,
// hyphens), as a consultation's.
var specialtyRe = regexp.MustCompile(`^[A-Za-z][A-Za-z -]{0,39}$`)

// StaffOut is one member of staff (contract StaffOut).
type StaffOut struct {
	ID                string   `json:"id"`
	FullName          string   `json:"full_name"`
	DisplayName       string   `json:"display_name"`
	Email             string   `json:"email"`
	Roles             []string `json:"roles"`
	Specialty         *string  `json:"specialty"`
	CanReview         bool     `json:"can_review"`
	ReviewSpecialties []string `json:"review_specialties"`
	Status            string   `json:"status"`
	Issues            []string `json:"issues"`
	SignIn            *string  `json:"sign_in"`
	LastActiveAt      *string  `json:"last_active_at"`
	JoinedAt          *string  `json:"joined_at"`
	InvitedAt         *string  `json:"invited_at"`
	InviteExpiresAt   *string  `json:"invite_expires_at"`
	IsYou             bool     `json:"is_you"`
	Version           int      `json:"version"`

	active bool
	uid    uuid.UUID
}

func (s StaffOut) has(role string) bool { return slices.Contains(s.Roles, role) }

const staffSelect = `SELECT u.id, COALESCE(u.full_name, u.display_name), u.display_name, u.email::text, u.specialty, u.is_active,
	ARRAY(SELECT r.role FROM user_roles r WHERE r.user_id = u.id AND r.role IN ('clinician', 'analyst', 'hospital_admin')),
	COALESCE(m.enabled, false), u.password_hash IS NOT NULL, u.oidc_subject IS NOT NULL,
	GREATEST(u.last_login_at, (SELECT max(s.last_seen_at) FROM sessions s WHERE s.user_id = u.id)),
	u.created_at, u.staff_version,
	COALESCE((SELECT hs.require_mfa_for_clinicians FROM hospital_settings hs WHERE hs.hospital_id = u.hospital_id), true)
	FROM users u LEFT JOIN user_mfa m ON m.user_id = u.id
	WHERE u.hospital_id = $1`

func scanStaff(row pgx.Row, me uuid.UUID) (StaffOut, error) {
	var s StaffOut
	var mfa, pwd, sso, requireClin bool
	var last *time.Time
	var joined time.Time
	if err := row.Scan(&s.uid, &s.FullName, &s.DisplayName, &s.Email, &s.Specialty, &s.active, &s.Roles, &mfa, &pwd, &sso,
		&last, &joined, &s.Version, &requireClin); err != nil {
		return s, err
	}
	s.ID = s.uid.String()
	s.Roles = sortStaffRoles(s.Roles)
	s.Status = "deactivated"
	if s.active {
		s.Status = "active"
	}
	s.Issues = []string{}
	if !mfa && authz.NeedsMFA(s.Roles, requireClin) {
		s.Issues = append(s.Issues, "mfa_not_set_up")
	}
	var method string
	switch {
	case sso:
		method = "sso"
	case pwd && mfa:
		method = "password_mfa"
	case pwd:
		method = "password"
	}
	if method != "" {
		s.SignIn = &method
	}
	// Core has no reviewer assignment: every active clinician reviews every
	// correction, in every specialty (empty means all).
	s.CanReview = s.active && s.has(authz.RoleClinician)
	s.ReviewSpecialties = []string{}
	s.LastActiveAt = stampPtr(last)
	s.JoinedAt = stampPtr(&joined)
	s.IsYou = s.uid == me
	return s, nil
}

func sortStaffRoles(in []string) []string {
	out := []string{}
	for _, r := range StaffRoles {
		if slices.Contains(in, r) {
			out = append(out, r)
		}
	}
	return out
}

// loadPerson reads one member of staff of the caller's hospital (lock:
// FOR UPDATE of the account row).
func loadPerson(ctx context.Context, tx pgx.Tx, p authz.Principal, id uuid.UUID, lock bool) (StaffOut, error) {
	q := staffSelect + ` AND u.id = $2`
	if lock {
		q += ` FOR UPDATE OF u`
	}
	s, err := scanStaff(tx.QueryRow(ctx, q, p.HospitalID, id), p.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, errNotFound
	}
	return s, err
}

// staffList is GET /hospital/staff: everyone at the caller's hospital.
func (d Deps) staffList(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	out := []StaffOut{}
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), staffSelect+` ORDER BY u.display_name, u.id`, p.HospitalID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			s, err := scanStaff(rows, p.UserID)
			if err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		d.fail(w, r, p, "staff list", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

// optString is a JSON field that may be absent, null or a string.
type optString struct {
	set bool
	val *string
}

func (o *optString) UnmarshalJSON(b []byte) error {
	o.set = true
	if string(b) == "null" {
		o.val = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	o.val = &s
	return nil
}

type patchIn struct {
	Version           *int      `json:"version"`
	Roles             *[]string `json:"roles"`
	Specialty         optString `json:"specialty"`
	CanReview         *bool     `json:"can_review"`
	ReviewSpecialties *[]string `json:"review_specialties"`
	stepUpIn
}

// change is a checked PATCH: the roles to add and remove, and the
// specialty.
type change struct {
	add, remove      []string
	setSpecialty     bool
	specialty        *string
	selfAdds         bool
	specialtyChanged bool
}

func (c change) empty() bool { return len(c.add) == 0 && len(c.remove) == 0 && !c.specialtyChanged }

// validRoles checks a new role set: one to three staff roles, each once,
// and Analyst only with Clinician.
func validRoles(roles []string) error {
	if len(roles) == 0 || len(roles) > len(StaffRoles) {
		return errInvalid
	}
	seen := map[string]bool{}
	for _, r := range roles {
		if !slices.Contains(StaffRoles, r) || seen[r] {
			return errInvalid
		}
		seen[r] = true
	}
	if seen[authz.RoleAnalyst] && !seen[authz.RoleClinician] {
		return errNotEnabled
	}
	return nil
}

// validPatch checks the body on its own (422 before anything is read).
func validPatch(in patchIn) error {
	if in.Version == nil || *in.Version < 1 {
		return errInvalid
	}
	if in.Roles == nil && !in.Specialty.set && in.CanReview == nil && in.ReviewSpecialties == nil {
		return errInvalid
	}
	if in.Roles != nil {
		if err := validRoles(*in.Roles); err != nil {
			return err
		}
	}
	if in.Specialty.set && in.Specialty.val != nil && !specialtyRe.MatchString(*in.Specialty.val) {
		return errInvalid
	}
	if in.ReviewSpecialties != nil && len(*in.ReviewSpecialties) > 0 {
		// Core has no per-specialty reviewing (frontend change recorded).
		return errInvalid
	}
	return nil
}

// plan works out what a valid PATCH changes for person s, and refuses it
// on the version, a self change or the last admin. activeAdmins is the
// hospital's active hospital admins other than s.
func plan(in patchIn, s StaffOut, activeAdmins int) (change, error) {
	var c change
	if *in.Version != s.Version {
		return c, errConflict
	}
	if in.Roles != nil {
		for _, r := range *in.Roles {
			if !s.has(r) {
				c.add = append(c.add, r)
			}
		}
		for _, r := range s.Roles {
			if !slices.Contains(*in.Roles, r) {
				c.remove = append(c.remove, r)
			}
		}
	}
	newRoles := s.Roles
	if in.Roles != nil {
		newRoles = *in.Roles
	}
	if in.CanReview != nil && *in.CanReview != (s.active && slices.Contains(newRoles, authz.RoleClinician)) {
		// Reviewing follows the clinician role (frontend change recorded).
		return c, errInvalid
	}
	if s.IsYou {
		if slices.Contains(c.remove, authz.RoleHospitalAdmin) {
			return c, errSelf
		}
		if slices.Contains(c.add, authz.RoleAnalyst) {
			return c, errSelf
		}
		c.selfAdds = len(c.add) > 0
	}
	if slices.Contains(c.remove, authz.RoleHospitalAdmin) && s.active && activeAdmins == 0 {
		return c, errLastAdmin
	}
	if in.Specialty.set {
		c.setSpecialty = true
		c.specialty = in.Specialty.val
		if c.specialty != nil {
			t := strings.TrimSpace(*c.specialty)
			c.specialty = &t
		}
		c.specialtyChanged = !eqPtr(c.specialty, s.Specialty)
	}
	return c, nil
}

func eqPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// otherActiveAdmins counts the hospital's active hospital admins other
// than id who can use the admin routes (two-step on).
func otherActiveAdmins(ctx context.Context, tx pgx.Tx, hid, id uuid.UUID) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM users u JOIN user_roles r ON r.user_id = u.id AND r.role = 'hospital_admin'
		WHERE u.hospital_id = $1 AND u.is_active AND u.id <> $2
		  AND EXISTS (SELECT 1 FROM user_mfa m WHERE m.user_id = u.id AND m.enabled)`, hid, id).Scan(&n)
	return n, err
}

// lockStaff serialises the hospital's staff writes (the last-admin rule
// under concurrency).
func lockStaff(ctx context.Context, tx pgx.Tx, hid uuid.UUID) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('mhai_staff:' || $1::text, 0))`, hid)
	return err
}

// staffPatch is PATCH /hospital/staff/{id} {version, roles?, specialty?,
// can_review?, review_specialties?, mfa_code | backup_code}.
func (d Deps) staffPatch(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := canonicalID(r.PathValue("id"))
	if !ok || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	var in patchIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	if err := validPatch(in); err != nil {
		d.fail(w, r, p, "staff edit", err)
		return
	}
	ctx := r.Context()
	// The refusals that don't need the code, read first.
	var c change
	var person StaffOut
	err := d.tx(ctx, func(tx pgx.Tx) error {
		s, err := loadPerson(ctx, tx, p, id, false)
		if err != nil {
			return err
		}
		n, err := otherActiveAdmins(ctx, tx, p.HospitalID, id)
		if err != nil {
			return err
		}
		person = s
		c, err = plan(in, s, n)
		return err
	})
	if err != nil {
		d.fail(w, r, p, "staff edit", err)
		return
	}
	if c.empty() {
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, http.StatusOK, person)
		return
	}
	if !d.stepUp(w, r, in.stepUpIn) {
		return
	}
	granted := false
	err = d.tx(ctx, func(tx pgx.Tx) error {
		if err := lockStaff(ctx, tx, p.HospitalID); err != nil {
			return err
		}
		s, err := loadPerson(ctx, tx, p, id, true)
		if err != nil {
			return err
		}
		n, err := otherActiveAdmins(ctx, tx, p.HospitalID, id)
		if err != nil {
			return err
		}
		c, err = plan(in, s, n)
		if err != nil {
			return err
		}
		if c.empty() {
			// A concurrent edit already made it: nothing to write.
			person = s
			return nil
		}
		granted, err = d.apply(ctx, tx, p, id, c)
		if err != nil {
			return err
		}
		person, err = loadPerson(ctx, tx, p, id, false)
		return err
	})
	if err != nil {
		d.fail(w, r, p, "staff edit", err)
		return
	}
	if granted {
		d.notify()(ctx, p.HospitalID, id)
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, person)
}

// apply makes change c to person id in tx, with its audit entries.
// granted reports an Analyst grant (the notice goes after commit).
func (d Deps) apply(ctx context.Context, tx pgx.Tx, p authz.Principal, id uuid.UUID, c change) (granted bool, err error) {
	rid := httpx.RequestIDFrom(ctx)
	// Clinician first, so an Analyst grant in the same edit finds it.
	if slices.Contains(c.add, authz.RoleClinician) {
		if _, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'clinician')`, id); err != nil {
			return false, err
		}
	}
	if slices.Contains(c.remove, authz.RoleAnalyst) {
		if _, err := analysis.RemoveAnalyst(ctx, tx, id, rid); err != nil {
			return false, err
		}
	}
	if slices.Contains(c.add, authz.RoleHospitalAdmin) {
		if _, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'hospital_admin')`, id); err != nil {
			return false, err
		}
	}
	for _, role := range []string{authz.RoleHospitalAdmin, authz.RoleClinician} {
		if slices.Contains(c.remove, role) {
			if _, err := tx.Exec(ctx, `DELETE FROM user_roles WHERE user_id = $1 AND role = $2`, id, role); err != nil {
				return false, err
			}
		}
	}
	if slices.Contains(c.add, authz.RoleAnalyst) {
		if granted, err = analysis.GrantAnalyst(ctx, tx, id, rid); err != nil {
			return false, err
		}
	}
	// An Analyst role that left with Clinician (00030's trigger) is audited
	// here, or the database refuses the commit.
	if _, err := analysis.AuditAnalystChanges(ctx, tx, rid); err != nil {
		return false, err
	}
	if c.setSpecialty && c.specialtyChanged {
		if _, err := tx.Exec(ctx, `UPDATE users SET specialty = $2 WHERE id = $1`, id, c.specialty); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET staff_version = staff_version + 1 WHERE id = $1`, id); err != nil {
		return false, err
	}
	ev := func(action string, payload map[string]any) error {
		return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
			RequestID: rid, Action: action, Entity: "user", EntityID: id.String(), Payload: payload})
	}
	added, removed := withoutAnalyst(c.add), withoutAnalyst(c.remove)
	if len(added) > 0 || len(removed) > 0 {
		if err := ev("staff.roles_changed", map[string]any{"added": added, "removed": removed, "self": c.selfAdds}); err != nil {
			return false, err
		}
	}
	if c.specialtyChanged {
		if err := ev("staff.specialty_changed", map[string]any{}); err != nil {
			return false, err
		}
	}
	return granted, nil
}

func withoutAnalyst(in []string) []string {
	out := []string{}
	for _, r := range in {
		if r != authz.RoleAnalyst {
			out = append(out, r)
		}
	}
	return out
}

// decodeOptional reads a JSON object body that may be empty (an action
// with no fields but the step-up code).
func decodeOptional(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, httpx.MaxBody+1))
	if err != nil || len(raw) > httpx.MaxBody {
		httpx.Error(w, r, "validation_failed")
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return httpx.Decode(w, r, v)
}

// staffStatus is POST /hospital/staff/{id}/deactivate or /reactivate
// {mfa_code | backup_code}. A deactivation revokes every session of the
// person (denylisted after commit), forgets their trusted devices and
// removes Analyst.
func (d Deps) staffStatus(activate bool) http.Handler {
	what := "staff deactivate"
	if activate {
		what = "staff reactivate"
	}
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
		check := func(tx pgx.Tx, lock bool) error {
			s, err := loadPerson(ctx, tx, p, id, lock)
			if err != nil {
				return err
			}
			if s.IsYou {
				return errSelf
			}
			if s.active == activate {
				return errConflict
			}
			if !activate && s.has(authz.RoleHospitalAdmin) {
				n, err := otherActiveAdmins(ctx, tx, p.HospitalID, id)
				if err != nil {
					return err
				}
				if n == 0 {
					return errLastAdmin
				}
			}
			return nil
		}
		if err := d.tx(ctx, func(tx pgx.Tx) error { return check(tx, false) }); err != nil {
			d.fail(w, r, p, what, err)
			return
		}
		if !d.stepUp(w, r, in) {
			return
		}
		revoked := map[string][]string{}
		var person StaffOut
		err := d.tx(ctx, func(tx pgx.Tx) error {
			if err := lockStaff(ctx, tx, p.HospitalID); err != nil {
				return err
			}
			if err := check(tx, true); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE users SET is_active = $2, staff_version = staff_version + 1 WHERE id = $1`,
				id, activate); err != nil {
				return err
			}
			action, payload := "staff.reactivated", map[string]any{}
			if !activate {
				rows, err := tx.Query(ctx, `UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL
					RETURNING id, access_jti`, id)
				if err != nil {
					return err
				}
				for rows.Next() {
					var sid uuid.UUID
					var jti *uuid.UUID
					if err := rows.Scan(&sid, &jti); err != nil {
						rows.Close()
						return err
					}
					var js []string
					if jti != nil {
						js = append(js, jti.String())
					}
					revoked[sid.String()] = js
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					return err
				}
				// Trusted devices go too, so a reactivation brings back no
				// two-step bypass, and Analyst is removed through 00030's
				// function (audited), so a reactivation needs a fresh grant.
				if _, err := tx.Exec(ctx, `DELETE FROM trusted_devices WHERE user_id = $1`, id); err != nil {
					return err
				}
				if _, err := analysis.RemoveAnalyst(ctx, tx, id, httpx.RequestIDFrom(ctx)); err != nil {
					return err
				}
				action, payload = "staff.deactivated", map[string]any{"sessions_revoked": len(revoked)}
			}
			if err := audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
				RequestID: httpx.RequestIDFrom(ctx), Action: action, Entity: "user", EntityID: id.String(),
				Payload: payload}); err != nil {
				return err
			}
			var err error
			person, err = loadPerson(ctx, tx, p, id, false)
			return err
		})
		if err != nil {
			d.fail(w, r, p, what, err)
			return
		}
		if len(revoked) > 0 && d.Auth != nil {
			// The database's revocation stands; the denylist only makes it
			// immediate (the guard's cache holds state for 30 seconds).
			if err := d.Auth.DenySessions(context.WithoutCancel(ctx), revoked); err != nil {
				d.log().Error("hospital: denylist after deactivation failed; sessions end when the guard's cache expires", "request_id", httpx.RequestIDFrom(ctx),
					"hospital_id", p.HospitalID.String(), "user_id", id.String())
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, http.StatusOK, person)
	})
}
