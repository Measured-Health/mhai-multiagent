package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

const keysUsage = "usage: core keys new|ensure --out PATH"

// keys runs:
//   - "keys new --out PATH": a fresh Ed25519 access-token signing key as a
//     PKCS#8 PEM file (what ACCESS_KEY_PATH points at), mode 0600. It never
//     overwrites a file.
//   - "keys ensure --out PATH": keeps an existing key (it must load), or
//     makes one as "new" does. The compose one-shot uses it.
//
// Either prints only the key id.
func keys(args []string, out io.Writer) error {
	if len(args) != 3 || (args[0] != "new" && args[0] != "ensure") || args[1] != "--out" || args[2] == "" {
		return errors.New(keysUsage)
	}
	if args[0] == "ensure" {
		if _, err := os.Stat(args[2]); err == nil {
			iss, err := token.LoadIssuer(args[2])
			if err != nil {
				return errors.New("keys: the existing key file is not a usable key")
			}
			fmt.Fprintf(out, "kept the existing access-token key, kid %s\n", iss.KeyID)
			return nil
		}
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return errors.New("keys: no randomness")
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return errors.New("keys: encode failed")
	}
	f, err := os.OpenFile(args[2], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("keys: cannot create the key file (it must not exist yet)")
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		_ = f.Close()
		return errors.New("keys: write failed")
	}
	if err := f.Close(); err != nil {
		return errors.New("keys: write failed")
	}
	sum := sha256.Sum256(priv.Public().(ed25519.PublicKey))
	fmt.Fprintf(out, "wrote a new Ed25519 access-token key, kid %s\n", hex.EncodeToString(sum[:8]))
	return nil
}
