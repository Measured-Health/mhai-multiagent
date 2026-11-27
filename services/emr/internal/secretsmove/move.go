// Package secretsmove moves connection credentials from the file store to
// OpenBao (plan Task 12, H3), when the emr service switches from staging
// with the file store to production with OpenBao (deploy/openbao/
// enable-openbao.sh). Each field is moved under its hospital (RLS), in one
// transaction: the metadata row is locked, the version it names read from
// the file store, written to OpenBao, and the row pointed at the new
// version. Idempotent: a row already naming an OpenBao version is skipped,
// so a second run moves nothing. The file store is left as it is (the
// operator removes it after checking, enable-openbao.sh
// --purge-file-store). Nothing here logs or returns a value or a path.
package secretsmove

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/secrets"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
)

// Result counts the fields: moved now, moved before, in the file store
// with no metadata row (left alone), and named by a row but missing from
// the file store (the connection needs its credentials again).
type Result struct {
	Moved, Already, Orphan, Missing int
}

// The closed errors.
var (
	ErrSource   = errors.New("secretsmove: the file store could not be read")
	ErrDest     = errors.New("secretsmove: OpenBao refused or is unavailable")
	ErrDatabase = errors.New("secretsmove: the database failed")
)

// FromFile moves every field of from (a file store, of its own
// environment) to to, under toEnv (the environment the service now runs
// as). It stops at the first failure; what was moved stays moved.
func FromFile(ctx context.Context, pool *pgxpool.Pool, from *secrets.FileStore, to secrets.Store, toEnv string) (Result, error) {
	var r Result
	paths, err := from.Fields(ctx)
	if err != nil {
		return r, ErrSource
	}
	for _, p := range paths {
		dest := p
		dest.Env = toEnv
		var written string
		err := dbx.WithHospital(ctx, pool, dbx.EMR, p.Hospital, func(tx pgx.Tx) error {
			var ver string
			err := tx.QueryRow(ctx, `SELECT secret_version FROM emr.credentials
				WHERE connection_id = $1 AND hospital_id = $2 AND field = $3 FOR UPDATE`,
				p.Connection, p.Hospital, p.Field).Scan(&ver)
			if errors.Is(err, pgx.ErrNoRows) {
				r.Orphan++
				return nil
			}
			if err != nil {
				return ErrDatabase
			}
			if secrets.IsOpenBaoVersion(ver) {
				r.Already++
				return nil
			}
			v, err := from.GetVersion(ctx, p, ver)
			if errors.Is(err, secrets.ErrNotFound) {
				r.Missing++
				return nil
			}
			if err != nil {
				return ErrSource
			}
			defer v.Zero()
			nv, err := to.Put(ctx, dest, v)
			if err != nil {
				return ErrDest
			}
			written = nv
			if _, err := tx.Exec(ctx, `UPDATE emr.credentials SET secret_version = $4
				WHERE connection_id = $1 AND hospital_id = $2 AND field = $3`,
				p.Connection, p.Hospital, p.Field, nv); err != nil {
				return ErrDatabase
			}
			return nil
		})
		if err != nil {
			if written != "" {
				// The row still names the file version: drop the copy.
				_ = to.DeleteVersion(context.WithoutCancel(ctx), dest, written)
			}
			switch {
			case errors.Is(err, ErrSource), errors.Is(err, ErrDest):
				return r, err
			default:
				return r, ErrDatabase
			}
		}
		if written != "" {
			r.Moved++
		}
	}
	return r, nil
}
