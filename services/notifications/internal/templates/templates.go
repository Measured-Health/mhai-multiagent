// Package templates holds the fixed email templates, one per message kind
// (notifications design §4.3, §8.2; §15 PR 4). They are compiled into the
// service. No template takes free text: a template's only variables are
// the allow-list in Vars (the sign-in or secret link, its expiry, and the
// hospital's display name for an invitation), and a test fails the build
// if a template names anything else (NT1). Nothing about a patient, a
// finding, a correction or the recipient's name can reach an email,
// because no such value is ever passed in (NT2).
//
// Subjects are fixed per kind and say only that something is waiting
// (they show on lock screens). Every email says MHAI never asks for a
// password or a two-step code by email. Links go only to the app's own
// address (NOTIFY_APP_URL, core's CORE_APP_URL): a link on any other host
// is refused (NT5). No images, no remote content, no tracking (NT4). The
// hospital name is refused if it holds a line break, a control character
// or an angle bracket, and is escaped in the HTML part (NT11).
//
// Security notices (a password changed or reset, two-step turned on, moved
// to a new app or reset by an admin, a backup code used or new ones made)
// carry no link at all, so a notice can never be mistaken for, or used as,
// a way to sign in. They say what changed and when (UTC), and who to tell if
// it wasn't the reader; never a code, a secret, an address or anything
// about a patient.
package templates

import (
	"bytes"
	"errors"
	htmltemplate "html/template"
	"net/url"
	"strings"
	texttemplate "text/template"
	"time"
)

// Vars are the only values a template may use.
type Vars struct {
	// Link is the secret link of an invite or reset, or the app's sign-in
	// address for the rest. Always on the app's host.
	Link string
	// Expires is when a secret link stops working ("" for a notice).
	Expires string
	// Hospital is the hospital's display name (invitations only).
	Hospital string
	// When is when a security notice's change happened (FormatWhen).
	When string
}

// AllowedFields are Vars' field names: the only names a template may use.
var AllowedFields = []string{"Link", "Expires", "Hospital", "When"}

// Kind is one message kind's fixed subject and bodies.
type Kind struct {
	Subject string
	Text    string // text/template
	HTML    string // html/template (auto-escaped)
	// SecretLink: the link is a single-use credential (invite, reset);
	// otherwise it is the app's sign-in address.
	SecretLink bool
	// NeedsHospital: the hospital's name is in it.
	NeedsHospital bool
	// Security: a security notice. It has no link and states When.
	Security bool
}

const safety = "MHAI will never ask for your password or two-step code by email."

const textFooter = "\n\n" + safety + "\nThis message was sent automatically; please don't reply with personal or patient information.\n"

const htmlFooter = `<p style="color:#555;font-size:13px">` + safety +
	`<br>This message was sent automatically; please don't reply with personal or patient information.</p>`

func page(body string) string {
	return `<!doctype html><html><head><meta charset="utf-8"></head>` +
		`<body style="font-family:Arial,sans-serif;font-size:15px;color:#111">` + body + htmlFooter + `</body></html>`
}

// notice is a message that only says something is waiting behind sign-in.
func notice(subject, line string) Kind {
	return Kind{
		Subject: subject,
		Text:    line + "\n\nSign in to see it: {{.Link}}" + textFooter,
		HTML:    page(`<p>` + line + `</p><p><a href="{{.Link}}">Sign in to MHAI</a></p>`),
	}
}

// SecurityContact is who a security notice says to tell if the change
// wasn't the reader's.
const SecurityContact = "info@measured-health.org"

const securityAdvice = "If this wasn't you, contact your hospital administrator or " + SecurityContact + " straight away."

// securityNotice says what changed on the account, when, and who to tell.
// what is fixed text ending before the time ("Your MHAI password was
// changed"); after is an optional fixed sentence.
func securityNotice(subject, what, after string) Kind {
	line := what + " on {{.When}}."
	if after != "" {
		line += " " + after
	}
	return Kind{
		Subject:  subject,
		Text:     line + "\n\nThis is about your MHAI account in the clinical app.\n\n" + securityAdvice + textFooter,
		HTML:     page(`<p>` + line + `</p><p>This is about your MHAI account in the clinical app.</p><p><strong>` + securityAdvice + `</strong></p>`),
		Security: true,
	}
}

// Kinds are the templates, by message kind (the outbox's kinds).
var Kinds = map[string]Kind{
	"invite": {
		Subject: "Your MHAI invitation",
		Text: "You have been invited to MHAI by {{.Hospital}}.\n\nSet your password with this link (it works once, until {{.Expires}}):\n" +
			"{{.Link}}\n\nIf you weren't expecting this, ignore this email." + textFooter,
		HTML: page(`<p>You have been invited to MHAI by {{.Hospital}}.</p><p><a href="{{.Link}}">Set your password</a> ` +
			`(the link works once, until {{.Expires}}).</p><p>If you weren't expecting this, ignore this email.</p>`),
		SecretLink:    true,
		NeedsHospital: true,
	},
	"password_reset": {
		Subject: "Reset your MHAI password",
		Text: "Someone asked to reset your MHAI password.\n\nSet a new one with this link (it works once, until {{.Expires}}):\n" +
			"{{.Link}}\n\nIf it wasn't you, ignore this email and tell your hospital's MHAI administrator." + textFooter,
		HTML: page(`<p>Someone asked to reset your MHAI password.</p><p><a href="{{.Link}}">Set a new password</a> ` +
			`(the link works once, until {{.Expires}}).</p><p>If it wasn't you, ignore this email and tell your hospital's MHAI administrator.</p>`),
		SecretLink: true,
	},
	"mfa_reset": securityNotice("Your MHAI two-step sign-in was reset",
		"Your MHAI two-step sign-in was reset by your hospital's administrator",
		"You will set it up again when you next sign in."),
	"security_password_changed": securityNotice("Your MHAI password was changed",
		"Your MHAI password was changed", "Your other signed-in devices were signed out."),
	"security_password_reset": securityNotice("Your MHAI password was reset",
		"Your MHAI password was set with a password reset link", "Every device signed in to your account was signed out."),
	"security_mfa_enabled": securityNotice("Two-step sign-in is now on for your MHAI account",
		"Two-step sign-in was turned on for your MHAI account", ""),
	"security_mfa_replaced": securityNotice("Your MHAI two-step app was changed",
		"Your MHAI two-step sign-in was moved to a new authenticator app", "Codes from the old app no longer work."),
	"security_backup_code_used": securityNotice("A backup code was used on your MHAI account",
		"One of your MHAI backup codes was used", "Each backup code works once."),
	"security_backup_codes_made": securityNotice("New MHAI backup codes were made",
		"New backup codes were made for your MHAI account", "Your old backup codes no longer work."),
	"mfa_reminder":    notice("Set up two-step sign-in for MHAI", "Your MHAI account still needs two-step sign-in set up."),
	"review_reminder": notice("You have a new item in MHAI", "You have an item waiting for your review in MHAI."),
	"review_assigned": notice("You have a new item in MHAI", "You have a new item in MHAI."),
}

// Rendered is one email's fixed subject and its two bodies.
type Rendered struct {
	Subject, Text, HTML string
}

var (
	// ErrKind: no template for the kind.
	ErrKind = errors.New("templates: no template for this kind")
	// ErrLink: the link is missing or not on the app's address.
	ErrLink = errors.New("templates: the link is not on the app's address")
	// ErrHospital: a hospital name missing where needed, too long, or with
	// a line break, a control character or an angle bracket.
	ErrHospital = errors.New("templates: the hospital name is not usable")
	// ErrExpiry: a secret link without its expiry.
	ErrExpiry = errors.New("templates: a secret link needs its expiry")
	// ErrWhen: a security notice without its time, or a time that isn't
	// plain text.
	ErrWhen = errors.New("templates: a security notice needs its time")
)

// App is the app's address that every link must be on.
type App struct{ URL *url.URL }

// ParseApp checks raw: https, or http in development only.
func ParseApp(raw string, development bool) (App, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return App{}, ErrLink
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && development) {
		return App{}, ErrLink
	}
	return App{URL: u}, nil
}

// SignIn is the app's sign-in address (the link of every notice).
func (a App) SignIn() string { return strings.TrimRight(a.URL.String(), "/") + "/" }

// owns reports whether link is on the app's scheme and host.
func (a App) owns(link string) bool {
	if a.URL == nil || strings.ContainsAny(link, "\r\n\"'<> ") {
		return false
	}
	u, err := url.Parse(link)
	return err == nil && u.Scheme == a.URL.Scheme && strings.EqualFold(u.Host, a.URL.Host) && u.User == nil && u.Fragment == ""
}

// HospitalName checks a hospital's display name for a template.
func HospitalName(s string) error {
	if s == "" || len(s) > 200 || strings.ContainsAny(s, "<>") {
		return ErrHospital
	}
	for _, r := range s {
		if r < ' ' || r == 0x7f {
			return ErrHospital
		}
	}
	return nil
}

// FormatExpiry is how an expiry is shown: UTC, unambiguous.
func FormatExpiry(t time.Time) string { return t.UTC().Format("2 January 2006 15:04 UTC") }

// FormatWhen is how a security notice's time is shown: UTC, unambiguous.
func FormatWhen(t time.Time) string { return t.UTC().Format("2 January 2006 at 15:04 UTC") }

// Render fills kind's template with v. A secret link must be on the app's
// host; a notice's link is the app's sign-in address whatever v says.
func (a App) Render(kind string, v Vars) (Rendered, error) {
	k, ok := Kinds[kind]
	if !ok {
		return Rendered{}, ErrKind
	}
	switch {
	case k.Security:
		// No link, whatever v says; only the time.
		if v.When == "" || len(v.When) > 64 || strings.ContainsAny(v.When, "<>\r\n") {
			return Rendered{}, ErrWhen
		}
		v = Vars{When: v.When}
	case !k.SecretLink:
		v = Vars{Link: a.SignIn()}
	case v.Expires == "":
		return Rendered{}, ErrExpiry
	}
	if !k.Security && !a.owns(v.Link) {
		return Rendered{}, ErrLink
	}
	if k.NeedsHospital {
		if err := HospitalName(v.Hospital); err != nil {
			return Rendered{}, err
		}
	} else {
		v.Hospital = ""
	}
	tt, err := texttemplate.New(kind).Option("missingkey=error").Parse(k.Text)
	if err != nil {
		return Rendered{}, err
	}
	ht, err := htmltemplate.New(kind).Option("missingkey=error").Parse(k.HTML)
	if err != nil {
		return Rendered{}, err
	}
	var tb, hb bytes.Buffer
	if err := tt.Execute(&tb, v); err != nil {
		return Rendered{}, err
	}
	if err := ht.Execute(&hb, v); err != nil {
		return Rendered{}, err
	}
	return Rendered{Subject: k.Subject, Text: tb.String(), HTML: hb.String()}, nil
}
