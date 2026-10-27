package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
)

// The role and two-step policy rules live in authz, shared with the
// principal middleware; these names keep the auth code readable.
type mfaPolicy = authz.MFAPolicy

func sortRoles(in []string) []string     { return authz.SortRoles(in) }
func hasPlatformRole(r []string) bool    { return authz.HasPlatformRole(r) }
func needsMFA(r []string, req bool) bool { return authz.NeedsMFA(r, req) }
func deviceMaySkip(r []string) bool      { return authz.DeviceMaySkip(r) }

func primaryRole(roles []string) string {
	s := sortRoles(roles)
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func sessionSatisfiesMFA(roles []string, p mfaPolicy, enrolled, mfa bool, amr []string) bool {
	return authz.SessionSatisfiesMFA(roles, p, enrolled, mfa, amr)
}

func needsCode(roles []string, p mfaPolicy, enrolled bool) bool {
	return authz.NeedsCode(roles, p, enrolled)
}

func deviceMayStandIn(roles []string, p mfaPolicy) bool { return authz.DeviceMayStandIn(roles, p) }

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

// usable reports whether the account may hold a session (authz.AccountUsable).
func (a *acctRec) usable() bool {
	return authz.AccountUsable(a.Active, a.HospitalID, a.HospitalStatus, a.Roles)
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
