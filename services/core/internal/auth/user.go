package auth

import (
	"context"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Role ranks: the primary role is the first present in this order
// (clinical: clinician > hospital_admin > analyst; platform:
// platform_admin > auditor).
var roleRank = map[string]int{
	"clinician": 0, "hospital_admin": 1, "analyst": 2,
	"platform_admin": 3, "auditor": 4,
}

var platformRoles = map[string]bool{"platform_admin": true, "auditor": true}

// mfaAlways are the roles that always need two-step verification, and that
// a trusted device never excuses.
var mfaAlways = map[string]bool{"hospital_admin": true, "analyst": true, "platform_admin": true, "auditor": true}

// normaliseRole maps the read alias admin to platform_admin.
func normaliseRole(r string) string {
	if r == "admin" {
		return "platform_admin"
	}
	return r
}

// sortRoles returns the known roles in rank order, deduplicated, with the
// alias mapped. Unknown roles are dropped.
func sortRoles(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, r := range in {
		r = normaliseRole(r)
		if _, ok := roleRank[r]; ok && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return roleRank[out[i]] < roleRank[out[j]] })
	return out
}

func primaryRole(roles []string) string {
	s := sortRoles(roles)
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func hasPlatformRole(roles []string) bool {
	for _, r := range roles {
		if platformRoles[normaliseRole(r)] {
			return true
		}
	}
	return false
}

func hasClinicalRole(roles []string) bool {
	for _, r := range roles {
		r = normaliseRole(r)
		if _, ok := roleRank[r]; ok && !platformRoles[r] {
			return true
		}
	}
	return false
}

// needsMFA: hospital admins, analysts, platform admins and auditors
// always; clinicians when their hospital requires it.
func needsMFA(roles []string, requireForClinicians bool) bool {
	for _, r := range roles {
		r = normaliseRole(r)
		if mfaAlways[r] || (r == "clinician" && requireForClinicians) {
			return true
		}
	}
	return false
}

// deviceMaySkip reports whether a trusted device may stand in for the code:
// only for an account none of whose roles is in mfaAlways.
func deviceMaySkip(roles []string) bool {
	for _, r := range roles {
		if mfaAlways[normaliseRole(r)] {
			return false
		}
	}
	return len(roles) > 0
}

// mfaPolicy is the hospital's two-step policy as it applies to an account.
type mfaPolicy struct {
	RequireForClinicians bool
	AllowTrustDevice     bool
}

// sessionSatisfiesMFA reports whether a session's proof (mfa, amr) meets
// the account's current roles and policy. Sign-in and every refresh use it,
// so a role gained or a policy tightened mid-session ends the session.
//   - No two-step needed: any session will do.
//   - Otherwise mfa must be set, and amr must show the proof: "otp" or
//     "backup" (a code), or "device", which counts only while the hospital
//     allows trusted devices and no role forbids them.
func sessionSatisfiesMFA(roles []string, p mfaPolicy, mfa bool, amr []string) bool {
	if !needsMFA(roles, p.RequireForClinicians) {
		return true
	}
	if !mfa {
		return false
	}
	if slices.Contains(amr, "otp") || slices.Contains(amr, "backup") {
		return true
	}
	if slices.Contains(amr, "device") {
		return p.AllowTrustDevice && deviceMaySkip(roles)
	}
	return false
}

func (a *acctRec) policy() mfaPolicy {
	return mfaPolicy{RequireForClinicians: a.RequireMFAClinicians, AllowTrustDevice: a.AllowTrustDevice}
}

// acctRec is a user as sign-in and refresh see it.
type acctRec struct {
	ID                   uuid.UUID
	HospitalID           *uuid.UUID
	Email                string
	PasswordHash         *string
	DisplayName          string
	FullName, RoleTitle  *string
	Specialty            *string
	Active, MustChangePW bool
	TermsAcceptedAt      *time.Time
	HospitalName         *string
	HospitalStatus       *string
	IdleMinutes          int
	ReviewHours          int
	RequireMFAClinicians bool
	AllowTrustDevice     bool
	MFAEnabled           bool
	RecoveryCodesLeft    int
	Roles                []string
}

func (a *acctRec) hospitalID() string {
	if a.HospitalID == nil {
		return ""
	}
	return a.HospitalID.String()
}

// usable reports whether the account may hold a session: active, with a
// consistent role set, and (for clinical accounts) a hospital that is
// running. Deactivated or removed hospitals are refused.
func (a *acctRec) usable() bool {
	if !a.Active || len(a.Roles) == 0 {
		return false
	}
	if a.HospitalID == nil {
		return hasPlatformRole(a.Roles) && !hasClinicalRole(a.Roles)
	}
	if hasPlatformRole(a.Roles) || a.HospitalStatus == nil {
		return false
	}
	switch *a.HospitalStatus {
	case "active", "setup_incomplete":
		return true
	}
	return false
}

// accountSelect reads everything sign-in needs in one statement (mhai_auth).
// See queries/auth.sql.
const accountSelect = `
SELECT u.id, u.hospital_id, u.email, u.password_hash, u.display_name, u.full_name, u.role_title, u.specialty,
       u.is_active, u.must_change_password, u.terms_accepted_at,
       h.name, h.status,
       COALESCE(hs.idle_timeout_minutes, $2), COALESCE(hs.review_overdue_hours, $3),
       COALESCE(hs.require_mfa_for_clinicians, true), COALESCE(hs.allow_trust_device, false),
       COALESCE(m.enabled, false), CASE WHEN m.enabled THEN cardinality(m.backup_code_hashes) ELSE 0 END,
       ARRAY(SELECT r.role FROM user_roles r WHERE r.user_id = u.id)
FROM users u
LEFT JOIN hospitals h          ON h.id = u.hospital_id
LEFT JOIN hospital_settings hs ON hs.hospital_id = u.hospital_id
LEFT JOIN user_mfa m           ON m.user_id = u.id
WHERE `

var errNoAccount = errors.New("auth: no such account")

func scanAccount(row pgx.Row) (*acctRec, error) {
	a := &acctRec{}
	err := row.Scan(&a.ID, &a.HospitalID, &a.Email, &a.PasswordHash, &a.DisplayName, &a.FullName, &a.RoleTitle, &a.Specialty,
		&a.Active, &a.MustChangePW, &a.TermsAcceptedAt, &a.HospitalName, &a.HospitalStatus,
		&a.IdleMinutes, &a.ReviewHours, &a.RequireMFAClinicians, &a.AllowTrustDevice,
		&a.MFAEnabled, &a.RecoveryCodesLeft, &a.Roles)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoAccount
	}
	if err != nil {
		return nil, errors.New("auth: account query failed")
	}
	a.Roles = sortRoles(a.Roles)
	return a, nil
}

// accountByEmail looks up a normalised email (citext, so case never matters).
func accountByEmail(ctx context.Context, tx pgx.Tx, email string) (*acctRec, error) {
	return scanAccount(tx.QueryRow(ctx, accountSelect+"u.email = $1", email, defaultIdleMinutes, defaultReviewHours))
}

func accountByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*acctRec, error) {
	return scanAccount(tx.QueryRow(ctx, accountSelect+"u.id = $1", id, defaultIdleMinutes, defaultReviewHours))
}

type hospitalRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// userView is the login `user`: exactly the contract's fields (§2.2).
type userView struct {
	ID                 string       `json:"id"`
	Role               string       `json:"role"`
	Roles              []string     `json:"roles"`
	Email              string       `json:"email"`
	DisplayName        string       `json:"display_name"`
	FullName           *string      `json:"full_name"`
	RoleTitle          *string      `json:"role_title"`
	Specialty          *string      `json:"specialty"`
	Hospital           *hospitalRef `json:"hospital"`
	MFAEnabled         bool         `json:"mfa_enabled"`
	MustChangePassword bool         `json:"must_change_password"`
	TermsAcceptedAt    *time.Time   `json:"terms_accepted_at"`
	RecoveryCodesLeft  int          `json:"recovery_codes_left"`
	IdleTimeoutMinutes int          `json:"idle_timeout_minutes"`
	ReviewOverdueHours int          `json:"review_overdue_hours"`
}

func (a *acctRec) view() userView {
	v := userView{
		ID: a.ID.String(), Role: primaryRole(a.Roles), Roles: a.Roles, Email: a.Email,
		DisplayName: a.DisplayName, FullName: a.FullName, RoleTitle: a.RoleTitle, Specialty: a.Specialty,
		MFAEnabled: a.MFAEnabled, MustChangePassword: a.MustChangePW, TermsAcceptedAt: a.TermsAcceptedAt,
		RecoveryCodesLeft: a.RecoveryCodesLeft, IdleTimeoutMinutes: a.IdleMinutes, ReviewOverdueHours: a.ReviewHours,
	}
	if a.HospitalID != nil && a.HospitalName != nil {
		v.Hospital = &hospitalRef{ID: a.HospitalID.String(), Name: *a.HospitalName}
	}
	return v
}
