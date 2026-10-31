package intel

import (
	"crypto/ed25519"
	"errors"
	"sync/atomic"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
)

// ErrTrustedKeys: the trusted-keys file couldn't be read. The path and the
// keys are never in the error.
var ErrTrustedKeys = errors.New("intel: the trusted request-keys file is missing or not valid")

// TrustedKeys is core's copy of the public trusted-keys file the workers
// read (REQUEST_TRUSTED_KEYS_PATH, spec §7.5): only the kids, so the
// dead-letter consumer can tell a job refused for a kid that was removed
// (an emergency rotation) from any other refusal. Reload re-reads it (on
// SIGHUP); a failed reload keeps the last good set. Safe for concurrent use.
type TrustedKeys struct {
	path string
	kids atomic.Pointer[map[string]bool]
}

// LoadTrustedKeys reads the file at path (reqtoken.ReadTrusted).
func LoadTrustedKeys(path string) (*TrustedKeys, error) {
	t := &TrustedKeys{path: path}
	if err := t.Reload(); err != nil {
		return nil, err
	}
	return t, nil
}

// NewTrustedKeys is a fixed set, for tests.
func NewTrustedKeys(keys map[string]ed25519.PublicKey) *TrustedKeys {
	t := &TrustedKeys{}
	t.set(keys)
	return t
}

// Reload re-reads the file. On error the previous set stays.
func (t *TrustedKeys) Reload() error {
	keys, err := reqtoken.ReadTrusted(t.path)
	if err != nil {
		return ErrTrustedKeys
	}
	t.set(keys)
	return nil
}

func (t *TrustedKeys) set(keys map[string]ed25519.PublicKey) {
	m := make(map[string]bool, len(keys))
	for k := range keys {
		m[k] = true
	}
	t.kids.Store(&m)
}

// Has reports whether kid is trusted now. A nil set trusts nothing it can
// name, and Known is false.
func (t *TrustedKeys) Has(kid string) bool {
	if t == nil {
		return false
	}
	m := t.kids.Load()
	return m != nil && (*m)[kid]
}

// Known reports whether t holds a set at all.
func (t *TrustedKeys) Known() bool {
	return t != nil && t.kids.Load() != nil
}
