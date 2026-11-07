// Package secrets is the emr service's secret store (EMR design §6.1; plan
// Tasks 10, 12): the only place a connection's credential values live.
// Core has no access to it; the database holds metadata only.
//
// FileStore is for the demo (APP_ENV development or staging, synthetic
// credentials): one file per secret version, 0400, in 0700 directories on
// a volume only the emr container mounts. Production needs the managed
// store (Task 12, waiting on H3); the file store refuses it.
package secrets

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// The closed errors: none names a path, a value or a directory.
var (
	ErrNotFound = errors.New("secrets: not set")
	ErrPath     = errors.New("secrets: not a secret path")
	ErrStore    = errors.New("secrets: store unavailable")
	ErrRefused  = errors.New("secrets: the file store is for development and staging only")
)

// Keep is how long a replaced version stays (design §6.3).
const Keep = 24 * time.Hour

var (
	fieldKey = regexp.MustCompile(`^[a-z_]{1,40}$`)
	envName  = regexp.MustCompile(`^(development|staging|production)$`)
	versionK = regexp.MustCompile(`^v[0-9]{19}-[0-9a-f]{8}$`)
)

// Path names one secret: mhai/<env>/emr/<hospital>/<connection>/<field>.
type Path struct {
	Env                  string
	Hospital, Connection uuid.UUID
	Field                string
}

func (p Path) valid() bool {
	return envName.MatchString(p.Env) && p.Hospital != uuid.Nil && p.Connection != uuid.Nil && fieldKey.MatchString(p.Field)
}

// String is the path.
func (p Path) String() string {
	return "mhai/" + p.Env + "/emr/" + p.Hospital.String() + "/" + p.Connection.String() + "/" + p.Field
}

// Store keeps secret values by path, in versions.
type Store interface {
	// Put writes a new version and returns its id.
	Put(ctx context.Context, p Path, v seal.Secret) (version string, err error)
	// Get reads the current version.
	Get(ctx context.Context, p Path) (seal.Secret, string, error)
	// GetVersion reads one version.
	GetVersion(ctx context.Context, p Path, version string) (seal.Secret, error)
	// DeleteVersion drops one version (a put rolled back).
	DeleteVersion(ctx context.Context, p Path, version string) error
	// DeleteAll drops every secret of a connection; idempotent.
	DeleteAll(ctx context.Context, hospital, connection uuid.UUID) error
	// DeleteVersionsBefore drops the versions other than the current one
	// older than keep.
	DeleteVersionsBefore(ctx context.Context, p Path, keep time.Duration) error
	// Ready checks the store can be written.
	Ready(ctx context.Context) error
}
