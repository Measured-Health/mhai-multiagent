// Package intel is core's side of the intelligence boundary (spec §6-§10):
// the request/reply client, the job publisher, the publish sweeper, the
// per-hospital in-flight cap, and the audit of what workers report.
//
// Every call carries a request token core mints here (internal/reqtoken).
// Its sub, hid and roles come only from the Caller, which comes from the
// verified principal (FromPrincipal) or is core itself (System); nothing in
// a body, subject or header sets them. Core checks the operation's roles
// before it mints, so a refused caller never reaches NATS; the worker
// repeats the check (spec §7.3 check 9). A request/reply call is never
// retried.
package intel

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
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
)

// The envelope headers (spec §6.2).
const (
	hdrToken     = "Mhai-Request-Token"
	hdrSchema    = "Mhai-Schema"
	hdrRequestID = "Mhai-Request-Id"
	hdrStatus    = "Mhai-Status"
	errorSchema  = "mhai.intelligence.v1.Error"
	metaSchema   = "mhai.intelligence.v1.ResponseMeta"
	statusOK     = "ok"
	maxRIDLen    = 128
)

// Caller is who a call acts for. Sub is the user id, or reqtoken.SvcCore
// for work no user started; Hid the hospital (every token carries one but
// a global job's, F10); Rid the request id (X-Request-ID, the audit chain);
// Roles the roles to mint (none for svc:core).
type Caller struct {
	Sub, Hid, Rid string
	Roles         []string
}

// FromPrincipal is the Caller for an authenticated request: the
// principal's user, hospital and current roles, and the request id.
func FromPrincipal(p authz.Principal, rid string) Caller {
	c := Caller{Sub: p.UserID.String(), Rid: rid, Roles: slices.Clone(p.Roles)}
	if p.HospitalID != uuid.Nil {
		c.Hid = p.HospitalID.String()
	}
	return c
}

// System is the Caller for core's own work (svc:core, no roles). hid is
// the hospital the work is for; only a global job may leave it empty.
func System(hid, rid string) Caller {
	return Caller{Sub: reqtoken.SvcCore, Hid: hid, Rid: rid}
}

// Client calls the intelligence workers. NC must be core's connection,
// whose inbox prefix is _INBOX_gateway (bus.Connect with
// bus.GatewayInboxPrefix): the only inbox a worker may answer (spec §7.3
// check 5, enforced by NATS for the gateway user).
type Client struct {
	NC     *nats.Conn
	JS     jetstream.JetStream
	Signer reqtoken.Signer
	Log    *slog.Logger
	// Trusted is the workers' trusted-keys file as core reads it
	// (REQUEST_TRUSTED_KEYS_PATH): the dead-letter consumer re-mints a job
	// refused for a kid no longer in it. Nil: no re-mint.
	Trusted *TrustedKeys
	// Metrics counts the background loops' outcomes; nil counts nowhere.
	Metrics *Metrics
}

func (c *Client) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// canonicalUUID is the lower-case, non-nil uuid form used in subjects.
func canonicalUUID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u != uuid.Nil && u.String() == s
}

// validRID is a request id core can put in a header and the audit chain.
func validRID(s string) bool {
	if s == "" || len(s) > maxRIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

// operation looks opName up and checks its kind and message types.
func operation(opName string, kind reqtoken.Kind, req, resp proto.Message) (authz.Operation, error) {
	op, ok := authz.OperationByName(opName)
	if !ok || op.Kind != kind || req == nil || string(proto.MessageName(req)) != op.RequestSchema {
		return authz.Operation{}, ErrMisuse
	}
	if kind == reqtoken.KindReq && (resp == nil || string(proto.MessageName(resp)) != op.ResponseSchema) {
		return authz.Operation{}, ErrMisuse
	}
	return op, nil
}

// checkCaller is core's own check of the caller against the operation
// (the worker repeats it): the identity's shape, then the roles. svc:core
// needs the operation to allow it and holds no roles; a user needs one of
// the operation's roles.
func checkCaller(op authz.Operation, c Caller) error {
	if !validRID(c.Rid) || (c.Hid != "" && !canonicalUUID(c.Hid)) {
		return ErrMisuse
	}
	if c.Sub == reqtoken.SvcCore {
		if !op.SvcCore || len(c.Roles) > 0 {
			return ErrNotAllowed
		}
		return nil
	}
	if !canonicalUUID(c.Sub) {
		return ErrMisuse
	}
	for _, r := range c.Roles {
		if slices.Contains(op.AnyRole, r) {
			return nil
		}
	}
	return ErrNotAllowed
}

// Request calls the request/reply operation opName for caller with req, and
// decodes the answer into resp. It returns the answer's ResponseMeta (nil
// when it has none) for AppendFacts.
//
// The token lives for the operation's timeout, or until ctx's deadline if
// that is sooner (under a second left: ErrTimeout, nothing sent), and is
// bound to the subject, the body, the request id and core's reply inbox.
// Errors: ErrNotAllowed, ErrMisuse, ErrNoHospital (nothing sent);
// ErrTimeout, ErrNoResponders (no answer); *Error (the worker's code);
// ErrBadReply. Nothing is retried. Map any of them with Code.
func (c *Client) Request(ctx context.Context, caller Caller, opName string, req, resp proto.Message) (*intelpb.ResponseMeta, error) {
	op, err := operation(opName, reqtoken.KindReq, req, resp)
	if err != nil {
		return nil, err
	}
	if err := checkCaller(op, caller); err != nil {
		return nil, err
	}
	// F10: a request token always carries a hospital. svc:core calls name
	// the hospital they are for; a hospital-less call would need a spec
	// change first.
	if caller.Hid == "" {
		return nil, ErrNoHospital
	}
	if c.NC == nil || !strings.HasPrefix(c.NC.NewRespInbox(), reqtoken.ReplyCore) {
		return nil, ErrMisuse
	}
	ttl := op.Timeout
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl); left < ttl {
			ttl = left
		}
	}
	if ttl < time.Second {
		return nil, ErrTimeout
	}
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		return nil, ErrMisuse
	}
	subject := "inference.v1." + op.Service + "." + op.Op
	tok, claims, err := c.Signer.Mint(reqtoken.Claims{
		Aud: op.Service, Sub: caller.Sub, Hid: caller.Hid, Roles: slices.Clone(caller.Roles), Rid: caller.Rid,
		Kind: reqtoken.KindReq, Subject: subject, ReplyPrefix: reqtoken.ReplyCore, Via: reqtoken.ViaCore,
	}, body, ttl)
	if err != nil {
		// Mint's errors are fixed values with no claim in them.
		c.log().Error("intel: refused to mint", "operation", op.Name, "request_id", caller.Rid, "error", err)
		return nil, ErrMisuse
	}
	m := nats.NewMsg(subject)
	m.Header.Set(hdrToken, tok)
	m.Header.Set(hdrSchema, op.RequestSchema)
	m.Header.Set(hdrRequestID, caller.Rid)
	m.Data = body
	rctx, cancel := context.WithTimeout(ctx, ttl)
	defer cancel()
	rep, err := c.NC.RequestMsgWithContext(rctx, m)
	if err != nil {
		out := ErrTimeout
		if errors.Is(err, nats.ErrNoResponders) {
			out = ErrNoResponders
		}
		c.log().Warn("intel: no answer", "operation", op.Name, "request_id", caller.Rid, "hospital_id", caller.Hid,
			"error", out)
		return nil, out
	}
	return c.decode(op, caller, claims, rep, resp)
}

// decode checks and reads a reply: the request id must be ours; ok needs
// the operation's response schema and a body that decodes; anything else
// needs an Error body whose code is the status and in the closed set.
func (c *Client) decode(op authz.Operation, caller Caller, claims reqtoken.Claims, rep *nats.Msg, resp proto.Message) (*intelpb.ResponseMeta, error) {
	bad := func(why string) (*intelpb.ResponseMeta, error) {
		c.log().Error("intel: malformed reply", "operation", op.Name, "request_id", caller.Rid, "why", why)
		return nil, ErrBadReply
	}
	status, schema := rep.Header.Get(hdrStatus), rep.Header.Get(hdrSchema)
	if rep.Header.Get(hdrRequestID) != caller.Rid {
		return bad("request id")
	}
	if status == statusOK {
		if schema != op.ResponseSchema {
			return bad("schema")
		}
		if err := proto.Unmarshal(rep.Data, resp); err != nil {
			return bad("body")
		}
		return metaOf(resp), nil
	}
	var e intelpb.Error
	if schema != errorSchema || proto.Unmarshal(rep.Data, &e) != nil {
		return bad("error body")
	}
	if _, ok := workerCodes[e.Code]; !ok || e.Code != status {
		return bad("error code")
	}
	we := &Error{Code: e.Code, Retryable: e.Retryable}
	attrs := []any{"operation", op.Name, "error_code", e.Code, "request_id", caller.Rid, "hospital_id", caller.Hid,
		"kid", claims.KeyID, "jti", claims.Jti}
	if refusalCodes[e.Code] {
		// A refusal is a bug or an attack (spec §10.1): loud, ids only.
		c.log().Error("intel: worker refused the call", attrs...)
	} else {
		c.log().Warn("intel: worker could not answer", attrs...)
	}
	return nil, we
}

// metaOf returns a response's ResponseMeta meta field, or nil.
func metaOf(resp proto.Message) *intelpb.ResponseMeta {
	r := resp.ProtoReflect()
	fd := r.Descriptor().Fields().ByName("meta")
	if fd == nil || fd.Message() == nil || fd.Message().FullName() != metaSchema || !r.Has(fd) {
		return nil
	}
	m, _ := r.Get(fd).Message().Interface().(*intelpb.ResponseMeta)
	return m
}
