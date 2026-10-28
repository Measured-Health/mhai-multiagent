package intel

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
)

// jsStoreFailed is JetStream's "store failed" API error (10077): what a
// publish gets when DiscardNew refuses it (maximum messages per subject,
// messages or bytes exceeded).
const jsStoreFailed jetstream.ErrorCode = 10077

// PublishJob publishes the job operation opName for caller: msg (the
// operation's request message) on jobs.v1.<caller's hospital>.<type>, or
// jobs.v1.global.<type> for an operation that needs no hospital and a
// caller with none. The subject's hospital is the token's hid: there is no
// other way to name a subject, so core only ever publishes on the caller's
// own hospital (the per-hospital limits then hold).
//
// jobID is the Nats-Msg-Id, so JetStream drops a second publish of the same
// id within its duplicate window: the job id on the first publish and on a
// sweep (Republish). The job token's exp is deadline (whole seconds), at
// least a second away and no later than the operation's limit.
//
// Errors: ErrNotAllowed, ErrMisuse, ErrNoHospital, ErrDeadline (nothing
// published); ErrBackpressure (JOBS refused it), ErrNoResponders,
// ErrTimeout, ErrPublish. Map any of them with Code.
func (c *Client) PublishJob(ctx context.Context, caller Caller, opName string, jobID uuid.UUID, msg proto.Message, deadline time.Time) error {
	op, err := operation(opName, reqtoken.KindJob, msg, nil)
	if err != nil {
		return err
	}
	if err := checkCaller(op, caller); err != nil {
		return err
	}
	if jobID == uuid.Nil || c.JS == nil {
		return ErrMisuse
	}
	var subject string
	switch {
	case caller.Hid != "":
		subject = "jobs.v1." + caller.Hid + "." + op.Op
	case op.NeedsHospital:
		return ErrNoHospital
	default:
		subject = "jobs.v1.global." + op.Op
	}
	// One clock reading for the check and the token, so exp is the
	// deadline (to the second).
	now := time.Now()
	if c.Signer.Now != nil {
		now = c.Signer.Now()
	}
	if deadline.Sub(now) < time.Second {
		return ErrDeadline
	}
	ttl := deadline.Sub(time.Unix(now.Unix(), 0))
	if ttl > op.Timeout+time.Second {
		return ErrMisuse
	}
	signer := c.Signer
	signer.Now = func() time.Time { return now }
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		return ErrMisuse
	}
	tok, _, err := signer.Mint(reqtoken.Claims{
		Aud: op.Service, Sub: caller.Sub, Hid: caller.Hid, Roles: slices.Clone(caller.Roles), Rid: caller.Rid,
		Kind: reqtoken.KindJob, Subject: subject, Via: reqtoken.ViaCore,
	}, body, ttl)
	if err != nil {
		c.log().Error("intel: refused to mint", "operation", op.Name, "request_id", caller.Rid, "error", err)
		return ErrMisuse
	}
	m := nats.NewMsg(subject)
	m.Header.Set(hdrToken, tok)
	m.Header.Set(hdrSchema, op.RequestSchema)
	m.Header.Set(hdrRequestID, caller.Rid)
	m.Data = body
	_, err = c.JS.PublishMsg(ctx, m, jetstream.WithMsgID(jobID.String()), jetstream.WithExpectStream(bus.JobsStream))
	if err == nil {
		return nil
	}
	out := classifyPublish(err)
	attrs := []any{"operation", op.Name, "job_id", jobID.String(), "request_id", caller.Rid, "hospital_id", caller.Hid, "error", out}
	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) {
		attrs = append(attrs, "js_err_code", int(apiErr.ErrorCode))
	}
	c.log().Warn("intel: job not published", attrs...)
	return out
}

// classifyPublish maps a JetStream publish error to one of ours. The
// driver's error is never passed on.
func classifyPublish(err error) error {
	var apiErr *jetstream.APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.ErrorCode == jsStoreFailed:
		return ErrBackpressure
	case errors.Is(err, jetstream.ErrNoStreamResponse), errors.Is(err, nats.ErrNoResponders):
		return ErrNoResponders
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, nats.ErrTimeout):
		return ErrTimeout
	}
	return ErrPublish
}
