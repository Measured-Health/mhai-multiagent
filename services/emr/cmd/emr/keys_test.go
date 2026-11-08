package main

import (
	"bytes"
	"crypto/ecdh"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

func readKey(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(b) != 32 {
		t.Fatalf("%s: not a 32-byte key (%v)", filepath.Base(path), err)
	}
	return b
}

// Plan Task 5 (design §6.2): emr keys seal-init writes an X25519 pair,
// seal-<kid>.key 0400 and seal-<kid>.pub 0444, the kid from the public
// key; it never overwrites, and prints neither key.
func TestSealInitModes(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := run([]string{"keys", "seal-init", "--out", dir}, nil, &out); err != nil {
		t.Fatal(err)
	}
	keys, _ := filepath.Glob(filepath.Join(dir, "seal-*.key"))
	pubs, _ := filepath.Glob(filepath.Join(dir, "seal-*.pub"))
	if len(keys) != 1 || len(pubs) != 1 || strings.TrimSuffix(keys[0], ".key") != strings.TrimSuffix(pubs[0], ".pub") {
		t.Fatalf("files: %v %v", keys, pubs)
	}
	for path, mode := range map[string]os.FileMode{keys[0]: 0o400, pubs[0]: 0o444} {
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != mode {
			t.Errorf("%s mode %v, want %v", filepath.Base(path), st.Mode().Perm(), mode)
		}
	}
	priv, err := ecdh.X25519().NewPrivateKey(readKey(t, keys[0]))
	if err != nil {
		t.Fatal(err)
	}
	pub := readKey(t, pubs[0])
	if !bytes.Equal(priv.PublicKey().Bytes(), pub) {
		t.Fatal("the public key isn't the private key's")
	}
	kid := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(pubs[0]), "seal-"), ".pub")
	if kid != SealKeyID(pub) || len(kid) != 16 {
		t.Fatalf("kid %q, want %q", kid, SealKeyID(pub))
	}
	for _, k := range [][]byte{priv.Bytes(), pub} {
		if strings.Contains(out.String(), base64.StdEncoding.EncodeToString(k)) {
			t.Fatal("seal-init printed a key")
		}
	}
	if !strings.Contains(out.String(), kid) {
		t.Fatalf("output doesn't name the kid: %s", out.String())
	}

	// What seal-init writes is what core seals to and the service opens
	// with (gokit/seal), under the same kid.
	sealKeys, err := loadSealKeys(dir)
	if err != nil || len(sealKeys) != 1 || sealKeys[kid].Public != [32]byte(pub) {
		t.Fatalf("loadSealKeys: %v", err)
	}
	if _, pkid, err := seal.LoadPublic(pubs[0]); err != nil || pkid != kid {
		t.Fatalf("LoadPublic: %q %v", pkid, err)
	}

	// Again: the pair is kept, byte for byte.
	before, _ := os.ReadFile(keys[0])
	out.Reset()
	if err := run([]string{"keys", "seal-init", "--out", dir}, nil, &out); err != nil {
		t.Fatalf("second run: %v", err)
	}
	after, _ := os.ReadFile(keys[0])
	now, _ := filepath.Glob(filepath.Join(dir, "seal-*"))
	if !bytes.Equal(before, after) || len(now) != 2 || !strings.Contains(out.String(), "kept") {
		t.Fatalf("second run changed the directory: %v %s", now, out.String())
	}

	// Half a pair, or a missing directory: refused, nothing written.
	half := t.TempDir()
	if err := os.WriteFile(filepath.Join(half, "seal-0000000000000000.pub"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"keys", "seal-init", "--out", half}, nil, &out); err == nil {
		t.Fatal("half a pair accepted")
	}
	if got, _ := filepath.Glob(filepath.Join(half, "*")); len(got) != 1 {
		t.Fatalf("wrote into a half pair: %v", got)
	}
	if err := run([]string{"keys", "seal-init", "--out", filepath.Join(dir, "missing")}, nil, &out); err == nil {
		t.Fatal("missing directory accepted")
	}
	for _, args := range [][]string{{"keys", "seal-init"}, {"keys", "seal-init", "--out"}, {"keys", "other", "--out", dir}} {
		if err := run(args, nil, &out); err != errUsage {
			t.Errorf("%v: %v", args, err)
		}
	}
}
