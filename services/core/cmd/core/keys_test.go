package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// core keys ensure --kind request writes the private key (0600) and adds
// its public half to the trusted file (0644); a second run keeps both.
// Output holds the kid only.
func TestKeysEnsureRequestWritesTrusted(t *testing.T) {
	dir := t.TempDir()
	key, trusted := filepath.Join(dir, "request.pem"), filepath.Join(dir, "trusted", "request-keys.json")
	if err := os.MkdirAll(filepath.Dir(trusted), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	args := []string{"ensure", "--out", key, "--kind", "request", "--trusted-out", trusted}
	if err := keys(args, &out); err != nil {
		t.Fatal(err)
	}
	s, err := reqtoken.LoadSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(key); st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(trusted); st.Mode().Perm() != 0o644 {
		t.Fatalf("trusted mode %v", st.Mode().Perm())
	}
	got, err := reqtoken.ReadTrusted(trusted)
	if err != nil || len(got) != 1 || !got[s.KeyID].Equal(s.PublicKeys()[s.KeyID]) {
		t.Fatalf("trusted %v %v", got, err)
	}
	if !strings.Contains(out.String(), s.KeyID) || strings.Contains(out.String(), "PRIVATE") ||
		strings.Contains(out.String(), key) {
		t.Fatalf("output %q", out.String())
	}
	raw, _ := os.ReadFile(trusted)
	if strings.Contains(string(raw), "PRIVATE") {
		t.Fatal("private key in the trusted file")
	}
	// Again: nothing changes.
	out.Reset()
	if err := keys(args, &out); err != nil || !strings.Contains(out.String(), s.KeyID) {
		t.Fatalf("second ensure: %v %q", err, out.String())
	}
	again, _ := reqtoken.ReadTrusted(trusted)
	if len(again) != 1 {
		t.Fatalf("second ensure changed the trusted set: %d", len(again))
	}
	// A lost trusted file is rebuilt from the kept key.
	_ = os.Remove(trusted)
	if err := keys(args, &out); err != nil {
		t.Fatal(err)
	}
	if again, _ := reqtoken.ReadTrusted(trusted); len(again) != 1 || again[s.KeyID] == nil {
		t.Fatalf("rebuilt %v", again)
	}
	// --kind request needs --trusted-out; access takes none; unknown kinds fail.
	for _, bad := range [][]string{
		{"ensure", "--out", filepath.Join(dir, "x.pem"), "--kind", "request"},
		{"ensure", "--out", filepath.Join(dir, "y.pem"), "--trusted-out", trusted},
		{"ensure", "--out", filepath.Join(dir, "z.pem"), "--kind", "refresh"},
		{"ensure", "--out", filepath.Join(dir, "w.pem"), "--kind", "request", "--trusted-out", trusted, "extra"},
	} {
		if err := keys(bad, &out); err == nil {
			t.Errorf("keys %v accepted", bad)
		}
	}
	for _, p := range []string{"x.pem", "y.pem", "z.pem", "w.pem"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err == nil {
			t.Errorf("%s written by a refused command", p)
		}
	}
	// A corrupt trusted file is refused, not overwritten.
	_ = os.WriteFile(trusted, []byte("junk"), 0o644)
	if err := keys(args, &out); err == nil {
		t.Fatal("ensure overwrote a corrupt trusted file")
	}
	if raw, _ := os.ReadFile(trusted); string(raw) != "junk" {
		t.Fatal("corrupt trusted file rewritten")
	}
}

// Rotation step 1 adds a second kid; a third is refused before any key is
// written.
func TestKeysThirdKidRefused(t *testing.T) {
	dir := t.TempDir()
	trusted := filepath.Join(dir, "request-keys.json")
	var out bytes.Buffer
	for _, name := range []string{"a.pem", "b.pem"} {
		if err := keys([]string{"new", "--kind", "request", "--out", filepath.Join(dir, name), "--trusted-out", trusted}, &out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if got, _ := reqtoken.ReadTrusted(trusted); len(got) != 2 {
		t.Fatalf("want two kids, got %d", len(got))
	}
	third := filepath.Join(dir, "c.pem")
	err := keys([]string{"new", "--kind", "request", "--out", third, "--trusted-out", trusted}, &out)
	if err == nil {
		t.Fatal("a third kid was accepted")
	}
	if strings.Contains(err.Error(), dir) {
		t.Fatalf("error names a path: %v", err)
	}
	if _, err := os.Stat(third); err == nil {
		t.Fatal("the third key was written although it can't be trusted")
	}
	if got, _ := reqtoken.ReadTrusted(trusted); len(got) != 2 {
		t.Fatalf("trusted set changed: %d", len(got))
	}
	// ensure on a new path is refused the same way.
	if err := keys([]string{"ensure", "--kind", "request", "--out", third, "--trusted-out", trusted}, &out); err == nil {
		t.Fatal("ensure added a third kid")
	}
}

// Rotation step 4: untrust removes one kid, never the last.
func TestKeysUntrustLastRefused(t *testing.T) {
	dir := t.TempDir()
	trusted := filepath.Join(dir, "request-keys.json")
	var out bytes.Buffer
	var kids []string
	for _, name := range []string{"a.pem", "b.pem"} {
		p := filepath.Join(dir, name)
		if err := keys([]string{"new", "--kind", "request", "--out", p, "--trusted-out", trusted}, &out); err != nil {
			t.Fatal(err)
		}
		s, _ := reqtoken.LoadSigner(p)
		kids = append(kids, s.KeyID)
	}
	if err := keys([]string{"untrust", "--kid", "0000000000000000", "--trusted-out", trusted}, &out); err == nil {
		t.Fatal("untrusted an unknown kid")
	}
	out.Reset()
	if err := keys([]string{"untrust", "--kid", kids[0], "--trusted-out", trusted}, &out); err != nil {
		t.Fatal(err)
	}
	got, _ := reqtoken.ReadTrusted(trusted)
	if len(got) != 1 || got[kids[1]] == nil || !strings.Contains(out.String(), kids[0]) {
		t.Fatalf("after untrust: %v %q", got, out.String())
	}
	if err := keys([]string{"untrust", "--kid", kids[1], "--trusted-out", trusted}, &out); err == nil {
		t.Fatal("removed the last trusted kid")
	}
	if got, _ := reqtoken.ReadTrusted(trusted); len(got) != 1 {
		t.Fatal("the last kid is gone")
	}
	for _, bad := range [][]string{
		{"untrust"}, {"untrust", "--kid", kids[1]}, {"untrust", "--trusted-out", trusted},
		{"untrust", "--kid", "", "--trusted-out", trusted},
		{"untrust", "--kid", kids[1], "--trusted-out", filepath.Join(dir, "missing.json")},
	} {
		if err := keys(bad, &out); err == nil {
			t.Errorf("keys %v accepted", bad)
		}
	}
}

// Without --kind the command makes an access-token key, as the compose
// one-shot expects, and writes no trusted file.
func TestKeysDefaultKindIsAccess(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "access.pem")
	var out bytes.Buffer
	if err := keys([]string{"ensure", "--out", key}, &out); err != nil {
		t.Fatal(err)
	}
	iss, err := token.LoadIssuer(key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "access-token key") || !strings.Contains(out.String(), iss.KeyID) {
		t.Fatalf("output %q", out.String())
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("wrote more than the key: %v", ents)
	}
	explicit := filepath.Join(dir, "access2.pem")
	if err := keys([]string{"new", "--kind", "access", "--out", explicit}, &out); err != nil {
		t.Fatal(err)
	}
}
