package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/proxyconf"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/logx"
)

// Synthetic test key, not a secret.
var testKey = bytes.Repeat([]byte{0x5a}, proxyconf.KeyBytes)

// fakeResolver answers each name from a queue of answers (the last one
// repeats) and counts every lookup.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][][]netip.Addr
	calls   map[string]int
}

func (r *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[host]++
	q := r.answers[host]
	if len(q) == 0 {
		return nil, errors.New("no such host")
	}
	a := q[0]
	if len(q) > 1 {
		r.answers[host] = q[1:]
	}
	return a, nil
}

func (r *fakeResolver) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		n += c
	}
	return n
}

func (r *fakeResolver) count(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[host]
}

// fakeDialer records every address dialled and echoes bytes back.
type fakeDialer struct {
	mu    sync.Mutex
	addrs []string
}

func (d *fakeDialer) DialContext(_ context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, network+" "+address)
	d.mu.Unlock()
	a, b := net.Pipe()
	go func() {
		_, _ = io.Copy(b, b)
		_ = b.Close()
	}()
	return a, nil
}

func (d *fakeDialer) dialled() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addrs...)
}

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, len(s))
	for i, a := range s {
		out[i] = netip.MustParseAddr(a)
	}
	return out
}

func prefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, a := range s {
		out[i] = netip.MustParsePrefix(a)
	}
	return out
}

var (
	hospA, hospB  = uuid.MustParse("0b1f0000-0000-4000-8000-00000000000a"), uuid.MustParse("0b1f0000-0000-4000-8000-00000000000b")
	connA, connB  = uuid.MustParse("c0110000-0000-4000-8000-00000000000a"), uuid.MustParse("c0110000-0000-4000-8000-00000000000b")
	connA2        = uuid.MustParse("c0110000-0000-4000-8000-0000000000a2")
	publicAddr    = "93.184.215.14"
	publicAddrTwo = "93.184.215.15"
)

// testList is two hospitals: A with connections A (fhir.hospital-a.example,
// range 10.99.1.0/24 plus 172.30.0.0/24, inside the internal network) and
// A2 (range 10.99.4.0/24), B with connection B (fhir.hospital-b.example,
// range 10.99.2.0/24).
func testList(at time.Time) *proxyconf.List {
	return &proxyconf.List{GeneratedAt: at, Conns: map[uuid.UUID]proxyconf.Entry{
		connA: {HospitalID: hospA, Hosts: []string{"fhir.hospital-a.example:443", "sftp.hospital-a.example:22",
			"inside.hospital-a.example:443", "internal.hospital-a.example:443", "meta.hospital-a.example:443",
			"vpn-b.hospital-a.example:443", "vpn-a2.hospital-a.example:443", "mixed.hospital-a.example:443"},
			CIDRs: prefixes("10.99.1.0/24", "172.30.0.0/24")},
		connA2: {HospitalID: hospA, Hosts: []string{"a2.hospital-a.example:443"}, CIDRs: prefixes("10.99.4.0/24")},
		connB:  {HospitalID: hospB, Hosts: []string{"fhir.hospital-b.example:443"}, CIDRs: prefixes("10.99.2.0/24")},
	}}
}

type harness struct {
	srv  *httptest.Server
	res  *fakeResolver
	dial *fakeDialer
	logs *bytes.Buffer
	now  time.Time
	list *proxyconf.List
	mu   sync.Mutex
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC), dial: &fakeDialer{}, logs: &bytes.Buffer{},
		res: &fakeResolver{answers: map[string][][]netip.Addr{
			"fhir.hospital-a.example":     {addrs(publicAddr)},
			"sftp.hospital-a.example":     {addrs(publicAddrTwo)},
			"fhir.hospital-b.example":     {addrs(publicAddrTwo)},
			"inside.hospital-a.example":   {addrs("10.99.1.5")},
			"internal.hospital-a.example": {addrs("172.30.0.7")},
			"meta.hospital-a.example":     {addrs("169.254.169.254")},
			"vpn-b.hospital-a.example":    {addrs("10.99.2.5")},
			"vpn-a2.hospital-a.example":   {addrs("10.99.4.5")},
			"mixed.hospital-a.example":    {addrs(publicAddr, "10.0.0.1")},
		}}}
	h.list = testList(h.now.Add(-10 * time.Second))
	p := &Proxy{
		Key:      testKey,
		List:     func() *proxyconf.List { h.mu.Lock(); defer h.mu.Unlock(); return h.list },
		Internal: prefixes("172.30.0.0/16"),
		Resolver: h.res,
		Dialer:   h.dial,
		Log:      logx.New(&syncWriter{w: h.logs, mu: &h.mu}),
		Now:      func() time.Time { return h.now },
	}
	h.srv = httptest.NewServer(p)
	t.Cleanup(h.srv.Close)
	return h
}

type syncWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(b)
}

func (h *harness) setList(l *proxyconf.List) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.list = l
}

func (h *harness) logText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logs.String()
}

// connect sends one request line with an optional Proxy-Authorization and
// returns the status, plus the open connection when it is 200.
func (h *harness) connect(t *testing.T, method, target, auth string) (int, http.Header, net.Conn) {
	t.Helper()
	c, err := net.Dial("tcp", h.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	host := target
	if u, err := url.Parse(target); err == nil && u.Scheme == "http" {
		host = u.Host
	}
	req := method + " " + target + " HTTP/1.1\r\nHost: " + host + "\r\n"
	if auth != "" {
		req += "Proxy-Authorization: " + auth + "\r\n"
	}
	if _, err := io.WriteString(c, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: method})
	if err != nil {
		_ = c.Close()
		t.Fatalf("%s %s: %v", method, target, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = c.Close()
		return resp.StatusCode, resp.Header, nil
	}
	t.Cleanup(func() { _ = c.Close() })
	return resp.StatusCode, resp.Header, c
}

func authFor(id uuid.UUID) string { return proxyconf.Authorization(testKey, id) }

func expect(t *testing.T, h *harness, target, auth string, want int) {
	t.Helper()
	got, _, _ := h.connect(t, http.MethodConnect, target, auth)
	if got != want {
		t.Errorf("CONNECT %s: %d, want %d", target, got, want)
	}
}

// A6: a name off the connection's list is refused before any DNS lookup
// (no name off the list is ever resolved: no DNS exfiltration channel); so
// are IP literals, other ports, other spellings and anything but CONNECT.
// A listed name is resolved once and tunnelled.
func TestCONNECTNotOnAllowList(t *testing.T) {
	h := newHarness(t)
	a := authFor(connA)
	for _, target := range []string{"unlisted.example.org:443", "x0123456789abcdef.invalid:443",
		"fhir.hospital-a.example:80", "fhir.hospital-a.example:22", "sftp.hospital-a.example:443",
		"FHIR.HOSPITAL-A.EXAMPLE:443", "fhir.hospital-a.example.:443", "evil.fhir.hospital-a.example:443",
		publicAddr + ":443", "[2606:2800:220:1::1]:443", "1572395022:443", "fhir.hospital-a.example"} {
		expect(t, h, target, a, http.StatusForbidden)
	}
	if n := h.res.total(); n != 0 {
		t.Fatalf("%d lookups for names off the list: refuse before resolving", n)
	}
	if got, _, _ := h.connect(t, http.MethodGet, "http://fhir.hospital-a.example/", a); got != http.StatusMethodNotAllowed {
		t.Errorf("GET through the proxy: %d, want 405", got)
	}
	if n := h.res.total(); n != 0 {
		t.Fatalf("a GET was resolved")
	}
	if d := h.dial.dialled(); len(d) != 0 {
		t.Fatalf("dialled %v", d)
	}

	got, _, c := h.connect(t, http.MethodConnect, "fhir.hospital-a.example:443", a)
	if got != http.StatusOK {
		t.Fatalf("listed host: %d, want 200", got)
	}
	if _, err := io.WriteString(c, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("tunnel: %q %v", buf, err)
	}
	if got, _, _ := h.connect(t, http.MethodConnect, "sftp.hospital-a.example:22", a); got != http.StatusOK {
		t.Errorf("listed sftp origin: %d, want 200", got)
	}
	if d := h.dial.dialled(); len(d) != 2 || d[0] != "tcp "+publicAddr+":443" || d[1] != "tcp "+publicAddrTwo+":22" {
		t.Errorf("dialled %v", d)
	}
	logs := h.logText()
	for _, leak := range []string{"hospital-a", "unlisted", "x0123456789abcdef", publicAddr, "Basic", proxyconf.Token(testKey, connA)} {
		if strings.Contains(logs, leak) {
			t.Errorf("the log carries %q", leak)
		}
	}
	if !strings.Contains(logs, `"host_hash"`) || !strings.Contains(logs, connA.String()) || !strings.Contains(logs, `"not_listed"`) {
		t.Errorf("the log lacks the connection id, the decision code or the host hash:\n%s", logs)
	}
}

// A29: the proxy resolves once, checks every address and dials exactly the
// address it checked: a second answer (rebinding to the metadata address)
// is never consulted for that CONNECT; one answer that mixes a public and
// a private address is refused whole.
func TestRebindingPinned(t *testing.T) {
	h := newHarness(t)
	h.res.answers["fhir.hospital-a.example"] = [][]netip.Addr{addrs(publicAddr), addrs("169.254.169.254")}
	a := authFor(connA)
	if got, _, _ := h.connect(t, http.MethodConnect, "fhir.hospital-a.example:443", a); got != http.StatusOK {
		t.Fatalf("first CONNECT: %d", got)
	}
	if n := h.res.count("fhir.hospital-a.example"); n != 1 {
		t.Fatalf("%d lookups for one CONNECT, want 1", n)
	}
	if d := h.dial.dialled(); len(d) != 1 || d[0] != "tcp "+publicAddr+":443" {
		t.Fatalf("dialled %v, want the checked address", d)
	}
	// The next CONNECT resolves again, gets the metadata address, refused.
	expect(t, h, "fhir.hospital-a.example:443", a, http.StatusForbidden)
	expect(t, h, "mixed.hospital-a.example:443", a, http.StatusForbidden)
	if d := h.dial.dialled(); len(d) != 1 {
		t.Fatalf("dialled %v after refusals", d)
	}
}

// A37: every CONNECT names its connection, and only that connection's
// hosts and ranges apply: hospital A's connection never reaches hospital
// B's host or B's declared range, nor another connection's range in its
// own hospital. A missing, forged or mismatched token is 407.
func TestCrossConnectionRefused(t *testing.T) {
	h := newHarness(t)
	a := authFor(connA)
	expect(t, h, "fhir.hospital-b.example:443", a, http.StatusForbidden)   // B's host
	expect(t, h, "vpn-b.hospital-a.example:443", a, http.StatusForbidden)  // into B's range
	expect(t, h, "vpn-a2.hospital-a.example:443", a, http.StatusForbidden) // into A2's range, same hospital
	expect(t, h, "a2.hospital-a.example:443", a, http.StatusForbidden)     // A2's host, same hospital
	expect(t, h, "fhir.hospital-a.example:443", authFor(connB), http.StatusForbidden)
	if d := h.dial.dialled(); len(d) != 0 {
		t.Fatalf("dialled %v", d)
	}

	enc := func(s string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(s)) }
	other := bytes.Repeat([]byte{0x33}, proxyconf.KeyBytes)
	for name, auth := range map[string]string{
		"no header":         "",
		"forged token":      enc(connA.String() + ":" + strings.Repeat("0", 64)),
		"A's id, B's token": enc(connA.String() + ":" + proxyconf.Token(testKey, connB)),
		"another key":       proxyconf.Authorization(other, connA),
		"id only":           enc(connA.String()),
		"upper-case id":     enc(strings.ToUpper(connA.String()) + ":" + proxyconf.Token(testKey, connA)),
		"bearer":            "Bearer " + proxyconf.Token(testKey, connA),
		"not base64":        "Basic %%%",
	} {
		got, hdr, _ := h.connect(t, http.MethodConnect, "fhir.hospital-a.example:443", auth)
		if got != http.StatusProxyAuthRequired {
			t.Errorf("%s: %d, want 407", name, got)
		}
		if !strings.HasPrefix(hdr.Get("Proxy-Authenticate"), "Basic") {
			t.Errorf("%s: Proxy-Authenticate %q", name, hdr.Get("Proxy-Authenticate"))
		}
	}
	// A valid token for a connection that isn't on the list reaches nothing.
	expect(t, h, "fhir.hospital-a.example:443", authFor(uuid.MustParse("c0110000-0000-4000-8000-0000000000ff")), http.StatusForbidden)
	if n := h.res.total(); n != 2 {
		t.Errorf("%d lookups; only vpn-b and vpn-a2, on A's own list, may resolve", n)
	}
}

// A private address is allowed only inside the named connection's declared
// ranges, and never inside the internal networks (backend, egress-emr)
// even when declared; loopback, link-local, metadata, CGNAT, multicast,
// unspecified and the IPv6 forms that embed IPv4 are refused always.
func TestPrivateOnlyWithinDeclaredRanges(t *testing.T) {
	h := newHarness(t)
	a := authFor(connA)
	expect(t, h, "inside.hospital-a.example:443", a, http.StatusOK)
	if d := h.dial.dialled(); len(d) != 1 || d[0] != "tcp 10.99.1.5:443" {
		t.Fatalf("dialled %v", d)
	}
	expect(t, h, "internal.hospital-a.example:443", a, http.StatusForbidden)
	expect(t, h, "meta.hospital-a.example:443", a, http.StatusForbidden)

	refused := []string{"10.99.3.5", "127.0.0.1", "0.0.0.0", "169.254.169.254", "169.254.1.1", "100.64.0.1",
		"224.0.0.1", "255.255.255.255", "192.0.2.1", "198.18.0.1", "240.0.0.1", "::1", "::", "fe80::1",
		"fd00:ec2::254", "fc00::1", "ff02::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "64:ff9b::a00:1",
		"2002:a00:1::1", "2001::1", "2001:db8::1", "::a00:1", "172.30.200.1", "192.168.1.1", "172.16.0.1"}
	for _, ip := range refused {
		h.res.answers["inside.hospital-a.example"] = [][]netip.Addr{addrs(ip)}
		expect(t, h, "inside.hospital-a.example:443", a, http.StatusForbidden)
	}
	if d := h.dial.dialled(); len(d) != 1 {
		t.Fatalf("dialled %v after refusals", d)
	}
	// An IPv4-mapped form of a declared address is that address.
	h.res.answers["inside.hospital-a.example"] = [][]netip.Addr{addrs("::ffff:10.99.1.6")}
	expect(t, h, "inside.hospital-a.example:443", a, http.StatusOK)
	// A lookup that fails or answers nothing is 502, never a dial.
	h.res.answers["inside.hospital-a.example"] = nil
	expect(t, h, "inside.hospital-a.example:443", a, http.StatusBadGateway)
}

// K5: with no allow-list, a stale one or one dated in the future, every
// CONNECT is refused, for every connection, before any lookup.
func TestStaleAllowListRefusesAll(t *testing.T) {
	h := newHarness(t)
	a := authFor(connA)
	for name, l := range map[string]*proxyconf.List{
		"missing":   nil,
		"stale":     testList(h.now.Add(-proxyconf.MaxAge - time.Second)),
		"in future": testList(h.now.Add(proxyconf.MaxSkew + time.Second)),
	} {
		h.setList(l)
		if got, _, _ := h.connect(t, http.MethodConnect, "fhir.hospital-a.example:443", a); got != http.StatusServiceUnavailable {
			t.Errorf("%s: %d, want 503", name, got)
		}
	}
	if n := h.res.total(); n != 0 {
		t.Fatalf("%d lookups with no usable list", n)
	}
	h.setList(testList(h.now.Add(-proxyconf.MaxAge + time.Second)))
	expect(t, h, "fhir.hospital-a.example:443", a, http.StatusOK)

	// The loader: a good file is used; a missing, malformed or half-written
	// one replaces it with nothing (never the previous list).
	dir := t.TempDir()
	path := filepath.Join(dir, proxyconf.FileName)
	l := &Loader{Path: path}
	l.Reload()
	if l.Current() != nil {
		t.Fatal("a missing file gave a list")
	}
	good, err := proxyconf.Marshal(proxyconf.File{GeneratedAt: h.now, Connections: []proxyconf.Conn{{
		ID: connA.String(), HospitalID: hospA.String(), Hosts: []string{"fhir.hospital-a.example:443"}, CIDRs: []string{}}}})
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"good": good, "malformed": []byte(`{"generated_at":`), "good again": good,
		"unknown field": bytes.Replace(good, []byte(`"connections"`), []byte(`"extra":1,"connections"`), 1)} {
		_ = name
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		l.Reload()
		if ok := l.Current() != nil; ok != bytes.Equal(body, good) {
			t.Errorf("%s: list present %v", name, ok)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	l.Reload()
	if l.Current() != nil {
		t.Error("a removed file left the previous list in use")
	}
}
