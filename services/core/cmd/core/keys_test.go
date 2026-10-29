package main

import (
	"bytes"
	"errors"
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
	fa := &fakeAudit{}
	dir := t.TempDir()
	trusted := filepath.Join(dir, "request-keys.json")
	var out bytes.Buffer
	for _, name := range []string{"a.pem", "b.pem"} {
		if err := keysWith([]string{"new", "--kind", "request", "--out", filepath.Join(dir, name), "--trusted-out", trusted}, &out, fa.fn()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if got, _ := reqtoken.ReadTrusted(trusted); len(got) != 2 {
		t.Fatalf("want two kids, got %d", len(got))
	}
	third := filepath.Join(dir, "c.pem")
	err := keysWith([]string{"new", "--kind", "request", "--out", third, "--trusted-out", trusted}, &out, fa.fn())
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
	if err := keysWith([]string{"ensure", "--kind", "request", "--out", third, "--trusted-out", trusted}, &out, fa.fn()); err == nil {
		t.Fatal("ensure added a third kid")
	}
}

// Rotation step 4: untrust removes one kid, never the last.
func TestKeysUntrustLastRefused(t *testing.T) {
	fa := &fakeAudit{}
	dir := t.TempDir()
	trusted := filepath.Join(dir, "request-keys.json")
	var out bytes.Buffer
	var kids []string
	for _, name := range []string{"a.pem", "b.pem"} {
		p := filepath.Join(dir, name)
		if err := keysWith([]string{"new", "--kind", "request", "--out", p, "--trusted-out", trusted}, &out, fa.fn()); err != nil {
			t.Fatal(err)
		}
		s, _ := reqtoken.LoadSigner(p)
		kids = append(kids, s.KeyID)
	}
	if err := keysWith([]string{"untrust", "--kid", "0000000000000000", "--trusted-out", trusted}, &out, fa.fn()); err == nil {
		t.Fatal("untrusted an unknown kid")
	}
	out.Reset()
	if err := keysWith([]string{"untrust", "--kid", kids[0], "--trusted-out", trusted}, &out, fa.fn()); err != nil {
		t.Fatal(err)
	}
	got, _ := reqtoken.ReadTrusted(trusted)
	if len(got) != 1 || got[kids[1]] == nil || !strings.Contains(out.String(), kids[0]) {
		t.Fatalf("after untrust: %v %q", got, out.String())
	}
	if err := keysWith([]string{"untrust", "--kid", kids[1], "--trusted-out", trusted}, &out, fa.fn()); err == nil {
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
		if err := keysWith(bad, &out, fa.fn()); err == nil {
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

// fakeAudit stands in for the provisioning login's audit of a rotation.
type fakeAudit struct {
	calls      []string
	fail       bool
	failCommit bool
}

func (f *fakeAudit) fn() rotationAuditor {
	return func(kid, mode, change string, c staged) error {
		f.calls = append(f.calls, kid+" "+mode+" "+change)
		if f.fail {
			return errors.New("keys: audit refused")
		}
		if err := c.prepare(); err != nil {
			c.abort()
			return err
		}
		if f.failCommit {
			// The audit transaction's commit failed after prepare.
			c.abort()
			return errors.New("keys: commit failed")
		}
		if err := c.commit(); err != nil {
			c.abort()
			return err
		}
		return nil
	}
}

// F29: adding a second request kid is a planned rotation and untrust is a
// planned or (--emergency) emergency one: each is audited before the files
// change, and when the audit can't be written nothing changes. The first
// key isn't a rotation and needs no database.
func TestKeysRotationsAreAudited(t *testing.T) {
	dir := t.TempDir()
	trusted := filepath.Join(dir, "request-keys.json")
	var out bytes.Buffer
	fa := &fakeAudit{}
	a, b := filepath.Join(dir, "a.pem"), filepath.Join(dir, "b.pem")
	if err := keysWith([]string{"ensure", "--kind", "request", "--out", a, "--trusted-out", trusted}, &out, fa.fn()); err != nil {
		t.Fatal(err)
	}
	if len(fa.calls) != 0 {
		t.Fatalf("first key audited as a rotation: %v", fa.calls)
	}
	// No database: the second kid is refused, and neither file changes.
	t.Setenv("DATABASE_URL", "")
	if err := keys([]string{"new", "--kind", "request", "--out", b, "--trusted-out", trusted}, &out); err == nil ||
		!strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("rotation without a database: %v", err)
	}
	if exists(b) {
		t.Fatal("key written without an audit")
	}
	// The audit refuses: same.
	fa.fail = true
	if err := keysWith([]string{"new", "--kind", "request", "--out", b, "--trusted-out", trusted}, &out, fa.fn()); err == nil {
		t.Fatal("rotation went ahead without its audit")
	}
	if got, _ := reqtoken.ReadTrusted(trusted); len(got) != 1 || exists(b) {
		t.Fatalf("files changed: %d trusted, key written %v", len(got), exists(b))
	}
	fa.fail, fa.calls = false, nil
	if err := keysWith([]string{"new", "--kind", "request", "--out", b, "--trusted-out", trusted}, &out, fa.fn()); err != nil {
		t.Fatal(err)
	}
	sb, _ := reqtoken.LoadSigner(b)
	sa, _ := reqtoken.LoadSigner(a)
	if len(fa.calls) != 1 || fa.calls[0] != sb.KeyID+" planned added" {
		t.Fatalf("calls %v", fa.calls)
	}
	fa.calls = nil
	if err := keysWith([]string{"untrust", "--kid", sa.KeyID, "--trusted-out", trusted, "--emergency"}, &out, fa.fn()); err != nil {
		t.Fatal(err)
	}
	if len(fa.calls) != 1 || fa.calls[0] != sa.KeyID+" emergency removed" {
		t.Fatalf("calls %v", fa.calls)
	}
	if got, _ := reqtoken.ReadTrusted(trusted); len(got) != 1 || got[sb.KeyID] == nil {
		t.Fatalf("trusted after untrust: %v", got)
	}
}

// Review fix 5: when the audit's commit fails after the files were staged,
// the old key and trusted files stay as they were and no temp file is left.
func TestKeysRotationCommitFailureLeavesFiles(t *testing.T) {
	dir := t.TempDir()
	trusted := filepath.Join(dir, "request-keys.json")
	var out bytes.Buffer
	fa := &fakeAudit{}
	a, b := filepath.Join(dir, "a.pem"), filepath.Join(dir, "b.pem")
	if err := keysWith([]string{"new", "--kind", "request", "--out", a, "--trusted-out", trusted}, &out, fa.fn()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(trusted)
	fa.failCommit = true
	if err := keysWith([]string{"new", "--kind", "request", "--out", b, "--trusted-out", trusted}, &out, fa.fn()); err == nil {
		t.Fatal("a failed commit reported success")
	}
	after, _ := os.ReadFile(trusted)
	if string(before) != string(after) || exists(b) {
		t.Fatalf("files changed after a failed commit (key written %v)", exists(b))
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		t.Fatalf("left behind: %v", ents)
	}
	// Untrust likewise (with two kids trusted first).
	fa.failCommit = false
	if err := keysWith([]string{"new", "--kind", "request", "--out", b, "--trusted-out", trusted}, &out, fa.fn()); err != nil {
		t.Fatal(err)
	}
	sa, _ := reqtoken.LoadSigner(a)
	before, _ = os.ReadFile(trusted)
	fa.failCommit = true
	if err := keysWith([]string{"untrust", "--kid", sa.KeyID, "--trusted-out", trusted, "--emergency"}, &out, fa.fn()); err == nil {
		t.Fatal("a failed commit reported success")
	}
	if after, _ := os.ReadFile(trusted); string(before) != string(after) {
		t.Fatal("trusted file changed after a failed commit")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 3 {
		t.Fatalf("left behind: %v", ents)
	}
}
