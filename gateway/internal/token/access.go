// Package token issues and verifies the gateway's access tokens (EdDSA
// JWTs, 15 minutes) and makes refresh tokens.
//
// The JWT is built by hand: one algorithm (EdDSA over Ed25519), one header
// shape and a fixed claim set, so there is no algorithm negotiation to get
// wrong. Errors are fixed values and never contain the token.
package token

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"strings"
	"time"
)

// Fixed values from the global constraints.
const (
	IssuerName = "mhai-gateway" // iss
	Audience   = "mhai-bff"     // aud
	AccessTTL  = 15 * time.Minute
)

// maxTokenLen bounds what Verify will parse.
const maxTokenLen = 8 << 10

// Claims are the access token's identity claims. Hid is "" for platform
// accounts.
type Claims struct {
	Sub   string
	Hid   string
	Roles []string
	MFA   bool
	AMR   []string
	Sid   string
	Jti   string

	// Set by Verify: when the token was issued and expires.
	IssuedAt, ExpiresAt time.Time
}

// Verification errors. ErrExpired is the only one a caller should tell
// apart (token_expired); the rest are all "unauthenticated".
var (
	ErrMalformed = errors.New("token: malformed")
	ErrSignature = errors.New("token: bad signature or unknown key")
	ErrClaims    = errors.New("token: wrong issuer, audience or claims")
	ErrExpired   = errors.New("token: expired")
	errKey       = errors.New("token: cannot load signing key (want a PEM PKCS#8 Ed25519 private key)")
)

// Issuer signs access tokens. Now defaults to time.Now.
type Issuer struct {
	Key   ed25519.PrivateKey
	KeyID string
	Now   func() time.Time
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

type payload struct {
	Iss   string   `json:"iss"`
	Aud   string   `json:"aud"`
	Sub   string   `json:"sub"`
	Hid   string   `json:"hid"`
	Roles []string `json:"roles"`
	MFA   bool     `json:"mfa"`
	AMR   []string `json:"amr"`
	Sid   string   `json:"sid"`
	Jti   string   `json:"jti"`
	Iat   int64    `json:"iat"`
	Exp   int64    `json:"exp"`
}

var b64 = base64.RawURLEncoding

func (i Issuer) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

// Access signs c into a token valid for AccessTTL and returns it with its
// expiry. Sub, Sid and Jti are required.
func (i Issuer) Access(c Claims) (string, time.Time, error) {
	if len(i.Key) != ed25519.PrivateKeySize || i.KeyID == "" {
		return "", time.Time{}, errors.New("token: issuer has no key")
	}
	if c.Sub == "" || c.Sid == "" || c.Jti == "" {
		return "", time.Time{}, errors.New("token: sub, sid and jti are required")
	}
	iat := i.now().Truncate(time.Second)
	exp := iat.Add(AccessTTL)
	roles, amr := c.Roles, c.AMR
	if roles == nil {
		roles = []string{}
	}
	if amr == nil {
		amr = []string{}
	}
	h, err := json.Marshal(header{Alg: "EdDSA", Typ: "JWT", Kid: i.KeyID})
	if err != nil {
		return "", time.Time{}, err
	}
	p, err := json.Marshal(payload{Iss: IssuerName, Aud: Audience, Sub: c.Sub, Hid: c.Hid, Roles: roles, MFA: c.MFA,
		AMR: amr, Sid: c.Sid, Jti: c.Jti, Iat: iat.Unix(), Exp: exp.Unix()})
	if err != nil {
		return "", time.Time{}, err
	}
	in := b64.EncodeToString(h) + "." + b64.EncodeToString(p)
	return in + "." + b64.EncodeToString(ed25519.Sign(i.Key, []byte(in))), exp, nil
}

// PublicKeys returns the verification key set for this issuer.
func (i Issuer) PublicKeys() map[string]ed25519.PublicKey {
	return map[string]ed25519.PublicKey{i.KeyID: i.Key.Public().(ed25519.PublicKey)}
}

// Verify checks tok's signature against keys (by kid), iss, aud and exp at
// the current time, and returns its claims.
func Verify(tok string, keys map[string]ed25519.PublicKey) (Claims, error) {
	return VerifyAt(tok, keys, time.Now())
}

// VerifyAt is Verify at the time now.
func VerifyAt(tok string, keys map[string]ed25519.PublicKey, now time.Time) (Claims, error) {
	if tok == "" || len(tok) > maxTokenLen {
		return Claims{}, ErrMalformed
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || parts[2] == "" {
		return Claims{}, ErrMalformed
	}
	hraw, err1 := b64.DecodeString(parts[0])
	praw, err2 := b64.DecodeString(parts[1])
	sig, err3 := b64.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return Claims{}, ErrMalformed
	}
	var h header
	if err := strictJSON(hraw, &h); err != nil {
		return Claims{}, ErrMalformed
	}
	if h.Alg != "EdDSA" || h.Typ != "JWT" {
		return Claims{}, ErrSignature
	}
	key, ok := keys[h.Kid]
	if !ok || len(key) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return Claims{}, ErrSignature
	}
	if !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return Claims{}, ErrSignature
	}
	var p payload
	if err := strictJSON(praw, &p); err != nil {
		return Claims{}, ErrMalformed
	}
	if p.Iss != IssuerName || p.Aud != Audience || p.Sub == "" || p.Sid == "" || p.Jti == "" || p.Exp == 0 || p.Iat == 0 {
		return Claims{}, ErrClaims
	}
	if p.Exp-p.Iat > int64(AccessTTL/time.Second) || p.Iat > now.Add(time.Minute).Unix() {
		return Claims{}, ErrClaims
	}
	if now.Unix() >= p.Exp {
		return Claims{}, ErrExpired
	}
	return Claims{Sub: p.Sub, Hid: p.Hid, Roles: p.Roles, MFA: p.MFA, AMR: p.AMR, Sid: p.Sid, Jti: p.Jti,
		IssuedAt: time.Unix(p.Iat, 0), ExpiresAt: time.Unix(p.Exp, 0)}, nil
}

func strictJSON(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// LoadIssuer reads the signing key from a PEM file holding a PKCS#8
// Ed25519 private key (ACCESS_KEY_PATH). The key id is the first 16 hex
// characters of sha256(public key). Errors don't name the path or content.
func LoadIssuer(path string) (Issuer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Issuer{}, errKey
	}
	blk, _ := pem.Decode(raw)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return Issuer{}, errKey
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return Issuer{}, errKey
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return Issuer{}, errKey
	}
	sum := sha256.Sum256(priv.Public().(ed25519.PublicKey))
	return Issuer{Key: priv, KeyID: hex.EncodeToString(sum[:8])}, nil
}
