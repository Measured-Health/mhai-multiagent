// Package envelope seals stored objects (documents design §6.2): a random
// 256-bit data key (DEK) per object, Tink's streaming AEAD
// (AES-256-GCM-HKDF, 1 MB segments) so a page can be read without holding
// the whole file, and associated data that binds the object to where it
// belongs:
//
//	mhai.documents/1|<hospital>|<document>|<kind>|<n>
//
// An object copied to another hospital's prefix, another document or
// another page fails to decrypt (DS3). The DEK is wrapped by the
// hospital's KEK (package kek) and kept by the caller in Postgres, never
// beside the object.
//
// Errors are closed sentinels: never a key, a byte of the object or a
// key's text.
package envelope

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"strconv"

	"github.com/google/uuid"
	"github.com/tink-crypto/tink-go/v2/streamingaead/subtle"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/kek"
)

// Kind is what an object is.
type Kind string

// The kinds (document_objects.kind).
const (
	Original Kind = "original"
	Page     Kind = "page"
)

// MaxPages is the most pages any purpose allows (§4.1).
const MaxPages = 50

// SegmentSize is the streaming AEAD's ciphertext segment size: 1 MB.
const SegmentSize = 1 << 20

var (
	// ErrRef: a reference with a nil id, an unknown kind or a page
	// number out of range.
	ErrRef = errors.New("envelope: invalid object reference")
	// ErrDecrypt: the object doesn't open under this reference and key.
	ErrDecrypt = errors.New("envelope: the object does not decrypt")
	// ErrSeal: sealing failed (reading the plaintext or writing out).
	ErrSeal = errors.New("envelope: sealing failed")
)

// Ref names one stored object.
type Ref struct {
	Hospital, Document uuid.UUID
	Kind               Kind
	// N is 0 for the original and the page number (1-50) for a page.
	N int
}

// Valid reports whether r names an object that can exist.
func (r Ref) Valid() error {
	switch {
	case r.Hospital == uuid.Nil || r.Document == uuid.Nil:
		return ErrRef
	case r.Kind == Original && r.N == 0:
		return nil
	case r.Kind == Page && r.N >= 1 && r.N <= MaxPages:
		return nil
	}
	return ErrRef
}

// Key is the object key: ids only (§6.1), the same derivation as
// document_objects' CHECK.
func (r Ref) Key() string {
	if r.Kind == Original {
		return "originals/" + r.Hospital.String() + "/" + r.Document.String()
	}
	return "pages/" + r.Hospital.String() + "/" + r.Document.String() + "/" + strconv.Itoa(r.N)
}

// AAD is the associated data the object is sealed with.
func (r Ref) AAD() []byte {
	return []byte("mhai.documents/1|" + r.Hospital.String() + "|" + r.Document.String() + "|" + string(r.Kind) + "|" +
		strconv.Itoa(r.N))
}

// QuarantineKey is where an upload lands before its scan (§4.2, §5.4):
// provider encryption only, deleted after the scan, 24 hours at most.
func QuarantineKey(hospital, document uuid.UUID) string {
	return "quarantine/" + hospital.String() + "/" + document.String()
}

// Sealed is what the caller stores in document_objects.
type Sealed struct {
	WrappedDEK []byte
	KEKVersion int
	// SHA256 and Size are the plaintext's.
	SHA256 [32]byte
	Size   int64
}

func newAEAD(dek []byte) (*subtle.AESGCMHKDF, error) {
	return subtle.NewAESGCMHKDF(dek, "SHA256", kek.DEKSize, SegmentSize, 0)
}

// Seal encrypts src into dst under a fresh DEK, wrapped by the hospital's
// KEK. Nothing is returned unless the whole object was written.
func Seal(ctx context.Context, keys kek.Provider, ref Ref, dst io.Writer, src io.Reader) (Sealed, error) {
	if err := ref.Valid(); err != nil {
		return Sealed{}, err
	}
	dek := make([]byte, kek.DEKSize)
	defer clear(dek)
	if _, err := rand.Read(dek); err != nil {
		return Sealed{}, ErrSeal
	}
	aad := ref.AAD()
	wrapped, version, err := keys.Wrap(ctx, ref.Hospital, dek, aad)
	if err != nil {
		return Sealed{}, err
	}
	a, err := newAEAD(dek)
	if err != nil {
		return Sealed{}, ErrSeal
	}
	w, err := a.NewEncryptingWriter(dst, aad)
	if err != nil {
		return Sealed{}, ErrSeal
	}
	h := sha256.New()
	n, err := io.Copy(w, io.TeeReader(src, h))
	if err != nil {
		return Sealed{}, ErrSeal
	}
	if err := w.Close(); err != nil {
		return Sealed{}, ErrSeal
	}
	s := Sealed{WrappedDEK: wrapped, KEKVersion: version, Size: n}
	copy(s.SHA256[:], h.Sum(nil))
	return s, nil
}

// Open returns a reader of the object's plaintext. Every segment is
// authenticated as it is read; a changed, truncated or misplaced object
// is ErrDecrypt (at Open or at a Read).
func Open(ctx context.Context, keys kek.Provider, ref Ref, wrapped []byte, kekVersion int, src io.Reader) (io.Reader, error) {
	if err := ref.Valid(); err != nil {
		return nil, err
	}
	aad := ref.AAD()
	dek, err := keys.Unwrap(ctx, ref.Hospital, kekVersion, wrapped, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	// The reader derives the object's key from dek when it reads the
	// header, so dek is not cleared here.
	a, err := newAEAD(dek)
	if err != nil {
		return nil, ErrDecrypt
	}
	r, err := a.NewDecryptingReader(src, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return decryptErrors{r}, nil
}

// decryptErrors turns every read error but io.EOF into ErrDecrypt.
type decryptErrors struct{ r io.Reader }

func (d decryptErrors) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	if err != nil && err != io.EOF {
		return n, ErrDecrypt
	}
	return n, err
}
