// Package analysis is Patient Analysis's orchestration in core (analysis
// backend design; ADR 0015): its keys and the nightly run that ingests,
// sweeps and freezes, as mhai_analysis_job only. Nothing here reaches a
// worker role or a NATS subject (T22).
package analysis

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

// KeyLen is each key's length in bytes.
const KeyLen = 32

var (
	errKeys = errors.New("analysis: four distinct 32-byte keys are required")
	errOpen = errors.New("analysis: the reference doesn't open")
)

// Keys are core's four Analysis keys. They never print: String, GoString
// and LogValue are redacted, and the fields are unexported.
type Keys struct {
	refHMAC, drill    []byte
	refAEAD, noteAEAD cipher.AEAD
}

// NewKeys takes the reference HMAC key, the reference encryption key, the
// drill-key HMAC key and the purpose-note key: 32 bytes each, all distinct.
func NewKeys(refHMAC, refEnc, drill, note []byte) (*Keys, error) {
	all := [][]byte{refHMAC, refEnc, drill, note}
	for i, k := range all {
		if len(k) != KeyLen {
			return nil, errKeys
		}
		for _, o := range all[:i] {
			if bytes.Equal(k, o) {
				return nil, errKeys
			}
		}
	}
	refAEAD, err := newGCM(refEnc)
	if err != nil {
		return nil, err
	}
	noteAEAD, err := newGCM(note)
	if err != nil {
		return nil, err
	}
	clone := func(b []byte) []byte { return append([]byte(nil), b...) }
	return &Keys{refHMAC: clone(refHMAC), drill: clone(drill), refAEAD: refAEAD, noteAEAD: noteAEAD}, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errKeys
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errKeys
	}
	return aead, nil
}

func (Keys) String() string       { return "analysis.Keys{redacted}" }
func (Keys) GoString() string     { return "analysis.Keys{redacted}" }
func (Keys) LogValue() slog.Value { return slog.StringValue("redacted") }

// Format covers every verb (%x and %d too).
func (k Keys) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(k.String())) }

// frame writes each part with its length, so two splits never collide.
func frame(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = binary.BigEndian.AppendUint32(out, uint32(len(p)))
		out = append(out, p...)
	}
	return out
}

// RefHMAC is the subject's matching key: HMAC-SHA256 of the hospital and
// the reference, so one patient's subjects at two hospitals can't be
// linked.
func (k *Keys) RefHMAC(hospital uuid.UUID, ref string) []byte {
	m := hmac.New(sha256.New, k.refHMAC)
	m.Write(frame(hospital[:], []byte(ref)))
	return m.Sum(nil)
}

func seal(aead cipher.AEAD, hospital uuid.UUID, pt []byte) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, pt, hospital[:]), nil
}

func open(aead cipher.AEAD, hospital uuid.UUID, ct []byte) ([]byte, error) {
	n := aead.NonceSize()
	if len(ct) < n+aead.Overhead() {
		return nil, errOpen
	}
	pt, err := aead.Open(nil, ct[:n], ct[n:], hospital[:])
	if err != nil {
		return nil, errOpen
	}
	return pt, nil
}

// SealNote and OpenNote are SealRef's twins for a named view's purpose
// note (design §8.3), under the note key.
func (k *Keys) SealNote(hospital uuid.UUID, note string) ([]byte, error) {
	return seal(k.noteAEAD, hospital, []byte(note))
}

func (k *Keys) OpenNote(hospital uuid.UUID, ct []byte) (string, error) {
	pt, err := open(k.noteAEAD, hospital, ct)
	return string(pt), err
}

// SealRef encrypts the reference with AES-256-GCM (a random nonce first),
// bound to the hospital as associated data.
func (k *Keys) SealRef(hospital uuid.UUID, ref string) ([]byte, error) {
	return seal(k.refAEAD, hospital, []byte(ref))
}

// OpenRef reverses SealRef for the same hospital only.
func (k *Keys) OpenRef(hospital uuid.UUID, ct []byte) (string, error) {
	pt, err := open(k.refAEAD, hospital, ct)
	return string(pt), err
}

// Drill is an opaque drill key: HMAC-SHA256 of the framed parts, 43
// characters of base64url.
func (k *Keys) Drill(parts ...[]byte) string {
	m := hmac.New(sha256.New, k.drill)
	m.Write(frame(parts...))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
