// Package emrctl is core's side of the emr service's control operations
// (EMR design §3.2, §6.2; plan Tasks 5, 11): the request/reply client for
// emr.v1.ctl.<op>, and the sealer that seals a credential to the emr
// service's public key at core's edge.
//
// Every call carries a request token core mints here: aud emr, the op's
// subject, reply prefix _INBOX_gateway., the body's hash, and stp when the
// caller's step-up code was checked in this request. Who may call each op
// is gokit/emrops, checked here before anything is sent and again by the
// emr responder. The responder answers nothing to a request it refuses, so
// a refusal reads as a timeout here. Nothing is retried. Errors and logs
// carry the op, codes and the request id, never a body.
package emrctl

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrops"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// The envelope headers (the emr responder's).
const (
	HeaderToken     = "Mhai-Request-Token"
	HeaderSchema    = "Mhai-Schema"
	HeaderRequestID = "Mhai-Request-Id"
	audience        = "emr"
	maxRIDLen       = 128
)

// The closed errors.
var (
	// ErrNotAllowed: the caller may not call the op, or a step-up op without
	// a checked step-up code. Nothing was sent.
	ErrNotAllowed = errors.New("emrctl: caller not allowed for the op")
	// ErrMisuse: an unknown op, the wrong message types, a bad hospital or
	// request id, or a connection without core's inbox. Nothing was sent.
	ErrMisuse = errors.New("emrctl: misuse")
	// ErrTimeout: no answer in time (a refused request is never answered).
	ErrTimeout = errors.New("emrctl: no answer")
	// ErrNoResponders: no emr service is subscribed.
	ErrNoResponders = errors.New("emrctl: no emr service")
	// ErrBadReply: an answer that isn't the op's reply.
	ErrBadReply = errors.New("emrctl: malformed reply")
)

// Caller is who a call acts for: the platform admin (Sub, Roles, StepUp)
// with Hid the connection's hospital, or svc:core.
type Caller struct {
	Sub, Hid, Rid string
	Roles         []string
	// StepUp: the handler checked a fresh step-up code in this request.
	StepUp bool
}

// FromPrincipal is the Caller for a platform admin's request about a
// connection of hospital hid.
func FromPrincipal(p authz.Principal, hid uuid.UUID, rid string, stepUp bool) Caller {
	return Caller{Sub: p.UserID.String(), Hid: hid.String(), Rid: rid, Roles: slices.Clone(p.Roles), StepUp: stepUp}
}

// System is the Caller for core's own calls (health, sync_now).
func System(hid, rid string) Caller {
	return Caller{Sub: reqtoken.SvcCore, Hid: hid, Rid: rid}
}

// Client calls the emr service. NC must be core's connection, whose inbox
// prefix is _INBOX_gateway.
type Client struct {
	NC     *nats.Conn
	Signer reqtoken.Signer
	Log    *slog.Logger
}

func (c *Client) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func canonicalUUID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u != uuid.Nil && u.String() == s
}

func validRID(s string) bool {
	if s == "" || len(s) > maxRIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if b := s[i]; b <= ' ' || b > '~' {
			return false
		}
	}
	return true
}

// check is core's own check before anything is sent: the op and its
// message types, the caller's shape and the op's caller rule (emrops), and
// the checked step-up code for a step-up op.
func check(opName string, caller Caller, req, resp proto.Message) (emrops.Op, error) {
	op, ok := emrops.Lookup(emrops.Subject(opName))
	if !ok || req == nil || resp == nil || string(proto.MessageName(req)) != op.Request ||
		string(proto.MessageName(resp)) != op.Response {
		return emrops.Op{}, ErrMisuse
	}
	if !validRID(caller.Rid) || !canonicalUUID(caller.Hid) {
		return emrops.Op{}, ErrMisuse
	}
	if !op.Allows(caller.Sub, caller.Roles) {
		return emrops.Op{}, ErrNotAllowed
	}
	if op.StepUp && !caller.StepUp {
		return emrops.Op{}, ErrNotAllowed
	}
	if caller.StepUp && (!op.StepUp || caller.Sub == reqtoken.SvcCore) {
		return emrops.Op{}, ErrMisuse // stp only where the op asks for it
	}
	return op, nil
}

// Request calls op for caller with req and decodes the answer into resp.
// The token lives for the op's timeout or until ctx's deadline if sooner
// (under a second left: ErrTimeout, nothing sent).
func (c *Client) Request(ctx context.Context, caller Caller, opName string, req, resp proto.Message) error {
	op, err := check(opName, caller, req, resp)
	if err != nil {
		return err
	}
	if c.NC == nil || !strings.HasPrefix(c.NC.NewRespInbox(), reqtoken.ReplyCore) {
		return ErrMisuse
	}
	ttl := op.Timeout
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl); left < ttl {
			ttl = left
		}
	}
	if ttl < time.Second {
		return ErrTimeout
	}
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		return ErrMisuse
	}
	subject := emrops.Subject(op.Op)
	tok, _, err := c.Signer.Mint(reqtoken.Claims{
		Aud: audience, Sub: caller.Sub, Hid: caller.Hid, Roles: slices.Clone(caller.Roles), Rid: caller.Rid,
		Kind: reqtoken.KindReq, Subject: subject, ReplyPrefix: reqtoken.ReplyCore, Via: reqtoken.ViaCore,
		StepUp: caller.StepUp,
	}, body, ttl)
	if err != nil {
		c.log().Error("emrctl: refused to mint", "op", op.Name, "request_id", caller.Rid, "error", err)
		return ErrMisuse
	}
	m := nats.NewMsg(subject)
	m.Header.Set(HeaderToken, tok)
	m.Header.Set(HeaderSchema, op.Request)
	m.Header.Set(HeaderRequestID, caller.Rid)
	m.Data = body
	rctx, cancel := context.WithTimeout(ctx, ttl)
	defer cancel()
	rep, err := c.NC.RequestMsgWithContext(rctx, m)
	if err != nil {
		out := ErrTimeout
		if errors.Is(err, nats.ErrNoResponders) {
			out = ErrNoResponders
		}
		c.log().Warn("emrctl: no answer", "op", op.Name, "request_id", caller.Rid, "hospital_id", caller.Hid, "error", out)
		return out
	}
	if rep.Header.Get(HeaderRequestID) != caller.Rid || rep.Header.Get(HeaderSchema) != op.Response {
		c.log().Error("emrctl: malformed reply", "op", op.Name, "request_id", caller.Rid, "why", "headers")
		return ErrBadReply
	}
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(rep.Data, resp); err != nil {
		c.log().Error("emrctl: malformed reply", "op", op.Name, "request_id", caller.Rid, "why", "body")
		return ErrBadReply
	}
	return nil
}
