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
	uid := "8a6e0f8e-58f4-4c44-9d1e-0f0c6f3b2a11"
	a, err := k.seal(uid, secret)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := k.seal(uid, secret)
	if bytes.Equal(a, b) {
		t.Fatal("two seals of one secret are equal (nonce reused)")
	}
	if len(a) != 12+len(secret)+16 {
		t.Fatalf("sealed length %d, want nonce + secret + tag", len(a))
	}
	if bytes.Contains(a, secret) || strings.Contains(string(a), base32.StdEncoding.EncodeToString(secret)) {
		t.Fatal("sealed form contains the secret")
	}
	if _, err := k.open("5b0b3a52-1f7e-4d2a-8f43-6c1d2b9e7a00", a); err == nil {
		t.Fatal("a secret opened for another user (copied row)")
	}
	got, err := k.open(uid, a)
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("open: %v", err)
	}
	a[len(a)-1] ^= 1
	if _, err := k.open(uid, a); err == nil {
		t.Fatal("a tampered secret opened")
	}
	other, _ := newMFAKeys([]byte("another-test-mfa-key-0123456789abcdef0123"))
	if _, err := other.open(uid, b); err == nil {
		t.Fatal("opened under another key")
	}
	if _, err := newMFAKeys([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := k.open(uid, []byte{1, 2, 3}); err == nil {
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

func TestDeviceNeverSkipsForPlatformRoles(t *testing.T) {
	for _, roles := range [][]string{{"platform_admin"}, {"auditor"}, {"admin"}, {"hospital_admin"}, {"analyst"}, {"clinician", "analyst"}} {
		if deviceMaySkip(roles) {
			t.Errorf("deviceMaySkip(%v) = true", roles)
		}
	}
	if !deviceMaySkip([]string{"clinician"}) {
		t.Error("clinician-only account refused")
	}
}

// An enrolled account needs a code (or an allowed device) whatever the policy.
func TestSessionSatisfiesMFAEnrolled(t *testing.T) {
	clin := []string{"clinician"}
	off := mfaPolicy{}
	if !sessionSatisfiesMFA(clin, off, false, false, []string{"pwd"}) {
		t.Error("no policy, not enrolled: refused")
	}
	if sessionSatisfiesMFA(clin, off, true, false, []string{"pwd"}) {
		t.Error("enrolled, password only: accepted")
	}
	if sessionSatisfiesMFA(clin, off, true, true, []string{"pwd", "device"}) {
		t.Error("enrolled, device while trust is off: accepted")
	}
	if !sessionSatisfiesMFA(clin, mfaPolicy{AllowTrustDevice: true}, true, true, []string{"pwd", "device"}) {
		t.Error("enrolled, device while trust is on: refused")
	}
	if !sessionSatisfiesMFA(clin, off, true, true, []string{"pwd", "otp"}) {
		t.Error("enrolled, otp: refused")
	}
}

func TestPasswordFingerprint(t *testing.T) {
	k, _ := newMFAKeys([]byte("test-only-mfa-key-0123456789abcdef0123456789"))
	h := "$argon2id$v=19$m=65536,t=2,p=1$c2FsdA$aGFzaA"
	f := k.pwFingerprint(&h)
	if f == "" || strings.Contains(f, "argon2") || f != k.pwFingerprint(&h) {
		t.Fatalf("fingerprint %q", f)
	}
	h2 := h + "x"
	if k.pwFingerprint(&h2) == f || k.pwFingerprint(nil) == f {
		t.Fatal("fingerprint doesn't follow the hash")
	}
}
