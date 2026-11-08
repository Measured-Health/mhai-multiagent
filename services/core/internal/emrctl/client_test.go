package emrctl

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

const (
	admin    = "11111111-1111-4111-8111-111111111111"
	hospital = "22222222-2222-4222-8222-222222222222"
)

func adminCaller(stepUp bool) Caller {
	return Caller{Sub: admin, Hid: hospital, Rid: "rid-1", Roles: []string{"platform_admin"}, StepUp: stepUp}
}

// Core's own check comes before anything is sent (the client has no
// connection here, so a call that got past it would be ErrMisuse for that).
func TestRequestRefusesBeforeSending(t *testing.T) {
	c := &Client{}
	ctx := context.Background()
	put := &emrpb.PutCredentialsRequest{ConnectionId: uuid.NewString()}
	cases := []struct {
		name   string
		caller Caller
		op     string
		req    proto.Message
		resp   proto.Message
		want   error
	}{
		{"put without step-up", adminCaller(false), "put_credentials", put, &emrpb.PutCredentialsReply{}, ErrNotAllowed},
		{"put as svc:core", System(hospital, "rid-1"), "put_credentials", put, &emrpb.PutCredentialsReply{}, ErrNotAllowed},
		{"put as a clinician", Caller{Sub: admin, Hid: hospital, Rid: "r", Roles: []string{"clinician"}, StepUp: true},
			"put_credentials", put, &emrpb.PutCredentialsReply{}, ErrNotAllowed},
		{"test as svc:core", System(hospital, "rid-1"), "test", &emrpb.TestRequest{}, &emrpb.TestReply{}, ErrNotAllowed},
		{"health as a user", adminCaller(false), "health", &emrpb.HealthRequest{}, &emrpb.HealthReply{}, ErrNotAllowed},
		{"stp on an op that has none", adminCaller(true), "test", &emrpb.TestRequest{}, &emrpb.TestReply{}, ErrMisuse},
		{"unknown op", adminCaller(false), "nope", &emrpb.TestRequest{}, &emrpb.TestReply{}, ErrMisuse},
		{"wrong request type", adminCaller(false), "test", &emrpb.HealthRequest{}, &emrpb.TestReply{}, ErrMisuse},
		{"wrong reply type", adminCaller(false), "test", &emrpb.TestRequest{}, &emrpb.HealthReply{}, ErrMisuse},
		{"no hospital", Caller{Sub: admin, Rid: "r", Roles: []string{"platform_admin"}}, "test", &emrpb.TestRequest{},
			&emrpb.TestReply{}, ErrMisuse},
		{"bad request id", Caller{Sub: admin, Hid: hospital, Rid: "has space", Roles: []string{"platform_admin"}}, "test",
			&emrpb.TestRequest{}, &emrpb.TestReply{}, ErrMisuse},
	}
	for _, tc := range cases {
		if err := c.Request(ctx, tc.caller, tc.op, tc.req, tc.resp); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	// Allowed calls get past the check (and stop at the missing connection).
	if err := c.Request(ctx, adminCaller(true), "put_credentials", put, &emrpb.PutCredentialsReply{}); !errors.Is(err, ErrMisuse) {
		t.Errorf("allowed put: %v", err)
	}
	if err := c.Request(ctx, System(hospital, "r"), "health", &emrpb.HealthRequest{}, &emrpb.HealthReply{}); !errors.Is(err, ErrMisuse) {
		t.Errorf("allowed health: %v", err)
	}
	if reqtoken.SvcCore != "svc:core" {
		t.Fatal("svc:core")
	}
}

// The sealer seals to the emr service's key, bound to the connection, the
// field and the origin; the box opens only with the private key.
func TestSealerBinds(t *testing.T) {
	kp, err := seal.GenerateKeyPair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSealer(kp.Public)
	conn := uuid.New()
	origin := []emrurl.Origin{"https://fhir.example.org:443"}
	f, err := s.Seal(conn, "private_key", origin, []byte("synthetic-test-secret-not-real-0006"))
	if err != nil || f.GetField() != "private_key" || f.GetKid() != seal.KeyID(kp.Public[:]) {
		t.Fatalf("seal: %v %v", f, err)
	}
	keys := seal.Keys{seal.KeyID(kp.Public[:]): kp}
	v, err := seal.Open(keys, f.GetBox(), seal.Binding{ConnectionID: conn, Field: "private_key", Origin: origin}, s.now())
	if err != nil || string(v.Bytes()) != "synthetic-test-secret-not-real-0006" {
		t.Fatalf("open: %v", err)
	}
	if _, err := seal.Open(keys, f.GetBox(), seal.Binding{ConnectionID: uuid.New(), Field: "private_key", Origin: origin}, s.now()); err == nil {
		t.Fatal("opened for another connection")
	}
	var none *Sealer
	if _, err := none.Seal(conn, "private_key", origin, []byte("x")); !errors.Is(err, ErrSeal) {
		t.Fatalf("nil sealer: %v", err)
	}
	if _, err := s.Seal(conn, "private_key", origin, nil); !errors.Is(err, ErrSeal) {
		t.Fatalf("empty value: %v", err)
	}
}
