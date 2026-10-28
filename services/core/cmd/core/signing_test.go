package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// The access-token and request-token keys must be different keys, not only
// different files (spec §7.2): a copied key would let one verifier's bug
// turn one token kind into the other.
func TestSigningKeysMustDiffer(t *testing.T) {
	_, a, _ := ed25519.GenerateKey(rand.Reader)
	_, b, _ := ed25519.GenerateKey(rand.Reader)
	kid := func(k ed25519.PrivateKey) string { return reqtoken.KeyID(k.Public().(ed25519.PublicKey)) }
	if err := checkSigningKeys(token.Issuer{Key: a, KeyID: kid(a)}, reqtoken.Signer{Key: b, KeyID: kid(b)}); err != nil {
		t.Fatalf("distinct keys refused: %v", err)
	}
	if err := checkSigningKeys(token.Issuer{Key: a, KeyID: kid(a)}, reqtoken.Signer{Key: a, KeyID: kid(a)}); err == nil {
		t.Fatal("one key used for both token kinds")
	}
	// Equal kids are refused even if the keys differ (kids must be unique).
	if err := checkSigningKeys(token.Issuer{Key: a, KeyID: "same"}, reqtoken.Signer{Key: b, KeyID: "same"}); err == nil {
		t.Fatal("equal kids accepted")
	}
}
