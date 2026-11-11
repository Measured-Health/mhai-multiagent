// Package proxyconf is the contract between the emr service and egress-emr
// (EMR design §6.4; plan Tasks 13–14, departure 15): the allow-list file
// the emr service writes and the proxy reads, and the per-connection
// Proxy-Authorization token both compute from K_proxy.
//
// The proxy has no database or NATS access. It trusts nothing in the file
// that it hasn't checked here: a file that fails any check is refused
// whole, and so is one older than MaxAge (fail closed).
package proxyconf

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// FileName is the allow-list's name in the shared directory.
const FileName = "allow.json"

// Limits on the allow-list.
const (
	// MaxAge: a file generated longer ago than this is stale, and the
	// proxy refuses every CONNECT (K5).
	MaxAge = 120 * time.Second
	// MaxSkew: a file dated further ahead than this is refused too.
	MaxSkew = 30 * time.Second
	// MaxFileBytes bounds the file the proxy reads.
	MaxFileBytes = 4 << 20
	// MaxConnections, MaxHosts and MaxCIDRs bound its contents (an origin
	// has at most 4 entries and a connection at most 8 declared ranges,
	// as core and migration 00020 check).
	MaxConnections = 10000
	MaxHosts       = 4
	MaxCIDRs       = 8
	// ReloadEvery is how often the proxy reads the file again.
	ReloadEvery = 1 * time.Second
)

// ErrMalformed is every refusal of a file: a fixed value that never
// carries the file's contents.
var ErrMalformed = errors.New("proxyconf: allow-list malformed")

// Conn is one connection in the file, as written.
type Conn struct {
	ID         string   `json:"id"`
	HospitalID string   `json:"hospital_id"`
	Hosts      []string `json:"hosts"`
	CIDRs      []string `json:"cidrs"`
}

// File is the allow-list as written.
type File struct {
	GeneratedAt time.Time `json:"generated_at"`
	Connections []Conn    `json:"connections"`
}

// Entry is one connection's rules, checked.
type Entry struct {
	HospitalID uuid.UUID
	// Hosts are "host:port", lower-case DNS names, port 443 or 22.
	Hosts []string
	// CIDRs are the connection's declared private ranges.
	CIDRs []netip.Prefix
}

// List is a checked allow-list.
type List struct {
	GeneratedAt time.Time
	Conns       map[uuid.UUID]Entry
	// Overlapping are the connections left out because one of their
	// ranges overlaps a range of another hospital's (A37): an alert for a
	// person, never a reason to refuse the other connections.
	Overlapping []uuid.UUID
}

// Fresh reports whether l may be used at now: generated at most MaxAge
// ago and not more than MaxSkew ahead. A nil list is never fresh.
func (l *List) Fresh(now time.Time) bool {
	if l == nil {
		return false
	}
	age := now.Sub(l.GeneratedAt)
	return age <= MaxAge && age >= -MaxSkew
}

// Marshal checks f as Parse would and returns its bytes. The emr service
// never writes a file the proxy would refuse.
func Marshal(f File) ([]byte, error) {
	f.GeneratedAt = f.GeneratedAt.UTC()
	if f.Connections == nil {
		f.Connections = []Conn{}
	}
	b, err := json.Marshal(f)
	if err != nil {
		return nil, ErrMalformed
	}
	if _, err := Parse(b); err != nil {
		return nil, err
	}
	return b, nil
}

// Parse reads and checks an allow-list: one JSON object, no unknown field,
// canonical non-nil ids each once, valid hosts and declarable ranges
// within the limits; anything else refuses the whole file. A connection
// with a range that overlaps another hospital's (A37; migration 00020
// refuses that too) is left out, with every connection it overlaps, and
// named in Overlapping; the others are kept.
func Parse(b []byte) (*List, error) {
	if len(b) == 0 || len(b) > MaxFileBytes {
		return nil, ErrMalformed
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var f File
	if err := dec.Decode(&f); err != nil || dec.More() || f.GeneratedAt.IsZero() || f.Connections == nil ||
		len(f.Connections) > MaxConnections {
		return nil, ErrMalformed
	}
	if _, err := dec.Token(); err == nil {
		return nil, ErrMalformed // trailing data
	}
	l := &List{GeneratedAt: f.GeneratedAt, Conns: make(map[uuid.UUID]Entry, len(f.Connections))}
	type held struct {
		p        netip.Prefix
		hospital uuid.UUID
		conn     uuid.UUID
	}
	byKey := map[netip.Prefix][]held{} // the /24 holding each range
	for _, c := range f.Connections {
		id, ok1 := canonicalID(c.ID)
		hid, ok2 := canonicalID(c.HospitalID)
		if !ok1 || !ok2 {
			return nil, ErrMalformed
		}
		if _, dup := l.Conns[id]; dup || len(c.Hosts) == 0 || len(c.Hosts) > MaxHosts || len(c.CIDRs) > MaxCIDRs {
			return nil, ErrMalformed
		}
		e := Entry{HospitalID: hid}
		for _, h := range c.Hosts {
			if !ValidHost(h) {
				return nil, ErrMalformed
			}
			if !slices.Contains(e.Hosts, h) {
				e.Hosts = append(e.Hosts, h)
			}
		}
		for _, s := range c.CIDRs {
			p, err := netip.ParsePrefix(s)
			if err != nil || !Declarable(p) || slices.Contains(e.CIDRs, p) {
				return nil, ErrMalformed
			}
			// A declarable range is at most a /24, so it overlaps another
			// only inside the same /24.
			key := netip.PrefixFrom(p.Addr(), 24).Masked()
			byKey[key] = append(byKey[key], held{p, hid, id})
			e.CIDRs = append(e.CIDRs, p)
		}
		l.Conns[id] = e
	}
	bad := map[uuid.UUID]bool{}
	for _, hs := range byKey {
		for i := range hs {
			for j := i + 1; j < len(hs); j++ {
				if hs[i].hospital != hs[j].hospital && hs[i].p.Overlaps(hs[j].p) {
					bad[hs[i].conn], bad[hs[j].conn] = true, true
				}
			}
		}
	}
	for id := range bad {
		delete(l.Conns, id)
		l.Overlapping = append(l.Overlapping, id)
	}
	slices.SortFunc(l.Overlapping, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return l, nil
}

// ReadFile reads and parses the allow-list at path, refusing a file over
// MaxFileBytes without reading it all.
func ReadFile(path string) (*List, error) {
	f, err := os.Open(path) // #nosec G304 -- the operator-configured allow-list path
	if err != nil {
		return nil, ErrMalformed
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil || len(b) > MaxFileBytes {
		return nil, ErrMalformed
	}
	return Parse(b)
}

func canonicalID(s string) (uuid.UUID, bool) {
	id, err := uuid.Parse(s)
	if err != nil || id == uuid.Nil || id.String() != s {
		return uuid.Nil, false
	}
	return id, true
}

// privateV4 are the ranges a connection may declare from (RFC 1918), as
// core's cidrsOK and migration 00020 allow.
var privateV4 = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16")}

// Declarable reports whether p is a range a connection may declare: an
// IPv4 RFC 1918 network of at most a /24, written as its network.
func Declarable(p netip.Prefix) bool {
	if !p.IsValid() || !p.Addr().Is4() || p.Bits() < 24 || p.Masked() != p {
		return false
	}
	for _, n := range privateV4 {
		if n.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

// ValidHost reports whether hp is "host:port" with host a lower-case DNS
// name of at least two labels (never an IP literal) and port 443 or 22.
func ValidHost(hp string) bool {
	host, port, err := net.SplitHostPort(hp)
	if err != nil || (port != "443" && port != "22") || hp != host+":"+port {
		return false
	}
	return ValidName(host)
}

// ValidName reports whether h is a lower-case DNS name of at least two
// labels, at most 253 bytes, not all-numeric in its last label (so never
// an IPv4 literal in any form) and never an IP literal.
func ValidName(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return false
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	last := labels[len(labels)-1]
	allDigits := true
	for i := 0; i < len(last); i++ {
		allDigits = allDigits && last[i] >= '0' && last[i] <= '9'
	}
	return !allDigits && !strings.HasPrefix(last, "0x")
}

// KeyFile is K_proxy's file name in its directory.
const KeyFile = "k_proxy"

// KeyBytes is K_proxy's length.
const KeyBytes = 32

// ErrKey is every refusal of a key file: a fixed value.
var ErrKey = errors.New("proxyconf: K_proxy missing or not a 32-byte hex key")

// ParseKey reads K_proxy: 64 hex characters, optionally followed by one
// newline; never all zero.
func ParseKey(b []byte) ([]byte, error) {
	s := strings.TrimSuffix(string(b), "\n")
	if len(s) != 2*KeyBytes {
		return nil, ErrKey
	}
	k, err := hex.DecodeString(s)
	if err != nil || bytes.Equal(k, make([]byte, KeyBytes)) {
		return nil, ErrKey
	}
	return k, nil
}

// ReadKey reads K_proxy from path.
func ReadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the operator-configured key path
	if err != nil || len(b) > 2*KeyBytes+1 {
		return nil, ErrKey
	}
	return ParseKey(b)
}

// TokenWindow is how long a Proxy-Authorization token is made for. The
// proxy accepts the current window and its two neighbours (clock skew, a
// request at the edge), so a captured header stops working within three
// windows on its own.
const TokenWindow = 5 * time.Minute

func window(at time.Time) int64 {
	return at.Unix() / int64(TokenWindow/time.Second)
}

func tokenAt(key []byte, id uuid.UUID, w int64) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id.String() + "|" + strconv.FormatInt(w, 10)))
	return hex.EncodeToString(m.Sum(nil))
}

// Token is hex(HMAC-SHA256(key, "<connection id>|<window of at>")).
func Token(key []byte, id uuid.UUID, at time.Time) string {
	return tokenAt(key, id, window(at))
}

// Authorization is the Proxy-Authorization value for connection id at
// time at: Basic base64(<id>:<Token>).
func Authorization(key []byte, id uuid.UUID, at time.Time) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(id.String()+":"+Token(key, id, at)))
}

// CheckAuthorization reads a Proxy-Authorization value and returns the
// connection id it names when its token is K_proxy's for that id in the
// window of now or a neighbouring one (each compared in constant time,
// all three always). Anything else is false, with the nil id.
func CheckAuthorization(key []byte, header string, now time.Time) (uuid.UUID, bool) {
	scheme, cred, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") || len(cred) > 256 {
		return uuid.Nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(cred)
	if err != nil {
		return uuid.Nil, false
	}
	idText, tok, ok := strings.Cut(string(raw), ":")
	if !ok {
		return uuid.Nil, false
	}
	id, ok := canonicalID(idText)
	if !ok || len(key) != KeyBytes {
		return uuid.Nil, false
	}
	w, match := window(now), false
	for _, d := range []int64{-1, 0, 1} {
		if hmac.Equal([]byte(tokenAt(key, id, w+d)), []byte(tok)) {
			match = true
		}
	}
	if !match {
		return uuid.Nil, false
	}
	return id, true
}
