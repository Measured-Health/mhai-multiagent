package connector

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

const redacted = "[redacted]"

// Creds are a connection's credentials for one test or fetch, by field. They
// never print: every formatting path, JSON and slog show [redacted]. Load
// them per use and Zero them after.
type Creds struct {
	m map[string]seal.Secret
}

// NewCreds holds m (by field key).
func NewCreds(m map[string]seal.Secret) Creds {
	c := Creds{m: map[string]seal.Secret{}}
	for k, v := range m {
		c.m[k] = v
	}
	return c
}

// Get is the field's secret; the zero Secret when not set.
func (c Creds) Get(field string) seal.Secret { return c.m[field] }

// Has reports whether field is set (non-empty).
func (c Creds) Has(field string) bool { return c.m[field].Len() > 0 }

// Zero overwrites and drops every value.
func (c *Creds) Zero() {
	for k, v := range c.m {
		v.Zero()
		delete(c.m, k)
	}
}

// String implements fmt.Stringer.
func (Creds) String() string { return redacted }

// GoString implements fmt.GoStringer.
func (Creds) GoString() string { return redacted }

// Format implements fmt.Formatter.
func (Creds) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// MarshalJSON implements json.Marshaler.
func (Creds) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

// MarshalText implements encoding.TextMarshaler.
func (Creds) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LogValue implements slog.LogValuer.
func (Creds) LogValue() slog.Value { return slog.StringValue(redacted) }
