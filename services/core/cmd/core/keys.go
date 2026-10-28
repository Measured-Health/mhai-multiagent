package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

const keysUsage = "usage: core keys new|ensure --out PATH [--kind access|request] [--trusted-out PATH] | " +
	"core keys untrust --kid KID --trusted-out PATH"

// keys runs:
//   - "keys new --out PATH [--kind access|request] [--trusted-out PATH]":
//     a fresh Ed25519 signing key as a PKCS#8 PEM file, mode 0600. It never
//     overwrites a file. --kind access (the default) is the access-token key
//     (ACCESS_KEY_PATH). --kind request is the request-token key
//     (REQUEST_KEY_PATH, spec §7.2) and needs --trusted-out: its public half
//     is added to that trusted-keys file (spec §7.5), which never holds more
//     than two kids.
//   - "keys ensure ...": the same, but keeps an existing key (it must load).
//     The compose one-shot uses it.
//   - "keys untrust --kid KID --trusted-out PATH": removes one kid from the
//     trusted file (rotation step 4, emergency rotation). It never leaves
//     the file empty.
//
// Each prints only key ids, never a key or a path.
func keys(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(keysUsage)
	}
	switch args[0] {
	case "new", "ensure":
		return keysMake(args[0] == "ensure", args[1:], out)
	case "untrust":
		return keysUntrust(args[1:], out)
	}
	return errors.New(keysUsage)
}

func keysFlags() *flag.FlagSet {
	fs := flag.NewFlagSet("keys", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func keysMake(ensure bool, args []string, out io.Writer) error {
	fs := keysFlags()
	path := fs.String("out", "", "")
	kind := fs.String("kind", "access", "")
	trusted := fs.String("trusted-out", "", "")
	if fs.Parse(args) != nil || fs.NArg() != 0 || *path == "" {
		return errors.New(keysUsage)
	}
	switch {
	case *kind == "access" && *trusted == "":
		return makeAccessKey(ensure, *path, out)
	case *kind == "request" && *trusted != "":
		return makeRequestKey(ensure, *path, *trusted, out)
	}
	return errors.New(keysUsage)
}

func makeAccessKey(ensure bool, path string, out io.Writer) error {
	if ensure && exists(path) {
		iss, err := token.LoadIssuer(path)
		if err != nil {
			return errors.New("keys: the existing key file is not a usable key")
		}
		fmt.Fprintf(out, "kept the existing access-token key, kid %s\n", iss.KeyID)
		return nil
	}
	priv, err := writeKey(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote a new Ed25519 access-token key, kid %s\n", reqtoken.KeyID(priv.Public().(ed25519.PublicKey)))
	return nil
}

// makeRequestKey checks the trusted file before it writes a key, so a key
// that can't be trusted (a third kid) is never made.
func makeRequestKey(ensure bool, path, trustedPath string, out io.Writer) error {
	set, err := readTrustedOrEmpty(trustedPath)
	if err != nil {
		return err
	}
	var priv ed25519.PrivateKey
	verb := "kept the existing"
	if ensure && exists(path) {
		s, err := reqtoken.LoadSigner(path)
		if err != nil {
			return errors.New("keys: the existing key file is not a usable key")
		}
		priv = s.Key
	} else {
		if len(set) >= reqtoken.MaxTrusted {
			return errors.New("keys: the trusted-keys file already holds two kids; untrust one first")
		}
		if priv, err = writeKey(path); err != nil {
			return err
		}
		verb = "wrote a new Ed25519"
	}
	pub := priv.Public().(ed25519.PublicKey)
	kid := reqtoken.KeyID(pub)
	if have, ok := set[kid]; ok {
		if !have.Equal(pub) {
			return errors.New("keys: the trusted-keys file holds another key under this kid")
		}
	} else {
		if len(set) >= reqtoken.MaxTrusted {
			return errors.New("keys: the trusted-keys file already holds two kids; untrust one first")
		}
		set[kid] = pub
		if err := reqtoken.WriteTrusted(trustedPath, set); err != nil {
			return errors.New("keys: cannot write the trusted-keys file")
		}
	}
	fmt.Fprintf(out, "%s request-token key, kid %s (%d trusted)\n", verb, kid, len(set))
	return nil
}

func keysUntrust(args []string, out io.Writer) error {
	fs := keysFlags()
	kid := fs.String("kid", "", "")
	trustedPath := fs.String("trusted-out", "", "")
	if fs.Parse(args) != nil || fs.NArg() != 0 || *kid == "" || *trustedPath == "" {
		return errors.New(keysUsage)
	}
	set, err := reqtoken.ReadTrusted(*trustedPath)
	if err != nil {
		return errors.New("keys: the trusted-keys file is missing or not valid")
	}
	if _, ok := set[*kid]; !ok {
		return errors.New("keys: that kid is not trusted")
	}
	if len(set) == 1 {
		return errors.New("keys: refusing to remove the last trusted kid (add the new key first)")
	}
	delete(set, *kid)
	if err := reqtoken.WriteTrusted(*trustedPath, set); err != nil {
		return errors.New("keys: cannot write the trusted-keys file")
	}
	fmt.Fprintf(out, "removed kid %s from the trusted request-token keys (%d left)\n", *kid, len(set))
	return nil
}

// readTrustedOrEmpty reads the trusted-keys file, or starts an empty set
// when there is none. A file that exists but isn't valid is an error: it is
// never overwritten.
func readTrustedOrEmpty(path string) (map[string]ed25519.PublicKey, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return map[string]ed25519.PublicKey{}, nil
	}
	set, err := reqtoken.ReadTrusted(path)
	if err != nil {
		return nil, errors.New("keys: the existing trusted-keys file is not valid")
	}
	return set, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writeKey makes a fresh Ed25519 key and writes it as PKCS#8 PEM, mode
// 0600, refusing to overwrite a file.
func writeKey(path string) (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, errors.New("keys: no randomness")
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, errors.New("keys: encode failed")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, errors.New("keys: cannot create the key file (it must not exist yet)")
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		_ = f.Close()
		return nil, errors.New("keys: write failed")
	}
	if err := f.Close(); err != nil {
		return nil, errors.New("keys: write failed")
	}
	return priv, nil
}
