package auth

import (
	"bytes"
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B (SHA-1 secret "12345678901234567890"), last 6 digits.
func TestTOTPVectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for _, v := range []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	} {
		step := totpStep(time.Unix(v.unix, 0))
		if got := totpCode(secret, step); got != v.want {
			t.Errorf("t=%d: got %s want %s", v.unix, got, v.want)
		}
	}
}

// matchTOTP accepts the step before, the current step and the step after,
// and only steps after the last one used.
func TestMatchTOTP(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := int64(1000)
	for _, d := range []int64{-1, 0, 1} {
		if s, ok := matchTOTP(secret, totpCode(secret, now+d), now, nil); !ok || s != now+d {
			t.Errorf("offset %d: step %d ok %v", d, s, ok)
		}
	}
	for _, d := range []int64{-2, 2} {
		if _, ok := matchTOTP(secret, totpCode(secret, now+d), now, nil); ok {
			t.Errorf("offset %d accepted", d)
		}
	}
	last := now
	if _, ok := matchTOTP(secret, totpCode(secret, now), now, &last); ok {
		t.Error("the last used step was accepted again")
	}
	if _, ok := matchTOTP(secret, totpCode(secret, now-1), now, &last); ok {
		t.Error("a step before the last used one was accepted")
	}
	if _, ok := matchTOTP(secret, totpCode(secret, now+1), now, &last); !ok {
		t.Error("the next step was refused")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef", " 12345"} {
		if _, ok := matchTOTP(secret, bad, now, nil); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSecretSealRoundTrip(t *testing.T) {
	k, err := newMFAKeys([]byte("test-only-mfa-key-0123456789abcdef0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("12345678901234567890")
	a, err := k.seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := k.seal(secret)
	if bytes.Equal(a, b) {
		t.Fatal("two seals of one secret are equal (nonce reused)")
	}
	if len(a) != 12+len(secret)+16 {
		t.Fatalf("sealed length %d, want nonce + secret + tag", len(a))
	}
	if bytes.Contains(a, secret) || strings.Contains(string(a), base32.StdEncoding.EncodeToString(secret)) {
		t.Fatal("sealed form contains the secret")
	}
	got, err := k.open(a)
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("open: %v", err)
	}
	a[len(a)-1] ^= 1
	if _, err := k.open(a); err == nil {
		t.Fatal("a tampered secret opened")
	}
	other, _ := newMFAKeys([]byte("another-test-mfa-key-0123456789abcdef0123"))
	if _, err := other.open(b); err == nil {
		t.Fatal("opened under another key")
	}
	if _, err := newMFAKeys([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := k.open([]byte{1, 2, 3}); err == nil {
		t.Fatal("short ciphertext opened")
	}
}

func TestBackupCodes(t *testing.T) {
	k, _ := newMFAKeys([]byte("test-only-mfa-key-0123456789abcdef0123456789"))
	codes := newBackupCodes()
	if len(codes) != 10 {
		t.Fatalf("%d codes", len(codes))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if len(c) != 11 || c[5] != '-' || seen[c] {
			t.Fatalf("bad or repeated code %q", c)
		}
		seen[c] = true
		h := k.hashBackup(c)
		if strings.Contains(h, c) || strings.Contains(h, strings.ReplaceAll(c, "-", "")) {
			t.Fatal("hash contains the code")
		}
		// Case, spaces and the dash don't matter.
		if k.hashBackup(" "+strings.ToUpper(strings.ReplaceAll(c, "-", ""))+" ") != h {
			t.Fatal("normalised form hashes differently")
		}
	}
	other, _ := newMFAKeys([]byte("another-test-mfa-key-0123456789abcdef0123"))
	if other.hashBackup(codes[0]) == k.hashBackup(codes[0]) {
		t.Fatal("backup hash does not depend on the key")
	}
	if looksLikeTOTP(codes[0]) || !looksLikeTOTP("123456") {
		t.Fatal("TOTP / backup classification")
	}
}
