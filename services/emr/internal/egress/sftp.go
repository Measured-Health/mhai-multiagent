package egress

// The SSH client for sftp origins (file_export, PR 11; design §5.4, A29).
// It goes through egress-emr like HTTP, with the connection's own
// Proxy-Authorization, to one of the connection's sftp origins only, and
// accepts only the host key whose SHA256 fingerprint the hospital typed
// in: there is no "accept any key" and no key learned on first use. PR 11
// opens its SFTP session over the client this returns (plan departure:
// the github.com/pkg/sftp dependency arrives with its only user).

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/proxyconf"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
)

// HandshakeTimeout bounds the CONNECT and the SSH handshake.
const HandshakeTimeout = 30 * time.Second

// validFingerprint: "SHA256:" and the unpadded base64 of 32 bytes.
func validFingerprint(fp string) bool {
	rest, ok := strings.CutPrefix(fp, "SHA256:")
	if !ok {
		return false
	}
	b, err := base64.RawStdEncoding.DecodeString(rest)
	return err == nil && len(b) == 32
}

// SSH opens an SSH client to origin (one of conn.Origin, sftp://host:port)
// through the proxy, as user with auth, accepting only the host key with
// fingerprint.
func SSH(ctx context.Context, conn connector.Config, proxyURL *url.URL, key []byte, origin emrurl.Origin,
	fingerprint, user string, auth []ssh.AuthMethod) (*ssh.Client, error) {
	if !strings.HasPrefix(string(origin), "sftp://") || !slices.Contains(conn.Origin, origin) {
		return nil, ErrOrigin
	}
	if !validFingerprint(fingerprint) {
		return nil, ErrHostKey
	}
	if proxyURL == nil || proxyURL.Scheme != "http" || proxyURL.Host == "" || len(key) != proxyconf.KeyBytes {
		return nil, ErrProxy
	}
	target := strings.TrimPrefix(string(origin), "sftp://")
	if !proxyconf.ValidHost(target) {
		return nil, ErrOrigin
	}
	ctx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()
	d := net.Dialer{Timeout: 10 * time.Second}
	c, err := d.DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, ErrProxy
	}
	deadline, _ := ctx.Deadline()
	_ = c.SetDeadline(deadline)
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\nProxy-Authorization: " +
		proxyconf.Authorization(key, conn.ConnectionID) + "\r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		_ = c.Close()
		return nil, ErrProxy
	}
	br := bufio.NewReader(io.LimitReader(c, MaxHeader))
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != http.StatusOK {
		_ = c.Close()
		return nil, ErrProxy
	}
	// Bytes the server sent after the proxy's answer (its SSH banner) are
	// already buffered: the SSH layer reads them first.
	buffered, _ := br.Peek(br.Buffered())
	tc := &prefixConn{Conn: c, prefix: append([]byte(nil), buffered...)}

	var mismatch atomic.Bool
	cfg := &ssh.ClientConfig{User: user, Auth: auth, Timeout: HandshakeTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			if subtle.ConstantTimeCompare([]byte(ssh.FingerprintSHA256(k)), []byte(fingerprint)) != 1 {
				mismatch.Store(true)
				return ErrHostKey
			}
			return nil
		}}
	sc, chans, reqs, err := ssh.NewClientConn(tc, target, cfg)
	if err != nil {
		_ = c.Close()
		if mismatch.Load() {
			return nil, ErrHostKey
		}
		return nil, ErrProxy
	}
	_ = c.SetDeadline(time.Time{})
	return ssh.NewClient(sc, chans, reqs), nil
}

// prefixConn reads prefix before the connection.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (p *prefixConn) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}
