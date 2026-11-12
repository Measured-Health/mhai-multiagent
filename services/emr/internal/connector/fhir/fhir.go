// Package fhir is the FHIR R4 connector (EMR design §5.1, §5.2; plan
// Tasks 22-23): SMART Backend Services sign-in, a read-only test, and
// polling by _lastUpdated with a safe cursor, bounded runs and references
// read from the same server only.
//
// The server is treated as hostile: every response goes through the emr
// service's egress client (size caps, no redirects with a credential,
// origin checks), is read into allow-listed structs only (a field we don't
// read is never decoded), and is refused whole when its shape, sort order
// or size isn't what was asked for. Nothing here logs, and no error carries
// a URL, a host, a header, a token or a body: failures are closed codes
// (connector.FetchError).
package fhir

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrtypes"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/recordlink"
)

// ID is the connector type.
const ID = "fhir_r4"

// Limits and windows (design §5.1, §7.1).
const (
	// PageCount is the _count asked for; a page with more entries is
	// refused.
	PageCount = 200
	// MaxIDs is the most ids one _id read names.
	MaxIDs = 100
	// MaxPages and MaxRecords bound one run (the rest continue next run).
	MaxPages   = 20
	MaxRecords = 4000
	// Overlap is taken off the cursor for clock skew and late commits.
	Overlap = 2 * time.Minute
	// BackfillDays is how far a first sync reads back.
	BackfillDays = 30
	// MaxRetryAfter caps how long a 429 may hold polling off.
	MaxRetryAfter = 15 * time.Minute
	// PageTimeout is one request's deadline.
	PageTimeout = 60 * time.Second
)

// SearchTypes are the resource types polled by _lastUpdated.
var SearchTypes = []string{"Observation", "MedicationRequest", "MedicationStatement"}

// FHIR is the connector. Clients returns the egress HTTP client for a
// connection (egress.HTTP); it is the only way out.
type FHIR struct {
	Clients func(cfg connector.Config) *http.Client
	// Now is the clock (nil: time.Now).
	Now func() time.Time
	// MaxPages, MaxRecords and BackfillDays override the defaults (tests).
	MaxPages, MaxRecords, BackfillDays int

	mu     sync.Mutex
	tokens map[uuid.UUID]cachedToken
}

var (
	_ connector.Connector  = (*FHIR)(nil)
	_ connector.CatchUp    = (*FHIR)(nil)
	_ connector.Reconciler = (*FHIR)(nil)
)

// New returns the connector.
func New(clients func(connector.Config) *http.Client) *FHIR {
	return &FHIR{Clients: clients}
}

func (c *FHIR) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *FHIR) limits() (pages, records, days int) {
	pages, records, days = MaxPages, MaxRecords, BackfillDays
	if c.MaxPages > 0 {
		pages = c.MaxPages
	}
	if c.MaxRecords > 0 {
		records = c.MaxRecords
	}
	if c.BackfillDays > 0 && c.BackfillDays <= 365 {
		days = c.BackfillDays
	}
	return
}

func (c *FHIR) client(cfg connector.Config) (*http.Client, error) {
	if c.Clients == nil {
		return nil, connector.Fail("unreachable", 0)
	}
	cl := c.Clients(cfg)
	if cl == nil {
		return nil, connector.Fail("unreachable", 0)
	}
	return cl, nil
}

// Type is the shared table's row.
func (c *FHIR) Type() connector.TypeInfo {
	t, _ := emrtypes.Lookup(ID)
	return t
}

// Validate checks the settings against the declared fields.
func (c *FHIR) Validate(cfg connector.Config) error {
	return connector.ValidateFields(c.Type(), cfg)
}

// RecordLink is the platform admin's template on the base address's host.
func (c *FHIR) RecordLink(cfg connector.Config, sourcePatientID string) (string, bool) {
	return recordlink.Build(cfg.Settings["record_link"], cfg.BaseURL, sourcePatientID)
}

// sinceContext bounds one request.
func pageContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, PageTimeout)
}
