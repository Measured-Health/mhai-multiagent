package email

import (
	"context"
	"io"
	"mime/quotedprintable"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/channel"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/templates"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/testsmtp"
)

// secret builds a password-like test value at run time (gitleaks).
func secret() string { return "synthetic-" + uuid.NewString() }

func start(t *testing.T, mode testsmtp.Mode) *testsmtp.Server {
	t.Helper()
	s, err := testsmtp.Start(mode, "mhai-sender", secret())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// dialTo connects to addr whatever the channel asks for; the certificate
// is still checked against "localhost".
func dialTo(addr string) func(ctx context.Context, network, _ string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
}

func newChannel(t *testing.T, s *testsmtp.Server, port int) *Channel {
	t.Helper()
	app, err := templates.ParseApp("https://app.example.test", false)
	if err != nil {
		t.Fatal(err)
	}
	return &Channel{App: app, Config: Config{Host: "localhost", Port: port, Username: s.User, Password: s.Password,
		From: "no-reply@mail.example.test", FromName: "MHAI", ReplyTo: "support@example.test", RootCAs: s.CAs,
		Timeout: 5 * time.Second, Dial: dialTo(s.Addr)}}
}

func notice(addr string) channel.Message {
	return channel.Message{ID: uuid.New(), Hospital: uuid.New(), Recipient: uuid.New(), Kind: "review_reminder", Address: addr}
}

func decoded(t *testing.T, data string) string {
	t.Helper()
	b, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Over implicit TLS (465) and STARTTLS (587), with the certificate
// verified and AUTH PLAIN: one message, from the configured From with the
// configured Reply-To, to the recipient only, the fixed subject, the
// sign-in link and the safety line, no tracking and no image.
func TestSendsOverTLS(t *testing.T) {
	for _, tc := range []struct {
		mode testsmtp.Mode
		port int
	}{{testsmtp.Implicit, 465}, {testsmtp.StartTLS, 587}} {
		s := start(t, tc.mode)
		c := newChannel(t, s, tc.port)
		res, err := c.Send(context.Background(), notice("synthetic.clinician@example.test"))
		if err != nil || res.Outcome != channel.Accepted || !strings.HasSuffix(res.ProviderID, "@mail.example.test>") {
			t.Fatalf("port %d: %+v %v", tc.port, res, err)
		}
		// The app's host, never the From domain: a mail server that hosts
		// the From domain refuses it as a HELO (impersonation).
		if h := s.Helos(); len(h) == 0 || h[0] != "app.example.test" {
			t.Errorf("port %d: EHLO names %v, want app.example.test first", tc.port, h)
		}
		msgs := s.Messages()
		if len(msgs) != 1 || msgs[0].From != "no-reply@mail.example.test" || len(msgs[0].To) != 1 ||
			msgs[0].To[0] != "synthetic.clinician@example.test" || s.Auths() != 1 {
			t.Fatalf("port %d: %+v", tc.port, msgs)
		}
		data := msgs[0].Data
		for _, want := range []string{`From: "MHAI" <no-reply@mail.example.test>`, "To: synthetic.clinician@example.test",
			"Reply-To: support@example.test", "Subject: You have a new item in MHAI", "Message-ID: " + res.ProviderID,
			"Auto-Submitted: auto-generated", "multipart/alternative"} {
			if !strings.Contains(data, want) {
				t.Errorf("port %d: no %q", tc.port, want)
			}
		}
		body := decoded(t, data)
		for _, want := range []string{"https://app.example.test/", "never ask for your password"} {
			if !strings.Contains(body, want) {
				t.Errorf("port %d: the body lacks %q", tc.port, want)
			}
		}
		for _, bad := range []string{"<img", "utm_", s.Password, "Bcc:"} {
			if strings.Contains(data, bad) {
				t.Errorf("port %d: the message carries %q", tc.port, bad)
			}
		}
	}
}

// Without an app address, EHLO falls back to the From domain.
func TestHelloName(t *testing.T) {
	c := &Channel{Config: Config{From: "no-reply@mail.example.test"}}
	if got := c.helloName(); got != "mail.example.test" {
		t.Errorf("no app: %q", got)
	}
	app, err := templates.ParseApp("https://ai.example.test", false)
	if err != nil {
		t.Fatal(err)
	}
	c.App = app
	if got := c.helloName(); got != "ai.example.test" {
		t.Errorf("with app: %q", got)
	}
}

// A secret link goes in the body only, never a header; an invitation
// names the hospital; without its link, an invite is never sent.
func TestSecretLinkInBodyOnly(t *testing.T) {
	s := start(t, testsmtp.Implicit)
	c := newChannel(t, s, 465)
	link := "https://app.example.test/reset-password?token=" + strings.ReplaceAll(uuid.NewString(), "-", "")
	m := channel.Message{ID: uuid.New(), Hospital: uuid.New(), Recipient: uuid.New(), Kind: "invite",
		Address: "synthetic.invitee@example.test", Link: link, Expires: templates.FormatExpiry(time.Now().Add(72 * time.Hour)),
		HospitalName: "Synthetic General Hospital"}
	if res, err := c.Send(context.Background(), m); err != nil || res.Outcome != channel.Accepted {
		t.Fatalf("%+v %v", res, err)
	}
	data := s.Messages()[0].Data
	head, _, _ := strings.Cut(data, "\r\n\r\n")
	if strings.Contains(head, "token=") || !strings.Contains(decoded(t, data), link) ||
		!strings.Contains(decoded(t, data), "Synthetic General Hospital") {
		t.Fatalf("the link or the name is misplaced:\n%s", head)
	}
	m.Link = ""
	if res, _ := c.Send(context.Background(), m); res.Outcome != channel.Permanent || len(s.Messages()) != 1 {
		t.Fatal("an invite went without its link")
	}
}

// Fail closed: a server that offers no STARTTLS, a certificate the client
// doesn't trust, or a wrong password sends nothing (retryable: the server
// or the settings, not the address); a refused recipient is permanent, a
// temporary refusal retryable; a bad address is never submitted.
func TestRefusals(t *testing.T) {
	ctx := context.Background()
	plain := start(t, testsmtp.NoTLS)
	if res, _ := newChannel(t, plain, 587).Send(ctx, notice("synthetic@example.test")); res.Outcome != channel.Retryable ||
		len(plain.Messages()) != 0 || plain.Auths() != 0 {
		t.Fatalf("no STARTTLS: %+v", res)
	}
	s := start(t, testsmtp.StartTLS)
	untrusted := newChannel(t, s, 587)
	untrusted.Config.RootCAs = start(t, testsmtp.Implicit).CAs // another server's CA
	if res, _ := untrusted.Send(ctx, notice("synthetic@example.test")); res.Outcome != channel.Retryable || s.Auths() != 0 {
		t.Fatalf("untrusted certificate: %+v", res)
	}
	implicitUntrusted := newChannel(t, start(t, testsmtp.Implicit), 465)
	implicitUntrusted.Config.RootCAs = s.CAs
	if res, _ := implicitUntrusted.Send(ctx, notice("synthetic@example.test")); res.Outcome != channel.Retryable {
		t.Fatalf("untrusted certificate on 465: %+v", res)
	}
	wrong := newChannel(t, s, 587)
	wrong.Config.Password = secret()
	if res, _ := wrong.Send(ctx, notice("synthetic@example.test")); res.Outcome != channel.Retryable || len(s.Messages()) != 0 {
		t.Fatalf("wrong password: %+v", res)
	}
	c := newChannel(t, s, 587)
	if res, _ := c.Send(ctx, notice("reject.me@example.test")); res.Outcome != channel.Permanent {
		t.Fatalf("550: %+v", res)
	}
	if res, _ := c.Send(ctx, notice("tempfail@example.test")); res.Outcome != channel.Retryable {
		t.Fatalf("451: %+v", res)
	}
	for _, addr := range []string{"", "not-an-address", "a@example.test\r\nBcc: b@example.test", "Name <a@example.test>",
		"a@example.test, b@example.test"} {
		if res, _ := c.Send(ctx, notice(addr)); res.Outcome != channel.Permanent {
			t.Errorf("%q: %+v", addr, res)
		}
	}
	if res, _ := c.Send(ctx, channel.Message{ID: uuid.New(), Kind: "unknown", Address: "a@example.test"}); res.Outcome != channel.Permanent {
		t.Fatal("unknown kind")
	}
	if len(s.Messages()) != 0 {
		t.Fatalf("%d messages sent", len(s.Messages()))
	}
	down := newChannel(t, s, 587)
	down.Config.Dial = dialTo("127.0.0.1:1")
	if res, _ := down.Send(ctx, notice("synthetic@example.test")); res.Outcome != channel.Retryable {
		t.Fatal("server down")
	}
}

func TestCheckConfig(t *testing.T) {
	good := Config{Host: "smtp.example.test", Port: 587, Username: "mhai", Password: secret(), From: "no-reply@example.test",
		ReplyTo: "support@example.test", FromName: "MHAI"}
	if err := CheckConfig(good); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*Config){
		"port 25":       func(c *Config) { c.Port = 25 },
		"no host":       func(c *Config) { c.Host = "" },
		"no user":       func(c *Config) { c.Username = "" },
		"no password":   func(c *Config) { c.Password = "" },
		"bad from":      func(c *Config) { c.From = "MHAI <no-reply@example.test>" },
		"bad reply-to":  func(c *Config) { c.ReplyTo = "x" },
		"name newline":  func(c *Config) { c.FromName = "MHAI\r\nBcc: x@example.test" },
		"host with url": func(c *Config) { c.Host = "smtp://x" },
	} {
		c := good
		edit(&c)
		if CheckConfig(c) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if Domain("a@Mail.Example.Test") != "mail.example.test" || !strings.Contains(good.Describe(), "starttls") ||
		strings.Contains(good.Describe(), good.Password) {
		t.Fatal("domain or describe")
	}
	if (&Channel{}).Name() != "email" || (&Channel{}).now().IsZero() {
		t.Fatal("name")
	}
}
