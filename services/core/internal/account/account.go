// Package account creates hospitals and users. Only the seed CLI calls it,
// on a provisioning login (a plain member of mhai_provision, see migration
// 00004): the gateway's own roles cannot insert users, roles or hospitals,
// and the provisioning role can do little else. The platform app's add
// hospital (internal/platform, migration 00059) shares its checks only
// (CheckNewHospital, NormaliseEmail, ValidEmail), never its writes.
package account

import (
	"errors"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	ErrBadHospitalName  = errors.New("hospital name must be 1 to 120 characters, with no control characters")
	ErrBadShortCode     = errors.New("short code must be 2 to 32 letters, digits, '-' or '_'")
	ErrBadTimeZone      = errors.New("unknown time zone; use an IANA name such as Africa/Lagos")
	ErrEmailTaken       = errors.New("an account with that email already exists")
	ErrShortCodeTaken   = errors.New("a hospital with that short code already exists")
	ErrHospitalNotFound = errors.New("no hospital with that short code")
	// The analyst role is granted to an existing clinician with two-step,
	// by a hospital admin, under the database's rules (migration 00030).
	ErrAnalystAtCreation = errors.New("analyst is not given at creation; grant it later with core users roles <email> --add analyst --by <admin email>")
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

// ValidEmail reports whether s, normalised, is a plain address (a
// hospital admin's invite takes the CLI's rule).
func ValidEmail(s string) bool { return validEmail(NormaliseEmail(s)) }

func validEmail(norm string) bool {
	if norm == "" || len(norm) > 254 {
		return false
	}
	a, err := mail.ParseAddress(norm)
	return err == nil && a.Name == "" && a.Address == norm
}

// timeZoneMax bounds a time zone name before it is looked up.
const timeZoneMax = 64

// ValidTimeZone reports whether tz is an IANA zone name (Africa/Lagos,
// UTC), as CheckNewHospital takes it; "Local" is not one.
func ValidTimeZone(tz string) bool { return validTimeZone(tz) == nil }

// HospitalNameMax is a hospital name's longest, in characters (mhai-web's
// NAME_MAX).
const HospitalNameMax = 120

// NormaliseHospitalName trims a hospital name; ok is false unless it is 1
// to HospitalNameMax characters of valid UTF-8 with no control characters.
func NormaliseHospitalName(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > HospitalNameMax ||
		strings.ContainsFunc(s, unicode.IsControl) {
		return "", false
	}
	return s, true
}

// CheckNewHospital is the one check of a new hospital's name, short code
// and time zone, for the CLI (CreateHospital) and the platform app (POST
// /platform/hospitals). It returns the name trimmed and the code
// normalised.
func CheckNewHospital(name, shortCode, timeZone string) (string, string, error) {
	code, ok := NormaliseShortCode(shortCode)
	if !ok {
		return "", "", ErrBadShortCode
	}
	n, ok := NormaliseHospitalName(name)
	if !ok {
		return "", "", ErrBadHospitalName
	}
	if err := validTimeZone(timeZone); err != nil {
		return "", "", err
	}
	return n, code, nil
}

func validTimeZone(tz string) error {
	if tz == "" || tz == "Local" || len(tz) > timeZoneMax {
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
	if err := ValidateRoles(u.Roles, code); err != nil {
		return err
	}
	if slices.Contains(u.Roles, "analyst") {
		return ErrAnalystAtCreation
	}
	return nil
}
