package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// Synthetic, obviously fake.
const canary = "synthetic-test-secret-not-real-0003"

func newDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func path() Path {
	return Path{Env: "development", Hospital: uuid.MustParse("aaaaaaaa-0000-4000-8000-00000000000a"),
		Connection: uuid.MustParse("cccccccc-0000-4000-8000-00000000000c"), Field: "api_key"}
}

// Design §6.1: the file store is for the demo; production needs the
// managed store.
func TestFileStoreRefusedInProduction(t *testing.T) {
	d := newDir(t)
	for _, env := range []string{"production", "", "Production", "prod"} {
		if _, err := NewFileStore(d, env); err == nil {
			t.Errorf("file store accepted APP_ENV %q", env)
		}
	}
	for _, env := range []string{"development", "staging"} {
		if _, err := NewFileStore(d, env); err != nil {
			t.Errorf("%s: %v", env, err)
		}
	}
	// A directory others can read, or that isn't there, is refused.
	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(loose, "development"); err == nil {
		t.Error("a 0755 directory accepted")
	}
	if _, err := NewFileStore(filepath.Join(t.TempDir(), "missing"), "development"); err == nil {
		t.Error("a missing directory accepted")
	}
	if _, err := NewFileStore("relative/dir", "development"); err == nil {
		t.Error("a relative directory accepted")
	}
}

// Files 0400, directories 0700; writes are whole (temp file and rename);
// the previous version stays for 24 hours, then goes.
func TestFileStoreModes(t *testing.T) {
	d := newDir(t)
	s, err := NewFileStore(d, "development")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	ctx := context.Background()
	p := path()
	if p.String() != "mhai/development/emr/aaaaaaaa-0000-4000-8000-00000000000a/cccccccc-0000-4000-8000-00000000000c/api_key" {
		t.Fatalf("path %q", p.String())
	}
	if _, _, err := s.Get(ctx, p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get before put: %v", err)
	}
	v1, err := s.Put(ctx, p, seal.NewSecret([]byte(canary)))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	v2, err := s.Put(ctx, p, seal.NewSecret([]byte(canary+"-2")))
	if err != nil || v2 == v1 || v2 < v1 {
		t.Fatalf("second put: %q after %q, %v", v2, v1, err)
	}
	got, ver, err := s.Get(ctx, p)
	if err != nil || ver != v2 || string(got.Bytes()) != canary+"-2" {
		t.Fatalf("get: %q %v", ver, err)
	}
	if old, err := s.GetVersion(ctx, p, v1); err != nil || string(old.Bytes()) != canary {
		t.Fatalf("previous version kept: %v", err)
	}
	err = filepath.WalkDir(d, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		st, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case st.Mode()&os.ModeSymlink != 0:
			t.Errorf("%s is a symlink", path)
		case st.IsDir() && st.Mode().Perm() != 0o700:
			t.Errorf("dir %s mode %o", path, st.Mode().Perm())
		case !st.IsDir() && st.Mode().Perm() != 0o400:
			t.Errorf("file %s mode %o", path, st.Mode().Perm())
		case !st.IsDir() && strings.Contains(filepath.Base(path), "tmp"):
			t.Errorf("temp file left: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// After 24 hours the old version goes; the current one stays.
	now = now.Add(25 * time.Hour)
	if err := s.DeleteVersionsBefore(ctx, p, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetVersion(ctx, p, v1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old version after 25 h: %v", err)
	}
	if _, ver, err := s.Get(ctx, p); err != nil || ver != v2 {
		t.Fatalf("current after cleanup: %q %v", ver, err)
	}
	// A version id is never a path.
	for _, bad := range []string{"../x", "", "v1/../../etc", strings.Repeat("v", 65)} {
		if _, err := s.GetVersion(ctx, p, bad); err == nil {
			t.Errorf("version %q accepted", bad)
		}
	}
	// One version can be dropped (a put that is rolled back).
	if err := s.DeleteVersion(ctx, p, v2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(ctx, p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after dropping the only version: %v", err)
	}
	// DeleteAll removes every field of the connection, and only it.
	other := p
	other.Connection = uuid.MustParse("dddddddd-0000-4000-8000-00000000000d")
	for _, q := range []Path{p, other} {
		if _, err := s.Put(ctx, q, seal.NewSecret([]byte(canary))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteAll(ctx, p.Hospital, p.Connection); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAll(ctx, p.Hospital, p.Connection); err != nil {
		t.Fatalf("second wipe: %v", err)
	}
	if _, _, err := s.Get(ctx, p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after wipe: %v", err)
	}
	if _, _, err := s.Get(ctx, other); err != nil {
		t.Fatalf("other connection wiped: %v", err)
	}
	if err := s.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	// Paths are checked: no other env, no nil ids, a field key.
	for _, bad := range []Path{{Env: "production", Hospital: p.Hospital, Connection: p.Connection, Field: "api_key"},
		{Env: "development", Connection: p.Connection, Field: "api_key"},
		{Env: "development", Hospital: p.Hospital, Connection: p.Connection, Field: "../api_key"}} {
		if _, err := s.Put(ctx, bad, seal.NewSecret([]byte(canary))); !errors.Is(err, ErrPath) {
			t.Errorf("path %+v: %v", bad, err)
		}
	}
	// Errors never carry the value or the directory.
	if _, err := NewFileStore(filepath.Join(d, "nope"), "development"); err == nil || strings.Contains(err.Error(), d) {
		t.Errorf("error names the directory: %v", err)
	}
}

// Put never prunes: a put whose metadata write then rolls back must not
// have dropped the version the metadata still names. The caller prunes,
// after its metadata commit.
func TestFileStorePutKeepsOldVersions(t *testing.T) {
	s, err := NewFileStore(newDir(t), "development")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	ctx := context.Background()
	p := path()
	v1, err := s.Put(ctx, p, seal.NewSecret([]byte(canary)))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(25 * time.Hour)
	if _, err := s.Put(ctx, p, seal.NewSecret([]byte(canary+"-2"))); err != nil {
		t.Fatal(err)
	}
	if old, err := s.GetVersion(ctx, p, v1); err != nil || string(old.Bytes()) != canary {
		t.Fatalf("a put dropped the version before it: %v", err)
	}
}
