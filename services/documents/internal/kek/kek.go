// Package kek holds the hospitals' key-encryption keys (KEKs) behind one
// interface (documents design §6.2, ADR 0016 decision 2). A KEK wraps the
// random data key (DEK) of each stored object; the wrapped DEK lives in
// Postgres (document_objects.wrapped_dek), never beside the object. One KEK
// per hospital, so a hospital's objects can't be opened with another
// hospital's key, and destroying a hospital's KEK makes every object of it
// unreadable, in every bucket version and backup.
//
// Providers: file (pilot and development, this package) and kms
// (production, PR 9). Errors are closed sentinels: they never carry a key,
// a wrapped key or a path.
package kek

import (
	"context"
	"errors"
	"strconv"

	"github.com/google/uuid"
)

// Provider wraps and unwraps data keys under a hospital's KEK.
type Provider interface {
	// Name is the provider's name for logs ("file").
	Name() string
	// Wrap wraps dek under the hospital's current KEK, bound to aad (the
	// object's associated data), and returns the KEK version used. A
	// hospital without a KEK gets one first (its first document, §6.2);
	// the key is stored before it is used.
	Wrap(ctx context.Context, hospital uuid.UUID, dek, aad []byte) (wrapped []byte, version int, err error)
	// Unwrap reverses Wrap. Any mismatch (another hospital, another
	// version, other associated data, a changed byte) is ErrUnwrap.
	Unwrap(ctx context.Context, hospital uuid.UUID, version int, wrapped, aad []byte) ([]byte, error)
	// Check reports whether the keys load and each one unwraps its test
	// value (§6.2 startup refusal). Until it returns nil the service is not
	// ready and Wrap and Unwrap refuse.
	Check(ctx context.Context) error
}

// DEKSize is the data key's size: 256 bits.
const DEKSize = 32

var (
	// ErrNotReady: the keyring didn't load or failed its check.
	ErrNotReady = errors.New("kek: the keyring is not ready")
	// ErrNoKey: the hospital has no KEK of that version.
	ErrNoKey = errors.New("kek: no such key")
	// ErrUnwrap: the wrapped key doesn't open under this hospital, version
	// and associated data.
	ErrUnwrap = errors.New("kek: the wrapped key does not open")
	// ErrInput: a nil hospital, a DEK of the wrong size or empty
	// associated data.
	ErrInput = errors.New("kek: invalid input")
)

// wrapAAD binds a wrapped DEK to its hospital, the KEK version and the
// object's associated data, so a wrapped key copied to another row (another
// hospital, document, kind or page) does not unwrap.
func wrapAAD(hospital uuid.UUID, version int, aad []byte) []byte {
	b := make([]byte, 0, 64+len(aad))
	b = append(b, "mhai.documents/kek/1|"...)
	b = append(b, hospital.String()...)
	b = append(b, '|')
	b = strconv.AppendInt(b, int64(version), 10)
	b = append(b, '|')
	return append(b, aad...)
}

// checkInput is what every provider checks before Wrap and Unwrap.
func checkInput(hospital uuid.UUID, aad []byte) error {
	if hospital == uuid.Nil || len(aad) == 0 {
		return ErrInput
	}
	return nil
}
