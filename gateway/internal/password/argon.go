// Package password hashes and checks account passwords: argon2id for storage
// and a length and breached-list policy for choosing them.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/text/unicode/norm"
)

type params struct {
	memKiB  uint32
	time    uint32
	threads uint8
	keyLen  uint32
}

// current holds the argon2id cost. Tuned on 2026-10-27 for about 250 ms per
// hash with at least 64 MiB of memory: on an 8-thread x86-64 dev machine,
// m=65536 KiB (64 MiB), t=2, p=1 took 237 ms per hash (t=3 took 400 ms), and
// the 64 MiB floor leaves time as the only knob. Re-measure on the CI runner
// and the production host before raising t; if you change any value here,
// regenerate dummyHash too (TestDummyHashMatchesParams fails until you do).
var current = params{memKiB: 64 * 1024, time: 2, threads: 1, keyLen: 32}

const saltLen = 16

// dummyHash is a hash of a password nobody has, made with current's
// parameters. DummyVerify checks against it so an unknown login costs the
// same time as a real one.
const dummyHash = "$argon2id$v=19$m=65536,t=2,p=1$bWhhaS1kdW1teS1zYWx0IQ$BuCaFNf7J529mTr9zKL/6dtH/LsFg3hpFM8ksVSiiR8"

// Limits on stored parameters, so a corrupt or hostile row can't make Verify
// allocate gigabytes or spin.
const (
	maxMemKiB  = 1 << 20 // 1 GiB
	maxTime    = 10
	maxThreads = 16
	maxKeyLen  = 64
	maxSaltLen = 64
	// Floors, so a downgraded row can't be verified (or answered) faster
	// than a real one. They match current's memory and time.
	minMemKiB = 64 * 1024
	minTime   = 2
)

// Hash returns the PHC string of pw:
// $argon2id$v=19$m=…,t=…,p=…$salt$hash.
func Hash(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", errors.New("password: no randomness")
	}
	key := argon2.IDKey([]byte(norm.NFC.String(pw)), salt, current.time, current.memKiB, current.threads, current.keyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version,
		current.memKiB, current.time, current.threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Verify reports whether pw matches the PHC string. A malformed string is an
// error, never a match. The comparison is constant-time.
func Verify(pw, encoded string) (bool, error) {
	p, salt, want, err := decode(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(norm.NFC.String(pw)), salt, p.time, p.memKiB, p.threads, p.keyLen)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// DummyVerify does the work of one Verify against a fixed hash and discards
// the result, for the sign-in path of a login that doesn't exist.
func DummyVerify(pw string) {
	_, _ = Verify(pw, dummyHash)
}

// NeedsRehash reports whether encoded was made with parameters other than the
// current ones, so sign-in can upgrade it after a successful Verify. A
// malformed string needs one too.
func NeedsRehash(encoded string) bool {
	p, _, _, err := decode(encoded)
	return err != nil || p != current
}

func decode(enc string) (p params, salt, key []byte, err error) {
	bad := errors.New("password: malformed hash")
	f := strings.Split(enc, "$")
	if len(f) != 6 || f[0] != "" || f[1] != "argon2id" || f[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return p, nil, nil, bad
	}
	var t, m, th uint32
	if n, e := fmt.Sscanf(f[3], "m=%d,t=%d,p=%d", &m, &t, &th); n != 3 || e != nil {
		return p, nil, nil, bad
	}
	// Re-encode and compare, so signs, spaces and trailing fields don't pass.
	if f[3] != fmt.Sprintf("m=%d,t=%d,p=%d", m, t, th) {
		return p, nil, nil, bad
	}
	if m < minMemKiB || m > maxMemKiB || t < minTime || t > maxTime || th < 1 || th > maxThreads {
		return p, nil, nil, bad
	}
	b64 := base64.RawStdEncoding
	if salt, err = b64.DecodeString(f[4]); err != nil || len(salt) < 8 || len(salt) > maxSaltLen {
		return p, nil, nil, bad
	}
	if key, err = b64.DecodeString(f[5]); err != nil || len(key) < 16 || len(key) > maxKeyLen {
		return p, nil, nil, bad
	}
	return params{memKiB: m, time: t, threads: uint8(th), keyLen: uint32(len(key))}, salt, key, nil
}
