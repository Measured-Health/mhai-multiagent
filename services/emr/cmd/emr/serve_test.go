package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

// Health reports the database honestly and claims nothing it doesn't have.
func TestHealthHandler(t *testing.T) {
	for _, tc := range []struct {
		db    pinger
		ready bool
	}{{fakeDB{}, true}, {fakeDB{errors.New("down")}, false}, {nil, false}} {
		rep, err := healthHandler(tc.db)(context.Background(), reqtoken.Claims{}, &emrpb.HealthRequest{})
		h, ok := rep.(*emrpb.HealthReply)
		if err != nil || !ok || h.GetDatabaseReady() != tc.ready || h.GetSecretStoreReady() || h.GetRuleSetVersion() != "" ||
			h.GetProxyAllowListAgeS() != -1 {
			t.Errorf("%v: %v %v", tc.db, rep, err)
		}
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"serve", "x"}} {
		if err := run(args, func(string) string { return "" }, nil); err != errUsage {
			t.Errorf("%v: %v", args, err)
		}
	}
}
