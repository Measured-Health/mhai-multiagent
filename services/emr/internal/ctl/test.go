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

	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/creds"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/scheduler"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrtypes"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
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
	if r.OK {
		// A passing test resumes a connection the scheduler stopped after
		// three sign-in refusals (design §15.2). Best effort: the reply is
		// the test's outcome either way.
		_ = scheduler.Resume(context.WithoutCancel(ctx), s.Pool, hid, id, s.now())
	}
	return testReply(r.Code), nil
}

// loadCreds reads every secret the type declares that is set for the
// current origin (internal/creds). ok is false when a required one isn't,
// or is a placeholder.
func (s *Credentials) loadCreds(ctx context.Context, ti emrtypes.TypeInfo, cn connRow) (connector.Creds, bool, error) {
	c, ok, err := creds.Load(ctx, s.Pool, s.Store, s.Env, ti, cn.cfg)
	switch {
	case errors.Is(err, creds.ErrStore):
		return c, false, errStore
	case err != nil:
		return c, false, errDatabase
	}
	return c, ok, nil
}
