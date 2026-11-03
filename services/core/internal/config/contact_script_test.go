package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSetupHostContactOKMatches runs deploy/setup-host.sh's contact_ok
// against SecurityContactOK. The script may be stricter only for what .env
// can't hold (a quote, backslash, backtick, $ or #) and an IP-literal domain; everywhere else the two
// must agree.
func TestSetupHostContactOKMatches(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "deploy", "setup-host.sh"))
	if err != nil {
		t.Skip("deploy/setup-host.sh is not beside this module")
	}
	text := string(src)
	a := strings.Index(text, "contact_ok() {")
	b := strings.Index(text, "set_contact() {")
	if a < 0 || b < a {
		t.Fatal("contact_ok not found in deploy/setup-host.sh")
	}
	fn := text[a:b]
	script := func(kind, v string) bool {
		cmd := exec.Command("bash", "-c", fn+"\ncontact_ok \"$1\" \"$2\"", "bash", kind, v)
		return cmd.Run() == nil
	}
	long := func(n int, c string) string { return strings.Repeat(c, n) }
	okEmail := "security@mh.example"
	okName := "Measured Health security"
	names := []string{
		okName, "A", "", " lead", "trail ", "\tTab", "tab\t", "a\nb", "a\rb", "a\x07b", "a\u200bb", "a\u00adb", "a\u0085b", "\u00a0nbsp",
		"nbsp\u00a0", "\u3000x", "x\u2028", long(120, "a"), long(121, "a"), long(120, "\u00e9"), long(121, "\u00e9"), "Dr Asha Rao", "P-AB12",
		"call P-ABCD1234 now", "P-ABC", "xP-ABCD", "P-abcd", "P-ABCDEFGHIJKLM", "(P-ABCD)", "P-AB12.", "Zo\u00eb M\u00fcller", "a\xffb",
		"say \"hi\"", "it's", "back\\slash", "tick`", "cost $5", "team #1", "a #b", "a#b",
	}
	emails := []string{
		okEmail, "", "a@b", "a@b.c", "a.b@c.d", ".a@b.c", "a.@b.c", "a..b@c.d", "a@.b", "a@b.", "a@b..c", "a b@c.d", "a@b c",
		"Name <a@b.c>", "<a@b.c>", "a@b.c (x)", "(x) a@b.c", "a@@b.c", "a@b@c", "@b.c", "a@", "a,b@c.d", "a;b@c.d", "a:b@c.d",
		"a(b)@c.d", "\"a\"@b.c", "a'b@c.d", "a$b@c.d", "a#b@c.d", "a\\b@c.d", "a`b@c.d", "a+tag@b.c", "a!%&*/=?^_{|}~-@b.c",
		"a@[x]", "\u00fc@b.c", "a@b\u00fc.c", "a@b\u00a0c", "a\u0085b@c.d", "a\nb@c.d", "a\xffb@c.d",
		long(64, "a") + "@" + long(63, "b") + ".c", long(250, "a") + "@b.c", long(246, "a") + "@b.cd",
		long(247, "a") + "@b.cd", long(248, "a") + "@b.cd", long(254, "a"), "\u00e9\u00e9\u00e9\u00e9\u00e9@" + long(120, "\u00e9") + ".\u00e9",
	}
	for _, n := range names {
		want := SecurityContactOK(n, okEmail) && !strings.ContainsAny(n, "$\"'\\`#")
		if got := script("name", n); got != want {
			t.Errorf("name %q: script %v, core %v", n, got, want)
		}
	}
	// An IP-literal domain (a@[1.2.3.4]) is the one address core accepts
	// that the script refuses: stricter is safe, and no contact needs it.
	if script("email", "a@[1.2.3.4]") {
		t.Error("the script accepts an IP-literal domain; core's test below assumes it does not")
	}
	for _, e := range emails {
		want := SecurityContactOK(okName, e) && !strings.ContainsAny(e, "$\"'\\`#")
		if got := script("email", e); got != want {
			t.Errorf("email %q: script %v, core %v", e, got, want)
		}
	}
}
