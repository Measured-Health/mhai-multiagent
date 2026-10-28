package reqtoken

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// MaxTrusted is how many request-token keys a worker trusts at once: the
// current one and, during a rotation, the next (spec §7.5).
const MaxTrusted = 2

// maxTrustedFile bounds what ReadTrusted reads.
const maxTrustedFile = 16 << 10

var (
	errTrusted      = errors.New("reqtoken: the trusted-keys file is missing, empty, over two keys, or holds a bad key")
	errTrustedWrite = errors.New("reqtoken: cannot write the trusted-keys file")
)

// trustedKeys is the only field of the file: {"keys":{"<kid>":"<b64url>"}}.
var trustedKeys = map[string]bool{"keys": true}

// ReadTrusted reads the trusted-keys file, {"keys":{"<kid>":"<base64url of
// the 32-byte public key>"}}: public keys only, one or two of them, each
// under the kid its key gives (KeyID). Anything else is refused. Errors
// don't name the path or a key.
func ReadTrusted(path string) (map[string]ed25519.PublicKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errTrusted
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxTrustedFile+1))
	if err != nil || len(raw) > maxTrustedFile {
		return nil, errTrusted
	}
	var file struct {
		Keys json.RawMessage `json:"keys"`
	}
	if present, err := strictObject(raw, trustedKeys, &file); err != nil || !present["keys"] {
		return nil, errTrusted
	}
	// The inner object: any kid may appear, but only once.
	var enc map[string]string
	if _, err := strictObject(file.Keys, nil, &enc); err != nil {
		return nil, errTrusted
	}
	keys := make(map[string]ed25519.PublicKey, len(enc))
	for kid, k := range enc {
		pub, err := b64.DecodeString(k)
		if err != nil {
			return nil, errTrusted
		}
		keys[kid] = pub
	}
	if err := checkTrusted(keys); err != nil {
		return nil, err
	}
	return keys, nil
}

// WriteTrusted writes keys as the trusted-keys file, atomically (a temp
// file in the same directory, then rename) and mode 0644: the file holds
// public keys only. It refuses what ReadTrusted would refuse.
func WriteTrusted(path string, keys map[string]ed25519.PublicKey) error {
	if err := checkTrusted(keys); err != nil {
		return err
	}
	enc := make(map[string]string, len(keys))
	for kid, k := range keys {
		enc[kid] = b64.EncodeToString(k)
	}
	raw, err := json.MarshalIndent(struct {
		Keys map[string]string `json:"keys"`
	}{enc}, "", "  ")
	if err != nil {
		return errTrustedWrite
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".trusted-*.tmp")
	if err != nil {
		return errTrustedWrite
	}
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return errTrustedWrite
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		return errTrustedWrite
	}
	if err := tmp.Sync(); err != nil {
		return errTrustedWrite
	}
	if err := tmp.Close(); err != nil {
		return errTrustedWrite
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return errTrustedWrite
	}
	done = true
	return nil
}

func checkTrusted(keys map[string]ed25519.PublicKey) error {
	if len(keys) == 0 || len(keys) > MaxTrusted {
		return errTrusted
	}
	for kid, k := range keys {
		if len(k) != ed25519.PublicKeySize || kid == "" || kid != KeyID(k) {
			return errTrusted
		}
	}
	return nil
}
