package ctl

// test (platform admin; EMR design §6.3, A28; plan Task 10): try the saved
// settings. Credentials go only to the origin they were entered for, so
// before the connector runs, every required secret must be set for the
// connection's current origin (recomputed from its saved addresses, and
// equal to the stored one), and usable (not empty or a placeholder, §17.4).
// Otherwise the answer is sign_in_refused and the connector is never
// called: nothing is sent. The reply is the outcome only; core records it.

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/secrets"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrtypes"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

func testReply(code string) *emrpb.TestReply {
	r := connector.Result(code)
	return &emrpb.TestReply{Ok: r.OK, Code: r.Code}
}

// Test is the test op.
func (s *Credentials) Test(ctx context.Context, c reqtoken.Claims, m proto.Message) (proto.Message, error) {
	req, ok := m.(*emrpb.TestRequest)
	if !ok || s.Store == nil || s.Connectors == nil {
		return nil, errRequest
	}
	hid, id, err := ids(c, req.GetConnectionId())
	if err != nil {
		return nil, err
	}
	cn, err := s.loadConn(ctx, hid, id)
	if err != nil {
		return nil, err
	}
	ti, known := emrtypes.Lookup(cn.cfg.Type)
	impl, built := s.Connectors.Get(cn.cfg.Type)
	if !known || !built {
		return nil, errConnection
	}
	if impl.Validate(cn.cfg) != nil {
		return testReply("unreadable_data"), nil
	}
	// The origin as the saved addresses give it now must be the one the
	// credentials were entered for.
	origin, err := emrurl.OriginsOf(cn.cfg.Type, cn.cfg.BaseURL, cn.cfg.Settings, s.development())
	if err != nil || !slices.Equal(origin, cn.stored) {
		return testReply("sign_in_refused"), nil
	}
	cn.cfg.Origin = origin
	creds, ok, err := s.loadCreds(ctx, ti, cn)
	if err != nil {
		return nil, err
	}
	defer creds.Zero()
	if !ok {
		return testReply("sign_in_refused"), nil
	}
	tctx, cancel := context.WithTimeout(ctx, TestTimeout)
	defer cancel()
	// Only now, with everything checked, may the connection go out: its
	// own hosts and ranges, on egress-emr's list, while the test runs.
	if s.Egress != nil {
		release, err := s.Egress.Hold(tctx, hid, id)
		if err != nil {
			return testReply("unreachable"), nil
		}
		defer release()
	}
	r := impl.Test(tctx, cn.cfg, creds)
	return testReply(r.Code), nil
}

// loadCreds reads every secret the type declares that is set for the
// current origin. ok is false when a required one isn't, or is a
// placeholder.
func (s *Credentials) loadCreds(ctx context.Context, ti emrtypes.TypeInfo, cn connRow) (connector.Creds, bool, error) {
	type meta struct {
		version string
		origin  []string
	}
	set := map[string]meta{}
	err := dbx.WithHospital(ctx, s.Pool, dbx.EMR, cn.cfg.HospitalID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT field, secret_version, origin FROM emr.credentials
			WHERE connection_id = $1 AND hospital_id = $2`, cn.cfg.ConnectionID, cn.cfg.HospitalID)
		if err != nil {
			return errDatabase
		}
		defer rows.Close()
		for rows.Next() {
			var f string
			var m meta
			if err := rows.Scan(&f, &m.version, &m.origin); err != nil {
				return errDatabase
			}
			slices.Sort(m.origin)
			set[f] = m
		}
		return rows.Err()
	})
	if err != nil {
		return connector.Creds{}, false, errDatabase
	}
	want := emrurl.Strings(cn.cfg.Origin)
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
		v, err := s.Store.GetVersion(ctx, secrets.Path{Env: s.Env, Hospital: cn.cfg.HospitalID,
			Connection: cn.cfg.ConnectionID, Field: f.Key}, m.version)
		switch {
		case errors.Is(err, secrets.ErrNotFound):
			if f.Required {
				return creds(), false, nil
			}
			continue
		case err != nil:
			return creds(), false, errStore
		}
		values[f.Key] = v
		if f.Required && connector.Placeholder(v.Bytes()) {
			return creds(), false, nil
		}
	}
	return creds(), true, nil
}
