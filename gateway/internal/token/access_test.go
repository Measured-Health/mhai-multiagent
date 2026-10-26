package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newIssuer(t *testing.T) Issuer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Issuer{Key: priv, KeyID: "k1"}
}

func claims() Claims {
	return Claims{Sub: "11111111-1111-1111-1111-111111111111", Hid: "22222222-2222-2222-2222-222222222222",
		Roles: []string{"clinician"}, MFA: false, AMR: []string{"pwd"},
		Sid: "33333333-3333-3333-3333-333333333333", Jti: "44444444-4444-4444-4444-444444444444"}
}

func segments(t *testing.T, tok string) (header, payload map[string]any) {
	t.Helper()
	p := strings.Split(tok, ".")
	if len(p) != 3 {
		t.Fatalf("want 3 segments, got %d", len(p))
	}
	for i, dst := range []*map[string]any{&header, &payload} {
		raw, err := base64.RawURLEncoding.DecodeString(p[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			t.Fatal(err)
		}
	}
	return header, payload
}

func TestAccessRoundTrip(t *testing.T) {
	iss := newIssuer(t)
	tok, exp, err := iss.Access(claims())
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(exp); d < 14*time.Minute || d > 15*time.Minute {
		t.Fatalf("exp in %v, want 15 minutes", d)
	}
	got, err := Verify(tok, iss.PublicKeys())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	want := claims()
	if got.Sub != want.Sub || got.Hid != want.Hid || got.Sid != want.Sid || got.Jti != want.Jti ||
		got.MFA != want.MFA || strings.Join(got.Roles, ",") != "clinician" || strings.Join(got.AMR, ",") != "pwd" {
		t.Fatalf("claims differ: %+v", got)
	}
	h, p := segments(t, tok)
	if h["alg"] != "EdDSA" || h["kid"] != "k1" || h["typ"] != "JWT" {
		t.Fatalf("header %v", h)
	}
	if p["iss"] != "mhai-gateway" || p["aud"] != "mhai-bff" {
		t.Fatalf("iss/aud %v %v", p["iss"], p["aud"])
	}
	if p["exp"].(float64)-p["iat"].(float64) != 900 {
		t.Fatalf("exp-iat = %v", p["exp"].(float64)-p["iat"].(float64))
	}
	for _, k := range []string{"sub", "hid", "roles", "mfa", "amr", "sid", "jti", "iat", "exp"} {
		if _, ok := p[k]; !ok {
			t.Fatalf("claim %s missing", k)
		}
	}
}

func TestAccessPlatformHidEmpty(t *testing.T) {
	iss := newIssuer(t)
	c := claims()
	c.Hid = ""
	c.Roles = []string{"platform_admin"}
	tok, _, err := iss.Access(c)
	if err != nil {
		t.Fatal(err)
	}
	_, p := segments(t, tok)
	if v, ok := p["hid"]; !ok || v != "" {
		t.Fatalf("hid = %v, %v; want present and empty", v, ok)
	}
}

func TestAccessRefusesMissingIDs(t *testing.T) {
	iss := newIssuer(t)
	for _, mut := range []func(*Claims){
		func(c *Claims) { c.Sub = "" },
		func(c *Claims) { c.Sid = "" },
		func(c *Claims) { c.Jti = "" },
	} {
		c := claims()
		mut(&c)
		if _, _, err := iss.Access(c); err == nil {
			t.Fatal("want error for missing id")
		}
	}
}

// sign builds a token from raw header and payload with key.
func sign(t *testing.T, key ed25519.PrivateKey, header, payload map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(header)
	p, _ := json.Marshal(payload)
	in := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
	return in + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(in)))
}

func validPayload() map[string]any {
	now := time.Now().Unix()
	return map[string]any{"iss": "mhai-gateway", "aud": "mhai-bff", "sub": "u", "hid": "", "roles": []string{"auditor"},
		"mfa": true, "amr": []string{"pwd", "otp"}, "sid": "s", "jti": "j", "iat": now, "exp": now + 900}
}

func TestVerifyRejects(t *testing.T) {
	iss := newIssuer(t)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	keys := iss.PublicKeys()
	hdr := map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": "k1"}

	if _, err := Verify(sign(t, iss.Key, hdr, validPayload()), keys); err != nil {
		t.Fatalf("control token refused: %v", err)
	}
	cases := map[string]string{}
	p := validPayload()
	p["aud"] = "mhai-python"
	cases["wrong aud"] = sign(t, iss.Key, hdr, p)
	p = validPayload()
	p["iss"] = "someone"
	cases["wrong iss"] = sign(t, iss.Key, hdr, p)
	p = validPayload()
	p["exp"] = time.Now().Add(-time.Minute).Unix()
	cases["expired"] = sign(t, iss.Key, hdr, p)
	p = validPayload()
	delete(p, "exp")
	cases["no exp"] = sign(t, iss.Key, hdr, p)
	cases["other key"] = sign(t, other, hdr, validPayload())
	cases["unknown kid"] = sign(t, iss.Key, map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": "k2"}, validPayload())
	cases["alg none"] = sign(t, iss.Key, map[string]any{"alg": "none", "typ": "JWT", "kid": "k1"}, validPayload())
	cases["alg HS256"] = sign(t, iss.Key, map[string]any{"alg": "HS256", "typ": "JWT", "kid": "k1"}, validPayload())
	good := sign(t, iss.Key, hdr, validPayload())
	parts := strings.Split(good, ".")
	cases["unsigned"] = parts[0] + "." + parts[1] + "."
	p = validPayload()
	p["sub"] = "someone-else"
	pb, _ := json.Marshal(p)
	cases["tampered payload"] = parts[0] + "." + base64.RawURLEncoding.EncodeToString(pb) + "." + parts[2]
	cases["garbage"] = "not-a-token"
	cases["empty"] = ""

	for name, tok := range cases {
		_, err := Verify(tok, keys)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	p = validPayload()
	p["exp"] = time.Now().Add(-time.Minute).Unix()
	if _, err := Verify(sign(t, iss.Key, hdr, p), keys); !errors.Is(err, ErrExpired) {
		t.Errorf("expired: want ErrExpired, got %v", err)
	}
}

func TestErrorsCarryNoToken(t *testing.T) {
	iss := newIssuer(t)
	tok, _, _ := iss.Access(claims())
	_, err := Verify(tok+"x", map[string]ed25519.PublicKey{})
	if err == nil || strings.Contains(err.Error(), tok[:20]) {
		t.Fatalf("error %v", err)
	}
}

func TestLoadKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "access.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	iss, err := LoadIssuer(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if iss.KeyID == "" || !iss.Key.Equal(priv) {
		t.Fatal("key or kid wrong")
	}
	tok, _, err := iss.Access(claims())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(tok, iss.PublicKeys()); err != nil {
		t.Fatal(err)
	}

	bad := filepath.Join(t.TempDir(), "bad.pem")
	_ = os.WriteFile(bad, []byte("not pem"), 0o600)
	if _, err := LoadIssuer(bad); err == nil || strings.Contains(err.Error(), bad) {
		t.Fatalf("bad key: %v", err)
	}
	if _, err := LoadIssuer(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestNewRefresh(t *testing.T) {
	plain, hash := NewRefresh()
	if len(plain) < 40 || hash == plain || hash != HashRefresh(plain) || len(hash) != 64 {
		t.Fatalf("plain %d chars, hash %q", len(plain), hash)
	}
	p2, _ := NewRefresh()
	if p2 == plain {
		t.Fatal("not random")
	}
}
