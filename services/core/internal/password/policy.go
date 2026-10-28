package password

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// MinLength is the shortest accepted password, in characters.
const MinLength = 12

// minEmailLocalLen is the shortest email local part that Check refuses
// inside a password.
const minEmailLocalLen = 4

// Every error here maps to 422 weak_password.
var (
	ErrTooShort      = errors.New("password: shorter than 12 characters")
	ErrBreached      = errors.New("password: appears in a breached-password list")
	ErrPattern       = errors.New("password: one repeated character, a sequence or a keyboard run")
	ErrContainsEmail = errors.New("password: contains the account's email name")
)

// Check applies the password policy: at least 12 characters (after Unicode
// NFC normalisation); not one repeated character, an ascending or descending
// run of letters or digits, or a keyboard row, covering the whole password;
// not containing the email's local part when that is 4 or more characters;
// and not on the bundled breached list (compared without regard to case).
// email may be empty. Errors never contain the password or the email.
func Check(pw, email string) error {
	pw = norm.NFC.String(pw)
	if utf8.RuneCountInString(pw) < MinLength {
		return ErrTooShort
	}
	lower := strings.ToLower(pw)
	if isPattern(lower) {
		return ErrPattern
	}
	if local := emailLocal(email); utf8.RuneCountInString(local) >= minEmailLocalLen && strings.Contains(lower, local) {
		return ErrContainsEmail
	}
	if isBreached(pw) {
		return ErrBreached
	}
	return nil
}

func emailLocal(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	if i := strings.LastIndex(email, "@"); i > 0 {
		return norm.NFC.String(email[:i])
	}
	return ""
}

var keyboardRows = []string{"qwertyuiop", "asdfghjkl", "zxcvbnm", "1234567890", "qwertyuiopasdfghjklzxcvbnm"}

// isPattern reports whether the lowercased password is a single repeated
// character, an ascending or descending run of letters or digits, or a
// stretch of a keyboard row (either direction, wrapping around).
func isPattern(s string) bool {
	rs := []rune(s)
	same, up, down := true, true, true
	for i := 1; i < len(rs); i++ {
		if rs[i] != rs[0] {
			same = false
		}
		if rs[i] != rs[i-1]+1 || !alnumPair(rs[i-1], rs[i]) {
			up = false
		}
		if rs[i] != rs[i-1]-1 || !alnumPair(rs[i-1], rs[i]) {
			down = false
		}
	}
	if same || up || down {
		return true
	}
	for _, row := range keyboardRows {
		rev := reverse(row)
		for _, r := range []string{row, rev} {
			if strings.Contains(strings.Repeat(r, len(s)/len(r)+2), s) {
				return true
			}
		}
	}
	return false
}

// alnumPair is true when a and b are both letters or both digits, so a run
// can't step across the gap between '9' and ':' or 'z' and '{'.
func alnumPair(a, b rune) bool {
	return (unicode.IsLetter(a) && unicode.IsLetter(b)) || (unicode.IsDigit(a) && unicode.IsDigit(b))
}

func reverse(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}
