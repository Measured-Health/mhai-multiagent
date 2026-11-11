package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/proxyconf"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// SealKeyID is a sealing public key's id: the first 16 hex characters of
// sha256(public key), the rule the request-token keys use.
func SealKeyID(pub []byte) string { return seal.KeyID(pub) }

// sealInit is "emr keys seal-init --out <dir>" (EMR design §6.2): an X25519
// key pair for the credential boxes core seals, as seal-<kid>.key (0400,
// base64 of the 32-byte private key; the emr service's alone) and
// seal-<kid>.pub (0444; the file core is given). It never overwrites: a
// directory that already holds a pair is kept as it is, and one holding
// half a pair is refused. Neither key is printed.
func sealInit(dir string, out io.Writer) error {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return errors.New("emr keys seal-init: --out must be an existing directory")
	}
	keys, _ := filepath.Glob(filepath.Join(dir, "seal-*.key"))
	pubs, _ := filepath.Glob(filepath.Join(dir, "seal-*.pub"))
	switch {
	case len(keys) > 0 && len(keys) == len(pubs):
		fmt.Fprintf(out, "sealing key pair present (%d); kept, nothing written\n", len(keys))
		return nil
	case len(keys) > 0 || len(pubs) > 0:
		return errors.New("emr keys seal-init: the directory holds half a sealing key pair; refusing to write")
	}
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return errors.New("emr keys seal-init: no randomness")
	}
	pub := priv.PublicKey().Bytes()
	kid := SealKeyID(pub)
	keyPath := filepath.Join(dir, "seal-"+kid+".key")
	pubPath := filepath.Join(dir, "seal-"+kid+".pub")
	if err := writeNew(keyPath, priv.Bytes(), 0o400); err != nil {
		return err
	}
	if err := writeNew(pubPath, pub, 0o444); err != nil {
		_ = os.Remove(keyPath)
		return err
	}
	fmt.Fprintf(out, "sealing key pair made: kid %s (give core seal-%s.pub only)\n", kid, kid)
	return nil
}

// writeNew writes base64(b) to a file that must not exist yet, with mode.
func writeNew(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("emr keys seal-init: cannot create a key file")
	}
	_, werr := f.WriteString(base64.StdEncoding.EncodeToString(b) + "\n")
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Chmod(path, mode) != nil {
		_ = os.Remove(path)
		return errors.New("emr keys seal-init: cannot write a key file")
	}
	return nil
}

// proxyInit is "emr keys proxy-init --out <dir>" (EMR design §6.4): K_proxy,
// the key the emr service and egress-emr share for the per-connection
// Proxy-Authorization, as <dir>/k_proxy (0400, 64 hex characters). It never
// overwrites: an existing usable key is kept, and an unusable one is
// refused. The key is never printed.
func proxyInit(dir string, out io.Writer) error {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return errors.New("emr keys proxy-init: --out must be an existing directory")
	}
	path := filepath.Join(dir, proxyconf.KeyFile)
	if _, err := os.Lstat(path); err == nil {
		if _, err := proxyconf.ReadKey(path); err != nil {
			return errors.New("emr keys proxy-init: the directory holds an unusable K_proxy; refusing to write")
		}
		fmt.Fprintln(out, "K_proxy present; kept, nothing written")
		return nil
	}
	k := make([]byte, proxyconf.KeyBytes)
	if _, err := rand.Read(k); err != nil {
		return errors.New("emr keys proxy-init: no randomness")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return errors.New("emr keys proxy-init: cannot create the key file")
	}
	_, werr := f.WriteString(hex.EncodeToString(k) + "\n")
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Chmod(path, 0o400) != nil {
		_ = os.Remove(path)
		return errors.New("emr keys proxy-init: cannot write the key file")
	}
	fmt.Fprintln(out, "K_proxy made (mount it read-only in the emr service and egress-emr only)")
	return nil
}
