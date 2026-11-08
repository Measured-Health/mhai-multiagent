package analysis

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// synthetic keys, distinct, 32 bytes each.
func testKeys(t *testing.T) *Keys {
	t.Helper()
	k, err := NewKeys(bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{3}, 32), bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestNewKeysRefusesShortAndEqual(t *testing.T) {
	a, b := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	if _, err := NewKeys(a[:16], b, bytes.Repeat([]byte{3}, 32), bytes.Repeat([]byte{4}, 32)); err == nil {
		t.Error("a short key was accepted")
	}
	if _, err := NewKeys(a, b, a, bytes.Repeat([]byte{4}, 32)); err == nil {
		t.Error("two equal keys were accepted")
	}
	if _, err := NewKeys(a, b, nil, bytes.Repeat([]byte{4}, 32)); err == nil {
		t.Error("a missing key was accepted")
	}
}

// N6: Keys never print their bytes, whatever the verb or logger.
func TestKeysNeverPrinted(t *testing.T) {
	k := testKeys(t)
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("keys", "keys", k, "value", *k)
	out := fmt.Sprintf("%v %+v %#v %s %v %+v %#v", k, k, k, k, *k, *k, *k) + logs.String()
	for _, raw := range [][]byte{bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{3}, 32), bytes.Repeat([]byte{4}, 32)} {
		for _, enc := range []string{string(raw), hex.EncodeToString(raw), base64.StdEncoding.EncodeToString(raw), fmt.Sprint(raw), fmt.Sprintf("%#v", raw)} {
			if strings.Contains(out, enc) {
				t.Fatalf("key bytes printed: %s", out)
			}
		}
	}
	if strings.Contains(out, "[1 1 1") || strings.Contains(out, "0x1, 0x1") {
		t.Fatalf("key bytes printed: %s", out)
	}
}

// A reference sealed for one hospital opens for it only.
func TestSealRefBoundToHospital(t *testing.T) {
	k := testKeys(t)
	a, b := uuid.New(), uuid.New()
	ct, err := k.SealRef(a, "emr-index-0001")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("emr-index-0001")) {
		t.Fatal("the reference is in clear")
	}
	if got, err := k.OpenRef(a, ct); err != nil || got != "emr-index-0001" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := k.OpenRef(b, ct); err == nil {
		t.Fatal("another hospital opened the reference")
	}
	ct2, _ := k.SealRef(a, "emr-index-0001")
	if bytes.Equal(ct, ct2) {
		t.Fatal("two seals share a nonce")
	}
	ct[len(ct)-1] ^= 1
	if _, err := k.OpenRef(a, ct); err == nil {
		t.Fatal("a tampered ciphertext opened")
	}
}

// The matching HMAC is stable per hospital and reference, and differs
// across hospitals, so two hospitals' subjects can't be linked.
func TestRefHMACPerHospital(t *testing.T) {
	k := testKeys(t)
	a, b := uuid.New(), uuid.New()
	x := k.RefHMAC(a, "emr-index-0001")
	if len(x) != 32 || !bytes.Equal(x, k.RefHMAC(a, "emr-index-0001")) {
		t.Fatal("not a stable 32-byte HMAC")
	}
	if bytes.Equal(x, k.RefHMAC(b, "emr-index-0001")) || bytes.Equal(x, k.RefHMAC(a, "emr-index-0002")) {
		t.Fatal("the HMAC ignores the hospital or the reference")
	}
}

func TestDrillKeyShape(t *testing.T) {
	k := testKeys(t)
	d := k.Drill([]byte("hospital"), []byte("period"), []byte("row"))
	if len(d) != 43 || strings.ContainsAny(d, "+/=") {
		t.Fatalf("drill key %q: want 43 characters of base64url", d)
	}
	if d == k.Drill([]byte("hospitalperiod"), []byte("row")) {
		t.Fatal("parts aren't framed: two splits give one key")
	}
}
