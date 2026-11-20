// Package notifyctl is core's side of the notifications service's control
// operations (notifications design §4.4, §7; §15 PR 5): the request/reply
// client for notify.v1.ctl.<op>, and the delivery.Sender that hands
// invites and reset links to the service.
//
// Every call carries a request token core mints here: aud notifications,
// sub svc:core, the hospital the message is for, the op's subject, reply
// prefix _INBOX_gateway. and the body's hash. A secret link travels only
// in that request's body, by NATS request/reply (never JetStream), and is
// never logged; the service holds it in memory only. A refused request is
// never answered, so it reads as a timeout here. Nothing is retried here:
// the service retries the send itself, and a failure answers the admin's
// action with 503 no_delivery.
package notifyctl

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/delivery"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/notifyops"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/notifypb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// The envelope headers (the notifications responder's).
const (
	HeaderToken     = "Mhai-Request-Token"
	HeaderSchema    = "Mhai-Schema"
	HeaderRequestID = "Mhai-Request-Id"
	audience        = "notifications"
)

// The closed errors.
var (
	ErrMisuse       = errors.New("notifyctl: misuse")
	ErrTimeout      = errors.New("notifyctl: no answer")
	ErrNoResponders = errors.New("notifyctl: no notifications service")
	ErrBadReply     = errors.New("notifyctl: malformed reply")
	// ErrNotSent: the service answered that the message was not sent.
	ErrNotSent = errors.New("notifyctl: not sent")
)

// Client calls the notifications service. NC must be core's connection,
// whose inbox prefix is _INBOX_gateway.
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

// Request calls op as svc:core for hospital with req and decodes the
// answer into resp.
func (c *Client) Request(ctx context.Context, hospital uuid.UUID, rid, opName string, req, resp proto.Message) error {
	op, ok := notifyops.Lookup(notifyops.Subject(opName))
	if !ok || req == nil || resp == nil || string(proto.MessageName(req)) != op.Request ||
		string(proto.MessageName(resp)) != op.Response || hospital == uuid.Nil || rid == "" || len(rid) > 128 {
		return ErrMisuse
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
	subject := notifyops.Subject(op.Op)
	tok, _, err := c.Signer.Mint(reqtoken.Claims{Aud: audience, Sub: reqtoken.SvcCore, Hid: hospital.String(), Rid: rid,
		Kind: reqtoken.KindReq, Subject: subject, ReplyPrefix: reqtoken.ReplyCore, Via: reqtoken.ViaCore}, body, ttl)
	if err != nil {
		c.log().Error("notifyctl: refused to mint", "op", op.Name, "request_id", rid)
		return ErrMisuse
	}
	m := nats.NewMsg(subject)
	m.Header.Set(HeaderToken, tok)
	m.Header.Set(HeaderSchema, op.Request)
	m.Header.Set(HeaderRequestID, rid)
	m.Data = body
	rctx, cancel := context.WithTimeout(ctx, ttl)
	defer cancel()
	rep, err := c.NC.RequestMsgWithContext(rctx, m)
	if err != nil {
		out := ErrTimeout
		if errors.Is(err, nats.ErrNoResponders) {
			out = ErrNoResponders
		}
		c.log().Warn("notifyctl: no answer", "op", op.Name, "request_id", rid, "hospital_id", hospital.String(), "reason", out.Error())
		return out
	}
	if rep.Header.Get(HeaderRequestID) != rid || rep.Header.Get(HeaderSchema) != op.Response {
		return ErrBadReply
	}
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(rep.Data, resp); err != nil {
		return ErrBadReply
	}
	return nil
}

// Sender is the delivery.Sender for CORE_DELIVERY=notify: an invite or a
// reset link goes to the service now, by send_link, with its link in
// memory; every other kind is delivered from the outbox by the drainer
// (its row is the hand-over), so Send has nothing to do for it.
type Sender struct {
	Client *Client
	Log    *slog.Logger
}

// Channel is "email": the outbox rows' channel.
func (Sender) Channel() string { return "email" }

// Send hands an invite or a reset to the service and waits for its
// answer: nil only when the service accepted it.
func (s Sender) Send(ctx context.Context, m delivery.Message) error {
	if m.Kind != delivery.Invite && m.Kind != delivery.PasswordReset {
		return nil
	}
	if s.Client == nil || m.Link == "" || m.Recipient == uuid.Nil {
		return ErrMisuse
	}
	rid := httpx.RequestIDFrom(ctx)
	if rid == "" {
		rid = uuid.NewString()
	}
	var rep notifypb.SendLinkReply
	err := s.Client.Request(ctx, m.HospitalID, rid, "send_link", &notifypb.SendLinkRequest{UserId: m.Recipient.String(),
		Purpose: string(m.Kind), Link: m.Link, ExpiresAt: m.ExpiresAt.Unix()}, &rep)
	if err != nil {
		return err
	}
	if rep.GetOutcome() != "accepted" {
		lg := s.Log
		if lg == nil {
			lg = slog.New(slog.NewTextHandler(io.Discard, nil))
		}
		lg.Warn("notifyctl: link not sent", "kind", string(m.Kind), "hospital_id", m.HospitalID.String(),
			"recipient_id", m.Recipient.String(), "request_id", rid, "outcome", closed(rep.GetOutcome()))
		return ErrNotSent
	}
	return nil
}

// closed is an outcome code safe to log, or "other".
func closed(s string) string {
	switch s {
	case "failed", "no_recipient", "not_ready", "rate_limited", "invalid":
		return s
	}
	return "other"
}
