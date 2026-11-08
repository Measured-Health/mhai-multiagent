package egress

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/proxyconf"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
)

// Synthetic test key, not a secret.
var testKey = []byte("0123456789abcdef0123456789abcdef")

// Synthetic, obviously fake.
const tokenCanary = "Bearer synthetic-token-not-real-0014"

// fakeProxy is a CONNECT proxy that maps names to local test servers and
// records every CONNECT (target and Proxy-Authorization).
type fakeProxy struct {
	srv    *httptest.Server
	routes map[string]string // host:port -> local address
	mu     sync.Mutex
	seen   []string
	auths  []string
}

func newFakeProxy(t *testing.T, routes map[string]string) *fakeProxy {
	t.Helper()
	p := &fakeProxy{routes: routes}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.seen = append(p.seen, r.Host)
		p.auths = append(p.auths, r.Header.Get("Proxy-Authorization"))
		p.mu.Unlock()
		dst, ok := p.routes[r.Host]
		if r.Method != http.MethodConnect || !ok {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		up, err := net.Dial("tcp", dst)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		c, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = up.Close()
			return
		}
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() { _, _ = io.Copy(up, brw); _ = up.Close() }()
		go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeProxy) url() *url.URL {
	u, _ := url.Parse(p.srv.URL)
	return u
}

func (p *fakeProxy) connects() ([]string, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...), append([]string(nil), p.auths...)
}

// tlsServer is an HTTPS server whose certificate names hosts, and a pool
// holding it.
func tlsServer(t *testing.T, h http.Handler, hosts ...string) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: hosts[0]}, DNSNames: hosts,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, BasicConstraintsValid: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, pool
}

// captured is a server that records whether anything reached it.
type captured struct {
	mu   sync.Mutex
	hits int
	auth []string
}

func (c *captured) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.hits++
	c.auth = append(c.auth, r.Header.Get("Authorization"))
	c.mu.Unlock()
	_, _ = io.WriteString(w, "{}")
}

func (c *captured) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}

const (
	hostA = "fhir.hospital-a.example"
	hostB = "evil.hospital-b.example"
)

var connA = connector.Config{ConnectionID: uuid.MustParse("c0110000-0000-4000-8000-00000000000a"),
	HospitalID: uuid.MustParse("0b1f0000-0000-4000-8000-00000000000a"), Type: "fhir_r4",
	BaseURL: "https://" + hostA + "/r4", Origin: []emrurl.Origin{"https://" + hostA + ":443"}}

// twoHosts starts hospital A's server (handler a) and a capturing server
// for hospital B, behind one fake proxy, and returns A's client.
func twoHosts(t *testing.T, a http.Handler) (*http.Client, *fakeProxy, *captured) {
	t.Helper()
	b := &captured{}
	sa, pool := tlsServer(t, a, hostA, hostB)
	sb, _ := tlsServer(t, b, hostA, hostB)
	// Both certificates must verify: one pool for both.
	cb, _ := x509.ParseCertificate(sb.TLS.Certificates[0].Certificate[0])
	pool.AddCert(cb)
	p := newFakeProxy(t, map[string]string{hostA + ":443": sa.Listener.Addr().String(), hostB + ":443": sb.Listener.Addr().String()})
	return newHTTP(connA, p.url(), testKey, pool), p, b
}

// A6: a redirect to another origin is never followed; a request that
// carries a credential follows no redirect at all, even on its own origin.
// A plain redirect on the same origin is followed.
func TestNoRedirectAcrossHosts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/away", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+hostB+"/stolen", http.StatusFound)
	})
	mux.HandleFunc("/here", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ok", http.StatusFound)
	})
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	c, p, b := twoHosts(t, mux)

	if _, err := c.Get("https://" + hostA + "/away"); !errors.Is(err, ErrRedirect) {
		t.Errorf("redirect to another host: %v, want ErrRedirect", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://"+hostA+"/here", nil)
	req.Header.Set("Authorization", tokenCanary)
	if _, err := c.Do(req); !errors.Is(err, ErrRedirect) {
		t.Errorf("redirect with a credential: %v, want ErrRedirect", err)
	}
	resp, err := c.Get("https://" + hostA + "/here")
	if err != nil {
		t.Fatalf("same-origin redirect without a credential: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ok" {
		t.Errorf("body %q", body)
	}
	if b.count() != 0 {
		t.Fatalf("hospital B's server received %d requests", b.count())
	}
	seen, _ := p.connects()
	for _, s := range seen {
		if s != hostA+":443" {
			t.Errorf("the proxy was asked for %s", s)
		}
	}
}

// A28: a request goes only to the connection's own origin, through
// egress-emr, with the connection's own Proxy-Authorization (from its id,
// never from the caller). A request elsewhere (a next link on another
// host, plain http, another port) is refused before anything is sent, and
// a caller can't set Proxy-Authorization itself.
func TestCredentialOnlyToOrigin(t *testing.T) {
	a := &captured{}
	c, p, b := twoHosts(t, a)
	for _, target := range []string{"https://" + hostB + "/Patient?page=2", "http://" + hostA + "/r4/Patient",
		"https://" + hostA + ":8443/r4/Patient", "https://" + strings.ToUpper(hostA) + ".evil.example/"} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", tokenCanary)
		if _, err := c.Do(req); !errors.Is(err, ErrOrigin) {
			t.Errorf("%s: %v, want ErrOrigin", target, err)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, "https://"+hostA+"/r4/metadata", nil)
	req.Header.Set("Proxy-Authorization", proxyconf.Authorization(testKey, uuid.New()))
	if _, err := c.Do(req); !errors.Is(err, ErrOrigin) {
		t.Errorf("caller-set Proxy-Authorization: %v, want ErrOrigin", err)
	}
	if b.count() != 0 || a.count() != 0 {
		t.Fatalf("sent: A %d, B %d", a.count(), b.count())
	}
	if seen, _ := p.connects(); len(seen) != 0 {
		t.Fatalf("the proxy was asked for %v before any allowed request", seen)
	}

	req, _ = http.NewRequest(http.MethodGet, "https://"+hostA+"/r4/metadata", nil)
	req.Header.Set("Authorization", tokenCanary)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("own origin: %v", err)
	}
	_ = resp.Body.Close()
	seen, auths := p.connects()
	if len(seen) != 1 || seen[0] != hostA+":443" || auths[0] != proxyconf.Authorization(testKey, connA.ConnectionID) {
		t.Errorf("CONNECT %v with %v", seen, auths)
	}
	if a.count() != 1 || a.auth[0] != tokenCanary {
		t.Errorf("A received %d", a.count())
	}
	if err := CheckOrigin(req, nil); !errors.Is(err, ErrOrigin) {
		t.Errorf("no origin at all: %v", err)
	}
}

// A7: a body over 20 MB fails with ErrTooLarge whether or not it declares
// its length, response headers over 64 KiB fail, and JSON deeper than the
// limit fails with ErrTooDeep. Nothing past the limit is returned.
func TestResponseCaps(t *testing.T) {
	mux := http.NewServeMux()
	chunk := make([]byte, 1<<20)
	mux.HandleFunc("/declared", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "209715200")
		for i := 0; i < 200; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/streamed", func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < 200; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	})
	mux.HandleFunc("/headers", func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < 100; i++ {
			w.Header().Add("X-Pad", strings.Repeat("a", 1000))
		}
	})
	c, _, _ := twoHosts(t, mux)

	if resp, err := c.Get("https://" + hostA + "/declared"); !errors.Is(err, ErrTooLarge) {
		if err == nil {
			_ = resp.Body.Close()
		}
		t.Errorf("declared 200 MB: %v, want ErrTooLarge", err)
	}
	resp, err := c.Get("https://" + hostA + "/streamed")
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if !errors.Is(err, ErrTooLarge) || n > MaxBody {
		t.Errorf("streamed 200 MB: read %d, %v; want at most %d and ErrTooLarge", n, err, MaxBody)
	}
	if resp, err := c.Get("https://" + hostA + "/headers"); err == nil {
		_ = resp.Body.Close()
		t.Error("100 KB of headers accepted")
	}

	deep := strings.Repeat("[", 10000) + strings.Repeat("]", 10000)
	var v any
	if err := DecodeJSON(strings.NewReader(deep), &v, MaxDepth); !errors.Is(err, ErrTooDeep) {
		t.Errorf("10,000-deep JSON: %v, want ErrTooDeep", err)
	}
	if v != nil {
		t.Error("a refused document was decoded")
	}
	objDeep := strings.Repeat(`{"a":`, 65) + "1" + strings.Repeat("}", 65)
	if err := DecodeJSON(strings.NewReader(objDeep), &v, MaxDepth); !errors.Is(err, ErrTooDeep) {
		t.Errorf("65-deep objects: %v", err)
	}
	// Brackets inside strings don't count.
	ok := `{"a":"` + strings.Repeat("[", 500) + `","b":[[1]]}`
	var out struct {
		A string  `json:"a"`
		B [][]int `json:"b"`
	}
	if err := DecodeJSON(strings.NewReader(ok), &out, MaxDepth); err != nil || out.B[0][0] != 1 {
		t.Errorf("a shallow document: %v", err)
	}
	for _, bad := range []string{`{"a":1} {"b":2}`, `{"a":`, ``, `{"a":"\x00` + "\xff" + `"}`} {
		if err := DecodeJSON(strings.NewReader(bad), &v, MaxDepth); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%q: %v, want ErrUnreadable", bad, err)
		}
	}
	if err := DecodeJSON(io.LimitReader(neverEnding{}, MaxBody+10), &v, MaxDepth); !errors.Is(err, ErrTooLarge) {
		t.Errorf("an endless document: %v, want ErrTooLarge", err)
	}
}

type neverEnding struct{}

func (neverEnding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

// A29 (SFTP): the SSH client goes through egress-emr to the connection's
// sftp origin only, and accepts only the host key whose SHA256 fingerprint
// was typed in; there is no "accept any key".
func TestSSHHostKeyPinned(t *testing.T) {
	_, hostKey, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	cfg := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(signer)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					_ = ch.Reject(ssh.Prohibited, "test")
				}
				_ = sc.Close()
			}()
		}
	}()
	sftpHost := "sftp.hospital-a.example"
	p := newFakeProxy(t, map[string]string{sftpHost + ":22": ln.Addr().String()})
	conn := connA
	conn.Type, conn.Origin = "file_export", []emrurl.Origin{emrurl.Origin("sftp://" + sftpHost + ":22")}
	auth := []ssh.AuthMethod{ssh.Password("synthetic-password-not-real")}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	good := ssh.FingerprintSHA256(signer.PublicKey())
	c, err := SSH(ctx, conn, p.url(), testKey, conn.Origin[0], good, "synthetic-user", auth)
	if err != nil {
		t.Fatalf("the right host key: %v", err)
	}
	_ = c.Close()
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := ssh.NewSignerFromKey(otherKey)
	if _, err := SSH(ctx, conn, p.url(), testKey, conn.Origin[0], ssh.FingerprintSHA256(otherSigner.PublicKey()), "synthetic-user", auth); !errors.Is(err, ErrHostKey) {
		t.Errorf("another host key: %v, want ErrHostKey", err)
	}
	for _, fp := range []string{"", "MD5:aa:bb", strings.TrimPrefix(good, "SHA256:"), good + "x"} {
		if _, err := SSH(ctx, conn, p.url(), testKey, conn.Origin[0], fp, "synthetic-user", auth); !errors.Is(err, ErrHostKey) {
			t.Errorf("fingerprint %q: %v, want ErrHostKey", fp, err)
		}
	}
	if _, err := SSH(ctx, conn, p.url(), testKey, "sftp://other.hospital-a.example:22", good, "synthetic-user", auth); !errors.Is(err, ErrOrigin) {
		t.Errorf("an origin not the connection's: %v, want ErrOrigin", err)
	}
	seen, auths := p.connects()
	for i := range seen {
		if seen[i] != sftpHost+":22" || auths[i] != proxyconf.Authorization(testKey, conn.ConnectionID) {
			t.Errorf("CONNECT %s with %q", seen[i], auths[i])
		}
	}
	// The response to CONNECT is read with a bound: a proxy that answers
	// with something else is refused.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer bad.Close()
	bu, _ := url.Parse(bad.URL)
	if _, err := SSH(ctx, conn, bu, testKey, conn.Origin[0], good, "synthetic-user", auth); !errors.Is(err, ErrProxy) {
		t.Errorf("a refusing proxy: %v, want ErrProxy", err)
	}
}
