package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// NewRefresh returns a fresh opaque token (32 random bytes, base64url) and
// the hash to store. Only the hash is ever written down.
func NewRefresh() (plain, hash string) {
	plain = NewOpaque()
	return plain, HashRefresh(plain)
}

// NewOpaque returns 32 random bytes as base64url, for refresh, MFA and
// device tokens.
func NewOpaque() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("token: no randomness") // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashRefresh is the stored form of an opaque token: hex(sha256(plain)).
// The tokens carry 256 bits of entropy, so an unsalted fast hash is enough.
func HashRefresh(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
