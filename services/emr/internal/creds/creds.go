// Package creds loads a connection's credentials for one test or fetch
// (EMR design §6.1, §6.3): every secret its type declares that is set for
// the connection's current origin, from the secret store, into
// connector.Creds. The control responder's test and the scheduler share it,
// so the rule (current origin only, no placeholder) is written once.
package creds

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/secrets"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrtypes"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// The closed errors: the database or the store failed (never the source's
// fault, so never sign_in_refused).
var (
	ErrDatabase = errors.New("creds: the database failed")
	ErrStore    = errors.New("creds: the secret store failed")
)

// Load reads the secrets of ti set for cfg.Origin (cfg.Origin must be the
// origin the saved addresses give now). ok is false when a required one
// isn't set for that origin, or is empty or a placeholder.
func Load(ctx context.Context, pool *pgxpool.Pool, store secrets.Store, env string, ti emrtypes.TypeInfo,
	cfg connector.Config) (connector.Creds, bool, error) {
	type meta struct {
		version string
		origin  []string
	}
	set := map[string]meta{}
	err := dbx.WithHospital(ctx, pool, dbx.EMR, cfg.HospitalID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT field, secret_version, origin FROM emr.credentials
			WHERE connection_id = $1 AND hospital_id = $2`, cfg.ConnectionID, cfg.HospitalID)
		if err != nil {
			return ErrDatabase
		}
		defer rows.Close()
		for rows.Next() {
			var f string
			var m meta
			if err := rows.Scan(&f, &m.version, &m.origin); err != nil {
				return ErrDatabase
			}
			slices.Sort(m.origin)
			set[f] = m
		}
		return rows.Err()
	})
	if err != nil {
		return connector.Creds{}, false, ErrDatabase
	}
	want := emrurl.Strings(cfg.Origin)
	values := map[string]seal.Secret{}
	creds := func() connector.Creds { return connector.NewCreds(values) }
	for _, f := range ti.Fields {
		if f.Kind != emrtypes.KindSecret {
			continue
		}
		m, has := set[f.Key]
		if !has || !slices.Equal(m.origin, want) {
			if f.Required {
				return creds(), false, nil
			}
			continue
		}
		v, err := store.GetVersion(ctx, secrets.Path{Env: env, Hospital: cfg.HospitalID, Connection: cfg.ConnectionID, Field: f.Key}, m.version)
		switch {
		case errors.Is(err, secrets.ErrNotFound):
			if f.Required {
				return creds(), false, nil
			}
			continue
		case err != nil:
			return creds(), false, ErrStore
		}
		values[f.Key] = v
		if f.Required && connector.Placeholder(v.Bytes()) {
			return creds(), false, nil
		}
	}
	return creds(), true, nil
}
