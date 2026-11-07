package db

import (
	"context"

	"github.com/pressly/goose/v3"
)

// SetTestPasswordSQL makes EnsureLogin rewrite its password statement with f
// (nil restores it), so a test can make that statement fail on the server.
func SetTestPasswordSQL(f func(string) string) { testPasswordSQL = f }

// MigrateUpTo applies migrations up to version, for tests that need the
// schema as it was before a later migration ran.
func MigrateUpTo(ctx context.Context, url string, version int64) error {
	return withProvider(url, func(p *goose.Provider) error {
		_, err := p.UpTo(ctx, version)
		return err
	})
}
