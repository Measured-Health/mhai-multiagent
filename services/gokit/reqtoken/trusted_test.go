package reqtoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newPub(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return KeyID(pub), pub
}

// The trusted-keys file: public keys only, one or two of them.
func TestTrustedFileLimits(t *testing.T) {
	dir := t.TempDir()
	k1, p1 := newPub(t)
	k2, p2 := newPub(t)
	k3, p3 := newPub(t)
	enc := func(p []byte) string { return b64.EncodeToString(p) }
	files := map[string]string{
		"empty file":       ``,
		"no keys":          `{"keys":{}}`,
		"null keys":        `{"keys":null}`,
		"three keys":       `{"keys":{"` + k1 + `":"` + enc(p1) + `","` + k2 + `":"` + enc(p2) + `","` + k3 + `":"` + enc(p3) + `"}}`,
		"31-byte key":      `{"keys":{"` + k1 + `":"` + enc(p1[:31]) + `"}}`,
		"33-byte key":      `{"keys":{"` + k1 + `":"` + enc(append(append([]byte(nil), p1...), 0)) + `"}}`,
		"kid not its key":  `{"keys":{"` + k1 + `":"` + enc(p2) + `"}}`,
		"empty kid":        `{"keys":{"":"` + enc(p1) + `"}}`,
		"duplicate kid":    `{"keys":{"` + k1 + `":"` + enc(p1) + `","` + k1 + `":"` + enc(p1) + `"}}`,
		"unknown field":    `{"keys":{"` + k1 + `":"` + enc(p1) + `"},"private":"x"}`,
		"padded base64":    `{"keys":{"` + k1 + `":"` + enc(p1) + `="}}`,
		"not json":         `keys`,
		"trailing data":    `{"keys":{"` + k1 + `":"` + enc(p1) + `"}} {}`,
		"key not a string": `{"keys":{"` + k1 + `":5}}`,
	}
	for name, content := range files {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".json")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := ReadTrusted(path)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), enc(p1)) {
			t.Errorf("%s: error names the path or a key: %v", name, err)
		}
	}
	if _, err := ReadTrusted(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing file accepted")
	}
	// Two keys are fine.
	ok := filepath.Join(dir, "ok.json")
	_ = os.WriteFile(ok, []byte(`{"keys":{"`+k1+`":"`+enc(p1)+`","`+k2+`":"`+enc(p2)+`"}}`), 0o644)
	got, err := ReadTrusted(ok)
	if err != nil || len(got) != 2 || !got[k1].Equal(p1) || !got[k2].Equal(p2) {
		t.Fatalf("two keys: %v %v", got, err)
	}
	// WriteTrusted refuses what ReadTrusted would refuse.
	for name, keys := range map[string]map[string]ed25519.PublicKey{
		"none":        {},
		"three":       {k1: p1, k2: p2, k3: p3},
		"short":       {k1: p1[:31]},
		"wrong kid":   {k1: p2},
		"nil key set": nil,
	} {
		if err := WriteTrusted(filepath.Join(dir, "w-"+name+".json"), keys); err == nil {
			t.Errorf("WriteTrusted %s: accepted", name)
		}
	}
}

// WriteTrusted writes atomically, mode 0644, and ReadTrusted reads it back.
func TestTrustedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "request-keys.json")
	k1, p1 := newPub(t)
	k2, p2 := newPub(t)
	if err := WriteTrusted(path, map[string]ed25519.PublicKey{k1: p1}); err != nil {
		t.Fatal(err)
	}
	if err := WriteTrusted(path, map[string]ed25519.PublicKey{k1: p1, k2: p2}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v %v", st.Mode().Perm(), err)
	}
	got, err := ReadTrusted(path)
	if err != nil || len(got) != 2 || !got[k1].Equal(p1) || !got[k2].Equal(p2) {
		t.Fatalf("read back: %v %v", got, err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "PRIVATE") || !strings.HasSuffix(string(raw), "\n") {
		t.Fatalf("content %q", raw)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("temp files left behind: %v", ents)
	}
}
