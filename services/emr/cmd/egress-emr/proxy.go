package main

// The CONNECT handler (EMR design §6.4; plan Task 13). In order, and
// refusing at the first failure:
//
//  1. CONNECT only (405);
//  2. Proxy-Authorization names a connection, with K_proxy's token for it
//     (constant time; 407);
//  3. the allow-list is there and fresh (503: fail closed, K5);
//  4. the connection is on it, and host:port is one of that connection's
//     hosts, exactly as written, port 443 or 22 (403). No name is resolved
//     before this point, so a name off the list never reaches DNS (no
//     exfiltration in the names asked for);
//  5. one lookup; every address in the answer must be one this connection
//     may reach (resolve.go; 403), else nothing is dialled;
//  6. dial the checked address itself (an IPv4 one first), never the name:
//     no second lookup, so a rebinding answer can't swap in another;
//  7. 200, then bytes both ways until either side closes or both are idle
//     for IdleTimeout.
//
// One log line per CONNECT: the connection id (once authenticated), the
// decision code, the status, the port and a hash of the host. Never the
// host, an address, a header or a byte of the tunnel.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/proxyconf"
)

// Defaults.
const (
	LookupTimeout = 5 * time.Second
	DialTimeout   = 10 * time.Second
	IdleTimeout   = 60 * time.Second
	MaxTunnels    = 256
)

// Proxy is egress-emr's handler.
type Proxy struct {
	// Key is K_proxy.
	Key []byte
	// List is the allow-list in use (nil: none).
	List func() *proxyconf.List
	// Internal are the networks never reached, declared or not.
	Internal []netip.Prefix
	Resolver Resolver
	Dialer   Dialer
	Log      *slog.Logger
	Now      func() time.Time
	// IdleTimeout and MaxTunnels default to the constants above.
	IdleTimeout time.Duration
	MaxTunnels  int

	once sync.Once
	sem  chan struct{}
}

// HostHash is how a host appears in the log: the first 16 hex characters
// of its SHA-256.
func HostHash(host string) string {
	s := sha256.Sum256([]byte(host))
	return hex.EncodeToString(s[:8])
}

type event struct {
	conn       uuid.UUID
	host, port string
}

func (p *Proxy) log(ev event, decision string, status int) {
	if p.Log == nil {
		return
	}
	attrs := []any{"decision", decision, "status", status}
	if ev.conn != uuid.Nil {
		attrs = append(attrs, "connection_id", ev.conn.String())
	}
	if ev.port != "" {
		attrs = append(attrs, "port", ev.port)
	}
	if ev.host != "" {
		attrs = append(attrs, "host_hash", HostHash(ev.host))
	}
	p.Log.Info("connect", attrs...)
}

func (p *Proxy) refuse(w http.ResponseWriter, ev event, decision string, status int) {
	p.log(ev, decision, status)
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)
}

func (p *Proxy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var ev event
	if r.Method != http.MethodConnect {
		p.refuse(w, ev, "not_connect", http.StatusMethodNotAllowed)
		return
	}
	id, ok := proxyconf.CheckAuthorization(p.Key, r.Header.Get("Proxy-Authorization"))
	if !ok {
		w.Header().Set("Proxy-Authenticate", `Basic realm="egress-emr"`)
		p.refuse(w, ev, "proxy_auth", http.StatusProxyAuthRequired)
		return
	}
	ev.conn = id
	var list *proxyconf.List
	if p.List != nil {
		list = p.List()
	}
	if !list.Fresh(p.now()) {
		p.refuse(w, ev, "allow_list_unavailable", http.StatusServiceUnavailable)
		return
	}
	entry, ok := list.Conns[id]
	if !ok {
		p.refuse(w, ev, "unknown_connection", http.StatusForbidden)
		return
	}
	// For CONNECT the request target is the authority, as sent.
	target := r.RequestURI
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		p.refuse(w, ev, "bad_target", http.StatusForbidden)
		return
	}
	ev.host, ev.port = host, port
	if !proxyconf.ValidHost(target) {
		p.refuse(w, ev, "bad_target", http.StatusForbidden)
		return
	}
	if !slices.Contains(entry.Hosts, target) {
		p.refuse(w, ev, "not_listed", http.StatusForbidden)
		return
	}

	// The one lookup.
	lctx, cancel := context.WithTimeout(r.Context(), LookupTimeout)
	answer, err := p.Resolver.LookupNetIP(lctx, "ip", host)
	cancel()
	if err != nil || len(answer) == 0 {
		p.refuse(w, ev, "lookup_failed", http.StatusBadGateway)
		return
	}
	for _, a := range answer {
		if !allowed(a, entry.CIDRs, p.Internal) {
			p.refuse(w, ev, "address_refused", http.StatusForbidden)
			return
		}
	}
	pick := answer[0].Unmap()
	for _, a := range answer {
		if a.Unmap().Is4() {
			pick = a.Unmap()
			break
		}
	}
	n, _ := strconv.ParseUint(port, 10, 16) // 443 or 22: ValidHost checked
	dst := netip.AddrPortFrom(pick.WithZone(""), uint16(n))

	p.once.Do(func() {
		size := p.MaxTunnels
		if size <= 0 {
			size = MaxTunnels
		}
		p.sem = make(chan struct{}, size)
	})
	select {
	case p.sem <- struct{}{}:
	default:
		p.refuse(w, ev, "busy", http.StatusServiceUnavailable)
		return
	}
	release := func() { <-p.sem }

	dctx, dcancel := context.WithTimeout(r.Context(), DialTimeout)
	up, err := p.Dialer.DialContext(dctx, "tcp", dst.String())
	dcancel()
	if err != nil {
		release()
		p.refuse(w, ev, "dial_failed", http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = up.Close()
		release()
		p.refuse(w, ev, "no_hijack", http.StatusInternalServerError)
		return
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		_ = up.Close()
		release()
		p.log(ev, "no_hijack", http.StatusInternalServerError)
		return
	}
	// The server's header deadline must not cut the tunnel.
	_ = c.SetDeadline(time.Time{})
	p.log(ev, "ok", http.StatusOK)
	go func() {
		defer release()
		if _, err := brw.WriteString("HTTP/1.1 200 Connection established\r\n\r\n"); err != nil || brw.Flush() != nil {
			_ = c.Close()
			_ = up.Close()
			return
		}
		// Bytes the client sent after the request line (a TLS hello sent
		// without waiting) are already in the reader.
		if k := brw.Reader.Buffered(); k > 0 {
			b, _ := brw.Reader.Peek(k)
			if _, err := up.Write(b); err != nil {
				_ = c.Close()
				_ = up.Close()
				return
			}
		}
		idle := p.IdleTimeout
		if idle <= 0 {
			idle = IdleTimeout
		}
		tunnel(c, up, idle)
	}()
}

// tunnel copies both ways until one side closes, or until neither side has
// moved a byte for idle.
func tunnel(a, b net.Conn, idle time.Duration) {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32<<10)
		for {
			_ = src.SetReadDeadline(time.Now().Add(idle))
			n, err := src.Read(buf)
			if n > 0 {
				last.Store(time.Now().UnixNano())
				_ = dst.SetWriteDeadline(time.Now().Add(idle))
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() && time.Since(time.Unix(0, last.Load())) < idle {
					continue // the other direction is busy
				}
				return
			}
		}
	}
	go cp(b, a)
	go cp(a, b)
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}
