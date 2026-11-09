package proxyconf

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var (
	idA = "c0110000-0000-4000-8000-00000000000a"
	idB = "c0110000-0000-4000-8000-00000000000b"
	hA  = "0b1f0000-0000-4000-8000-00000000000a"
	hB  = "0b1f0000-0000-4000-8000-00000000000b"
)

func file(conns ...Conn) File {
	return File{GeneratedAt: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC), Connections: conns}
}

// The allow-list is refused whole on any doubt: bad ids, hosts, ports,
// ranges, limits, unknown fields, and a range of one hospital that
// overlaps another hospital's (A37).
func TestParseRefusals(t *testing.T) {
	ok := Conn{ID: idA, HospitalID: hA, Hosts: []string{"fhir.hospital-a.example:443"}, CIDRs: []string{"10.99.1.0/24"}}
	if _, err := Marshal(file(ok)); err != nil {
		t.Fatalf("a good file: %v", err)
	}
	bad := map[string]File{
		"upper-case id":   file(Conn{ID: strings.ToUpper(idA), HospitalID: hA, Hosts: ok.Hosts}),
		"nil id":          file(Conn{ID: uuid.Nil.String(), HospitalID: hA, Hosts: ok.Hosts}),
		"duplicate id":    file(ok, ok),
		"no hosts":        file(Conn{ID: idA, HospitalID: hA}),
		"port 80":         file(Conn{ID: idA, HospitalID: hA, Hosts: []string{"fhir.hospital-a.example:80"}}),
		"ip literal":      file(Conn{ID: idA, HospitalID: hA, Hosts: []string{"10.0.0.1:443"}}),
		"ipv6 literal":    file(Conn{ID: idA, HospitalID: hA, Hosts: []string{"[::1]:443"}}),
		"single label":    file(Conn{ID: idA, HospitalID: hA, Hosts: []string{"postgres:443"}}),
		"upper-case host": file(Conn{ID: idA, HospitalID: hA, Hosts: []string{"FHIR.example.org:443"}}),
		"numeric host":    file(Conn{ID: idA, HospitalID: hA, Hosts: []string{"127.1:443"}}),
		"public range":    file(Conn{ID: idA, HospitalID: hA, Hosts: ok.Hosts, CIDRs: []string{"8.8.8.0/24"}}),
		"wide range":      file(Conn{ID: idA, HospitalID: hA, Hosts: ok.Hosts, CIDRs: []string{"10.99.0.0/23"}}),
		"not a network":   file(Conn{ID: idA, HospitalID: hA, Hosts: ok.Hosts, CIDRs: []string{"10.99.1.1/24"}}),
		"cgnat range":     file(Conn{ID: idA, HospitalID: hA, Hosts: ok.Hosts, CIDRs: []string{"100.64.1.0/24"}}),
		"ipv6 range":      file(Conn{ID: idA, HospitalID: hA, Hosts: ok.Hosts, CIDRs: []string{"fd00::/120"}}),
		"too many hosts":  file(Conn{ID: idA, HospitalID: hA, Hosts: []string{"a.example.org:443", "b.example.org:443", "c.example.org:443", "d.example.org:443", "e.example.org:443"}}),
	}
	for name, f := range bad {
		if _, err := Marshal(f); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v, want ErrMalformed", name, err)
		}
	}
	// Overlap inside one hospital is the database's to allow; the proxy
	// still applies each connection's own ranges only.
	same := Conn{ID: idB, HospitalID: hA, Hosts: []string{"fhir.hospital-a.example:443"}, CIDRs: []string{"10.99.1.0/25"}}
	if _, err := Marshal(file(ok, same)); err != nil {
		t.Errorf("overlap within a hospital: %v", err)
	}
	good, _ := Marshal(file(ok))
	for name, b := range map[string][]byte{
		"unknown field":    bytes.Replace(good, []byte(`"connections"`), []byte(`"x":1,"connections"`), 1),
		"trailing data":    append(append([]byte{}, good...), []byte(` {}`)...),
		"null connections": []byte(`{"generated_at":"2026-11-08T12:00:00Z","connections":null}`),
		"no time":          []byte(`{"connections":[]}`),
		"empty":            nil,
		"too big":          bytes.Repeat([]byte(" "), MaxFileBytes+1),
	} {
		if _, err := Parse(b); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v, want ErrMalformed", name, err)
		}
	}
}

func TestFresh(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]bool{0: true, -MaxAge: true, -MaxAge - time.Second: false,
		MaxSkew: true, MaxSkew + time.Second: false} {
		if got := (&List{GeneratedAt: now.Add(d)}).Fresh(now); got != want {
			t.Errorf("generated %s from now: fresh %v", d, got)
		}
	}
	if (*List)(nil).Fresh(now) {
		t.Error("no list is fresh")
	}
}

// The token is per connection and per key, checked in constant time.
func TestAuthorization(t *testing.T) {
	key := bytes.Repeat([]byte{7}, KeyBytes)
	a, b := uuid.MustParse(idA), uuid.MustParse(idB)
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	if id, ok := CheckAuthorization(key, Authorization(key, a, now), now); !ok || id != a {
		t.Fatalf("round trip: %v %v", id, ok)
	}
	if Token(key, a, now) == Token(key, b, now) || len(Token(key, a, now)) != 64 {
		t.Error("tokens")
	}
	if _, ok := CheckAuthorization(key, "basic "+strings.TrimPrefix(Authorization(key, a, now), "Basic "), now); !ok {
		t.Error("the scheme is case-insensitive")
	}
	if _, ok := CheckAuthorization(key[:16], Authorization(key[:16], a, now), now); ok {
		t.Error("a short key was accepted")
	}
	if _, err := ParseKey([]byte(strings.Repeat("ab", 32) + "\n")); err != nil {
		t.Errorf("a key file: %v", err)
	}
	for _, bad := range []string{"", strings.Repeat("ab", 31), strings.Repeat("zz", 32), strings.Repeat("00", 32), strings.Repeat("ab", 32) + "\n\n"} {
		if _, err := ParseKey([]byte(bad)); !errors.Is(err, ErrKey) {
			t.Errorf("key %q accepted", bad)
		}
	}
}

// Review item 5: a token is bound to its time window. One from the
// neighbouring window is accepted (clock skew, a request at the edge);
// one copied 15 minutes ago is refused, so a captured header stops
// working on its own.
func TestTokenBoundToTimeWindow(t *testing.T) {
	key := bytes.Repeat([]byte{7}, KeyBytes)
	a := uuid.MustParse(idA)
	now := time.Date(2026, 11, 9, 12, 2, 30, 0, time.UTC)
	for d, want := range map[time.Duration]bool{0: true, -TokenWindow: true, TokenWindow: true,
		-3 * TokenWindow: false, 3 * TokenWindow: false, -24 * time.Hour: false} {
		if _, ok := CheckAuthorization(key, Authorization(key, a, now.Add(d)), now); ok != want {
			t.Errorf("token made %s from now: accepted %v, want %v", d, ok, want)
		}
	}
	if Token(key, a, now) == Token(key, a, now.Add(3*TokenWindow)) {
		t.Error("the token doesn't change with the window")
	}
}

// Review item 4: ranges that overlap across hospitals refuse only the
// connections that hold them; the rest of the file is used. Ranges in one
// /24 that don't overlap are both kept.
func TestOverlapRefusesOnlyThoseConnections(t *testing.T) {
	idC := "c0110000-0000-4000-8000-00000000000c"
	hC := "0b1f0000-0000-4000-8000-00000000000c"
	idD := "c0110000-0000-4000-8000-00000000000d"
	b, err := Marshal(file(
		Conn{ID: idA, HospitalID: hA, Hosts: []string{"fhir.hospital-a.example:443"}, CIDRs: []string{"10.99.1.0/24"}},
		Conn{ID: idB, HospitalID: hB, Hosts: []string{"fhir.hospital-b.example:443"}, CIDRs: []string{"10.99.1.128/25"}},
		Conn{ID: idC, HospitalID: hC, Hosts: []string{"fhir.hospital-c.example:443"}, CIDRs: []string{"10.99.7.0/25"}},
		Conn{ID: idD, HospitalID: hA, Hosts: []string{"fhir.hospital-a.example:443"}, CIDRs: []string{"10.99.7.128/25"}},
	))
	if err != nil {
		t.Fatalf("one overlap refused the whole file: %v", err)
	}
	l, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{idA: false, idB: false, idC: true, idD: true} {
		if _, ok := l.Conns[uuid.MustParse(id)]; ok != want {
			t.Errorf("%s listed %v, want %v", id, ok, want)
		}
	}
	if len(l.Overlapping) != 2 || !slices.Contains(l.Overlapping, uuid.MustParse(idA)) || !slices.Contains(l.Overlapping, uuid.MustParse(idB)) {
		t.Errorf("overlapping %v, want A and B", l.Overlapping)
	}
}
