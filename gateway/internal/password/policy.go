package password

import (
	"errors"
	"unicode/utf8"
)

// MinLength is the shortest accepted password, in characters.
const MinLength = 12

// ErrTooShort and ErrBreached both map to 422 weak_password.
var (
	ErrTooShort = errors.New("password: shorter than 12 characters")
	ErrBreached = errors.New("password: appears in a breached-password list")
)

// Check applies the password policy: at least 12 characters, and not on the
// bundled breached list (compared without regard to case). Errors never
// contain the password.
func Check(pw string) error {
	if utf8.RuneCountInString(pw) < MinLength {
		return ErrTooShort
	}
	if isBreached(pw) {
		return ErrBreached
	}
	return nil
}
