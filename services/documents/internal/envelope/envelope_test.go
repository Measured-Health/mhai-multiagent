package envelope

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/kek"
)

// Synthetic ids only.
var (
	hospA = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	hospB = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	docA  = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	docB  = uuid.MustParse("66666666-6666-4666-8666-666666666666")
)

func ring(t *testing.T) kek.Provider {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := kek.InitFile(path); err != nil {
		t.Fatal(err)
	}
	return kek.NewFile(path, kek.Create)
}

func random(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRefKeysAndAssociatedData(t *testing.T) {
	orig := Ref{Hospital: hospA, Document: docA, Kind: Original}
	page := Ref{Hospital: hospA, Document: docA, Kind: Page, N: 3}
	if orig.Key() != "originals/"+hospA.String()+"/"+docA.String() {
		t.Fatal(orig.Key())
	}
	if page.Key() != "pages/"+hospA.String()+"/"+docA.String()+"/3" {
		t.Fatal(page.Key())
	}
	if string(page.AAD()) != "mhai.documents/1|"+hospA.String()+"|"+docA.String()+"|page|3" {
		t.Fatal(string(page.AAD()))
	}
	if QuarantineKey(hospA, docA) != "quarantine/"+hospA.String()+"/"+docA.String() {
		t.Fatal(QuarantineKey(hospA, docA))
	}
	for name, r := range map[string]Ref{
		"nil hospital":  {Document: docA, Kind: Original},
		"nil document":  {Hospital: hospA, Kind: Original},
		"original n":    {Hospital: hospA, Document: docA, Kind: Original, N: 1},
		"page 0":        {Hospital: hospA, Document: docA, Kind: Page},
		"page 51":       {Hospital: hospA, Document: docA, Kind: Page, N: 51},
		"unknown kind":  {Hospital: hospA, Document: docA, Kind: "thumb", N: 1},
		"negative page": {Hospital: hospA, Document: docA, Kind: Page, N: -1},
	} {
		if err := r.Valid(); !errors.Is(err, ErrRef) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if orig.Valid() != nil || page.Valid() != nil {
		t.Fatal("valid refs refused")
	}
}

// Sealing round-trips across several 1 MB segments, and the result carries
// the plaintext's size and SHA-256, not the ciphertext's.
func TestSealOpenRoundTrip(t *testing.T) {
	ctx := context.Background()
	k := ring(t)
	for _, n := range []int{1, 100, SegmentSize - 1, SegmentSize, 2*SegmentSize + 17} {
		plain := random(t, n)
		ref := Ref{Hospital: hospA, Document: docA, Kind: Page, N: 1}
		var ct bytes.Buffer
		s, err := Seal(ctx, k, ref, &ct, bytes.NewReader(plain))
		if err != nil {
			t.Fatalf("%d: seal %v", n, err)
		}
		if s.Size != int64(n) || s.SHA256 != sha256.Sum256(plain) || s.KEKVersion != 1 {
			t.Fatalf("%d: sealed %+v", n, s)
		}
		if bytes.Contains(ct.Bytes(), plain[:min(n, 64)]) && n >= 16 {
			t.Fatalf("%d: the ciphertext holds the plaintext", n)
		}
		r, err := Open(ctx, k, ref, s.WrappedDEK, s.KEKVersion, bytes.NewReader(ct.Bytes()))
		if err != nil {
			t.Fatalf("%d: open %v", n, err)
		}
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("%d: read %v", n, err)
		}
	}
}

// DS3: an object copied to another hospital's prefix, another document or
// another page (with its wrapped key) fails to decrypt; so does a changed,
// truncated or extended object.
func TestOpenRefusesAnObjectOutOfPlace(t *testing.T) {
	ctx := context.Background()
	k := ring(t)
	src := Ref{Hospital: hospA, Document: docA, Kind: Page, N: 1}
	plain := random(t, SegmentSize+500)
	var ct bytes.Buffer
	s, err := Seal(ctx, k, src, &ct, bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	// Hospital B has a KEK of its own.
	if _, err := Seal(ctx, k, Ref{Hospital: hospB, Document: docB, Kind: Original}, io.Discard, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	c := ct.Bytes()
	flip := bytes.Clone(c)
	flip[len(flip)/2] ^= 1
	for name, tc := range map[string]struct {
		ref Ref
		ct  []byte
	}{
		"other hospital": {Ref{Hospital: hospB, Document: docA, Kind: Page, N: 1}, c},
		"other document": {Ref{Hospital: hospA, Document: docB, Kind: Page, N: 1}, c},
		"other page":     {Ref{Hospital: hospA, Document: docA, Kind: Page, N: 2}, c},
		"as original":    {Ref{Hospital: hospA, Document: docA, Kind: Original}, c},
		"changed byte":   {src, flip},
		"truncated":      {src, c[:len(c)-100]},
		"cut at segment": {src, c[:SegmentSize]},
		"extended":       {src, append(bytes.Clone(c), 0)},
		"empty":          {src, nil},
	} {
		r, err := Open(ctx, k, tc.ref, s.WrappedDEK, s.KEKVersion, bytes.NewReader(tc.ct))
		if err == nil {
			_, err = io.ReadAll(r)
		}
		if !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: %v, want ErrDecrypt", name, err)
		}
	}
	// Another object's wrapped key, even in the same document, doesn't
	// open this one.
	s2, err := Seal(ctx, k, Ref{Hospital: hospA, Document: docA, Kind: Page, N: 2}, io.Discard, strings.NewReader("y"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, k, src, s2.WrappedDEK, s2.KEKVersion, bytes.NewReader(c))
	if err == nil {
		_, err = io.ReadAll(r)
	}
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("another object's key: %v", err)
	}
}

func TestSealRefusesBadRefsAndAKeyringThatIsNotReady(t *testing.T) {
	ctx := context.Background()
	if _, err := Seal(ctx, ring(t), Ref{Hospital: hospA, Kind: Original}, io.Discard, strings.NewReader("x")); !errors.Is(err, ErrRef) {
		t.Fatalf("bad ref: %v", err)
	}
	broken := kek.NewFile(filepath.Join(t.TempDir(), "none.json"), kek.Create)
	ref := Ref{Hospital: hospA, Document: docA, Kind: Original}
	if _, err := Seal(ctx, broken, ref, io.Discard, strings.NewReader("x")); !errors.Is(err, kek.ErrNotReady) {
		t.Fatalf("no keyring: %v", err)
	}
	if _, err := Open(ctx, broken, ref, make([]byte, 60), 1, strings.NewReader("x")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("open without keyring: %v", err)
	}
	if _, err := Open(ctx, broken, Ref{}, make([]byte, 60), 1, strings.NewReader("x")); !errors.Is(err, ErrRef) {
		t.Fatalf("open bad ref: %v", err)
	}
	if _, err := Seal(ctx, ring(t), ref, failWriter{}, strings.NewReader("x")); err == nil {
		t.Fatal("a failed write sealed")
	}
	if _, err := Seal(ctx, ring(t), ref, io.Discard, failReader{}); err == nil {
		t.Fatal("a failed read sealed")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("synthetic write failure") }

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("synthetic read failure") }

// A provisioned keyring without the hospital's KEK: Seal (any caller, not
// only presign_upload) is refused with kek.ErrNoKey before a byte is
// written, and the keyring is left as it was.
func TestSealRefusesAHospitalWithoutAProvisionedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := kek.InitFile(path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := kek.AddHospital(path, hospA); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	keys := kek.NewFile(path, kek.Provisioned)
	var dst bytes.Buffer
	_, err := Seal(context.Background(), keys, Ref{Hospital: hospB, Document: docB, Kind: Original}, &dst, strings.NewReader("x"))
	if !errors.Is(err, kek.ErrNoKey) || dst.Len() != 0 {
		t.Fatalf("seal for B: %v, %d bytes", err, dst.Len())
	}
	if _, err := Seal(context.Background(), keys, Ref{Hospital: hospA, Document: docA, Kind: Original}, &dst,
		strings.NewReader("x")); err != nil {
		t.Fatalf("seal for A: %v", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("the provisioned keyring changed")
	}
}
