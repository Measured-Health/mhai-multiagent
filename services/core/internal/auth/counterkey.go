package auth

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
)

// CounterKeyLabel is the HKDF-SHA256 info label that derives the counter
// key from MFA_ENCRYPTION_KEY (Config.MFAKey). Its own label keeps it apart
// from the MFA seal, backup and fingerprint keys. Changing the label or the
// master key renames every counter: live lockouts are forgotten.
const CounterKeyLabel = "mhai lockout key v1"

var errCounterKey = errors.New("auth: counter key not configured")

// counterKey derives the HMAC key for the login and MFA counter names.
// Without a usable master key, the caller fails closed.
func (s *Service) counterKey() ([]byte, error) {
	if len(s.MFAKey) < minMFAKeyLen {
		return nil, errCounterKey
	}
	k, err := hkdf.Key(sha256.New, s.MFAKey, nil, CounterKeyLabel, 32)
	if err != nil {
		return nil, errCounterKey
	}
	return k, nil
}

func (s *Service) counterName(parts ...string) (string, error) {
	k, err := s.counterKey()
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, k)
	for i, p := range parts {
		if i > 0 {
			m.Write([]byte{0})
		}
		m.Write([]byte(p))
	}
	return hex.EncodeToString(m.Sum(nil)), nil
}

// LockoutKey is the login_attempts key for (login, client IP):
// hex(HMAC-SHA256(counter key, "login" | lower(trimmed email) | ip)). Without
// the server key, a copy of the KV store can't confirm a guessed email
// (m3). The email is never a key.
func (s *Service) LockoutKey(email, ip string) (string, error) {
	return s.counterName("login", account.NormaliseEmail(email), ip)
}

// CeilingKey is the login_ceiling key for a login across every IP, keyed
// the same way as LockoutKey.
func (s *Service) CeilingKey(email string) (string, error) {
	return s.counterName("ceiling", account.NormaliseEmail(email))
}

// mfaFailKey is the per-account wrong-code counter in the MFA bucket.
func (s *Service) mfaFailKey(userID string) (string, error) {
	n, err := s.counterName("mfafail", userID)
	if err != nil {
		return "", err
	}
	return "mfafail." + n, nil
}
