// Package connector is the connector interface (EMR design §4; plan Task
// 9). A connector fetches from one kind of hospital system, maps its format
// to raw records and builds the record link; it never normalises, matches
// patients or runs rules. Its types and declared fields are the shared
// table's (gokit/emrtypes), so core and the emr service agree on them.
//
// Credentials reach a connector only as Creds, which never print, and a
// static test (ast_test.go) fails any connector code that passes Creds or a
// seal.Secret to a logger or a print function.
package connector

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrtypes"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
)

// TypeInfo and Field are the shared table's (gokit/emrtypes).
type (
	TypeInfo = emrtypes.TypeInfo
	Field    = emrtypes.Field
)

// Config is a connection's saved configuration, as the emr service reads it
// under the connection's hospital. It holds no secret.
type Config struct {
	ConnectionID, HospitalID uuid.UUID
	Type, BaseURL            string
	Settings                 map[string]string
	// Origin is the credential origin (design §6.3), sorted.
	Origin      []emrurl.Origin
	PollSeconds int
}

// Cursor is a connector's opaque position in its source.
type Cursor []byte

// TestResult is a test's outcome: a closed code (emrtypes.Codes).
type TestResult struct {
	OK   bool
	Code string
}

// Result is the TestResult for code; a code outside the closed set reads
// as unreadable_data, never as itself.
func Result(code string) TestResult {
	if !emrtypes.IsCode(code) {
		code = "unreadable_data"
	}
	return TestResult{OK: code == "ok", Code: code}
}

// RawRecord is one record as fetched, mapped by its connector to the
// allow-list's field names (EMR design §14.1). Kind is patient, encounter,
// location, observation or medication. RawSink keeps only the listed
// fields of the right shape and drops the rest before anything is stored.
// A reference to a patient, an encounter or a location is the source's
// plain id ("123", not "Patient/123"); codings and identifiers are
// []Coding.
type RawRecord struct {
	Kind, SourceID, SourceVersion string
	SourceUpdatedAt               time.Time
	Fields                        map[string]any
}

// Coding is one code from the source (FHIR Coding), or one identifier
// (System is the identifier's namespace, Code its value).
type Coding struct {
	System  string `json:"system,omitempty"`
	Code    string `json:"code,omitempty"`
	Display string `json:"display,omitempty"`
}

// RawSink takes raw records: it enforces the allow-list and the size
// limits before anything is stored (plan Task 17).
type RawSink interface {
	Put(ctx context.Context, r RawRecord) error
}

// Connector is one kind of hospital system (design §4).
type Connector interface {
	// Type is the connector's row of the shared table.
	Type() TypeInfo
	// Validate checks cfg's shape only, without the network.
	Validate(cfg Config) error
	// Test is a read-only probe with a deadline in ctx: a closed code.
	Test(ctx context.Context, cfg Config, creds Creds) TestResult
	// Fetch reads from cur on, writing through sink, and returns the new
	// cursor.
	Fetch(ctx context.Context, cfg Config, creds Creds, cur Cursor, sink RawSink) (Cursor, error)
	// RecordLink is the "open record" link for a source patient id.
	RecordLink(cfg Config, sourcePatientID string) (string, bool)
}

// ErrConfig is Validate's refusal: a fixed value, naming no setting's value.
var ErrConfig = errors.New("connector: configuration does not fit the type")

// ValidateFields checks cfg's settings against t's declared text fields:
// every required one present, nothing undeclared, each within its maximum.
// A secret field never appears in settings.
func ValidateFields(t TypeInfo, cfg Config) error {
	if cfg.Type != t.ID {
		return ErrConfig
	}
	for k, v := range cfg.Settings {
		f, ok := t.Field(k)
		if !ok || f.Kind != emrtypes.KindText || len(v) > f.Max {
			return ErrConfig
		}
	}
	for _, f := range t.Fields {
		if f.Kind == emrtypes.KindText && f.Required && cfg.Settings[f.Key] == "" {
			return ErrConfig
		}
	}
	return nil
}

// placeholders are values that mean "not set" (design §17.4: a placeholder
// or empty secret marks the connection sign_in_refused).
var placeholders = [][]byte{[]byte("changeme"), []byte("change-me"), []byte("change_me"), []byte("placeholder"),
	[]byte("replace-me"), []byte("replaceme"), []byte("todo"), []byte("xxx")}

// Placeholder reports whether v is empty or a placeholder value.
func Placeholder(v []byte) bool {
	t := bytes.ToLower(bytes.TrimSpace(v))
	if len(t) == 0 {
		return true
	}
	for _, p := range placeholders {
		if bytes.Equal(t, p) || (len(p) > 4 && bytes.Contains(t, p)) {
			return true
		}
	}
	return false
}

// FetchError is a fetch's failure as a closed code (emrtypes.Codes), with
// how long the source asked us to wait (429 Retry-After; 0 when it named
// none). Its text is fixed: never a URL, a host or a body.
type FetchError struct {
	Code       string
	RetryAfter time.Duration
}

func (e *FetchError) Error() string { return "connector: fetch failed: " + e.Code }

// Fail is a FetchError for code; a code outside the closed set reads as
// unreadable_data.
func Fail(code string, retryAfter time.Duration) *FetchError {
	if !emrtypes.IsCode(code) || code == "ok" {
		code = "unreadable_data"
	}
	return &FetchError{Code: code, RetryAfter: retryAfter}
}

// CodeOf is err's closed code: ok for nil, the FetchError's code, and
// unreachable for anything else (our own database or the sink: never the
// source's fault, never sign_in_refused).
func CodeOf(err error) string {
	if err == nil {
		return "ok"
	}
	var fe *FetchError
	if errors.As(err, &fe) {
		return fe.Code
	}
	return "unreachable"
}

// CatchUp is a connector whose cursor tells whether the last fetch read
// everything there was (the end of a first sync's backfill, design §7.1).
type CatchUp interface {
	CaughtUp(cur Cursor) bool
}

// Reconciler is a connector that can re-read a window it has already read
// (the daily reconciliation, design §5.1): Rewind returns cur set to read
// from since again, without ever moving its high-water mark backward.
type Reconciler interface {
	Rewind(cur Cursor, since time.Time) Cursor
}
