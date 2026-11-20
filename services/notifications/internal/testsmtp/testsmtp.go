// Package testsmtp is a small SMTP submission server for the email
// channel's tests: TLS with a certificate made at run time for
// "localhost" (its CA exported for the client to trust), implicit TLS
// (465 style) or STARTTLS (587 style), AUTH PLAIN against a user and
// password the test makes at run time, and the messages it accepted. A
// recipient whose local part starts with "reject" is refused with 550, one
// with "tempfail" with 451. It relays nothing anywhere: tests only.
package testsmtp

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

// Mode is how the server offers TLS.
type Mode int

const (
	Implicit Mode = iota // TLS from the first byte (465 style)
	StartTLS             // a plaintext greeting, STARTTLS before AUTH (587 style)
	NoTLS                // never offers STARTTLS (a server the client must refuse)
)

// Message is one accepted message.
type Message struct {
	From string
	To   []string
	Data string
}

// Server is a running test server.
type Server struct {
	Addr     string         // 127.0.0.1:port
	CAs      *x509.CertPool // trust this to verify the server
	User     string
	Password string
	mode     Mode
	ln       net.Listener
	tlsConf  *tls.Config
	mu       sync.Mutex
	msgs     []Message
	auths    int
}

// Start runs a server in mode until Close.
func Start(mode Mode, user, password string) (*Server, error) {
	cert, pool, err := selfSigned()
	if err != nil {
		return nil, err
	}
	s := &Server{CAs: pool, User: user, Password: password, mode: mode,
		tlsConf: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	var ln net.Listener
	if mode == Implicit {
		ln, err = tls.Listen("tcp", "127.0.0.1:0", s.tlsConf)
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return nil, err
	}
	s.ln, s.Addr = ln, ln.Addr().String()
	go s.serve()
	return s, nil
}

// Close stops the server.
func (s *Server) Close() { _ = s.ln.Close() }

// Messages are the accepted messages so far.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.msgs...)
}

// Auths is how many AUTHs succeeded.
func (s *Server) Auths() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auths
}

func (s *Server) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.session(c)
	}
}

func addrOf(line string) string {
	i, j := strings.Index(line, "<"), strings.Index(line, ">")
	if i < 0 || j < i {
		return ""
	}
	return line[i+1 : j]
}

func (s *Server) session(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	r, w := bufio.NewReader(c), bufio.NewWriter(c)
	say := func(line string) { _, _ = w.WriteString(line + "\r\n"); _ = w.Flush() }
	isTLS, authed := s.mode == Implicit, false
	var cur Message
	say("220 localhost test ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch strings.ToUpper(strings.SplitN(line, " ", 2)[0]) {
		case "EHLO", "HELO":
			_, _ = w.WriteString("250-localhost\r\n")
			if s.mode == StartTLS && !isTLS {
				_, _ = w.WriteString("250-STARTTLS\r\n")
			}
			if isTLS {
				_, _ = w.WriteString("250-AUTH PLAIN\r\n")
			}
			say("250 8BITMIME")
		case "STARTTLS":
			if s.mode != StartTLS || isTLS {
				say("502 not offered")
				continue
			}
			say("220 go ahead")
			tc := tls.Server(c, s.tlsConf)
			if err := tc.Handshake(); err != nil {
				return
			}
			c, isTLS = tc, true
			r, w = bufio.NewReader(c), bufio.NewWriter(c)
		case "AUTH":
			parts := strings.Fields(line)
			if !isTLS {
				say("530 TLS first")
				continue
			}
			if len(parts) != 3 || strings.ToUpper(parts[1]) != "PLAIN" {
				say("504 PLAIN only")
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(parts[2])
			f := strings.Split(string(raw), "\x00")
			if err != nil || len(f) != 3 || f[1] != s.User || f[2] != s.Password {
				say("535 authentication failed")
				continue
			}
			authed = true
			s.mu.Lock()
			s.auths++
			s.mu.Unlock()
			say("235 ok")
		case "MAIL":
			if !authed {
				say("530 auth first")
				continue
			}
			cur = Message{From: addrOf(line)}
			say("250 ok")
		case "RCPT":
			to := addrOf(line)
			switch {
			case strings.HasPrefix(to, "reject"):
				say("550 no such user")
			case strings.HasPrefix(to, "tempfail"):
				say("451 try later")
			default:
				cur.To = append(cur.To, to)
				say("250 ok")
			}
		case "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, "."))
			}
			s.mu.Lock()
			cur.Data = b.String()
			s.msgs = append(s.msgs, cur)
			s.mu.Unlock()
			say("250 queued")
		case "RSET", "NOOP":
			say("250 ok")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("500 unknown")
		}
	}
}

// selfSigned makes a certificate for localhost and 127.0.0.1, and a pool
// that trusts it.
func selfSigned() (tls.Certificate, *x509.CertPool, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool, nil
}
