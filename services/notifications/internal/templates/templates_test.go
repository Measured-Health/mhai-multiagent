package templates

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"text/template/parse"
	"time"
)

func app(t *testing.T) App {
	t.Helper()
	a, err := ParseApp("https://app.example.test", false)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// fields lists every field a template names, and any other construct as
// "construct:…" or "non-field:…".
func fields(t *testing.T, name, src string) []string {
	t.Helper()
	trees, err := parse.Parse(name, src, "{{", "}}")
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var out []string
	var walk func(n parse.Node)
	walk = func(n parse.Node) {
		switch x := n.(type) {
		case *parse.ListNode:
			if x == nil {
				return
			}
			for _, c := range x.Nodes {
				walk(c)
			}
		case *parse.ActionNode:
			for _, c := range x.Pipe.Cmds {
				for _, a := range c.Args {
					if f, ok := a.(*parse.FieldNode); ok {
						out = append(out, strings.Join(f.Ident, "."))
					} else {
						out = append(out, "non-field:"+a.String())
					}
				}
			}
		case *parse.TextNode:
		default:
			out = append(out, "construct:"+n.String())
		}
	}
	for _, tr := range trees {
		walk(tr.Root)
	}
	return out
}

func allowed(t *testing.T, name, src string) bool {
	if _, err := parse.Parse(name, src, "{{", "}}"); err != nil {
		return false // a function call, say: not even parsable without one
	}
	for _, f := range fields(t, name, src) {
		if !slices.Contains(AllowedFields, f) {
			return false
		}
	}
	return true
}

// NT1: every kind's subject is fixed text, and its bodies name only the
// allow-listed variables, with no other construct (no range, call, index
// or function). A template that names anything else fails here.
func TestTemplatesUseOnlyTheAllowList(t *testing.T) {
	want := []string{"invite", "password_reset", "mfa_reset", "mfa_reminder", "review_reminder", "review_assigned"}
	if len(Kinds) != len(want) {
		t.Fatalf("%d kinds", len(Kinds))
	}
	for _, kind := range want {
		k, ok := Kinds[kind]
		if !ok {
			t.Fatalf("no template for %s", kind)
		}
		if strings.Contains(k.Subject, "{{") || k.Subject == "" {
			t.Errorf("%s: subject %q", kind, k.Subject)
		}
		for part, src := range map[string]string{"text": k.Text, "html": k.HTML} {
			if !allowed(t, kind+"/"+part, src) {
				t.Errorf("%s %s names something outside the allow-list: %v", kind, part, fields(t, kind, src))
			}
			if slices.Contains(fields(t, kind, src), "Hospital") && !k.NeedsHospital {
				t.Errorf("%s names the hospital", kind)
			}
		}
	}
	for _, bad := range []string{"{{.Patient}}", `{{printf "%s" .Link}}`, "{{range .X}}{{end}}", "{{.Link.Host}}"} {
		if allowed(t, "bad", bad) {
			t.Errorf("the allow-list check passed %s", bad)
		}
	}
}

// NT2, NT4: no rendered subject or body carries anything but the fixed
// words, the link, the expiry and (for an invitation) the hospital: no
// image, script or remote URL but the app's, no tracking parameter; every
// email carries the safety line; subjects carry no data.
func TestRenderedEmailsCarryNothingElse(t *testing.T) {
	a := app(t)
	exp := FormatExpiry(time.Date(2026, 11, 27, 9, 30, 0, 0, time.UTC))
	link := "https://app.example.test/reset-password?token=SYNTHlinkValue0123"
	urlRe := regexp.MustCompile(`https?://[^\s"'<>]+`)
	for kind, k := range Kinds {
		r, err := a.Render(kind, Vars{Link: link, Expires: exp, Hospital: "Synthetic General Hospital"})
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		all := r.Subject + "\n" + r.Text + "\n" + r.HTML
		for _, bad := range []string{"<img", "<script", "<iframe", "<link", "src=", "utm_", "Synthetic Patient", "MRN", "{{",
			"<no value>"} {
			if strings.Contains(all, bad) {
				t.Errorf("%s carries %q", kind, bad)
			}
		}
		for _, u := range urlRe.FindAllString(all, -1) {
			if !strings.HasPrefix(u, "https://app.example.test/") {
				t.Errorf("%s links outside the app: %s", kind, u)
			}
		}
		if !strings.Contains(r.Text, safety) || !strings.Contains(r.HTML, safety) {
			t.Errorf("%s lacks the safety line", kind)
		}
		if strings.Contains(all, "SYNTHlinkValue0123") != k.SecretLink {
			t.Errorf("%s: the secret link present %v", kind, !k.SecretLink)
		}
		if strings.Contains(r.Subject, "SYNTH") || strings.Contains(r.Subject, "Synthetic") {
			t.Errorf("%s: the subject carries data", kind)
		}
		if strings.Contains(all, "Synthetic General Hospital") != k.NeedsHospital {
			t.Errorf("%s: the hospital's name", kind)
		}
	}
}

// NT5, NT11: a link on another host or scheme, with credentials or a line
// break is refused; a hospital name with a line break, a control
// character or an angle bracket is refused, and quotes and ampersands are
// escaped in the HTML.
func TestRenderRefusals(t *testing.T) {
	a := app(t)
	exp := FormatExpiry(time.Now())
	for _, link := range []string{"", "https://evil.example/reset?token=x", "http://app.example.test/reset",
		"https://user@app.example.test/reset", "https://app.example.test.evil.example/x", "https://app.example.test/x#frag",
		"https://app.example.test/x\r\nBcc: someone@example.test", "javascript:alert(1)", "//evil.example/x"} {
		if _, err := a.Render("password_reset", Vars{Link: link, Expires: exp}); err != ErrLink {
			t.Errorf("%q: %v", link, err)
		}
	}
	for _, name := range []string{"", "Bad\r\nBcc: x@example.test", "<b>Hospital</b>", "Tab\there", strings.Repeat("x", 201)} {
		if _, err := a.Render("invite", Vars{Link: "https://app.example.test/accept?token=t", Expires: exp, Hospital: name}); err != ErrHospital {
			t.Errorf("%q: %v", name, err)
		}
	}
	r, err := a.Render("invite", Vars{Link: "https://app.example.test/accept?token=t&x=1", Expires: exp, Hospital: `St "Synth" & Co`})
	if err != nil || !strings.Contains(r.HTML, "St &#34;Synth&#34; &amp; Co") || !strings.Contains(r.Text, `St "Synth" & Co`) {
		t.Fatalf("escaping: %v\n%s", err, r.HTML)
	}
	if _, err := a.Render("nope", Vars{}); err != ErrKind {
		t.Fatal(err)
	}
	if _, err := a.Render("invite", Vars{Link: "https://app.example.test/a", Hospital: "H"}); err != ErrExpiry {
		t.Fatal(err)
	}
	r, err = a.Render("review_reminder", Vars{Link: "https://evil.example/", Hospital: "Synth"})
	if err != nil || !strings.Contains(r.Text, "https://app.example.test/") || strings.Contains(r.Text, "evil") ||
		strings.Contains(r.Text, "Synth") {
		t.Fatalf("notice: %v %s", err, r.Text)
	}
}

func TestParseApp(t *testing.T) {
	for raw, dev := range map[string]bool{"https://app.example.test": false, "https://app.example.test/": false,
		"http://localhost:3000": true} {
		if _, err := ParseApp(raw, dev); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	for raw, dev := range map[string]bool{"http://app.example.test": false, "ftp://x": true, "https://": false,
		"https://u@app.example.test": false, "https://app.example.test/?q=1": false, "%%": false} {
		if _, err := ParseApp(raw, dev); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
	if got := FormatExpiry(time.Date(2026, 11, 27, 9, 5, 0, 0, time.FixedZone("WAT", 3600))); got != "27 November 2026 08:05 UTC" {
		t.Fatal(got)
	}
	if (App{}).owns("https://x") {
		t.Fatal("an empty app owns a link")
	}
}
