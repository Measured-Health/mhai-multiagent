package auth

import (
	"crypto/rand"
	"errors"

	"github.com/google/uuid"
)

// TOTPSealer seals a TOTP secret into user_mfa.secret_enc exactly as
// enrolment does (the key derived from MFA_ENCRYPTION_KEY, bound to the
// user's id), for `core demo seed` only: the public demo's accounts are
// enrolled on a published demo secret, or on a random one nobody holds.
// Nothing here logs or returns a secret in an error.
type TOTPSealer struct{ k *mfaKeys }

// NewTOTPSealer derives the sealing key from master (MFA_ENCRYPTION_KEY,
// at least 32 bytes, as core reads it).
func NewTOTPSealer(master []byte) (*TOTPSealer, error) {
	k, err := newMFAKeys(master)
	if err != nil {
		return nil, err
	}
	return &TOTPSealer{k: k}, nil
}

// Seal encrypts secret for user.
func (s *TOTPSealer) Seal(user uuid.UUID, secret []byte) ([]byte, error) {
	return s.k.seal(user.String(), secret)
}

// Open decrypts what Seal (or enrolment) stored for user.
func (s *TOTPSealer) Open(user uuid.UUID, sealed []byte) ([]byte, error) {
	return s.k.open(user.String(), sealed)
}

// DecodeTOTPSecret reads an otpauth base32 secret (no padding) of the
// size core enrols (160 bits).
func DecodeTOTPSecret(b32secret string) ([]byte, error) {
	b, err := b32.DecodeString(b32secret)
	if err != nil || len(b) != totpSecretBytes {
		return nil, errors.New("auth: not a 160-bit base32 TOTP secret")
	}
	return b, nil
}

// RandomTOTPSecret is a fresh 160-bit secret, as enrolment makes.
func RandomTOTPSecret() ([]byte, error) {
	b := make([]byte, totpSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, errors.New("auth: no randomness")
	}
	return b, nil
}
