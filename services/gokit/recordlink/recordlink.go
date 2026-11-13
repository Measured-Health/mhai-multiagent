// Package recordlink builds the "open record" link of an EMR alert (EMR
// design §5.1, A25; plan Task 25, departure 16): the platform admin's
// template (settings.record_link), the connection's base address and the
// source patient id. Core builds it at read time and the emr service's
// connectors delegate to it, so the rule is written once.
package recordlink

import (
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Placeholder is where the source patient id goes.
const Placeholder = "{id}"

// maxLen bounds the template and the result.
const maxLen = 500

// Build returns the link, or ("", false) for anything but an https
// template on the base address's host and port, with exactly one {id} in
// its path, no query, fragment, userinfo or dot segment, and an id that is
// printable, at most 256 bytes, and not "." or "..". The id is escaped with
// url.PathEscape, so it can't leave its path segment.
func Build(template, baseURL, sourcePatientID string) (string, bool) {
	if template == "" || len(template) > maxLen || strings.Count(template, Placeholder) != 1 ||
		strings.ContainsAny(template, "?#\\@") || !strings.HasPrefix(template, "https://") {
		return "", false
	}
	if !validID(sourcePatientID) {
		return "", false
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme != "https" || base.User != nil || base.Host == "" {
		return "", false
	}
	// The placeholder must be in the path: parse with a neutral stand-in.
	const stand = "MHAIRECORDID"
	if strings.Contains(template, stand) {
		return "", false
	}
	t, err := url.Parse(strings.Replace(template, Placeholder, stand, 1))
	if err != nil || t.Scheme != "https" || t.User != nil || t.Opaque != "" || t.RawQuery != "" || t.Fragment != "" ||
		t.ForceQuery || !strings.Contains(t.EscapedPath(), stand) || strings.Contains(t.Host, stand) {
		return "", false
	}
	for _, seg := range strings.Split(t.EscapedPath(), "/") {
		if seg == "." || seg == ".." {
			return "", false
		}
	}
	if !sameHost(t, base) {
		return "", false
	}
	host := strings.ToLower(t.Host)
	out := "https://" + host + strings.Replace(t.EscapedPath(), stand, url.PathEscape(sourcePatientID), 1)
	if len(out) > maxLen {
		return "", false
	}
	// The result must parse back to the same host.
	if u, err := url.Parse(out); err != nil || !sameHost(u, base) || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	return out, true
}

func validID(s string) bool {
	if s == "" || len(s) > 256 || s == "." || s == ".." || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	return "443"
}

func sameHost(a, b *url.URL) bool {
	return a.Hostname() != "" && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}
