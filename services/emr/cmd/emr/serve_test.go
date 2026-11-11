package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func (f fakeDB) Ready(context.Context) error { return f.err }

// Health reports the database and the secret store honestly and claims
// nothing it doesn't have.
func TestHealthHandler(t *testing.T) {
	for _, tc := range []struct {
		db    pinger
		ready bool
	}{{fakeDB{}, true}, {fakeDB{errors.New("down")}, false}, {nil, false}} {
		rep, err := healthHandler(tc.db, nil, nil)(context.Background(), reqtoken.Claims{}, &emrpb.HealthRequest{})
		h, ok := rep.(*emrpb.HealthReply)
		if err != nil || !ok || h.GetDatabaseReady() != tc.ready || h.GetSecretStoreReady() || h.GetRuleSetVersion() != "" ||
			h.GetProxyAllowListAgeS() != -1 {
			t.Errorf("%v: %v %v", tc.db, rep, err)
		}
	}
	for _, tc := range []struct {
		store readier
		ready bool
	}{{fakeDB{}, true}, {fakeDB{errors.New("gone")}, false}} {
		rep, _ := healthHandler(fakeDB{}, tc.store, nil)(context.Background(), reqtoken.Claims{}, &emrpb.HealthRequest{})
		if rep.(*emrpb.HealthReply).GetSecretStoreReady() != tc.ready {
			t.Errorf("store %v: %v", tc.store, rep)
		}
	}
}

// With egress configured, health reports the allow-list's age in seconds;
// before the first write it is -1.
func TestHealthAllowListAge(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		ok   bool
		want int64
	}{{42 * time.Second, true, 42}, {0, false, -1}} {
		age := func(time.Time) (time.Duration, bool) { return tc.age, tc.ok }
		rep, _ := healthHandler(fakeDB{}, nil, age)(context.Background(), reqtoken.Claims{}, &emrpb.HealthRequest{})
		if got := rep.(*emrpb.HealthReply).GetProxyAllowListAgeS(); got != tc.want {
			t.Errorf("age %v %v: %d, want %d", tc.age, tc.ok, got, tc.want)
		}
	}
}

// The service refuses to start without its sealing key (design §17.4), and
// never uses the file store in production.
func TestServeRefusesWithoutSealingKey(t *testing.T) {
	env := map[string]string{"APP_ENV": "development", "EMR_NATS_URL": "nats://127.0.0.1:1", "EMR_NATS_PASSWORD": "synthetic-dev-pw",
		"EMR_DATABASE_URL": "postgres://u@127.0.0.1:1/x", "EMR_TRUSTED_KEYS_PATH": t.TempDir() + "/none.json",
		"EMR_SEAL_KEY_PATH": t.TempDir()}
	if _, err := loadSealKeys(env["EMR_SEAL_KEY_PATH"]); err == nil {
		t.Fatal("an empty sealing key directory accepted")
	}
	if _, err := openStore("", "development"); err != nil {
		t.Fatalf("no store configured: %v", err)
	}
	if s, err := openStore(t.TempDir(), "production"); err == nil || s != nil {
		t.Fatal("the file store opened in production")
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"serve", "x"}, {"unmapped", "--hospital", "x", "--connection", "y"},
		{"unmapped", "--connection", "x"}} {
		if err := run(args, func(string) string { return "" }, nil); err != errUsage {
			t.Errorf("%v: %v", args, err)
		}
	}
}
