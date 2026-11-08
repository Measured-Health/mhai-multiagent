// Package egress is the emr service's only way out (EMR design §6.4, §17.3
// E3; plan Task 14): HTTP and SSH clients that reach a hospital system
// only through egress-emr, only at the connection's own credential origin,
// with the connection's own Proxy-Authorization; the response limits; and
// the allow-list writer the proxy reads.
//
// Errors are the fixed values below: never a URL, a host, a header or a
// response body.
package egress

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/proxyconf"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
)

// Limits (design §17.3 E3, A7).
const (
	// MaxBody is the largest response body read, in bytes.
	MaxBody = 20 << 20
	// MaxHeader is the largest response header block.
	MaxHeader = 64 << 10
	// MaxDepth is the deepest JSON nesting DecodeJSON accepts.
	MaxDepth = 64
	// MaxRedirects bounds a chain of same-origin redirects.
	MaxRedirects = 5
)

// The closed errors.
var (
	// ErrOrigin: a request to anything but one of the connection's own
	// origins, over https, as written; or one that sets
	// Proxy-Authorization itself. Nothing is sent.
	ErrOrigin = errors.New("egress: not the connection's origin")
	// ErrRedirect: a redirect to another origin, any redirect of a request
	// that carries a credential, or too many.
	ErrRedirect = errors.New("egress: redirect refused")
	// ErrTooLarge: a body over MaxBody.
	ErrTooLarge = errors.New("egress: response too large")
	// ErrTooDeep: JSON nested deeper than allowed.
	ErrTooDeep = errors.New("egress: JSON nested too deep")
	// ErrUnreadable: not one valid JSON document.
	ErrUnreadable = errors.New("egress: response unreadable")
	// ErrHostKey: an SSH host key other than the typed fingerprint, or a
	// fingerprint that isn't a SHA256 one.
	ErrHostKey = errors.New("egress: host key refused")
	// ErrProxy: no proxy, the proxy refused the CONNECT, or the SSH
	// handshake behind it failed.
	ErrProxy = errors.New("egress: the egress proxy refused")
)

// HTTP is the client for one connection: every request goes through
// egress-emr at proxyURL with the Proxy-Authorization for
// conn.ConnectionID, and only to conn.Origin. TLS 1.2 or later with the
// system roots; no custom CA.
func HTTP(conn connector.Config, proxyURL *url.URL, key []byte) *http.Client {
	return newHTTP(conn, proxyURL, key, nil)
}

// newHTTP is HTTP with roots, for tests (nil: the system roots).
func newHTTP(conn connector.Config, proxyURL *url.URL, key []byte, roots *x509.CertPool) *http.Client {
	origins := slices.Clone(conn.Origin)
	var auth string
	if len(key) == proxyconf.KeyBytes {
		auth = proxyconf.Authorization(key, conn.ConnectionID)
	}
	t := &http.Transport{
		// Fail closed: with no proxy or key there is no way out at all,
		// never a direct connection.
		Proxy: func(*http.Request) (*url.URL, error) {
			if proxyURL == nil || proxyURL.Scheme != "http" || auth == "" {
				return nil, ErrProxy
			}
			return proxyURL, nil
		},
		ProxyConnectHeader:     http.Header{"Proxy-Authorization": {auth}},
		DialContext:            (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		ForceAttemptHTTP2:      true,
		MaxResponseHeaderBytes: MaxHeader,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  30 * time.Second,
		IdleConnTimeout:        60 * time.Second,
		MaxIdleConns:           4,
	}
	return &http.Client{
		Transport:     &guard{next: t, origins: origins},
		CheckRedirect: checkRedirect(origins),
		Timeout:       2 * time.Minute,
	}
}

// guard checks every request before anything is sent, and caps every
// response.
type guard struct {
	next    http.RoundTripper
	origins []emrurl.Origin
}

func (g *guard) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Proxy-Authorization") != "" {
		return nil, ErrOrigin
	}
	if err := CheckOrigin(req, g.origins); err != nil {
		return nil, err
	}
	resp, err := g.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > MaxBody {
		_ = resp.Body.Close()
		return nil, ErrTooLarge
	}
	resp.Body = &capped{rc: resp.Body, left: MaxBody}
	return resp, nil
}

// CheckOrigin refuses a request whose URL isn't https at one of origins,
// exactly as written (lower-case host, the port explicit or 443), with no
// userinfo and no Host header of its own.
func CheckOrigin(req *http.Request, origins []emrurl.Origin) error {
	u := req.URL
	if u == nil || u.Scheme != "https" || u.User != nil || u.Host == "" || (req.Host != "" && req.Host != u.Host) {
		return ErrOrigin
	}
	o, ok := originOf(u)
	if !ok || !slices.Contains(origins, o) {
		return ErrOrigin
	}
	return nil
}

func originOf(u *url.URL) (emrurl.Origin, bool) {
	host, port := u.Hostname(), u.Port()
	if host == "" {
		return "", false
	}
	if port == "" {
		port = "443"
	}
	hp := net.JoinHostPort(host, port)
	return emrurl.Origin(u.Scheme + "://" + hp), true
}

// carriesCredential: a header that may hold one, or a method with a body
// (a token request's client assertion).
func carriesCredential(r *http.Request) bool {
	return r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" ||
		(r.Method != "" && r.Method != http.MethodGet && r.Method != http.MethodHead)
}

func checkRedirect(origins []emrurl.Origin) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return ErrRedirect
		}
		first := via[0]
		if carriesCredential(first) || len(via) >= MaxRedirects {
			return ErrRedirect
		}
		a, ok1 := originOf(first.URL)
		b, ok2 := originOf(req.URL)
		if !ok1 || !ok2 || a != b || req.URL.Scheme != "https" || !slices.Contains(origins, b) {
			return ErrRedirect
		}
		return nil
	}
}

// capped returns at most left bytes, and ErrTooLarge (never a silent EOF)
// when the body has more.
type capped struct {
	rc   io.ReadCloser
	left int64
}

func (c *capped) Read(p []byte) (int, error) {
	if c.left <= 0 {
		var one [1]byte
		n, err := c.rc.Read(one[:])
		if n > 0 {
			return 0, ErrTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.rc.Read(p)
	c.left -= int64(n)
	return n, err
}

func (c *capped) Close() error { return c.rc.Close() }
