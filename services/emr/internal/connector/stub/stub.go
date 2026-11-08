// Package stub is the development connector (plan Task 9): it calls
// nothing and reads nothing. Its test passes with a usable access key and
// is sign_in_refused with none or a placeholder; it counts its calls, so
// the A28 tests can show nothing was attempted. The registry offers it only
// when APP_ENV is development.
package stub

import (
	"bytes"
	"context"
	"sync/atomic"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrtypes"
)

// ID is the stub's type id.
const ID = "stub"

// SecretField is the stub's one secret.
const SecretField = "api_key"

// Stub is the development connector.
type Stub struct {
	calls atomic.Int64
}

// New returns a stub.
func New() *Stub { return &Stub{} }

// Calls is how many times Test or Fetch ran.
func (s *Stub) Calls() int64 { return s.calls.Load() }

// Type is the stub's row of the shared table.
func (s *Stub) Type() connector.TypeInfo {
	t, _ := emrtypes.Lookup(ID)
	return t
}

// Validate checks the settings against the declared fields.
func (s *Stub) Validate(cfg connector.Config) error { return connector.ValidateFields(s.Type(), cfg) }

// Test answers ok for a usable key, sign_in_refused otherwise.
func (s *Stub) Test(ctx context.Context, _ connector.Config, creds connector.Creds) connector.TestResult {
	s.calls.Add(1)
	if ctx.Err() != nil {
		return connector.Result("timed_out")
	}
	if !Usable(creds.Get(SecretField).Bytes()) {
		return connector.Result("sign_in_refused")
	}
	return connector.Result("ok")
}

// Fetch reads nothing.
func (s *Stub) Fetch(_ context.Context, _ connector.Config, _ connector.Creds, cur connector.Cursor, _ connector.RawSink) (connector.Cursor, error) {
	s.calls.Add(1)
	return cur, nil
}

// RecordLink builds no link.
func (s *Stub) RecordLink(connector.Config, string) (string, bool) { return "", false }

// Usable reports whether v is a usable secret: not empty and not a
// placeholder (design §17.4).
func Usable(v []byte) bool {
	return len(bytes.TrimSpace(v)) > 0 && !connector.Placeholder(v)
}
