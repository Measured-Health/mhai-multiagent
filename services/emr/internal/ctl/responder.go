// Package ctl is the emr service's control responder: emr.v1.ctl.<op>,
// queue group emr (EMR design §3.2, plan Task 4). Core is the only caller.
// Every request is checked, in order, before any handler runs:
//
//  1. size (the body, and the token inside reqtoken's check);
//  2. reqtoken.VerifyAt for aud emr, the subject it arrived on and kind req:
//     signature, claims, expiry;
//  3. (the same call) the subject table's binding of subject, audience and
//     body;
//  4. (the same call) the token's kind is req;
//  5. reqtoken.CheckReply: the reply goes to _INBOX_gateway. only;
//  6. the replay set (a jti answered once per process; full fails closed);
//  7. the op (gokit/emrops) and its caller rule, and for a StepUp op
//     the token's stp claim (core checked a fresh step-up code);
//  8. the Mhai-Request-Id header equals the token's rid;
//  9. the schema: the header names the op's request message and the body
//     is exactly that message, with no unknown field.
//
// Any failure: no reply, one log line with the reason code (and the rid,
// once the token has verified), a metric. Nothing from the request body or
// the token is logged.
package ctl

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrops"
	_ "github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb" // registers the emr messages
	"github.com/Measured-Health/mhai-multiagent/services/gokit/replay"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// Headers, as core's intelligence client uses them.
const (
	HeaderToken     = "Mhai-Request-Token"
	HeaderSchema    = "Mhai-Schema"
	HeaderRequestID = "Mhai-Request-Id"
)

// Audience is the aud every control token carries.
const Audience = "emr"

// QueueGroup is the responder's queue group (the emr NATS user may
// subscribe emr.v1.ctl.> in this group only).
const QueueGroup = "emr"

// Subjects is what the responder subscribes.
const Subjects = emrops.SubjectPrefix + ">"

// ReplayWindow is how long past a token's expiry its jti is remembered
// (clock skew); MaxReplayIDs bounds the set.
const (
	ReplayWindow = 2 * time.Minute
	MaxReplayIDs = 100000
)

// Handler serves one op. req is the op's request message, already
// checked; the reply must be the op's response message. An error means no
// reply (logged as handler_error, never with the error's text).
type Handler func(ctx context.Context, c reqtoken.Claims, req proto.Message) (proto.Message, error)

// Responder answers control requests.
type Responder struct {
	// Keys returns the trusted request-token keys (the trusted-keys file).
	Keys     func() map[string]ed25519.PublicKey
	Replay   *replay.Set
	Handlers map[string]Handler // by op (emrops.Op.Op)
	Log      *slog.Logger
	Metrics  *Metrics
	Now      func() time.Time // nil is time.Now
}

// Metrics counts outcomes by reason code ("ok" for an answer).
type Metrics struct {
	mu sync.Mutex
	n  map[string]int64
}

func (m *Metrics) add(reason string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.n == nil {
		m.n = map[string]int64{}
	}
	m.n[reason]++
}

// Count is the number of outcomes with reason.
func (m *Metrics) Count(reason string) int64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n[reason]
}

func (r *Responder) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (r *Responder) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// refuse records a refusal. rid is "" until the token has verified: before
// that it is the caller's word, not ours.
func (r *Responder) refuse(reason, rid string) (*nats.Msg, string) {
	r.Metrics.add(reason)
	args := []any{"reason", reason}
	if rid != "" {
		args = append(args, "rid", rid)
	}
	r.log().Warn("emr ctl: refused", args...)
	return nil, reason
}

// Process checks m and, if every check passes, runs its op's handler. It
// returns the reply to send (nil: send nothing) and the reason code ("ok"
// for a reply).
func (r *Responder) Process(ctx context.Context, m *nats.Msg) (*nats.Msg, string) {
	// 1. Size: the body before anything is parsed.
	if len(m.Data) > reqtoken.MaxReqBody {
		return r.refuse("size", "")
	}
	tok := ""
	if m.Header != nil {
		tok = m.Header.Get(HeaderToken)
	}
	if tok == "" {
		return r.refuse("malformed", "")
	}
	var keys map[string]ed25519.PublicKey
	if r.Keys != nil {
		keys = r.Keys()
	}
	// 2-4. Signature, claims, expiry, binding to subject, kind and body.
	c, err := reqtoken.VerifyAt(tok, keys, reqtoken.Expect{Aud: Audience, Subject: m.Subject, Kind: reqtoken.KindReq},
		m.Data, r.now())
	if err != nil {
		return r.refuse(reqtoken.Reason(err), "")
	}
	// 5. The reply subject: core's inbox, the token's rpl.
	if err := reqtoken.CheckReply(c, m.Reply, false); err != nil {
		return r.refuse(reqtoken.Reason(err), c.Rid)
	}
	// 6. Replay, after verification (an unverified jti could fill the set).
	if r.Replay == nil {
		return r.refuse("replay_full", c.Rid)
	}
	switch seen, err := r.Replay.SeenOrAddAt(c.Jti, c.ExpiresAt, r.now()); {
	case err != nil:
		return r.refuse("replay_full", c.Rid)
	case seen:
		return r.refuse("replay", c.Rid)
	}
	// 7. The op and who may call it.
	op, ok := emrops.Lookup(m.Subject)
	if !ok {
		return r.refuse("unknown_op", c.Rid)
	}
	if !op.Allows(c.Sub, c.Roles) {
		return r.refuse("caller", c.Rid)
	}
	// 7b. Step-up: core's signed word (stp) that it checked a fresh code
	// for this request. The emr service can't check the code itself.
	if op.StepUp && !c.StepUp {
		return r.refuse("step_up", c.Rid)
	}
	// 8. The request id header is the token's.
	if m.Header.Get(HeaderRequestID) != c.Rid {
		return r.refuse("request_id", c.Rid)
	}
	// 9. The schema, strictly.
	if m.Header.Get(HeaderSchema) != op.Request {
		return r.refuse("schema", c.Rid)
	}
	req, err := decode(op.Request, m.Data)
	if err != nil {
		return r.refuse("schema", c.Rid)
	}
	h, ok := r.Handlers[op.Op]
	if !ok || h == nil {
		return r.refuse("not_ready", c.Rid)
	}
	ctx, cancel := context.WithTimeout(ctx, op.Timeout)
	defer cancel()
	rep, err := h(ctx, c, req)
	if err != nil || rep == nil || string(rep.ProtoReflect().Descriptor().FullName()) != op.Response {
		return r.refuse("handler_error", c.Rid)
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(rep)
	if err != nil || len(b) > reqtoken.MaxReqBody {
		return r.refuse("handler_error", c.Rid)
	}
	out := nats.NewMsg(m.Reply)
	out.Data = b
	out.Header.Set(HeaderSchema, op.Response)
	out.Header.Set(HeaderRequestID, c.Rid)
	r.Metrics.add("ok")
	r.log().Info("emr ctl: answered", "op", op.Name, "rid", c.Rid)
	return out, "ok"
}

var errSchema = errors.New("ctl: body is not the op's request message")

// decode reads body as the message named full, refusing unknown fields.
func decode(full string, body []byte) (proto.Message, error) {
	mt, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(full))
	if err != nil {
		return nil, errSchema
	}
	msg := mt.New().Interface()
	if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(body, msg); err != nil {
		return nil, errSchema
	}
	if hasUnknown(msg.ProtoReflect()) {
		return nil, errSchema
	}
	return msg, nil
}

// hasUnknown reports whether m or any message inside it kept unknown
// fields.
func hasUnknown(m protoreflect.Message) bool {
	if len(m.GetUnknown()) > 0 {
		return true
	}
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Message() != nil:
			l := v.List()
			for i := 0; i < l.Len() && !found; i++ {
				found = hasUnknown(l.Get(i).Message())
			}
		case fd.IsMap():
		case fd.Message() != nil:
			found = hasUnknown(v.Message())
		}
		return !found
	})
	return found
}

// Serve subscribes the control subjects in queue group emr and answers
// until ctx ends or stop is called.
func (r *Responder) Serve(ctx context.Context, nc *nats.Conn) (stop func(), err error) {
	sub, err := nc.QueueSubscribe(Subjects, QueueGroup, func(m *nats.Msg) {
		out, _ := r.Process(ctx, m)
		if out != nil {
			if err := m.RespondMsg(out); err != nil {
				r.log().Error("emr ctl: reply not sent", "reason", "publish_failed")
			}
		}
	})
	if err != nil {
		return nil, errors.New("ctl: cannot subscribe the control subjects")
	}
	var once sync.Once
	stop = func() { once.Do(func() { _ = sub.Drain() }) }
	go func() {
		<-ctx.Done()
		stop()
	}()
	return stop, nil
}
