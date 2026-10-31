// Package account creates hospitals and users. Only the seed CLI calls it,
// on a provisioning login (a plain member of mhai_provision, see migration
// 00004): the gateway's own roles cannot insert users, roles or hospitals,
// and the provisioning role can do little else.
package account

import (
	"errors"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
)

// NormaliseEmail trims surrounding space and lowercases, so the stored login
// is canonical and two accounts can't differ only by case. Every place that
// reads or writes a login email goes through it.
func NormaliseEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// Errors carry no input (no email, name or hospital code).
var (
	ErrRoleHospital     = errors.New("platform roles (platform_admin, auditor) need an account with no hospital; clinical roles (clinician, hospital_admin, analyst) need --hospital")
	ErrRoleMix          = errors.New("clinical roles can be combined with each other; a platform role must be the only role on its account")
	ErrBadRole          = errors.New("unknown, duplicate or missing role; use clinician, hospital_admin, analyst, platform_admin or auditor")
	ErrNotProvisioner   = errors.New("this database login cannot act as mhai_provision; connect as a login that is a member of it")
	ErrBadEmail         = errors.New("invalid email address")
	ErrBadName          = errors.New("name is required")
	ErrBadShortCode     = errors.New("short code must be 2 to 32 letters, digits, '-' or '_'")
	ErrBadTimeZone      = errors.New("unknown time zone; use an IANA name such as Africa/Lagos")
	ErrEmailTaken       = errors.New("an account with that email already exists")
	ErrShortCodeTaken   = errors.New("a hospital with that short code already exists")
	ErrHospitalNotFound = errors.New("no hospital with that short code")
)

var platformRoles = map[string]bool{"platform_admin": true, "auditor": true}

// clinicalRoles is authz's list (the roles a clinical route may admit), so
// the two can't drift apart (final review M-8).
var clinicalRoles = func() map[string]bool {
	m := map[string]bool{}
	for _, r := range authz.ClinicalRoles() {
		m[r] = true
	}
	return m
}()

// ValidateRoles checks the role set against whether the account has a
// hospital. The database triggers enforce the same pairing; this gives a
// clear message first.
func ValidateRoles(roles []string, hospitalShortCode string) error {
	if len(roles) == 0 {
		return ErrBadRole
	}
	seen := map[string]bool{}
	var plat, clin int
	for _, r := range roles {
		if seen[r] || (!platformRoles[r] && !clinicalRoles[r]) {
			return ErrBadRole
		}
		seen[r] = true
		if platformRoles[r] {
			plat++
		} else {
			clin++
		}
	}
	if plat > 0 && (clin > 0 || plat > 1) {
		return ErrRoleMix
	}
	if (plat > 0) != (hospitalShortCode == "") {
		return ErrRoleHospital
	}
	return nil
}

var shortCodeRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{1,31}$`)

// NormaliseShortCode upper-cases a hospital short code. ok is false if it is
// not a valid code.
func NormaliseShortCode(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !shortCodeRE.MatchString(s) {
		return "", false
	}
	return strings.ToUpper(s), true
}

func validEmail(norm string) bool {
	if norm == "" || len(norm) > 254 {
		return false
	}
	a, err := mail.ParseAddress(norm)
	return err == nil && a.Name == "" && a.Address == norm
}

func validTimeZone(tz string) error {
	if tz == "" {
		return ErrBadTimeZone
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return ErrBadTimeZone
	}
	return nil
}

// ValidateNewUser checks everything about u except the password, with no
// database access, so the CLI can fail before it asks for one.
func ValidateNewUser(u NewUser) error {
	if !validEmail(NormaliseEmail(u.Email)) {
		return ErrBadEmail
	}
	if strings.TrimSpace(u.Name) == "" {
		return ErrBadName
	}
	code := ""
	if strings.TrimSpace(u.HospitalShortCode) != "" {
		var ok bool
		if code, ok = NormaliseShortCode(u.HospitalShortCode); !ok {
			return ErrBadShortCode
		}
	}
	return ValidateRoles(u.Roles, code)
}
