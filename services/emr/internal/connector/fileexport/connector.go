package fileexport

// The connector (plan Task 44): not yet built.

import (
	"context"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrtypes"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
)

// ID is the connector type.
const ID = "file_export"

// SecretField is the SSH private key.
const SecretField = "access_key"

// MaxFiles is the most files one run reads.
const MaxFiles = 20

// The subfolders files are moved to.
const (
	Processed = "processed"
	Rejected  = "rejected"
)

// Dialer opens the SSH client for a connection.
type Dialer func(ctx context.Context, cfg connector.Config, origin emrurl.Origin, fingerprint, user string,
	auth []ssh.AuthMethod) (*ssh.Client, error)

// Metrics are counts only.
type Metrics struct {
	FilesLoaded, FilesRejected, RowsRejected, Skipped atomic.Int64
}

// FileExport is the connector.
type FileExport struct {
	Dial     Dialer
	Now      func() time.Time
	MaxFiles int
	Metrics  *Metrics
}

// New returns the connector.
func New(dial Dialer) *FileExport { return &FileExport{Dial: dial, Metrics: &Metrics{}} }

// Type is the shared table's row.
func (c *FileExport) Type() connector.TypeInfo {
	t, _ := emrtypes.Lookup(ID)
	return t
}

// Validate: not yet.
func (c *FileExport) Validate(cfg connector.Config) error { return connector.ValidateFields(c.Type(), cfg) }

// RecordLink: none.
func (c *FileExport) RecordLink(connector.Config, string) (string, bool) { return "", false }

// Test: not yet.
func (c *FileExport) Test(context.Context, connector.Config, connector.Creds) connector.TestResult {
	return connector.Result("unreachable")
}

// Fetch: not yet.
func (c *FileExport) Fetch(_ context.Context, _ connector.Config, _ connector.Creds, cur connector.Cursor, _ connector.RawSink) (connector.Cursor, error) {
	return cur, connector.Fail("unreachable", 0)
}

// CaughtUp: not yet.
func (c *FileExport) CaughtUp(connector.Cursor) bool { return false }

// NewFileAt: not yet.
func (c *FileExport) NewFileAt(connector.Cursor) (time.Time, bool) { return time.Time{}, false }
