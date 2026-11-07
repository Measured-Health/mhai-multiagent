package token_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// Synthetic ids only.
const (
	crossUser     = "11111111-1111-4111-8111-111111111111"
	crossHospital = "22222222-2222-4222-8222-222222222222"
	crossRid      = "44444444-4444-4444-8444-444444444444"
	crossJti      = "55555555-5555-4555-8555-555555555555"
)

var crossNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func crossSigner(t *testing.T) reqtoken.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return reqtoken.Signer{Key: priv, KeyID: reqtoken.KeyID(priv.Public().(ed25519.PublicKey)),
		Now: func() time.Time { return crossNow }}
}

func crossExpect() reqtoken.Expect {
	return reqtoken.Expect{Aud: "echo", Subject: "inference.v1.echo.ping", Kind: reqtoken.KindReq}
}

// reqtoken lives in services/gokit and can't import this package: its
// issuer name must still be core's.
func TestRequestTokenIssuerIsCoresIssuer(t *testing.T) {
	if reqtoken.Issuer != token.IssuerName {
		t.Fatalf("reqtoken.Issuer %q, token.IssuerName %q", reqtoken.Issuer, token.IssuerName)
	}
}

// A6: an access token, even signed with a trusted request key, is not a
// request token (its typ is JWT).
func TestAccessTokenRefusedAsRequestToken(t *testing.T) {
	s := crossSigner(t)
	iss := token.Issuer{Key: s.Key, KeyID: s.KeyID, Now: func() time.Time { return crossNow }}
	acc, _, err := iss.Access(token.Claims{Sub: crossUser, Hid: crossHospital, Roles: []string{"clinician"},
		Sid: crossRid, Jti: crossJti})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reqtoken.VerifyAt(acc, s.PublicKeys(), crossExpect(), nil, crossNow); !errors.Is(err, reqtoken.ErrSignature) {
		t.Fatalf("access token as request token: %v, want ErrSignature", err)
	}
}

// A6, reverse: a request token is not an access token.
func TestRequestTokenRefusedAsAccessToken(t *testing.T) {
	s := crossSigner(t)
	tok, _, err := s.Mint(reqtoken.Claims{Aud: "echo", Sub: crossUser, Hid: crossHospital, Roles: []string{"clinician"},
		Rid: crossRid, Kind: reqtoken.KindReq, Subject: "inference.v1.echo.ping", ReplyPrefix: reqtoken.ReplyCore,
		Via: reqtoken.ViaCore}, nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := token.VerifyAt(tok, s.PublicKeys(), crossNow); err == nil {
		t.Fatal("token.VerifyAt accepted a request token")
	}
}

// reqtoken.LoadSigner reads the same PEM PKCS#8 file as token.LoadIssuer,
// with the same key id.
func TestRequestSignerMatchesAccessIssuer(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	path := filepath.Join(t.TempDir(), "request.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := reqtoken.LoadSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := token.LoadIssuer(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.KeyID != iss.KeyID || !s.Key.Equal(iss.Key) {
		t.Fatalf("kid %q vs %q", s.KeyID, iss.KeyID)
	}
}
