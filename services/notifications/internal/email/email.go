// Package email is the email channel: SMTP submission to Measured
// Health's own mailbox (notifications design §4.7, §12.1; §15 PR 4; the
// product owner, 20 Nov: N1 is "our own mailbox via SMTP submission" for
// now).
//
// It speaks submission only, always over TLS with the server's
// certificate verified: implicit TLS on 465, or STARTTLS on 587 (a server
// that doesn't offer STARTTLS is refused before anything is sent, and so
// is one whose certificate doesn't verify). It signs in with the
// configured user and the password read from a file, never logged and
// never in an error. It sends the fixed templates only
// (internal/templates), to the recipient's address read at send time,
// from the configured From with the configured Reply-To. No tracking.
//
// A refused recipient (a 5xx answer to RCPT) is Permanent; anything else
// that goes wrong (a timeout, a 4xx, the server down, sign-in) is
// Retryable, so the sender's back-off and breaker handle it. Nothing of the
// server's text is kept (it may echo the address).
package email

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/channel"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/templates"
)

// Config is the SMTP submission settings.
type Config struct {
	Host     string // the server's name; TLS checks the certificate against it
	Port     int    // 465 (implicit TLS) or 587 (STARTTLS)
	Username string
	Password string // from NOTIFY_SMTP_PASSWORD_FILE; never logged
	From     string // a bare address
	FromName string // shown with From ("" for none)
	ReplyTo  string // the support address
	// RootCAs adds trusted roots (a private CA, a test server's); nil is
	// the system's. Verification is never turned off.
	RootCAs *x509.CertPool
	// Dial replaces the dialer (tests); nil is net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Timeout bounds one send (30 s when 0).
	Timeout time.Duration
}

// The closed errors.
var (
	ErrConfig  = errors.New("email: settings are invalid")
	ErrAddress = errors.New("email: the address is not usable")
	errNoTLS   = errors.New("email: the server offers no TLS")
	errAuth    = errors.New("email: sign-in refused")
)

// CheckConfig refuses incomplete or unsafe settings.
func CheckConfig(c Config) error {
	if c.Host == "" || strings.ContainsAny(c.Host, " /:@\r\n") || (c.Port != 465 && c.Port != 587) ||
		c.Username == "" || c.Password == "" || strings.ContainsAny(c.FromName, "\r\n\"<>") {
		return ErrConfig
	}
	for _, a := range []string{c.From, c.ReplyTo} {
		if _, err := plainAddress(a); err != nil {
			return ErrConfig
		}
	}
	return nil
}

// plainAddress parses one bare address: no name, no list, no line break.
func plainAddress(s string) (string, error) {
	if s == "" || len(s) > 254 || strings.ContainsAny(s, "\r\n<>,; \"") {
		return "", ErrAddress
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" || a.Address != s {
		return "", ErrAddress
	}
	return a.Address, nil
}

// Domain is an address's domain, lower-cased.
func Domain(addr string) string {
	_, d, _ := strings.Cut(addr, "@")
	return strings.ToLower(d)
}

// Describe is the settings' shape for the start log: never the password,
// the user or an address.
func (c Config) Describe() string {
	mode := "starttls"
	if c.Port == 465 {
		mode = "implicit_tls"
	}
	return c.Host + ":" + strconv.Itoa(c.Port) + " " + mode
}

// Channel sends rendered templates over SMTP submission.
type Channel struct {
	Config Config
	App    templates.App
	Now    func() time.Time
}

// Name is "email".
func (c *Channel) Name() string { return "email" }

func (c *Channel) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Send renders m's kind and submits it. A kind with a secret link and no
// link, a template that doesn't render or a bad address is Permanent and
// never submitted.
func (c *Channel) Send(ctx context.Context, m channel.Message) (channel.Result, error) {
	k, ok := templates.Kinds[m.Kind]
	if !ok || (k.SecretLink && m.Link == "") {
		return channel.Result{Outcome: channel.Permanent}, nil
	}
	r, err := c.App.Render(m.Kind, templates.Vars{Link: m.Link, Expires: m.Expires, Hospital: m.HospitalName})
	if err != nil {
		return channel.Result{Outcome: channel.Permanent}, nil
	}
	to, err := plainAddress(m.Address)
	if err != nil {
		return channel.Result{Outcome: channel.Permanent}, nil
	}
	msgID := "<" + m.ID.String() + "." + randomHex(6) + "@" + Domain(c.Config.From) + ">"
	body, err := c.compose(to, msgID, r)
	if err != nil {
		return channel.Result{Outcome: channel.Permanent}, nil
	}
	if err := c.submit(ctx, to, body); err != nil {
		var p permanentError
		if errors.As(err, &p) {
			return channel.Result{Outcome: channel.Permanent}, nil
		}
		return channel.Result{Outcome: channel.Retryable}, nil
	}
	return channel.Result{Outcome: channel.Accepted, ProviderID: msgID}, nil
}

// compose builds the MIME message: fixed headers, nothing from data but
// the checked addresses; text and HTML parts, quoted-printable.
func (c *Channel) compose(to, msgID string, r templates.Rendered) ([]byte, error) {
	from := c.Config.From
	if c.Config.FromName != "" {
		from = (&mail.Address{Name: c.Config.FromName, Address: c.Config.From}).String()
	}
	boundary := "mhai-" + randomHex(12)
	var b bytes.Buffer
	for _, line := range []string{
		"From: " + from,
		"To: " + to,
		"Reply-To: " + c.Config.ReplyTo,
		"Subject: " + mime.QEncoding.Encode("utf-8", r.Subject),
		"Date: " + c.now().UTC().Format(time.RFC1123Z),
		"Message-ID: " + msgID,
		"MIME-Version: 1.0",
		"Auto-Submitted: auto-generated",
		`Content-Type: multipart/alternative; boundary="` + boundary + `"`,
	} {
		if strings.ContainsAny(line, "\r\n") {
			return nil, ErrAddress
		}
		b.WriteString(line + "\r\n")
	}
	b.WriteString("\r\n")
	for _, part := range []struct{ ctype, body string }{{"text/plain", r.Text}, {"text/html", r.HTML}} {
		b.WriteString("--" + boundary + "\r\nContent-Type: " + part.ctype + "; charset=utf-8\r\n" +
			"Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		qp := quotedprintable.NewWriter(&b)
		if _, err := qp.Write([]byte(strings.ReplaceAll(part.body, "\n", "\r\n"))); err != nil {
			return nil, err
		}
		if err := qp.Close(); err != nil {
			return nil, err
		}
		b.WriteString("\r\n")
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes(), nil
}

// permanentError is a refusal of the recipient (5xx to RCPT).
type permanentError struct{ code int }

func (e permanentError) Error() string {
	return "email: recipient refused (" + strconv.Itoa(e.code) + ")"
}

func (c *Channel) tlsConfig() *tls.Config {
	return &tls.Config{ServerName: c.Config.Host, RootCAs: c.Config.RootCAs, MinVersion: tls.VersionTLS12}
}

func (c *Channel) dial(ctx context.Context) (net.Conn, error) {
	dial := c.Config.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second}
		dial = d.DialContext
	}
	conn, err := dial(ctx, "tcp", net.JoinHostPort(c.Config.Host, strconv.Itoa(c.Config.Port)))
	if err != nil {
		return nil, err
	}
	if c.Config.Port != 465 {
		return conn, nil
	}
	tc := tls.Client(conn, c.tlsConfig())
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tc, nil
}

// submit sends one message to one recipient.
func (c *Channel) submit(ctx context.Context, to string, body []byte) error {
	timeout := c.Config.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	cl, err := smtp.NewClient(conn, c.Config.Host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer func() { _ = cl.Close() }()
	if err := cl.Hello(Domain(c.Config.From)); err != nil {
		return err
	}
	if c.Config.Port == 587 {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return errNoTLS
		}
		if err := cl.StartTLS(c.tlsConfig()); err != nil {
			return err
		}
	}
	if _, isTLS := cl.TLSConnectionState(); !isTLS {
		return errNoTLS
	}
	if err := cl.Auth(smtp.PlainAuth("", c.Config.Username, c.Config.Password, c.Config.Host)); err != nil {
		return errAuth
	}
	if err := cl.Mail(c.Config.From); err != nil {
		return err
	}
	if err := cl.Rcpt(to); err != nil {
		var te *textproto.Error
		if errors.As(err, &te) && te.Code >= 500 && te.Code < 600 {
			return permanentError{code: te.Code}
		}
		return err
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}
