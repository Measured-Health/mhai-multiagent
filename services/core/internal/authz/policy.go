package authz

import (
	"slices"
	"sort"

	"github.com/google/uuid"
)

// The roles (global constraints). Clinical accounts combine clinician,
// hospital_admin and analyst; platform accounts hold platform_admin or
// auditor. admin is read as platform_admin.
const (
	RoleClinician     = "clinician"
	RoleHospitalAdmin = "hospital_admin"
	RoleAnalyst       = "analyst"
	RolePlatformAdmin = "platform_admin"
	RoleAuditor       = "auditor"
)

// roleRank orders roles for the primary role (clinical: clinician >
// hospital_admin > analyst; platform: platform_admin > auditor).
var roleRank = map[string]int{
	RoleClinician: 0, RoleHospitalAdmin: 1, RoleAnalyst: 2,
	RolePlatformAdmin: 3, RoleAuditor: 4,
}

var platformRoles = map[string]bool{RolePlatformAdmin: true, RoleAuditor: true}

// mfaAlways are the roles that always need two-step verification, and that
// a trusted device never excuses.
var mfaAlways = map[string]bool{RoleHospitalAdmin: true, RoleAnalyst: true, RolePlatformAdmin: true, RoleAuditor: true}

// NormaliseRole maps the read alias admin to platform_admin.
func NormaliseRole(r string) string {
	if r == "admin" {
		return RolePlatformAdmin
	}
	return r
}

// KnownRole reports whether r (after the alias) is a role.
func KnownRole(r string) bool {
	_, ok := roleRank[NormaliseRole(r)]
	return ok
}

// IsPlatformRole reports whether r (after the alias) is a platform role.
func IsPlatformRole(r string) bool { return platformRoles[NormaliseRole(r)] }

// SortRoles returns the known roles in rank order, deduplicated, with the
// alias mapped. Unknown roles are dropped.
func SortRoles(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, r := range in {
		r = NormaliseRole(r)
		if _, ok := roleRank[r]; ok && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return roleRank[out[i]] < roleRank[out[j]] })
	return out
}

// HasPlatformRole reports whether any of roles is a platform role.
func HasPlatformRole(roles []string) bool {
	return slices.ContainsFunc(roles, IsPlatformRole)
}

// HasClinicalRole reports whether any of roles is a clinical role.
func HasClinicalRole(roles []string) bool {
	return slices.ContainsFunc(roles, func(r string) bool { return KnownRole(r) && !IsPlatformRole(r) })
}

// NeedsMFA: hospital admins, analysts, platform admins and auditors
// always; clinicians when their hospital requires it.
func NeedsMFA(roles []string, requireForClinicians bool) bool {
	for _, r := range roles {
		r = NormaliseRole(r)
		if mfaAlways[r] || (r == RoleClinician && requireForClinicians) {
			return true
		}
	}
	return false
}

// DeviceMaySkip reports whether a trusted device may stand in for the code:
// only for an account none of whose roles is in mfaAlways.
func DeviceMaySkip(roles []string) bool {
	for _, r := range roles {
		if mfaAlways[NormaliseRole(r)] {
			return false
		}
	}
	return len(roles) > 0
}

// MFAPolicy is the hospital's two-step policy as it applies to an account.
type MFAPolicy struct {
	RequireForClinicians bool
	AllowTrustDevice     bool
}

// NeedsCode reports whether sign-in must prove a second factor: the roles
// and policy require two-step, or the account has turned it on.
func NeedsCode(roles []string, p MFAPolicy, enrolled bool) bool {
	return enrolled || NeedsMFA(roles, p.RequireForClinicians)
}

// DeviceMayStandIn reports whether a trusted device may replace the code:
// the hospital allows trusted devices and no role forbids them.
func DeviceMayStandIn(roles []string, p MFAPolicy) bool {
	return p.AllowTrustDevice && DeviceMaySkip(roles)
}

// SessionSatisfiesMFA reports whether a session's proof (mfa, amr) meets
// the account's current roles and policy. Sign-in, every refresh and the
// principal middleware use it, so a role gained or a policy tightened
// mid-session ends the session.
//   - A code is needed when the roles and policy require two-step, or the
//     account has turned it on (enrolled).
//   - No code needed: any session will do.
//   - Otherwise mfa must be set, and amr must show the proof: "otp" or
//     "backup" (a code), or "device", which counts only while the hospital
//     allows trusted devices and no role forbids them.
func SessionSatisfiesMFA(roles []string, p MFAPolicy, enrolled, mfa bool, amr []string) bool {
	if !NeedsCode(roles, p, enrolled) {
		return true
	}
	if !mfa {
		return false
	}
	if slices.Contains(amr, "otp") || slices.Contains(amr, "backup") {
		return true
	}
	if slices.Contains(amr, "device") {
		return DeviceMayStandIn(roles, p)
	}
	return false
}

// HospitalRunning reports whether a hospital status lets its accounts hold
// a session. Sign-in, refresh and the principal middleware share it.
// setup_incomplete is allowed so a new hospital's admin can finish setup.
func HospitalRunning(status string) bool {
	switch status {
	case "active", "setup_incomplete":
		return true
	}
	return false
}

// AccountUsable reports whether an account may hold a session: active,
// with a consistent role set, and (for clinical accounts) a hospital that
// is running. A platform account has no hospital and only platform roles;
// a clinical account has a hospital and no platform role.
func AccountUsable(active bool, hospitalID *uuid.UUID, hospitalStatus *string, roles []string) bool {
	if !active || len(roles) == 0 {
		return false
	}
	if hospitalID == nil {
		return HasPlatformRole(roles) && !HasClinicalRole(roles)
	}
	if HasPlatformRole(roles) || hospitalStatus == nil {
		return false
	}
	return HospitalRunning(*hospitalStatus)
}
