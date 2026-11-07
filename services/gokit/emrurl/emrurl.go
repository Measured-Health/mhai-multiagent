// Package emrurl checks the addresses an EMR connection names and derives
// its credential origin (EMR design §4.1, §6.3, §6.4; plan Task 7). Core
// runs it at save and the emr service at connect, so the rule is written
// once. It is the early check, for a useful error: what a name resolves to
// is the egress proxy's to check (it resolves once and dials what it
// checked).
//
// An address is https (sftp for file_export), at most 500 characters, a
// DNS name of at least two labels (a loopback IP literal only in
// development, for test servers), with no userinfo, query or fragment. The host is
// lower-cased, IDNA-normalised and loses a trailing dot; the origin always
// names its port. Errors are fixed values and never carry the address.
package emrurl

import (
	"errors"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// Origin is scheme://host:port, the port always explicit.
type Origin string

// MaxLen is the longest address accepted, in bytes.
const MaxLen = 500

// The closed errors.
var (
	// ErrScheme: not one of the allowed schemes.
	ErrScheme = errors.New("emrurl: scheme not allowed")
	// ErrShape: too long, malformed, userinfo, a query or a fragment, a
	// bad port or host name.
	ErrShape = errors.New("emrurl: malformed address")
	// ErrAddress: an IP literal, localhost, a single-label or reserved
	// internal name.
	ErrAddress = errors.New("emrurl: address not allowed")
	// ErrLinkHost: a record link on a host other than the base address's.
	ErrLinkHost = errors.New("emrurl: record link not on the base address's host")
	// ErrType: not a connector type.
	ErrType = errors.New("emrurl: unknown connector type")
)

var defaultPort = map[string]string{"https": "443", "sftp": "22"}

// reservedSuffixes are names that resolve inside a network, or are
// special-use (RFC 6761 .test, .invalid, .localhost; RFC 7686 .onion;
// RFC 8375 .home.arpa, under .arpa), never a hospital's public system. They
// are refused in development too.
var reservedSuffixes = []string{".localhost", ".localdomain", ".local", ".internal", ".arpa", ".home", ".lan",
	".intranet", ".corp", ".test", ".invalid", ".onion"}

var lookup = idna.New(idna.MapForLookup(), idna.BidiRule(), idna.StrictDomainName(true), idna.Transitional(false))

// CheckURL checks raw against the rules above for one of the allowed
// schemes ("https", "sftp"), and returns it parsed with its host
// normalised, and its origin.
func CheckURL(raw string, allowed []string, development bool) (*url.URL, Origin, error) {
	if raw == "" || len(raw) > MaxLen {
		return nil, "", ErrShape
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c <= ' ' || c == 0x7f || c == '\\' {
			return nil, "", ErrShape
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", ErrShape
	}
	if u.Scheme == "" || !slices.Contains(allowed, u.Scheme) || defaultPort[u.Scheme] == "" ||
		!strings.HasPrefix(raw, u.Scheme+"://") {
		if u.Scheme != "" && slices.Contains(allowed, u.Scheme) {
			return nil, "", ErrShape // "https:host": no authority
		}
		return nil, "", ErrScheme
	}
	if u.Opaque != "" || u.User != nil || strings.ContainsAny(raw, "?#") || u.RawQuery != "" || u.Fragment != "" ||
		u.ForceQuery || u.Host == "" {
		return nil, "", ErrShape
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return nil, "", ErrShape
	}
	if port == "" {
		port = defaultPort[u.Scheme]
	} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return nil, "", ErrShape
	}
	host, err := normaliseHost(u.Hostname(), development)
	if err != nil {
		return nil, "", err
	}
	hp := host
	if strings.Contains(host, ":") {
		hp = "[" + host + "]"
	}
	if u.Port() != "" {
		u.Host = hp + ":" + port
	} else {
		u.Host = hp
	}
	return u, Origin(u.Scheme + "://" + hp + ":" + port), nil
}

// normaliseHost lower-cases, drops one trailing dot and IDNA-normalises a
// DNS name, and refuses addresses that aren't a public name.
func normaliseHost(h string, development bool) (string, error) {
	if h == "" {
		return "", ErrShape
	}
	if ip := net.ParseIP(h); ip != nil {
		// Development allows a loopback literal (test servers) and nothing
		// else: never a private, link-local or metadata address.
		if development && ip.IsLoopback() {
			return ip.String(), nil
		}
		return "", ErrAddress
	}
	if strings.ContainsAny(h, "[]%") {
		return "", ErrShape
	}
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	a, err := lookup.ToASCII(h)
	if err != nil || a == "" || len(a) > 253 {
		return "", ErrShape
	}
	labels := strings.Split(a, ".")
	for _, l := range labels {
		if !dnsLabel(l) {
			return "", ErrShape
		}
	}
	last := labels[len(labels)-1]
	if allDigits(last) || strings.HasPrefix(last, "0x") {
		return "", ErrAddress // a numeric form of an IP address (127.1, 2130706433, 0x7f.0.0.1)
	}
	if len(labels) < 2 || a == "localhost" {
		return "", ErrAddress // a single label is a local or compose service name
	}
	for _, s := range reservedSuffixes {
		if strings.HasSuffix("."+a, s) {
			return "", ErrAddress
		}
	}
	return a, nil
}

func dnsLabel(l string) bool {
	if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// Schemes is the scheme each connector type's base address must use
// (design §4.1; stub is the development connector).
var Schemes = map[string]string{
	"fhir_r4":          "https",
	"document_archive": "https",
	"file_export":      "sftp",
	"stub":             "https",
}

// URLSettings are the settings that hold an address, by type: each gets the
// base address's checks (design §4.1).
var URLSettings = map[string][]string{
	"fhir_r4":          {"token_url", "record_link"},
	"document_archive": {"record_link"},
	"file_export":      {"record_link"},
	"stub":             {"record_link"},
}

// OriginsOf is a connection's credential origin (design §6.3): the base
// address's origin plus, for fhir_r4, its token_url's; sorted, each once.
// Every address setting is checked as the base address is; a record_link
// must be https on the base address's host (it is not an origin: no
// credential goes to it). token_url on a type that has none is refused.
func OriginsOf(connType, baseURL string, settings map[string]string, development bool) ([]Origin, error) {
	scheme, ok := Schemes[connType]
	if !ok {
		return nil, ErrType
	}
	base, origin, err := CheckURL(baseURL, []string{scheme}, development)
	if err != nil {
		return nil, err
	}
	out := []Origin{origin}
	for k := range settings {
		if (k == "token_url" || k == "record_link") && !slices.Contains(URLSettings[connType], k) {
			return nil, ErrShape
		}
	}
	if tok, ok := settings["token_url"]; ok && slices.Contains(URLSettings[connType], "token_url") {
		_, o, err := CheckURL(tok, []string{"https"}, development)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	if link, ok := settings["record_link"]; ok {
		lu, _, err := CheckURL(link, []string{"https"}, development)
		if err != nil {
			return nil, err
		}
		if lu.Hostname() != base.Hostname() {
			return nil, ErrLinkHost
		}
	}
	slices.Sort(out)
	return out, nil
}

// Strings returns origins as strings, in order.
func Strings(origins []Origin) []string {
	out := make([]string, len(origins))
	for i, o := range origins {
		out[i] = string(o)
	}
	return out
}
