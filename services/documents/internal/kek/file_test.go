package kek

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// Synthetic ids only.
var (
	hospA = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	hospB = uuid.MustParse("33333333-3333-4333-8333-333333333333")
)

func newDEK(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, DEKSize)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// initRing makes an empty keyring in a fresh directory and opens it.
func initRing(t *testing.T) (*File, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := InitFile(path); err != nil {
		t.Fatalf("init: %v", err)
	}
	f := NewFile(path)
	if err := f.Check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	return f, path
}

func TestInitFileMakesAnOwnerOnlyEmptyKeyring(t *testing.T) {
	_, path := initRing(t)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o400 {
		t.Fatalf("mode %v, want 0400", st.Mode().Perm())
	}
	// Init never overwrites a keyring: that would destroy every KEK.
	if err := InitFile(path); !errors.Is(err, ErrExists) {
		t.Fatalf("second init: %v, want ErrExists", err)
	}
}

func TestWrapCreatesTheHospitalsKeyAndRoundTrips(t *testing.T) {
	f, path := initRing(t)
	ctx := context.Background()
	dek, aad := newDEK(t), []byte("mhai.documents/1|a|b|original|0")
	w, v, err := f.Wrap(ctx, hospA, dek, aad)
	if err != nil || v != 1 {
		t.Fatalf("wrap: v%d %v", v, err)
	}
	if bytes.Contains(w, dek) || len(w) != 60 {
		t.Fatalf("wrapped key: %d bytes", len(w))
	}
	got, err := f.Unwrap(ctx, hospA, v, w, aad)
	if err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("unwrap: %v", err)
	}
	// The key was stored before it was used: a fresh provider (a restart)
	// unwraps it, and the file stays owner-read-only.
	f2 := NewFile(path)
	if err := f2.Check(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, err := f2.Unwrap(ctx, hospA, v, w, aad); err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("unwrap after reload: %v", err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o400 {
		t.Fatalf("mode after write %v", st.Mode().Perm())
	}
	// No temporary file is left beside it (only the lock file).
	if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 2 {
		t.Fatalf("%d files in the keyring directory", len(ents))
	}
}

// DS3 (key side): a wrapped key moved to another hospital, version or
// object, or changed, doesn't unwrap; hospital B's KEK is its own.
func TestUnwrapRefusesAnotherHospitalVersionObjectOrByte(t *testing.T) {
	f, _ := initRing(t)
	ctx := context.Background()
	dek, aad := newDEK(t), []byte("mhai.documents/1|a|b|page|1")
	w, v, err := f.Wrap(ctx, hospA, dek, aad)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Wrap(ctx, hospB, newDEK(t), aad); err != nil {
		t.Fatal(err)
	}
	flipped := bytes.Clone(w)
	flipped[len(flipped)-1] ^= 1
	for name, tc := range map[string]struct {
		h   uuid.UUID
		v   int
		w   []byte
		aad []byte
		err error
	}{
		"other hospital": {hospB, v, w, aad, ErrUnwrap},
		"other page":     {hospA, v, w, []byte("mhai.documents/1|a|b|page|2"), ErrUnwrap},
		"other version":  {hospA, v + 1, w, aad, ErrNoKey},
		"changed byte":   {hospA, v, flipped, aad, ErrUnwrap},
		"short":          {hospA, v, w[:20], aad, ErrUnwrap},
		"no hospital":    {uuid.Nil, v, w, aad, ErrInput},
		"no aad":         {hospA, v, w, nil, ErrInput},
		"unknown hosp":   {uuid.New(), 1, w, aad, ErrNoKey},
	} {
		if _, err := f.Unwrap(ctx, tc.h, tc.v, tc.w, tc.aad); !errors.Is(err, tc.err) {
			t.Errorf("%s: %v, want %v", name, err, tc.err)
		}
	}
	if _, _, err := f.Wrap(ctx, hospA, dek[:16], aad); !errors.Is(err, ErrInput) {
		t.Errorf("short dek: %v", err)
	}
}

// Concurrent first documents of several hospitals: every KEK is stored once
// and every wrap still opens after a reload.
func TestConcurrentWrapsStoreEveryKey(t *testing.T) {
	f, path := initRing(t)
	ctx := context.Background()
	type res struct {
		h   uuid.UUID
		w   []byte
		dek []byte
	}
	var mu sync.Mutex
	var out []res
	var wg sync.WaitGroup
	hs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(h uuid.UUID) {
			defer wg.Done()
			dek := newDEK(t)
			w, _, err := f.Wrap(ctx, h, dek, []byte("x"))
			if err != nil {
				t.Errorf("wrap: %v", err)
				return
			}
			mu.Lock()
			out = append(out, res{h, w, dek})
			mu.Unlock()
		}(hs[i%len(hs)])
	}
	wg.Wait()
	f2 := NewFile(path)
	for _, r := range out {
		if got, err := f2.Unwrap(ctx, r.h, 1, r.w, []byte("x")); err != nil || !bytes.Equal(got, r.dek) {
			t.Fatalf("unwrap after reload: %v", err)
		}
	}
}

// Two providers on one keyring (two replicas by mistake): each hospital's
// first key is written once, whoever comes second adopts it, and every
// wrap opens in both.
func TestTwoProcessesShareOneKeyring(t *testing.T) {
	f1, path := initRing(t)
	f2 := NewFile(path)
	ctx := context.Background()
	if err := f2.Check(ctx); err != nil {
		t.Fatal(err)
	}
	dekA, dekB := newDEK(t), newDEK(t)
	w1, _, err := f1.Wrap(ctx, hospA, dekA, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	// f2 loaded before hospital A had a key: it adopts f1's rather than
	// writing its own over it.
	w2, _, err := f2.Wrap(ctx, hospA, dekB, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []*File{f1, f2, NewFile(path)} {
		if got, err := f.Unwrap(ctx, hospA, 1, w1, []byte("x")); err != nil || !bytes.Equal(got, dekA) {
			t.Fatalf("w1: %v", err)
		}
		if got, err := f.Unwrap(ctx, hospA, 1, w2, []byte("x")); err != nil || !bytes.Equal(got, dekB) {
			t.Fatalf("w2: %v", err)
		}
	}
	// A key f2 added is found by f1, which loaded before it existed.
	w3, _, err := f2.Wrap(ctx, hospB, dekA, []byte("y"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f1.Unwrap(ctx, hospB, 1, w3, []byte("y")); err != nil || !bytes.Equal(got, dekA) {
		t.Fatalf("w3 through f1: %v", err)
	}
}

// writeRing writes raw as the keyring with mode.
func writeRing(t *testing.T, raw string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := os.WriteFile(path, []byte(raw), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// ringWith is a keyring holding one hospital with key (base64) and check.
func ringWith(key, check string) string {
	b, _ := json.Marshal(map[string]any{"format": keyringFormat, "hospitals": map[string]any{
		hospA.String(): map[string]any{"current": 1, "keys": map[string]any{"1": map[string]string{"key": key, "check": check}}}}})
	return string(b)
}

// A real key and its check value for hospital A, version 1.
func realEntry(t *testing.T) (key, check string) {
	t.Helper()
	k := newDEK(t)
	c, err := makeCheck(k, hospA, 1)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k), base64.StdEncoding.EncodeToString(c)
}

// §6.2 startup refusal: no keyring, a keyring others can read, a symlink,
// a placeholder key, a key whose test unwrap fails, or anything malformed:
// Check fails, and Wrap and Unwrap refuse, with a closed error that never
// carries the key or the path.
func TestCheckRefusesBadKeyrings(t *testing.T) {
	ctx := context.Background()
	key, check := realEntry(t)
	_, otherCheck := realEntry(t)
	same := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	typed := base64.StdEncoding.EncodeToString([]byte("change-me-change-me-change-me-ok"))
	short := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	dir := t.TempDir()
	good := writeRing(t, ringWith(key, check), 0o400)
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		path string
		want error
	}{
		"missing":           {filepath.Join(dir, "none.json"), ErrKeyringMissing},
		"group readable":    {writeRing(t, ringWith(key, check), 0o440), ErrKeyringMode},
		"world readable":    {writeRing(t, ringWith(key, check), 0o604), ErrKeyringMode},
		"symlink":           {link, ErrKeyringMode},
		"directory":         {dir, ErrKeyringMode},
		"placeholder same":  {writeRing(t, ringWith(same, check), 0o400), ErrKeyringPlaceholder},
		"placeholder typed": {writeRing(t, ringWith(typed, check), 0o400), ErrKeyringPlaceholder},
		"short key":         {writeRing(t, ringWith(short, check), 0o400), ErrKeyringInvalid},
		"check fails":       {writeRing(t, ringWith(key, otherCheck), 0o400), ErrKeyringCheck},
		"not json":          {writeRing(t, "{", 0o400), ErrKeyringInvalid},
		"unknown field":     {writeRing(t, `{"format":"`+keyringFormat+`","hospitals":{},"extra":1}`, 0o400), ErrKeyringInvalid},
		"wrong format":      {writeRing(t, `{"format":"other/1","hospitals":{}}`, 0o400), ErrKeyringInvalid},
		"bad hospital id": {writeRing(t, strings.Replace(ringWith(key, check), hospA.String(), "not-a-uuid", 1), 0o400),
			ErrKeyringInvalid},
		"current missing": {writeRing(t, strings.Replace(ringWith(key, check), `"current":1`, `"current":2`, 1), 0o400),
			ErrKeyringInvalid},
		"bad version": {writeRing(t, strings.Replace(ringWith(key, check), `"1":{`, `"0":{`, 1), 0o400),
			ErrKeyringInvalid},
	}
	for name, tc := range cases {
		f := NewFile(tc.path)
		err := f.Check(ctx)
		if !errors.Is(err, tc.want) || !errors.Is(err, ErrNotReady) {
			t.Errorf("%s: %v, want %v (and ErrNotReady)", name, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), tc.path) || strings.Contains(err.Error(), key) {
			t.Errorf("%s: the error carries the path or the key", name)
		}
		if _, _, err := f.Wrap(ctx, hospA, newDEK(t), []byte("x")); !errors.Is(err, ErrNotReady) {
			t.Errorf("%s: wrap %v", name, err)
		}
		if _, err := f.Unwrap(ctx, hospA, 1, make([]byte, 60), []byte("x")); !errors.Is(err, ErrNotReady) {
			t.Errorf("%s: unwrap %v", name, err)
		}
	}
	if f := NewFile(good); f.Check(ctx) != nil || f.Name() != "file" {
		t.Fatal("the good keyring should load")
	}
}

// A keyring fixed on disk is picked up by the next Check: no restart.
func TestCheckRetriesUntilTheKeyringLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyring.json")
	f := NewFile(path)
	if err := f.Check(context.Background()); !errors.Is(err, ErrKeyringMissing) {
		t.Fatalf("missing: %v", err)
	}
	if err := InitFile(path); err != nil {
		t.Fatal(err)
	}
	if err := f.Check(context.Background()); err != nil {
		t.Fatalf("after init: %v", err)
	}
}

// A keyring whose directory can't be written: the new hospital's key can't
// be stored, so it is never used.
func TestWrapRefusesAKeyItCannotStore(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f, path := initRing(t)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, _, err := f.Wrap(context.Background(), hospA, newDEK(t), []byte("x")); !errors.Is(err, ErrNotReady) {
		t.Fatalf("wrap: %v", err)
	}
	if _, err := f.Unwrap(context.Background(), hospA, 1, make([]byte, 60), []byte("x")); !errors.Is(err, ErrNoKey) {
		t.Fatalf("the unstored key was kept: %v", err)
	}
}
