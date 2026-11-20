package ctl

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/logx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/notifyops"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/notifypb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/replay"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// Synthetic ids only.
const (
	user     = "11111111-1111-4111-8111-111111111111"
	hospital = "22222222-2222-4222-8222-222222222222"
	rid      = "44444444-4444-4444-8444-444444444444"
	reply    = "_INBOX_gateway.ctl.1"
	health   = "notify.v1.ctl.health"
	schemaRq = "mhai.notify.v1.HealthRequest"

	// bound is a synthetic hospital-bound op, added to the table by
	// withBoundOp: PR 2 has none (send_link, PR 5, will be one).
	bound = "notify.v1.ctl.bound_test"
)

// withBoundOp adds a hospital-bound op to the table for one test.
func withBoundOp(t *testing.T) {
	t.Helper()
	saved := notifyops.Ops
	notifyops.Ops = append(append([]notifyops.Op(nil), saved...), notifyops.Op{Name: "notifications.bound_test",
		Op: "bound_test", SvcCore: true, HospitalBound: true, Timeout: 5 * time.Second,
		Request: schemaRq, Response: "mhai.notify.v1.HealthReply"})
	t.Cleanup(func() { notifyops.Ops = saved })
}

var now = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

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
	f.r = &Responder{
		Keys:    func() map[string]ed25519.PublicKey { return f.signer.PublicKeys() },
		Replay:  replay.New(2*time.Minute, 100),
		Log:     logx.New(f.logs),
		Metrics: &Metrics{},
		Now:     func() time.Time { return now.Add(time.Second) },
		Handlers: map[string]Handler{
			"health": func(context.Context, reqtoken.Claims, proto.Message) (proto.Message, error) {
				f.ran.Add(1)
				return &notifypb.HealthReply{DatabaseReady: true}, nil
			},
			"bound_test": func(_ context.Context, c reqtoken.Claims, _ proto.Message) (proto.Message, error) {
				f.ran.Add(1)
				h, err := Hospital(c)
				if err != nil || h.String() != hospital {
					return nil, errors.New("synthetic: wrong hospital")
				}
				return &notifypb.HealthReply{Channels: []string{"log"}}, nil
			},
		},
	}
	return f
}

// msg builds a control message the way core's client will (PR 5): the
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

func coreClaims(op string) reqtoken.Claims {
	return reqtoken.Claims{Aud: "notifications", Sub: reqtoken.SvcCore, Hid: hospital, Rid: rid,
		Kind: reqtoken.KindReq, Subject: "notify.v1.ctl." + op, ReplyPrefix: reqtoken.ReplyCore, Via: reqtoken.ViaCore}
}

func userClaims(roles ...string) reqtoken.Claims {
	c := coreClaims("health")
	c.Sub, c.Roles = user, roles
	return c
}

func (f *fixture) healthMsg(t *testing.T) *nats.Msg {
	return f.msg(t, coreClaims("health"), health, schemaRq, nil)
}

// The happy path the refusals are measured against.
func TestHealthAsSvcCore(t *testing.T) {
	f := newFixture(t)
	out, reason := f.r.Process(context.Background(), f.healthMsg(t))
	if out == nil || reason != "ok" || f.ran.Load() != 1 {
		t.Fatalf("health: %v %q ran %d", out, reason, f.ran.Load())
	}
	var h notifypb.HealthReply
	if err := proto.Unmarshal(out.Data, &h); err != nil || !h.GetDatabaseReady() || len(h.GetChannels()) != 0 {
		t.Fatalf("reply: %v %v", &h, err)
	}
	if out.Subject != reply || out.Header.Get(HeaderSchema) != "mhai.notify.v1.HealthReply" ||
		out.Header.Get(HeaderRequestID) != rid || out.Header.Get(HeaderToken) != "" {
		t.Fatalf("reply: %s %v", out.Subject, out.Header)
	}
	if f.r.Metrics.Count("ok") != 1 {
		t.Fatalf("ok count %d", f.r.Metrics.Count("ok"))
	}
	if !strings.Contains(f.logs.String(), `"op":"notifications.health"`) {
		t.Fatalf("answer not logged by op: %s", f.logs.String())
	}
}

// A hospital-bound op runs with the token's hospital, and one whose token
// names no hospital never reaches its handler.
func TestHospitalBoundOp(t *testing.T) {
	withBoundOp(t)
	f := newFixture(t)
	out, reason := f.r.Process(context.Background(), f.msg(t, coreClaims("bound_test"), bound, schemaRq, nil))
	if out == nil || reason != "ok" || f.ran.Load() != 1 {
		t.Fatalf("bound: %v %q ran %d", out, reason, f.ran.Load())
	}
	c := coreClaims("bound_test")
	c.Hid = "00000000-0000-0000-0000-000000000000"
	if out, reason := f.r.Process(context.Background(), f.msg(t, c, bound, schemaRq, nil)); out != nil || reason != "hospital" {
		t.Fatalf("nil hospital: %v %q", out, reason)
	}
}

func TestHospitalOfAToken(t *testing.T) {
	for hid, ok := range map[string]bool{
		hospital: true, "": false, "00000000-0000-0000-0000-000000000000": false, "not-a-uuid": false,
		"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee": true, "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE": false,
		"{" + hospital + "}": false, "urn:uuid:" + hospital: false,
	} {
		h, err := Hospital(reqtoken.Claims{Hid: hid})
		if (err == nil) != ok || (ok && h.String() != hid) || (!ok && !errors.Is(err, ErrHospital)) {
			t.Errorf("%q: %v %v", hid, h, err)
		}
	}
}

// ADR 0018, NT7: forged, replayed, misdirected and wrong-caller
// control requests get no reply, and no handler runs. A platform admin, an
// auditor or any user is refused on every op. Each refusal is counted and
// logged by its reason code only.
func TestCtlRefusals(t *testing.T) {
	cases := []struct {
		name, reason string
		build        func(t *testing.T, f *fixture) *nats.Msg
	}{
		{"token for another subject", "binding", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.msg(t, coreClaims("health"), health, schemaRq, nil)
			m.Subject = "notify.v1.ctl.unsubscribe_check"
			return m
		}},
		{"another body", "binding", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.healthMsg(t)
			m.Data = []byte{0x0a, 0x00}
			return m
		}},
		{"aud documents on the notifications subject", "claims", func(t *testing.T, f *fixture) *nats.Msg {
			c := coreClaims("health")
			c.Aud, c.Subject = "documents", "documents.v1.ctl.health"
			m := f.msg(t, c, "documents.v1.ctl.health", schemaRq, nil)
			m.Subject = health
			return m
		}},
		{"aud pipeline", "claims", func(t *testing.T, f *fixture) *nats.Msg {
			c := coreClaims("health")
			c.Aud, c.Subject = "pipeline", "inference.v1.pipeline.health"
			m := f.msg(t, c, "inference.v1.pipeline.health", schemaRq, nil)
			m.Subject = health
			return m
		}},
		{"signed by an unknown key", "signature", func(t *testing.T, f *fixture) *nats.Msg {
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			s := reqtoken.Signer{Key: other, KeyID: reqtoken.KeyID(other.Public().(ed25519.PublicKey)),
				Now: func() time.Time { return now }}
			tok, _, err := s.Mint(coreClaims("health"), nil, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			m := f.healthMsg(t)
			m.Header.Set(HeaderToken, tok)
			return m
		}},
		{"expired", "expired", func(t *testing.T, f *fixture) *nats.Msg {
			f.r.Now = func() time.Time { return now.Add(5 * time.Second) }
			return f.healthMsg(t)
		}},
		{"no token", "malformed", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.healthMsg(t)
			m.Header.Del(HeaderToken)
			return m
		}},
		{"no headers", "malformed", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.healthMsg(t)
			m.Header = nil
			return m
		}},
		{"garbage token", "malformed", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.healthMsg(t)
			m.Header.Set(HeaderToken, "not.a.token")
			return m
		}},
		{"oversize body", "size", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.healthMsg(t)
			m.Data = make([]byte, reqtoken.MaxReqBody+1)
			return m
		}},
		{"reply outside _INBOX_gateway.", "reply_subject", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.healthMsg(t)
			m.Reply = "_INBOX_worker.notifications.x"
			return m
		}},
		{"reply to a bff inbox", "reply_subject", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.healthMsg(t)
			m.Reply = "_INBOX_bff.clinical.x"
			return m
		}},
		{"no reply subject", "reply_subject", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.healthMsg(t)
			m.Reply = ""
			return m
		}},
		{"health as a platform admin", "caller", func(t *testing.T, f *fixture) *nats.Msg {
			return f.msg(t, userClaims("platform_admin"), health, schemaRq, nil)
		}},
		{"health as an auditor", "caller", func(t *testing.T, f *fixture) *nats.Msg {
			return f.msg(t, userClaims("auditor"), health, schemaRq, nil)
		}},
		{"health as a hospital admin", "caller", func(t *testing.T, f *fixture) *nats.Msg {
			return f.msg(t, userClaims("hospital_admin"), health, schemaRq, nil)
		}},
		{"unknown op", "unknown_op", func(t *testing.T, f *fixture) *nats.Msg {
			return f.msg(t, coreClaims("nope"), "notify.v1.ctl.nope", schemaRq, nil)
		}},
		{"an op of a later PR", "unknown_op", func(t *testing.T, f *fixture) *nats.Msg {
			return f.msg(t, coreClaims("unsubscribe_check"), "notify.v1.ctl.unsubscribe_check", schemaRq, nil)
		}},
		{"health as a clinician", "caller", func(t *testing.T, f *fixture) *nats.Msg {
			return f.msg(t, userClaims("clinician"), health, schemaRq, nil)
		}},
		{"wrong schema header", "schema", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.healthMsg(t)
			m.Header.Set(HeaderSchema, "mhai.emr.v1.HealthRequest")
			return m
		}},
		{"reply message named as the request", "schema", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.healthMsg(t)
			m.Header.Set(HeaderSchema, "mhai.notify.v1.HealthReply")
			return m
		}},
		{"unknown field in the body", "schema", func(t *testing.T, f *fixture) *nats.Msg {
			body := protowire.AppendTag(nil, 99, protowire.BytesType)
			body = protowire.AppendString(body, "smuggled")
			return f.msg(t, coreClaims("health"), health, schemaRq, body)
		}},
		{"not protobuf", "schema", func(t *testing.T, f *fixture) *nats.Msg {
			return f.msg(t, coreClaims("health"), health, schemaRq, []byte{0xff, 0xff, 0xff})
		}},
		{"request id differs from rid", "request_id", func(t *testing.T, f *fixture) *nats.Msg {
			m := f.healthMsg(t)
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
			if reason != tc.reason || f.r.Metrics.Count(tc.reason) != 1 || f.r.Metrics.Count("ok") != 0 {
				t.Fatalf("reason %q (count %d), want %q", reason, f.r.Metrics.Count(tc.reason), tc.reason)
			}
			logged := f.logs.String()
			if !strings.Contains(logged, `"reason":"`+tc.reason+`"`) {
				t.Errorf("log lacks the reason: %s", logged)
			}
			if tok := m.Header.Get(HeaderToken); len(tok) > 20 && strings.Contains(logged, tok[:20]) {
				t.Errorf("log carries the token: %s", logged)
			}
			for _, id := range []string{hospital, user, "smuggled"} {
				if strings.Contains(logged, id) {
					t.Errorf("log carries %s from the request: %s", id, logged)
				}
			}
		})
	}
}

// Replay: the same message twice is answered once, and a full replay set
// fails closed.
func TestCtlRefusesReplayedJti(t *testing.T) {
	f := newFixture(t)
	m := f.healthMsg(t)
	if out, reason := f.r.Process(context.Background(), m); out == nil || reason != "ok" {
		t.Fatalf("first: %q", reason)
	}
	if out, reason := f.r.Process(context.Background(), m); out != nil || reason != "replay" || f.ran.Load() != 1 {
		t.Fatalf("replay answered: %q ran %d", reason, f.ran.Load())
	}
	f.r.Replay = replay.New(time.Minute, 0)
	if out, reason := f.r.Process(context.Background(), f.healthMsg(t)); out != nil || reason != "replay_full" {
		t.Fatalf("full set: %q", reason)
	}
	f.r.Replay = nil
	if out, reason := f.r.Process(context.Background(), f.healthMsg(t)); out != nil || reason != "replay_full" {
		t.Fatalf("no set: %q", reason)
	}
}

// Twenty copies of one message at once: exactly one answer.
func TestCtlReplayRace(t *testing.T) {
	f := newFixture(t)
	m := f.healthMsg(t)
	var wg sync.WaitGroup
	var oks atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out, _ := f.r.Process(context.Background(), m); out != nil {
				oks.Add(1)
			}
		}()
	}
	wg.Wait()
	if oks.Load() != 1 || f.ran.Load() != 1 {
		t.Fatalf("%d answers, %d handler runs", oks.Load(), f.ran.Load())
	}
}

// No keys (an empty or unread trusted-keys file) answers nothing.
func TestCtlWithoutKeysRefuses(t *testing.T) {
	f := newFixture(t)
	f.r.Keys = nil
	if out, reason := f.r.Process(context.Background(), f.healthMsg(t)); out != nil || reason != "signature" {
		t.Fatalf("%q", reason)
	}
}

// A handler error is a refusal too: no reply, the reason handler_error,
// nothing of the error in the log. So is a reply of the wrong type, and an
// op whose handler isn't built yet.
func TestHandlerErrorNoReply(t *testing.T) {
	f := newFixture(t)
	f.r.Handlers["health"] = func(context.Context, reqtoken.Claims, proto.Message) (proto.Message, error) {
		return nil, errors.New("synthetic failure naming " + hospital)
	}
	if out, reason := f.r.Process(context.Background(), f.healthMsg(t)); out != nil || reason != "handler_error" {
		t.Fatalf("%q", reason)
	}
	if strings.Contains(f.logs.String(), hospital) {
		t.Fatalf("log carries the error: %s", f.logs.String())
	}
	f.r.Handlers["health"] = func(context.Context, reqtoken.Claims, proto.Message) (proto.Message, error) {
		return &notifypb.HealthRequest{}, nil
	}
	if out, reason := f.r.Process(context.Background(), f.healthMsg(t)); out != nil || reason != "handler_error" {
		t.Fatalf("wrong reply type: %q", reason)
	}
	f.r.Handlers["health"] = func(context.Context, reqtoken.Claims, proto.Message) (proto.Message, error) {
		return nil, nil
	}
	if out, reason := f.r.Process(context.Background(), f.healthMsg(t)); out != nil || reason != "handler_error" {
		t.Fatalf("nil reply: %q", reason)
	}
	delete(f.r.Handlers, "health")
	if out, reason := f.r.Process(context.Background(), f.healthMsg(t)); out != nil || reason != "not_ready" {
		t.Fatalf("no handler: %q", reason)
	}
}

// The handler gets a deadline: the op's timeout.
func TestHandlerHasTheOpsDeadline(t *testing.T) {
	f := newFixture(t)
	var left time.Duration
	f.r.Handlers["health"] = func(ctx context.Context, _ reqtoken.Claims, _ proto.Message) (proto.Message, error) {
		d, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("no deadline")
		}
		left = time.Until(d)
		return &notifypb.HealthReply{}, nil
	}
	if out, reason := f.r.Process(context.Background(), f.healthMsg(t)); out == nil || reason != "ok" {
		t.Fatalf("%q", reason)
	}
	if left <= 0 || left > 5*time.Second {
		t.Fatalf("deadline in %v, want within 5s", left)
	}
}

// The responder's subjects and queue group are the NATS user's
// (services/nats/notifications.conf).
func TestSubscriptionShape(t *testing.T) {
	if Subjects != "notify.v1.ctl.>" || QueueGroup != "notifications" || Audience != "notifications" {
		t.Fatalf("%s %s %s", Subjects, QueueGroup, Audience)
	}
}

// The strict schema check holds for every message of the contract: no
// map fields, whose message values a careless decoder could skip.
func TestContractHasNoMaps(t *testing.T) {
	msgs := notifypb.File_mhai_notify_v1_control_proto.Messages()
	if msgs.Len() == 0 {
		t.Fatal("no messages")
	}
	for i := 0; i < msgs.Len(); i++ {
		fields := msgs.Get(i).Fields()
		for j := 0; j < fields.Len(); j++ {
			if fields.Get(j).IsMap() {
				t.Errorf("%s.%s is a map", msgs.Get(i).FullName(), fields.Get(j).Name())
			}
		}
	}
}

// hasUnknown finds an unknown field inside a message-typed map value.
func TestHasUnknownInsideMapValues(t *testing.T) {
	// structpb.Struct holds map<string, Value>: a known message with a map.
	v := structpb.NewStringValue("synthetic")
	v.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
	st := &structpb.Struct{Fields: map[string]*structpb.Value{"k": v}}
	if !hasUnknown(st.ProtoReflect()) {
		t.Fatal("an unknown field inside a map value was not found")
	}
	if hasUnknown((&structpb.Struct{Fields: map[string]*structpb.Value{"k": structpb.NewNullValue()}}).ProtoReflect()) {
		t.Fatal("a clean map was flagged")
	}
}
