package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP uses HMAC-SHA1; authenticator apps expect it
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TOTP parameters (design §6.4): 30-second steps, 6 digits, SHA-1, one step
// of tolerance either side, 160-bit secrets.
const (
	totpPeriod      = 30
	totpDigits      = 6
	totpSkew        = 1
	totpSecretBytes = 20
	backupCodeCount = 10
	minMFAKeyLen    = 32
)

// totpStep is the RFC 6238 time step of t.
func totpStep(t time.Time) int64 { return t.Unix() / totpPeriod }

// totpCode is the 6-digit RFC 6238 / RFC 4226 code for step.
func totpCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1000000)
}

// looksLikeTOTP reports whether code has the shape of a TOTP code (six
// ASCII digits); anything else is treated as a backup code.
func looksLikeTOTP(code string) bool {
	if len(code) != totpDigits {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}

// matchTOTP checks code against the steps now-1, now and now+1, accepting
// only a step after lastUsed (so a code can't be used twice, and an older
// code can't follow a newer one). It returns the matched step. Every
// candidate is compared in constant time.
func matchTOTP(secret []byte, code string, now int64, lastUsed *int64) (int64, bool) {
	if !looksLikeTOTP(code) {
		return 0, false
	}
	var step int64
	found := false
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		s := now + d
		if subtle.ConstantTimeCompare([]byte(totpCode(secret, s)), []byte(code)) == 1 && !found {
			step, found = s, true
		}
	}
	if !found || (lastUsed != nil && step <= *lastUsed) {
		return 0, false
	}
	return step, true
}

// b32 is the otpauth secret encoding (RFC 4648 base32, no padding).
var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// mfaKeys holds the keys derived from Config.MFAKey: one for sealing TOTP
// secrets (AES-256-GCM) and one for hashing backup codes (HMAC-SHA256).
type mfaKeys struct {
	aead    cipher.AEAD
	backupK []byte
}

// errMFAKey means the service has no usable MFA key; callers fail closed.
var errMFAKey = errors.New("auth: mfa key not configured")

// newMFAKeys derives the two keys from the configured secret (at least 32
// bytes). Separate SHA-256 derivations with fixed labels keep the keys
// independent of each other.
func newMFAKeys(master []byte) (*mfaKeys, error) {
	if len(master) < minMFAKeyLen {
		return nil, errMFAKey
	}
	derive := func(label string) []byte {
		h := sha256.New()
		h.Write([]byte(label))
		h.Write(master)
		return h.Sum(nil)
	}
	block, err := aes.NewCipher(derive("mhai-mfa-secret-seal-v1\x00"))
	if err != nil {
		return nil, errMFAKey
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errMFAKey
	}
	return &mfaKeys{aead: aead, backupK: derive("mhai-mfa-backup-hmac-v1\x00")}, nil
}

// seal encrypts a TOTP secret: nonce (12 bytes) || ciphertext || tag.
func (k *mfaKeys) seal(secret []byte) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.New("auth: no randomness")
	}
	return k.aead.Seal(nonce, nonce, secret, []byte("user_mfa.secret_enc")), nil
}

// open decrypts what seal produced. A wrong key or tampered value fails.
func (k *mfaKeys) open(sealed []byte) ([]byte, error) {
	n := k.aead.NonceSize()
	if len(sealed) < n+k.aead.Overhead() {
		return nil, errors.New("auth: sealed secret too short")
	}
	out, err := k.aead.Open(nil, sealed[:n], sealed[n:], []byte("user_mfa.secret_enc"))
	if err != nil {
		return nil, errors.New("auth: sealed secret does not open")
	}
	return out, nil
}

// backupAlphabet has no 0/o, 1/l/i, so codes read back cleanly.
const backupAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// newBackupCodes returns 10 fresh codes like "k7mq2-x9tbe" (10 symbols from
// a 31-letter alphabet, about 49 bits each).
func newBackupCodes() []string {
	out := make([]string, 0, backupCodeCount)
	seen := map[string]bool{}
	for len(out) < backupCodeCount {
		var b strings.Builder
		buf := make([]byte, 10)
		if _, err := rand.Read(buf); err != nil {
			panic("auth: no randomness") // crypto/rand never fails on supported platforms
		}
		for i, x := range buf {
			if i == 5 {
				b.WriteByte('-')
			}
			// 256 % 31 bias is negligible next to the attempt limits; use
			// rejection anyway to keep the distribution exact.
			for int(x) >= 256-256%len(backupAlphabet) {
				var one [1]byte
				if _, err := rand.Read(one[:]); err != nil {
					panic("auth: no randomness")
				}
				x = one[0]
			}
			b.WriteByte(backupAlphabet[int(x)%len(backupAlphabet)])
		}
		if c := b.String(); !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// normaliseBackup ignores case, spaces and dashes.
func normaliseBackup(code string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '-' || r == ' ' || r == '\t':
			return -1
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return r
	}, code)
}

// hashBackup is the stored form of a backup code: hex(HMAC-SHA256(key,
// normalised code)). Keyed, so a database copy alone can't be brute-forced;
// deterministic, so a code is found and removed in one statement.
func (k *mfaKeys) hashBackup(code string) string {
	m := hmac.New(sha256.New, k.backupK)
	m.Write([]byte(normaliseBackup(code)))
	return hex.EncodeToString(m.Sum(nil))
}

// newSecret returns a fresh random TOTP secret.
func newSecret() []byte {
	s := make([]byte, totpSecretBytes)
	if _, err := rand.Read(s); err != nil {
		panic("auth: no randomness")
	}
	return s
}
