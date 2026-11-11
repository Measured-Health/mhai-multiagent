package sink

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// tag is markup: '<', an optional '/', '!' or '?', a letter, then anything
// up to the next '>'. "<0.5" or "<5.0 and >2" is not markup and stays.
var tag = regexp.MustCompile(`<[/!?]?[A-Za-z][^<>]*>`)

// opener is a '<' (or a run of them) that could still open markup: one
// followed by a letter, '/', '!' or '?'.
var opener = regexp.MustCompile(`<+([A-Za-z/!?])`)

// CleanText makes source text tidy to store and show (EMR design §17.3 E4,
// A8): valid UTF-8 in NFC, control (C0, C1) and format (Cf: bidi
// overrides, zero-width) characters removed, markup removed until nothing
// changes (so nested or split tags such as "<scr<x>ipt>" can't rebuild
// one), any '<' left before a letter, '/', '!' or '?' dropped, whitespace
// collapsed to single spaces, at most max runes. It is used on every
// display string. A comparator such as "<0.5" or "> 2" stays.
//
// CleanText is not an XSS control. Source text stays untrusted: every
// renderer must HTML-escape it (mhai-web renders text only), and a prompt
// that includes it must treat it as untrusted input (design §10.5).
func CleanText(s string, max int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = norm.NFC.String(s)
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case unicode.Is(unicode.Cc, r), unicode.Is(unicode.Cf, r), r == utf8.RuneError:
			return -1
		}
		return r
	}, s)
	for {
		next := tag.ReplaceAllString(s, " ")
		next = opener.ReplaceAllString(next, "$1")
		if next == s {
			break
		}
		s = next
	}
	s = strings.Join(strings.Fields(s), " ")
	if max >= 0 && utf8.RuneCountInString(s) > max {
		r := []rune(s)
		s = strings.TrimSpace(string(r[:max]))
	}
	return s
}

// CSVCell neutralises a cell a spreadsheet would read as a formula (A7): a
// value beginning '=', '+', '-', '@', a tab or a carriage return gets a
// leading apostrophe before it is stored.
func CSVCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}
