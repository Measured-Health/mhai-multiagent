package seal

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
)

// Synthetic values only: the secret is obviously fake.
const canary = "synthetic-test-secret-not-real-0001"

var (
	now   = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	connA = uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001")
	connB = uuid.MustParse("bbbbbbbb-0000-4000-8000-000000000002")
	orig  = []emrurl.Origin{"https://auth.example.org:443", "https://fhir.example.org:443"}
)

func newPair(t *testing.T) KeyPair {
	t.Helper()
	kp, err := GenerateKeyPair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return kp
}

func sealed(t *testing.T, kp KeyPair, b Binding, at time.Time) []byte {
	t.Helper()
	box, err := Seal(&kp.Public, KeyID(kp.Public[:]), b, []byte(canary), at)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return box
}

func TestSealOpenRoundTrip(t *testing.T) {
	kp := newPair(t)
	b := Binding{ConnectionID: connA, Field: "private_key", Origin: orig}
	box := sealed(t, kp, b, now)
	if bytes.Contains(box, []byte(canary)) {
		t.Fatal("the box holds the value in clear")
	}
	// The origin is a set: its order doesn't matter.
	want := Binding{ConnectionID: connA, Field: "private_key", Origin: []emrurl.Origin{orig[1], orig[0]}}
	s, err := Open(Keys{KeyID(kp.Public[:]): kp}, box, want, now.Add(30*time.Second))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(s.Bytes()) != canary {
		t.Fatal("wrong value")
	}
	s.Zero()
	if len(s.Bytes()) != 0 {
		t.Fatal("Zero left the value")
	}
	// Value bounds: 1 to 4000 bytes.
	for _, v := range [][]byte{nil, {}, bytes.Repeat([]byte("x"), 4001)} {
		if _, err := Seal(&kp.Public, KeyID(kp.Public[:]), b, v, now); !errors.Is(err, ErrValue) {
			t.Errorf("value of %d bytes: %v", len(v), err)
		}
	}
	if _, err := Seal(&kp.Public, KeyID(kp.Public[:]), b, bytes.Repeat([]byte("x"), 4000), now); err != nil {
		t.Errorf("4000 bytes: %v", err)
	}
	// A binding without a connection, field or origin is refused.
	for _, bad := range []Binding{{Field: "private_key", Origin: orig}, {ConnectionID: connA, Origin: orig},
		{ConnectionID: connA, Field: "private_key"}, {ConnectionID: connA, Field: "Private-Key", Origin: orig}} {
		if _, err := Seal(&kp.Public, KeyID(kp.Public[:]), bad, []byte(canary), now); !errors.Is(err, ErrBinding) {
			t.Errorf("binding %+v: %v", bad, err)
		}
	}
}

// A35 (design §6.2, E23): a box replayed into another connection, another
// field, a changed origin, after 60 s, or under an unknown key is refused,
// and nothing of the value comes back.
func TestOpenRefusesReplayedBox(t *testing.T) {
	kp := newPair(t)
	keys := Keys{KeyID(kp.Public[:]): kp}
	b := Binding{ConnectionID: connA, Field: "private_key", Origin: orig}
	box := sealed(t, kp, b, now)
	cases := map[string]struct {
		box  []byte
		want Binding
		at   time.Time
		keys Keys
	}{
		"other connection": {box, Binding{ConnectionID: connB, Field: "private_key", Origin: orig}, now, keys},
		"other field":      {box, Binding{ConnectionID: connA, Field: "client_secret", Origin: orig}, now, keys},
		"changed origin": {box, Binding{ConnectionID: connA, Field: "private_key",
			Origin: []emrurl.Origin{"https://fhir.example.org:443", "https://evil.example.org:443"}}, now, keys},
		"origin shrunk": {box, Binding{ConnectionID: connA, Field: "private_key", Origin: orig[1:]}, now, keys},
		"origin grown": {box, Binding{ConnectionID: connA, Field: "private_key",
			Origin: append([]emrurl.Origin{"https://more.example.org:443"}, orig...)}, now, keys},
		"61 s old":        {box, b, now.Add(61 * time.Second), keys},
		"from the future": {sealed(t, kp, b, now.Add(6*time.Second)), b, now, keys},
		"unknown kid":     {box, b, now, Keys{"0000000000000000": newPair(t)}},
		"no keys":         {box, b, now, Keys{}},
		"tampered":        {append(append([]byte{}, box[:len(box)-1]...), box[len(box)-1]^1), b, now, keys},
		"truncated":       {box[:20], b, now, keys},
		"empty":           {nil, b, now, keys},
	}
	for name, tc := range cases {
		s, err := Open(tc.keys, tc.box, tc.want, tc.at)
		if err == nil || len(s.Bytes()) != 0 {
			t.Errorf("%s: opened (%v)", name, err)
			continue
		}
		if strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), connA.String()) {
			t.Errorf("%s: error carries a value: %v", name, err)
		}
	}
	// Exactly 60 s old still opens; 5 s ahead (clock skew) too.
	if _, err := Open(keys, box, b, now.Add(60*time.Second)); err != nil {
		t.Errorf("60 s: %v", err)
	}
	if _, err := Open(keys, sealed(t, kp, b, now.Add(5*time.Second)), b, now); err != nil {
		t.Errorf("5 s ahead: %v", err)
	}
	// More than two keys loaded is a configuration error.
	three := Keys{"a": newPair(t), "b": newPair(t), KeyID(kp.Public[:]): kp}
	if _, err := Open(three, box, b, now); !errors.Is(err, ErrKeys) {
		t.Errorf("three keys: %v", err)
	}
}

// Design §6.3: during a rotation core seals to the new key while boxes
// sealed to the old one still open, with both loaded.
func TestTwoKeysDuringRotation(t *testing.T) {
	old, cur := newPair(t), newPair(t)
	keys := Keys{KeyID(old.Public[:]): old, KeyID(cur.Public[:]): cur}
	b := Binding{ConnectionID: connA, Field: "api_key", Origin: orig[1:]}
	for _, kp := range []KeyPair{old, cur} {
		s, err := Open(keys, sealed(t, kp, b, now), b, now)
		if err != nil || string(s.Bytes()) != canary {
			t.Fatalf("open under %s: %v", KeyID(kp.Public[:]), err)
		}
	}
	// With the old key dropped, its boxes no longer open.
	if _, err := Open(Keys{KeyID(cur.Public[:]): cur}, sealed(t, old, b, now), b, now); err == nil {
		t.Fatal("a box for a dropped key opened")
	}
	// The kid in the box is the key's: a box claiming the new kid but sealed
	// to the old key does not open.
	box := sealed(t, old, b, now)
	copy(box, KeyID(cur.Public[:]))
	if _, err := Open(keys, box, b, now); err == nil {
		t.Fatal("a box under a borrowed kid opened")
	}
}

// A secret never prints: every formatting path shows [redacted].
func TestSecretRedacted(t *testing.T) {
	kp := newPair(t)
	b := Binding{ConnectionID: connA, Field: "private_key", Origin: orig}
	s, err := Open(Keys{KeyID(kp.Public[:]): kp}, sealed(t, kp, b, now), b, now)
	if err != nil {
		t.Fatal(err)
	}
	type wrap struct {
		S Secret
		P *Secret
	}
	outs := []string{
		fmt.Sprint(s), fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), fmt.Sprintf("%s", s),
		fmt.Sprintf("%q", s), fmt.Sprintf("%x", s), fmt.Sprintf("%d", s), s.String(), s.GoString(),
		fmt.Sprintf("%+v", wrap{s, &s}), fmt.Sprintf("%#v", wrap{s, &s}), fmt.Sprint(&s),
	}
	j, err := json.Marshal(wrap{s, &s})
	if err != nil {
		t.Fatal(err)
	}
	outs = append(outs, string(j))
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "secret", s, "ptr", &s, slog.Any("any", s))
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "secret", s)
	outs = append(outs, buf.String())
	for _, o := range outs {
		if strings.Contains(o, canary) || strings.Contains(o, "synthetic-test") {
			t.Errorf("secret printed: %s", o)
		}
		if !strings.Contains(o, "[redacted]") {
			t.Errorf("no redaction mark: %s", o)
		}
	}
	// The zero Secret is redacted too, and has no bytes.
	var z Secret
	if fmt.Sprint(z) != "[redacted]" || len(z.Bytes()) != 0 {
		t.Errorf("zero secret: %q", fmt.Sprint(z))
	}
}

// Core and the emr service name a sealing key the same way (emr keys
// seal-init writes seal-<kid>.pub).
func TestKeyIDAndLoad(t *testing.T) {
	kp := newPair(t)
	if id := KeyID(kp.Public[:]); len(id) != 16 {
		t.Fatalf("kid %q", id)
	}
	dir := t.TempDir()
	pubPath, keyPath := writePair(t, dir, kp)
	pub, kid, err := LoadPublic(pubPath)
	if err != nil || *pub != kp.Public || kid != KeyID(kp.Public[:]) {
		t.Fatalf("LoadPublic: %v", err)
	}
	keys, err := LoadPrivateDir(dir)
	if err != nil || len(keys) != 1 || keys[kid].Public != kp.Public {
		t.Fatalf("LoadPrivateDir: %v %d", err, len(keys))
	}
	_ = keyPath
}

// writePair writes kp as emr keys seal-init does.
func writePair(t *testing.T, dir string, kp KeyPair) (pubPath, keyPath string) {
	t.Helper()
	kid := KeyID(kp.Public[:])
	pubPath, keyPath = dir+"/seal-"+kid+".pub", dir+"/seal-"+kid+".key"
	for path, b := range map[string][]byte{pubPath: kp.Public[:], keyPath: kp.Private[:]} {
		if err := writeB64(path, b); err != nil {
			t.Fatal(err)
		}
	}
	return pubPath, keyPath
}
