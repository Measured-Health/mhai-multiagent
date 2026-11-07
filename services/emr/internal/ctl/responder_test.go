package ctl

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/logx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/replay"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// Synthetic ids only.
const (
	admin    = "11111111-1111-4111-8111-111111111111"
	hospital = "22222222-2222-4222-8222-222222222222"
	rid      = "44444444-4444-4444-8444-444444444444"
	conn     = "88888888-8888-4888-8888-888888888888"
	reply    = "_INBOX_gateway.ctl.1"
)

var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

type fixture struct {
	signer reqtoken.Signer
	r      *Responder
	ran    atomic.Int32
	logs   *bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{signer: reqtoken.Signer{Key: priv, KeyID: reqtoken.KeyID(priv.Public().(ed25519.PublicKey)),
		Now: func() time.Time { return now }}, logs: &bytes.Buffer{}}
	count := func(reply proto.Message) Handler {
		return func(context.Context, reqtoken.Claims, proto.Message) (proto.Message, error) {
			f.ran.Add(1)
			return reply, nil
		}
	}
	f.r = &Responder{
		Keys:    func() map[string]ed25519.PublicKey { return f.signer.PublicKeys() },
		Replay:  replay.New(2*time.Minute, 100),
		Log:     logx.New(f.logs),
		Metrics: &Metrics{},
		Now:     func() time.Time { return now.Add(time.Second) },
		Handlers: map[string]Handler{
			"health":          count(&emrpb.HealthReply{DatabaseReady: true, ProxyAllowListAgeS: -1}),
			"test":            count(&emrpb.TestReply{Ok: true, Code: "ok"}),
			"put_credentials": count(&emrpb.PutCredentialsReply{}),
		},
	}
	return f
}

func marshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// msg builds a control message the way core's emrctl client will: the
// token, schema and request id in headers, the body, the reply subject.
func (f *fixture) msg(t *testing.T, c reqtoken.Claims, subject, schema string, body []byte) *nats.Msg {
	t.Helper()
	tok, _, err := f.signer.Mint(c, body, 5*time.Second)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	m := nats.NewMsg(subject)
	m.Reply, m.Data = reply, body
	m.Header.Set(HeaderToken, tok)
	m.Header.Set(HeaderSchema, schema)
	m.Header.Set(HeaderRequestID, c.Rid)
	return m
}

func adminClaims(op string) reqtoken.Claims {
	return reqtoken.Claims{Aud: "emr", Sub: admin, Hid: hospital, Roles: []string{"platform_admin"}, Rid: rid,
		Kind: reqtoken.KindReq, Subject: "emr.v1.ctl." + op, ReplyPrefix: reqtoken.ReplyCore, Via: reqtoken.ViaCore}
}

func coreClaims(op string) reqtoken.Claims {
	return reqtoken.Claims{Aud: "emr", Sub: reqtoken.SvcCore, Hid: hospital, Rid: rid,
		Kind: reqtoken.KindReq, Subject: "emr.v1.ctl." + op, ReplyPrefix: reqtoken.ReplyCore, Via: reqtoken.ViaCore}
}

func (f *fixture) testMsg(t *testing.T) *nats.Msg {
	return f.msg(t, adminClaims("test"), "emr.v1.ctl.test", "mhai.emr.v1.TestRequest",
		marshal(t, &emrpb.TestRequest{ConnectionId: conn}))
}

// The happy paths the refusals are measured against.
func TestHealthAsSvcCore(t *testing.T) {
	f := newFixture(t)
	out, reason := f.r.Process(context.Background(), f.msg(t, coreClaims("health"), "emr.v1.ctl.health",
		"mhai.emr.v1.HealthRequest", nil))
	if out == nil || reason != "ok" || f.ran.Load() != 1 {
		t.Fatalf("health: %v %q ran %d", out, reason, f.ran.Load())
	}
	var h emrpb.HealthReply
	if err := proto.Unmarshal(out.Data, &h); err != nil || !h.GetDatabaseReady() || h.GetProxyAllowListAgeS() != -1 {
		t.Fatalf("reply: %v %v", &h, err)
	}
	if out.Header.Get(HeaderSchema) != "mhai.emr.v1.HealthReply" || out.Header.Get(HeaderRequestID) != rid {
		t.Fatalf("reply headers: %v", out.Header)
	}
	if out, reason := f.r.Process(context.Background(), f.testMsg(t)); out == nil || reason != "ok" {
		t.Fatalf("test as platform admin: %q", reason)
	}
	if f.r.Metrics.Count("ok") != 2 {
		t.Fatalf("ok count %d", f.r.Metrics.Count("ok"))
	}
}

// A4: forged, replayed and misdirected control requests get no reply, and
// no handler runs. Each refusal is counted and logged by its reason code
// only.
func TestCtlRefusals(t *testing.T) {
	cases := []struct {
		name, reason string
		build        func(t *testing.T, f *fixture) *nats.Msg
	}{
		{"token for another subject", "binding", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.msg(t, coreClaims("health"), "emr.v1.ctl.health", "mhai.emr.v1.HealthRequest", nil)
			m.Subject = "emr.v1.ctl.sync_now"
			return m
		}},
		{"another body", "binding", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.testMsg(t)
			m.Data = marshal(t, &emrpb.TestRequest{ConnectionId: "99999999-9999-4999-8999-999999999999"})
			return m
		}},
		{"aud pipeline", "claims", func(t *testing.T, f *fixture) *nats.Msg {
			c := reqtoken.Claims{Aud: "pipeline", Sub: reqtoken.SvcCore, Hid: hospital, Rid: rid, Kind: reqtoken.KindReq,
				Subject: "inference.v1.pipeline.health", ReplyPrefix: reqtoken.ReplyCore, Via: reqtoken.ViaCore}
			m := f.msg(t, c, "inference.v1.pipeline.health", "mhai.emr.v1.HealthRequest", nil)
			m.Subject = "emr.v1.ctl.health"
			return m
		}},
		{"expired", "expired", func(t *testing.T, f *fixture) *nats.Msg {
			f.r.Now = func() time.Time { return now.Add(5 * time.Second) }
			return f.testMsg(t)
		}},
		{"no token", "malformed", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.testMsg(t)
			m.Header.Del(HeaderToken)
			return m
		}},
		{"oversize body", "size", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.testMsg(t)
			m.Data = make([]byte, reqtoken.MaxReqBody+1)
			return m
		}},
		{"reply outside _INBOX_gateway.", "reply_subject", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.testMsg(t)
			m.Reply = "_INBOX_worker.emr.x"
			return m
		}},
		{"no reply subject", "reply_subject", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.testMsg(t)
			m.Reply = ""
			return m
		}},
		{"put_credentials as svc:core", "caller", func(t *testing.T, f *fixture) *nats.Msg {
			return f.msg(t, coreClaims("put_credentials"), "emr.v1.ctl.put_credentials",
				"mhai.emr.v1.PutCredentialsRequest", marshal(t, &emrpb.PutCredentialsRequest{ConnectionId: conn}))
		}},
		{"health as a user", "caller", func(t *testing.T, f *fixture) *nats.Msg {
			return f.msg(t, adminClaims("health"), "emr.v1.ctl.health", "mhai.emr.v1.HealthRequest", nil)
		}},
		{"test as a clinician", "caller", func(t *testing.T, f *fixture) *nats.Msg {
			c := adminClaims("test")
			c.Roles = []string{"clinician"}
			return f.msg(t, c, "emr.v1.ctl.test", "mhai.emr.v1.TestRequest", marshal(t, &emrpb.TestRequest{ConnectionId: conn}))
		}},
		{"unknown op", "unknown_op", func(t *testing.T, f *fixture) *nats.Msg {
			return f.msg(t, coreClaims("nope"), "emr.v1.ctl.nope", "mhai.emr.v1.HealthRequest", nil)
		}},
		{"wrong schema header", "schema", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.testMsg(t)
			m.Header.Set(HeaderSchema, "mhai.emr.v1.HealthRequest")
			return m
		}},
		{"unknown field in the body", "schema", func(t *testing.T, f *fixture) *nats.Msg {
			body := protowire.AppendTag(marshal(t, &emrpb.TestRequest{ConnectionId: conn}), 99, protowire.BytesType)
			body = protowire.AppendString(body, "smuggled")
			return f.msg(t, adminClaims("test"), "emr.v1.ctl.test", "mhai.emr.v1.TestRequest", body)
		}},
		{"not protobuf", "schema", func(t *testing.T, f *fixture) *nats.Msg {
			return f.msg(t, adminClaims("test"), "emr.v1.ctl.test", "mhai.emr.v1.TestRequest", []byte{0xff, 0xff, 0xff})
		}},
		{"request id differs from rid", "request_id", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.testMsg(t)
			m.Header.Set(HeaderRequestID, "55555555-5555-4555-8555-555555555555")
			return m
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			m := tc.build(t, f)
			out, reason := f.r.Process(context.Background(), m)
			if out != nil || f.ran.Load() != 0 {
				t.Fatalf("answered (%v) or ran a handler (%d)", out, f.ran.Load())
			}
			if reason != tc.reason || f.r.Metrics.Count(tc.reason) != 1 {
				t.Fatalf("reason %q (count %d), want %q", reason, f.r.Metrics.Count(tc.reason), tc.reason)
			}
			logged := f.logs.String()
			if !strings.Contains(logged, `"reason":"`+tc.reason+`"`) {
				t.Errorf("log lacks the reason: %s", logged)
			}
			if tok := m.Header.Get(HeaderToken); tok != "" && strings.Contains(logged, tok[:20]) {
				t.Errorf("log carries the token: %s", logged)
			}
			if strings.Contains(logged, conn) || strings.Contains(logged, admin) {
				t.Errorf("log carries an id from the request: %s", logged)
			}
		})
	}
}

// A4, replay: the same message twice is answered once.
func TestCtlRefusesReplayedJti(t *testing.T) {
	f := newFixture(t)
	m := f.testMsg(t)
	if out, reason := f.r.Process(context.Background(), m); out == nil || reason != "ok" {
		t.Fatalf("first: %q", reason)
	}
	if out, reason := f.r.Process(context.Background(), m); out != nil || reason != "replay" || f.ran.Load() != 1 {
		t.Fatalf("replay answered: %q ran %d", reason, f.ran.Load())
	}
	// Full replay set: fail closed.
	f.r.Replay = replay.New(time.Minute, 0)
	if out, reason := f.r.Process(context.Background(), f.testMsg(t)); out != nil || reason != "replay_full" {
		t.Fatalf("full set: %q", reason)
	}
}

// A handler error is a refusal too: no reply, the reason handler_error,
// nothing of the error in the log.
func TestHandlerErrorNoReply(t *testing.T) {
	f := newFixture(t)
	f.r.Handlers["test"] = func(context.Context, reqtoken.Claims, proto.Message) (proto.Message, error) {
		return nil, errors.New("synthetic failure naming " + conn)
	}
	if out, reason := f.r.Process(context.Background(), f.testMsg(t)); out != nil || reason != "handler_error" {
		t.Fatalf("%q", reason)
	}
	if strings.Contains(f.logs.String(), conn) {
		t.Fatalf("log carries the error: %s", f.logs.String())
	}
	// An op with no handler yet (a later PR) is refused the same way.
	delete(f.r.Handlers, "test")
	if out, reason := f.r.Process(context.Background(), f.testMsg(t)); out != nil || reason != "not_ready" {
		t.Fatalf("%q", reason)
	}
}
