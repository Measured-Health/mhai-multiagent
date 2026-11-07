// Package seal seals a connection's credential to the emr service's public
// key, bound to its use (EMR design §6.2, E23; plan Task 8). Core seals at
// its edge, before anything else touches a request body; only the emr
// service, which holds the private key, opens.
//
// The box is an anonymous NaCl box (X25519, XSalsa20-Poly1305) over strict
// JSON {connection_id, field, origin, issued_at, value}, prefixed with the
// sealing key's id (16 hex characters). Open refuses a box for another
// connection, another field or another origin set, one issued more than
// 60 s ago or more than 5 s ahead, and one under a key it doesn't hold, so
// a box can't be replayed into another connection, field or address.
//
// A Secret never prints: every formatting path, JSON and slog show
// [redacted]. Errors are fixed values with no input in them.
package seal

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
)

// Limits (design §6.2, §16.1).
const (
	MaxValue  = 4000
	MaxAge    = 60 * time.Second
	MaxAhead  = 5 * time.Second
	KeyIDLen  = 16
	maxKeys   = 2 // during a rotation (design §6.3)
	redaction = "[redacted]"
)

// The closed errors.
var (
	ErrValue   = errors.New("seal: a value is 1 to 4000 bytes")
	ErrBinding = errors.New("seal: binding needs a connection, a field name and an origin")
	ErrKey     = errors.New("seal: unusable sealing key")
	ErrKeys    = errors.New("seal: one or two sealing keys, by id")
	ErrOpen    = errors.New("seal: box refused")
)

// fieldName is a credential field key (design §4: [a-z_]+).
var fieldName = regexp.MustCompile(`^[a-z_]{1,40}$`)

// Binding is what a box is for: one connection's field, entered for an
// origin set.
type Binding struct {
	ConnectionID uuid.UUID
	Field        string
	Origin       []emrurl.Origin
}

func (b Binding) valid() bool {
	return b.ConnectionID != uuid.Nil && fieldName.MatchString(b.Field) && len(b.Origin) > 0
}

// origins is the binding's origin set, sorted and each once.
func (b Binding) origins() []string {
	out := emrurl.Strings(b.Origin)
	slices.Sort(out)
	return slices.Compact(out)
}

type plain struct {
	ConnectionID string   `json:"connection_id"`
	Field        string   `json:"field"`
	Origin       []string `json:"origin"`
	IssuedAt     int64    `json:"issued_at"` // unix milliseconds
	Value        []byte   `json:"value"`
}

// KeyID is a sealing public key's id: the first 16 hex characters of
// sha256(public key), the rule emr keys seal-init names its files by.
func KeyID(pub []byte) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// Seal seals value for b to pub (whose id is kid) at now, and returns
// kid || box.
func Seal(pub *[32]byte, kid string, b Binding, value []byte, now time.Time) ([]byte, error) {
	if pub == nil || len(kid) != KeyIDLen || kid != KeyID(pub[:]) {
		return nil, ErrKey
	}
	if len(value) < 1 || len(value) > MaxValue {
		return nil, ErrValue
	}
	if !b.valid() {
		return nil, ErrBinding
	}
	msg, err := json.Marshal(plain{ConnectionID: b.ConnectionID.String(), Field: b.Field, Origin: b.origins(),
		IssuedAt: now.UnixMilli(), Value: value})
	if err != nil {
		return nil, ErrBinding
	}
	defer clear(msg)
	out, err := box.SealAnonymous([]byte(kid), msg, pub, rand.Reader)
	if err != nil {
		return nil, ErrKey
	}
	return out, nil
}

// KeyPair is an X25519 sealing key pair.
type KeyPair struct {
	Public, Private [32]byte
}

// Keys are the sealing key pairs the emr service holds, by kid: one, or two
// during a rotation.
type Keys map[string]KeyPair

// GenerateKeyPair makes a key pair (tests; emr keys seal-init in service).
func GenerateKeyPair(r io.Reader) (KeyPair, error) {
	pub, priv, err := box.GenerateKey(r)
	if err != nil {
		return KeyPair{}, ErrKey
	}
	return KeyPair{Public: *pub, Private: *priv}, nil
}

// Open opens a box under keys for want at now. It refuses (ErrOpen) a box
// under a kid it doesn't hold, one that doesn't open, and one whose
// connection, field or origin set differs from want's, or whose issue time
// is more than 60 s old or more than 5 s ahead.
func Open(keys Keys, sealed []byte, want Binding, now time.Time) (Secret, error) {
	if len(keys) == 0 || len(keys) > maxKeys {
		return Secret{}, ErrKeys
	}
	if !want.valid() || len(sealed) <= KeyIDLen {
		return Secret{}, ErrOpen
	}
	kp, ok := keys[string(sealed[:KeyIDLen])]
	if !ok || KeyID(kp.Public[:]) != string(sealed[:KeyIDLen]) {
		return Secret{}, ErrOpen
	}
	msg, ok := box.OpenAnonymous(nil, sealed[KeyIDLen:], &kp.Public, &kp.Private)
	if !ok {
		return Secret{}, ErrOpen
	}
	defer clear(msg)
	var p plain
	if err := strictDecode(msg, &p); err != nil {
		return Secret{}, ErrOpen
	}
	defer clear(p.Value)
	issued := time.UnixMilli(p.IssuedAt)
	switch {
	case p.ConnectionID != want.ConnectionID.String(), p.Field != want.Field,
		!slices.Equal(p.Origin, want.origins()),
		now.Sub(issued) > MaxAge, issued.Sub(now) > MaxAhead,
		len(p.Value) < 1, len(p.Value) > MaxValue:
		return Secret{}, ErrOpen
	}
	return NewSecret(p.Value), nil
}

// strictDecode reads exactly the five keys, each once, nothing else.
func strictDecode(raw []byte, p *plain) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || len(m) != 5 {
		return ErrOpen
	}
	for _, k := range []string{"connection_id", "field", "origin", "issued_at", "value"} {
		if v, ok := m[k]; !ok || string(v) == "null" {
			return ErrOpen
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(p); err != nil {
		return ErrOpen
	}
	return nil
}

// Secret is a credential value in memory. It never prints.
type Secret struct {
	b []byte
}

// NewSecret copies b into a Secret.
func NewSecret(b []byte) Secret {
	return Secret{b: slices.Clone(b)}
}

// Bytes is the value itself. Never log it, put it in an error or a reply.
func (s Secret) Bytes() []byte { return s.b }

// Len is the value's length.
func (s Secret) Len() int { return len(s.b) }

// Zero overwrites the value (best effort: Go may have copied it).
func (s *Secret) Zero() {
	clear(s.b)
	s.b = nil
}

// String implements fmt.Stringer.
func (Secret) String() string { return redaction }

// GoString implements fmt.GoStringer (%#v).
func (Secret) GoString() string { return redaction }

// Format implements fmt.Formatter: every verb prints [redacted].
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redaction) }

// MarshalJSON implements json.Marshaler.
func (Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redaction) }

// MarshalText implements encoding.TextMarshaler.
func (Secret) MarshalText() ([]byte, error) { return []byte(redaction), nil }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redaction) }

// LoadPublic reads a sealing public key file (seal-<kid>.pub: base64 of 32
// bytes) and returns the key and its id. Errors never name the path.
func LoadPublic(path string) (*[32]byte, string, error) {
	b, err := readB64(path, 4096)
	if err != nil || len(b) != 32 {
		return nil, "", ErrKey
	}
	var pub [32]byte
	copy(pub[:], b)
	if isZero(pub[:]) {
		return nil, "", ErrKey
	}
	return &pub, KeyID(pub[:]), nil
}

// LoadPrivateDir reads the sealing private keys (seal-<kid>.key: base64 of
// 32 bytes) in dir: one, or two during a rotation. The file's kid must be
// the id of the public key it derives.
func LoadPrivateDir(dir string) (Keys, error) {
	files, err := filepath.Glob(filepath.Join(dir, "seal-*.key"))
	if err != nil || len(files) == 0 || len(files) > maxKeys {
		return nil, ErrKeys
	}
	out := Keys{}
	for _, f := range files {
		b, err := readB64(f, 4096)
		if err != nil || len(b) != 32 {
			return nil, ErrKey
		}
		var kp KeyPair
		copy(kp.Private[:], b)
		clear(b)
		pub, err := curve25519.X25519(kp.Private[:], curve25519.Basepoint)
		if err != nil {
			return nil, ErrKey
		}
		copy(kp.Public[:], pub)
		kid := KeyID(kp.Public[:])
		if filepath.Base(f) != "seal-"+kid+".key" {
			return nil, ErrKey
		}
		out[kid] = kp
	}
	return out, nil
}

func readB64(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrKey
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(raw)) > max {
		return nil, ErrKey
	}
	defer clear(raw)
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, ErrKey
	}
	return b, nil
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
