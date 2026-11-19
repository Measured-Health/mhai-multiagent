// Package docsctl is core's side of the documents service's control
// operations, documents.v1.ctl.<op> (documents design §8; ADR 0016; §13 PR
// 5): presign_upload for a browser upload's POST policy, page_image for a
// stamped page.
//
// Every call is core's own (sub svc:core, no roles) for one hospital: the
// token carries aud documents, the op's subject, reply prefix
// _INBOX_gateway., the body's hash and the hospital as hid, which the
// documents service reads rows under (HospitalBound ops). Who may call
// each op is gokit/docops, checked here before anything is sent and again
// by the responder. The responder answers nothing to a request it refuses,
// so a refusal reads as a timeout here. Nothing is retried. Errors and logs
// carry the op, codes and the request id, never a body: a POST policy, a
// page or a viewer's name never reaches a log.
package docsctl

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

	"github.com/Measured-Health/mhai-multiagent/services/gokit/docops"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// The envelope headers (the documents responder's).
const (
	HeaderToken     = "Mhai-Request-Token"
	HeaderSchema    = "Mhai-Schema"
	HeaderRequestID = "Mhai-Request-Id"
	audience        = "documents"
	maxRIDLen       = 128
)

// The closed errors.
var (
	// ErrMisuse: an unknown op, the wrong message types, a bad hospital or
	// request id, or a connection without core's inbox. Nothing was sent.
	ErrMisuse = errors.New("docsctl: misuse")
	// ErrTimeout: no answer in time (a refused request is never answered).
	ErrTimeout = errors.New("docsctl: no answer")
	// ErrNoResponders: no documents service is subscribed.
	ErrNoResponders = errors.New("docsctl: no documents service")
	// ErrBadReply: an answer that isn't the op's reply, or larger than the
	// op allows.
	ErrBadReply = errors.New("docsctl: malformed reply")
)

// Requester is the documents service's control client (Client; a fake in
// tests).
type Requester interface {
	Request(ctx context.Context, hospital uuid.UUID, rid, opName string, req, resp proto.Message) error
}

// Client calls the documents service. NC must be core's connection, whose
// inbox prefix is _INBOX_gateway.
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

// ValidRID reports whether s may be a request id: 1 to 128 printable
// ASCII characters without spaces.
func ValidRID(s string) bool {
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
// message types, the hospital and request id, and the op's caller rule
// (svc:core).
func check(opName string, hospital uuid.UUID, rid string, req, resp proto.Message) (docops.Op, error) {
	op, ok := docops.Lookup(docops.Subject(opName))
	if !ok || req == nil || resp == nil || string(proto.MessageName(req)) != op.Request ||
		string(proto.MessageName(resp)) != op.Response {
		return docops.Op{}, ErrMisuse
	}
	if hospital == uuid.Nil || !ValidRID(rid) || !op.Allows(reqtoken.SvcCore, nil) {
		return docops.Op{}, ErrMisuse
	}
	return op, nil
}

// Request calls op for hospital with req and decodes the answer into resp.
// The token lives for the op's timeout or until ctx's deadline if sooner
// (under a second left: ErrTimeout, nothing sent).
func (c *Client) Request(ctx context.Context, hospital uuid.UUID, rid, opName string, req, resp proto.Message) error {
	op, err := check(opName, hospital, rid, req, resp)
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
	subject := docops.Subject(op.Op)
	tok, _, err := c.Signer.Mint(reqtoken.Claims{
		Aud: audience, Sub: reqtoken.SvcCore, Hid: hospital.String(), Rid: rid,
		Kind: reqtoken.KindReq, Subject: subject, ReplyPrefix: reqtoken.ReplyCore, Via: reqtoken.ViaCore,
	}, body, ttl)
	if err != nil {
		c.log().Error("docsctl: refused to mint", "op", op.Name, "request_id", rid)
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
		c.log().Warn("docsctl: no answer", "op", op.Name, "request_id", rid, "hospital_id", hospital.String(),
			"reason", reasonOf(out))
		return out
	}
	limit := reqtoken.MaxReqBody
	if op.MaxReply > 0 {
		limit = op.MaxReply
	}
	if rep.Header.Get(HeaderRequestID) != rid || rep.Header.Get(HeaderSchema) != op.Response || len(rep.Data) > limit {
		c.log().Error("docsctl: malformed reply", "op", op.Name, "request_id", rid, "why", "headers")
		return ErrBadReply
	}
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(rep.Data, resp); err != nil {
		c.log().Error("docsctl: malformed reply", "op", op.Name, "request_id", rid, "why", "body")
		return ErrBadReply
	}
	return nil
}

func reasonOf(err error) string {
	if errors.Is(err, ErrNoResponders) {
		return "no_responders"
	}
	return "timeout"
}
