package kek

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// snapshot is what a provisioned server must never change: the keyring's
// bytes and mtime, and the directory's entries (no lock file, no
// temporary file).
type snapshot struct {
	raw   []byte
	mtime time.Time
	names []string
}

func snap(t *testing.T, path string) snapshot {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	s := snapshot{raw: raw, mtime: st.ModTime()}
	for _, e := range ents {
		s.names = append(s.names, e.Name())
	}
	return s
}

func (s snapshot) same(o snapshot) bool {
	return bytes.Equal(s.raw, o.raw) && s.mtime.Equal(o.mtime) && strings.Join(s.names, "|") == strings.Join(o.names, "|")
}

// provisionedRing makes a keyring holding hospitals (added with
// AddHospital, as an operator would) and returns its path.
func provisionedRing(t *testing.T, hospitals ...uuid.UUID) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := InitFile(path); err != nil {
		t.Fatal(err)
	}
	for _, h := range hospitals {
		if _, _, err := AddHospital(path, h); err != nil {
			t.Fatalf("add %s: %v", h, err)
		}
	}
	// What the service sees is the copy: the lock file is the admin
	// machine's, not the server's.
	_ = os.Remove(path + ".lock")
	return path
}

// Two servers on one copied keyring in provisioned mode: neither ever
// makes a key, so they can never disagree on a hospital's KEK; a hospital
// without one is ErrNoKey on both, and the file is left exactly as it was.
func TestProvisionedServersNeverCreateAKey(t *testing.T) {
	path := provisionedRing(t, hospA)
	before := snap(t, path)
	ctx := context.Background()
	s1, s2 := NewFile(path, Provisioned), NewFile(path, Provisioned)
	for _, f := range []*File{s1, s2} {
		if err := f.Check(ctx); err != nil {
			t.Fatalf("check: %v", err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(f *File) {
			defer wg.Done()
			if _, _, err := f.Wrap(ctx, hospB, newDEK(t), []byte("x")); !errors.Is(err, ErrNoKey) {
				t.Errorf("wrap for a hospital without a key: %v, want ErrNoKey", err)
			}
			if err := f.Has(ctx, hospB); !errors.Is(err, ErrNoKey) {
				t.Errorf("has: %v, want ErrNoKey", err)
			}
		}([]*File{s1, s2}[i%2])
	}
	wg.Wait()
	if !before.same(snap(t, path)) {
		t.Fatal("a provisioned server changed the keyring or its directory")
	}
	// Hospital A's key is the one in the file: a document sealed on one
	// server opens on the other.
	dek := newDEK(t)
	w, v, err := s1.Wrap(ctx, hospA, dek, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s2.Unwrap(ctx, hospA, v, w, []byte("x")); err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("unwrap on the other server: %v", err)
	}
	if err := s2.Has(ctx, hospA); err != nil {
		t.Fatalf("has A: %v", err)
	}
	if !before.same(snap(t, path)) {
		t.Fatal("a provisioned server changed the keyring")
	}
}

// A hospital without a key is reported (hospital id only) and refused, and
// every other hospital still works: the keyring stays ready.
func TestProvisionedUnknownHospitalIsRefusedOthersWork(t *testing.T) {
	path := provisionedRing(t, hospA)
	f := NewFile(path, Provisioned)
	var missing []uuid.UUID
	f.Missing = func(h uuid.UUID) { missing = append(missing, h) }
	ctx := context.Background()
	if _, _, err := f.Wrap(ctx, hospB, newDEK(t), []byte("x")); !errors.Is(err, ErrNoKey) || errors.Is(err, ErrNotReady) {
		t.Fatalf("wrap B: %v", err)
	}
	if err := f.Has(ctx, hospB); !errors.Is(err, ErrNoKey) {
		t.Fatalf("has B: %v", err)
	}
	if _, err := f.Unwrap(ctx, hospB, 1, make([]byte, 60), []byte("x")); !errors.Is(err, ErrNoKey) {
		t.Fatalf("unwrap B: %v", err)
	}
	if len(missing) != 3 || missing[0] != hospB || missing[2] != hospB {
		t.Fatalf("missing events: %v", missing)
	}
	if err := f.Check(ctx); err != nil {
		t.Fatalf("the keyring stopped being ready: %v", err)
	}
	dek := newDEK(t)
	w, v, err := f.Wrap(ctx, hospA, dek, []byte("x"))
	if err != nil {
		t.Fatalf("wrap A: %v", err)
	}
	if got, err := f.Unwrap(ctx, hospA, v, w, []byte("x")); err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("unwrap A: %v", err)
	}
	if len(missing) != 3 {
		t.Fatalf("a known hospital was reported missing: %v", missing)
	}
	// Bad input is still bad input, not "missing".
	if err := f.Has(ctx, uuid.Nil); !errors.Is(err, ErrInput) {
		t.Fatalf("has nil: %v", err)
	}
}

// A provisioned server reads its copy once: a key added to the file after
// it loaded is not picked up until a restart (which also logs the new
// fingerprint), so what it holds is what it reported at start.
func TestProvisionedReadsTheCopyOnce(t *testing.T) {
	path := provisionedRing(t, hospA)
	f := NewFile(path, Provisioned)
	ctx := context.Background()
	var loads []string
	f.Loaded = func(fp string, n int) { loads = append(loads, fp) }
	if err := f.Check(ctx); err != nil {
		t.Fatal(err)
	}
	fp1, n1, ok := f.Fingerprint()
	if !ok || n1 != 1 || len(loads) != 1 || loads[0] != fp1 {
		t.Fatalf("fingerprint %q %d %v, loads %v", fp1, n1, ok, loads)
	}
	if _, _, err := AddHospital(path, hospB); err != nil {
		t.Fatal(err)
	}
	if err := f.Has(ctx, hospB); !errors.Is(err, ErrNoKey) {
		t.Fatalf("picked up a key without a restart: %v", err)
	}
	if _, err := f.Unwrap(ctx, hospB, 1, make([]byte, 60), []byte("x")); !errors.Is(err, ErrNoKey) {
		t.Fatalf("unwrap re-read the file: %v", err)
	}
	if fp, _, _ := f.Fingerprint(); fp != fp1 || len(loads) != 1 {
		t.Fatal("the fingerprint changed without a restart")
	}
	restarted := NewFile(path, Provisioned)
	if err := restarted.Has(ctx, hospB); err != nil {
		t.Fatalf("after a restart: %v", err)
	}
}

// The fingerprint is the SHA-256 of the file's bytes (so an operator can
// compare it with sha256sum), changes when a hospital is added, and is the
// same on every server holding the same copy.
func TestFingerprintChangesWhenAHospitalIsAdded(t *testing.T) {
	path := provisionedRing(t, hospA)
	ctx := context.Background()
	f := NewFile(path, Provisioned)
	if _, _, ok := f.Fingerprint(); ok {
		t.Fatal("a fingerprint before the keyring loaded")
	}
	if err := f.Check(ctx); err != nil {
		t.Fatal(err)
	}
	fp1, n1, _ := f.Fingerprint()
	raw, _ := os.ReadFile(path)
	sum := sha256.Sum256(raw)
	if fp1 != hex.EncodeToString(sum[:]) || n1 != 1 {
		t.Fatalf("fingerprint %q (%d hospitals), want the file's sha256", fp1, n1)
	}
	fpAdd, nAdd, err := AddHospital(path, hospB)
	if err != nil {
		t.Fatal(err)
	}
	g := NewFile(path, Provisioned)
	if err := g.Check(ctx); err != nil {
		t.Fatal(err)
	}
	fp2, n2, _ := g.Fingerprint()
	if fp2 == fp1 || n2 != 2 || fp2 != fpAdd || nAdd != 2 {
		t.Fatalf("after add: %q (%d), add said %q (%d), before %q", fp2, n2, fpAdd, nAdd, fp1)
	}
	// The fingerprint carries no key: it is 64 hex characters and nothing
	// of the file's text.
	if len(fp2) != 64 || strings.Contains(string(raw), fp2) {
		t.Fatalf("fingerprint %q", fp2)
	}
	// In create mode the fingerprint follows the keys it adds.
	c, _ := initRing(t)
	fpc1, _, _ := c.Fingerprint()
	if _, _, err := c.Wrap(ctx, hospA, newDEK(t), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if fpc2, nc, _ := c.Fingerprint(); fpc2 == fpc1 || nc != 1 {
		t.Fatalf("create mode fingerprint %q -> %q (%d)", fpc1, fpc2, nc)
	}
}

// add-hospital: one key, with its test value, written by a rename; a
// duplicate is refused and changes nothing; so is an id that isn't a
// canonical, non-nil uuid (the format ctl.Hospital accepts).
func TestAddHospital(t *testing.T) {
	path := provisionedRing(t)
	if _, n, err := AddHospital(path, hospA); err != nil || n != 1 {
		t.Fatalf("add: %d %v", n, err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o400 {
		t.Fatalf("mode after add: %v", err)
	}
	f := NewFile(path, Provisioned)
	if err := f.Check(context.Background()); err != nil {
		t.Fatalf("the added key fails its check: %v", err)
	}
	before := snap(t, path)
	if _, _, err := AddHospital(path, hospA); !errors.Is(err, ErrHospitalExists) {
		t.Fatalf("duplicate: %v, want ErrHospitalExists", err)
	}
	if !bytes.Equal(before.raw, snap(t, path).raw) || !before.mtime.Equal(snap(t, path).mtime) {
		t.Fatal("a refused add changed the keyring")
	}
	if _, _, err := AddHospital(path, uuid.Nil); !errors.Is(err, ErrInput) {
		t.Fatalf("nil hospital: %v", err)
	}
	for _, id := range []string{"", "not-a-uuid", "00000000-0000-0000-0000-000000000000",
		"AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE", "{" + hospB.String() + "}", "urn:uuid:" + hospB.String(),
		strings.ReplaceAll(hospB.String(), "-", ""), hospB.String() + " "} {
		if _, err := ParseHospital(id); !errors.Is(err, ErrInput) {
			t.Errorf("%q: %v, want ErrInput", id, err)
		}
	}
	if h, err := ParseHospital(hospB.String()); err != nil || h != hospB {
		t.Fatalf("canonical id: %v", err)
	}
	// No keyring (init first), or one others can read: refused, nothing made.
	if _, _, err := AddHospital(filepath.Join(t.TempDir(), "none.json"), hospA); !errors.Is(err, ErrKeyringMissing) {
		t.Fatalf("missing: %v", err)
	}
}

// Two admins adding the same hospital at once: exactly one key is written.
func TestAddHospitalConcurrentOnlyOneWins(t *testing.T) {
	path := provisionedRing(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, lost := 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := AddHospital(path, hospA)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrHospitalExists):
				lost++
			default:
				t.Errorf("add: %v", err)
			}
		}()
	}
	wg.Wait()
	if won != 1 || lost != 7 {
		t.Fatalf("won %d, lost %d", won, lost)
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".keyring-") {
			t.Fatalf("a temporary file was left: %s", e.Name())
		}
	}
}

// Abuse case: the mode and symlink refusals hold in provisioned mode and
// for add-hospital. A world- or group-readable keyring, or a symlink to a
// good one, never loads, and add-hospital never writes through one.
func TestProvisionedStillRefusesAnExposedOrLinkedKeyring(t *testing.T) {
	ctx := context.Background()
	good := provisionedRing(t, hospA)
	raw, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"world readable": writeRing(t, string(raw), 0o604),
		"group readable": writeRing(t, string(raw), 0o640),
		"symlink":        link,
	} {
		f := NewFile(path, Provisioned)
		if err := f.Check(ctx); !errors.Is(err, ErrKeyringMode) {
			t.Errorf("%s: check %v, want ErrKeyringMode", name, err)
		}
		if err := f.Has(ctx, hospA); !errors.Is(err, ErrNotReady) {
			t.Errorf("%s: has %v, want ErrNotReady", name, err)
		}
		if _, _, err := f.Wrap(ctx, hospA, newDEK(t), []byte("x")); !errors.Is(err, ErrNotReady) {
			t.Errorf("%s: wrap %v", name, err)
		}
		if _, ok := os.Lstat(path); ok != nil {
			t.Fatal(ok)
		}
		if _, _, err := AddHospital(path, hospB); !errors.Is(err, ErrKeyringMode) {
			t.Errorf("%s: add-hospital %v, want ErrKeyringMode", name, err)
		}
	}
	// The link still points at the good keyring, which add-hospital did
	// not touch.
	if after, _ := os.ReadFile(good); !bytes.Equal(after, raw) {
		t.Fatal("add-hospital wrote through a symlink")
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced")
	}
}
