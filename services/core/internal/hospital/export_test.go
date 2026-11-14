package hospital

import "net/http"

// WithStepUpCheck returns d with its step-up check replaced (tests only).
func WithStepUpCheck(d Deps, f func(r *http.Request, mfaCode, backupCode string) error) Deps {
	d.stepUpCheck = f
	return d
}

// Handlers maps "METHOD pattern" to each route's handler, for tests that
// serve them with a principal of their own in the context.
func Handlers(d Deps) map[string]http.Handler {
	out := map[string]http.Handler{}
	for _, rt := range routes(d) {
		out[rt.Method+" "+rt.Pattern] = rt.Handler
	}
	return out
}

// Plan exposes plan for the unit tests.
func Plan(version int, roles []string, specialty *string, s StaffOut, activeAdmins int) (add, remove []string, self bool, err error) {
	in := patchIn{Version: &version}
	if roles != nil {
		in.Roles = &roles
	}
	if specialty != nil {
		in.Specialty = optString{set: true, val: specialty}
	}
	c, err := plan(in, s, activeAdmins)
	return c.add, c.remove, c.selfAdds, err
}

// ValidRoles exposes validRoles.
func ValidRoles(roles []string) error { return validRoles(roles) }

// SettingsChange exposes settingsChange.
func SettingsChange(review, idle *int, requireMFA, trust *bool, cur SettingsOut) ([]string, bool) {
	return settingsChange(settingsIn{ReviewOverdueHours: review, IdleTimeoutMinutes: idle,
		RequireMFAForClinicians: requireMFA, AllowTrustDevice: trust}, cur)
}

// CodeFor exposes codeFor.
var CodeFor = codeFor

// Errors for the unit tests.
var (
	ErrSelf      = errSelf
	ErrLastAdmin = errLastAdmin
	ErrConflict  = errConflict
	ErrInvalid   = errInvalid
	ErrEligible  = errNotEnabled
)

// Person builds a member of staff for the unit tests.
func Person(roles []string, version int, you, active bool) StaffOut {
	return StaffOut{Roles: roles, Version: version, IsYou: you, active: active}
}
