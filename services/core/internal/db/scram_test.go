package db

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

// RFC 7677 section 3: user "user", password "pencil". The verifier's
// ServerKey must give the RFC's server signature for its auth message.
func TestScramVerifierRFC7677(t *testing.T) {
	salt, _ := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	v, err := scramVerifierSalt("pencil", salt, 4096)
	if err != nil {
		t.Fatal(err)
	}
	head, keys, ok := strings.Cut(v, "$")
	if !ok || head != "SCRAM-SHA-256" {
		t.Fatalf("verifier %q", v)
	}
	params, keyPair, _ := strings.Cut(keys, "$")
	if params != "4096:W22ZaJ0SNY7soEsUEjb6gQ==" {
		t.Fatalf("params %q", params)
	}
	_, serverB64, _ := strings.Cut(keyPair, ":")
	serverKey, _ := base64.StdEncoding.DecodeString(serverB64)
	auth := "n=user,r=rOprNGfwEbeRWgbNEkqO," +
		"r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096," +
		"c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"
	m := hmac.New(sha256.New, serverKey)
	m.Write([]byte(auth))
	if got := base64.StdEncoding.EncodeToString(m.Sum(nil)); got != "6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=" {
		t.Fatalf("server signature %s", got)
	}
}

// The statement carries the verifier, never the password, and each call
// uses a fresh salt.
func TestLoginStatementHasNoPassword(t *testing.T) {
	pw := "synthetic-login-password-0123456789"
	a, err := loginStatement("CREATE", "core_login", pw)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := loginStatement("ALTER", "core_login", pw)
	for _, s := range []string{a, b} {
		if strings.Contains(s, pw) || strings.Contains(s, "synthetic-login") {
			t.Fatalf("statement holds the password: %s", s)
		}
		if !strings.Contains(s, "PASSWORD 'SCRAM-SHA-256$4096:") || !strings.Contains(s, `ROLE "core_login" LOGIN`) {
			t.Fatalf("statement %s", s)
		}
	}
	if a[strings.Index(a, "PASSWORD"):] == b[strings.Index(b, "PASSWORD"):] {
		t.Fatal("same salt twice")
	}
	if _, err := loginStatement("DROP", "core_login", pw); err == nil {
		t.Fatal("bad verb accepted")
	}
}
