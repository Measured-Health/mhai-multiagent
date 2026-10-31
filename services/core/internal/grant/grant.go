// Package grant is core's grant endpoint (spec §17.2, §17.3).
//
// When the clinical BFF calls a bff_direct intelligence operation itself,
// the worker holds the user's access token, not a request token. Before any
// work it asks core, on authz.v1.grant.<service> (which only that service's
// NATS user may publish), for a request token bound to that exact call.
// Core runs, in this order:
//
//  1. the reply subject is the asking service's own inbox
//     (_INBOX_worker.<service>.<literal tokens>), else no reply at all;
//  2. the in-flight bound (past it, CodeOverloaded at once);
//  3. the access token's Ed25519 signature and claims, with no I/O
//     (authz.Guard.VerifyAccessToken);
//  4. per-session and per-hospital rates, keyed on the verified sid and hid
//     (F8; past them, rate_limited);
//  5. the denylist, the session, account and hospital state and two-step
//     (authz.Guard.Principal, the principal middleware's own code);
//  6. the operation: it exists, is bff_direct, belongs to the subject's
//     service, and allows one of the principal's roles;
//  7. the call: subject inference.v1.<service>.<op>, reply prefix
//     _INBOX_bff.clinical., a well-formed body hash and a deadline;
//  8. patient ids: none exist in this plan (departure 7, F9), so any is
//     not_found;
//  9. the BFF's request id, not seen by this copy in the last 2 minutes
//     (departure 15);
//  10. mint: aud the service, nsub the subject, bh the body hash, rpl the
//     BFF prefix, via grant, rid the BFF's request id, and exp the smaller
//     of the operation's timeout and the BFF's deadline.
//
// The reply carries Mhai-Status ("ok", or the refusal code) and Mhai-Schema
// (GrantResponse or GrantRefused). Refusal codes are core's own problem
// codes (Codes); the worker answers the BFF grant_refused:<code>.
//
// Audit (F7): intelligence.granted, and intelligence.grant_refused once the
// principal is known, go through authz.Tx; token and session refusals go
// through db.WithAuth (mhai_auth), as sign-in failures do, bounded per
// service; rate_limited, overloaded and dropped requests are metrics only.
// Each row has a request id core makes; the BFF's request id is only the
// replay key and the token's rid. Nothing here logs, audits or returns the
// access token or the minted token.
package grant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/telemetry"
)

// Subjects, the queue group and the envelope.
const (
	SubjectPrefix = "authz.v1.grant."
	Subject       = SubjectPrefix + ">"
	Queue         = "core"

	// BFFReplyPrefix is the only reply prefix a grant binds: the clinical
	// BFF's inbox. Only the clinical app calls intelligence directly.
	BFFReplyPrefix = "_INBOX_bff.clinical."

	HeaderStatus   = "Mhai-Status"
	HeaderSchema   = "Mhai-Schema"
	StatusOK       = "ok"
	SchemaResponse = "mhai.intelligence.v1.GrantResponse"
	SchemaRefused  = "mhai.intelligence.v1.GrantRefused"

	// ReplayWindow is how long a BFF request id is remembered.
	ReplayWindow = 2 * time.Minute
	// MaxRequest bounds a GrantRequest's bytes (an access token is well
	// under 4 KB).
	MaxRequest = 16 << 10
	// MaxReply bounds the reply subject. nats.go's are about 60 bytes.
	MaxReply = 256
	// handleTimeout bounds one grant's work (denylist, database, audit).
	handleTimeout = 5 * time.Second
)

// CodeOverloaded is the code core answers when the in-flight bound is
// reached (spec §17.3's "overloaded"). The BFF shows a grant refusal as
// core's own code and status, so it is core's 503 code, unavailable:
// overloaded is not a core problem code.
const CodeOverloaded = "unavailable"

// codes is the closed set of grant refusal codes, each one of core's own
// problem codes (httpx.Codes; spec §10.1's grant_refused row, F21, F33).
var codes = []string{
	"unauthenticated", "token_expired", "mfa_required", "forbidden", "not_found", "rate_limited", "not_ready",
	CodeOverloaded,
}

// Codes returns the grant refusal codes. The caller gets a copy.
func Codes() []string { return slices.Clone(codes) }

// refusalCode is the code to send for code: itself when it is a grant code,
// else forbidden (fail closed: an unexpected code from the principal check,
// such as not_platform, never reaches the worker as-is).
func refusalCode(code string) string {
	if slices.Contains(codes, code) {
		return code
	}
	return "forbidden"
}

// Metrics counts what the endpoint did.
type Metrics struct {
	Granted      atomic.Int64
	Refused      atomic.Int64 // refusals other than the rate and in-flight bounds
	RateLimited  atomic.Int64
	Overloaded   atomic.Int64
	DroppedReply atomic.Int64 // no reply: the reply subject or the subject was wrong
	AuditSkipped atomic.Int64 // token refusals past the per-service audit rate
	AuditFailed  atomic.Int64
	InFlight     atomic.Int64
	// VerifiedRefused counts refusals of a token that verified (its audit
	// rows are uncapped, so the rate of this counter is how a flood of them
	// shows). RefusalFlood counts the times one session passed a multiple of
	// FloodEvery such refusals in a minute: a sampled, high-water signal.
	VerifiedRefused atomic.Int64
	RefusalFlood    atomic.Int64
}

// Server answers grant requests. Build one per copy of core.
type Server struct {
	Guard  *authz.Guard
	Signer reqtoken.Signer
	Pool   *pgxpool.Pool
	Log    *slog.Logger
	Limits Limits
	// Metrics, when set, is counted into; otherwise the server keeps its own.
	Metrics *Metrics
	// AllowTestOnly grants test-only operations (echo.ping). Only a
	// development core sets it; the default refuses them.
	AllowTestOnly bool
	// SummaryEvery is how often non-zero counts are logged as one
	// summary line, and the interval of the sampled per-request drop and
	// overload logs (default DefaultSummaryEvery). Task 14 exports the
	// metrics properly.
	SummaryEvery time.Duration

	lim                          Limits
	flood                        *floodCounter
	sem                          chan struct{}
	session, hospital, refusalsA *rateLimiter
	seen                         *replaySet
	dropLog, busyLog             *sampler

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Serve subscribes authz.v1.grant.> in queue group core. stop unsubscribes
// and waits for the grants in hand; it also runs when ctx ends. Call Serve
// once per Server.
func (s *Server) Serve(ctx context.Context, nc *nats.Conn) (stop func(), err error) {
	if s.Guard == nil || s.Pool == nil || s.Signer.KeyID == "" || len(s.Signer.PublicKeys()) == 0 {
		return nil, errNotConfigured
	}
	if s.Metrics == nil {
		s.Metrics = &Metrics{}
	}
	s.lim = s.Limits.withDefaults()
	s.sem = make(chan struct{}, s.lim.InFlight)
	s.session = newRateLimiter(s.lim.PerSession, maxLimiterKeys)
	s.hospital = newRateLimiter(s.lim.PerHospital, maxLimiterKeys)
	s.refusalsA = newRateLimiter(refusalAuditRate, maxLimiterKeys)
	s.seen = newReplaySet(ReplayWindow, maxReplayIDs)
	s.flood = newFloodCounter(time.Minute, maxLimiterKeys)
	if s.SummaryEvery <= 0 {
		s.SummaryEvery = DefaultSummaryEvery
	}
	s.dropLog, s.busyLog = newSampler(s.SummaryEvery), newSampler(s.SummaryEvery)
	sub, err := nc.QueueSubscribe(Subject, Queue, s.onMsg)
	if err != nil {
		return nil, err
	}
	if err := nc.Flush(); err != nil {
		_ = sub.Unsubscribe()
		return nil, err
	}
	var once sync.Once
	done := make(chan struct{})
	stop = func() {
		once.Do(func() {
			_ = sub.Unsubscribe()
			_ = nc.FlushTimeout(time.Second)
			s.mu.Lock()
			s.closed = true
			s.mu.Unlock()
			s.wg.Wait()
			close(done)
		})
	}
	go s.summarise(done)
	context.AfterFunc(ctx, stop)
	return stop, nil
}

type configError string

func (e configError) Error() string { return string(e) }

const errNotConfigured = configError("grant: server needs a guard, a pool and a signing key")

// serviceOf returns the service in authz.v1.grant.<service>: one subject
// name ([a-z0-9_]+). NATS lets only that service's user publish there.
func serviceOf(subject string) (string, bool) {
	svc, ok := strings.CutPrefix(subject, SubjectPrefix)
	if !ok || svc == "" {
		return "", false
	}
	for _, c := range svc {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return "", false
		}
	}
	return svc, true
}

// replyOK is the responder reply-subject check (Task 4 ruling): the reply
// must be the asking service's own inbox, _INBOX_worker.<service>. followed
// by literal tokens. Core may publish to JOBS, $KV and $JS, so answering
// anything else would let a compromised worker steer core's one reply.
func replyOK(service, reply string) bool {
	return bus.ReplyUnder(bus.WorkerInbox(service)+".", reply, MaxReply)
}

// validRequestID is the BFF's Mhai-Request-Id: 8-64 of [A-Za-z0-9_-].
func validRequestID(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// grantTTL is the token's lifetime: the smaller of the operation's timeout
// and the BFF's deadline. No deadline is refused. Mint signs whole seconds
// of at least one, so a deadline under a second gets one second.
func grantTTL(timeout time.Duration, deadlineMS uint32) (time.Duration, bool) {
	if timeout <= 0 || deadlineMS == 0 {
		return 0, false
	}
	ttl := min(timeout, time.Duration(deadlineMS)*time.Millisecond)
	return max(ttl, time.Second), true
}

// newRequestID is core's own request id for the audit rows, as
// httpx.RequestID makes one.
func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// onMsg is the subscription callback: the subject and reply-subject checks
// (else drop), the in-flight bound (else CodeOverloaded at once), then the
// grant on its own goroutine.
func (s *Server) onMsg(m *nats.Msg) {
	service, ok := serviceOf(m.Subject)
	if !ok || !replyOK(service, m.Reply) {
		s.Metrics.DroppedReply.Add(1)
		// The reply subject is attacker-chosen: never logged.
		if n, ok := s.dropLog.take(); ok {
			s.log().Error("grant: reply subject refused", "service", service, "count", n)
		}
		return
	}
	select {
	case s.sem <- struct{}{}:
	default:
		s.Metrics.Overloaded.Add(1)
		if n, ok := s.busyLog.take(); ok {
			s.log().Warn("grant: busy, refused", "service", service, "reason", CodeOverloaded, "count", n)
		}
		s.respond(m, service, refusedMsg(CodeOverloaded))
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.sem
		return // stopping: the worker times out and asks again
	}
	s.wg.Add(1)
	s.mu.Unlock()
	s.Metrics.InFlight.Add(1)
	go func() {
		replied := false
		defer func() {
			if p := recover(); p != nil {
				s.log().Error("grant: handler panicked", "service", service)
				if !replied { // the reply subject passed onMsg's check
					s.respond(m, service, refusedMsg("not_ready"))
				}
			}
			s.Metrics.InFlight.Add(-1)
			<-s.sem
			s.wg.Done()
		}()
		// The worker's trace context, if it sent a valid one (untrusted:
		// it only parents the span).
		ctx, cancel := context.WithTimeout(telemetry.Extract(context.Background(), m.Header), handleTimeout)
		defer cancel()
		out := s.handle(ctx, service, m.Data)
		replied = true
		s.respond(m, service, out)
	}()
}

func (s *Server) respond(m *nats.Msg, service string, out *nats.Msg) {
	out.Subject = m.Reply
	if err := m.RespondMsg(out); err != nil {
		s.log().Warn("grant: reply failed", "service", service)
	}
}

func refusedMsg(code string) *nats.Msg {
	out := nats.NewMsg("")
	out.Header.Set(HeaderStatus, code)
	out.Header.Set(HeaderSchema, SchemaRefused)
	out.Data, _ = proto.Marshal(&intelpb.GrantRefused{Code: code})
	return out
}

func grantedMsg(tok string) *nats.Msg {
	out := nats.NewMsg("")
	out.Header.Set(HeaderStatus, StatusOK)
	out.Header.Set(HeaderSchema, SchemaResponse)
	out.Data, _ = proto.Marshal(&intelpb.GrantResponse{RequestToken: tok})
	return out
}

// call is what handle knows about one grant, for its logs and audit rows:
// ids and codes only.
type call struct {
	service string
	op      string // the operation's name when it is in the table, else ""
	rid     string // core's request id
	tok     authz.AccessToken
}

// handle runs checks 3-10 and returns the reply, inside the span
// "grant <operation>" (the operation as the table names it, else
// "grant unknown": the name is never built from the request).
func (s *Server) handle(ctx context.Context, service string, data []byte) (out *nats.Msg) {
	c := call{service: service, rid: newRequestID()}
	ctx, span := telemetry.Start(ctx, trace.SpanKindServer, "grant unknown",
		attribute.String("service", service), attribute.String("request_id", c.rid))
	defer func() {
		status := "not_ready" // out is nil only when the handler panicked
		if out != nil {
			status = out.Header.Get(HeaderStatus)
		}
		telemetry.End(span, status)
	}()
	var req intelpb.GrantRequest
	if len(data) > MaxRequest || proto.Unmarshal(data, &req) != nil {
		return s.refuseEarly(ctx, c, "forbidden")
	}
	if op, ok := authz.OperationByName(req.Operation); ok {
		c.op = op.Name
		span.SetName("grant " + op.Name)
		span.SetAttributes(attribute.String("operation", op.Name))
	}
	// 3. Signature and claims, no I/O.
	tok, code := s.Guard.VerifyAccessToken(req.AccessToken)
	req.AccessToken = "" // not needed again; keep it out of anything below
	if code != "" {
		return s.refuseEarly(ctx, c, code)
	}
	c.tok = tok
	if hid := tok.HospitalID(); hid != uuid.Nil {
		span.SetAttributes(attribute.String("hospital_id", hid.String()))
	}
	// 4. Rates, on the verified sid and hid only (F8).
	now := s.lim.Now()
	// The session first, so one session spends at most its own rate of
	// its hospital's.
	if !s.session.allow(tok.SessionID().String(), now) || !s.hospital.allow(tok.HospitalID().String(), now) {
		s.Metrics.RateLimited.Add(1)
		return refusedMsg("rate_limited")
	}
	// 5. Denylist, account, hospital, two-step: the middleware's own path.
	p, code := s.Guard.Principal(ctx, tok, httpx.AudienceClinical, c.rid)
	if code != "" {
		return s.refuseEarly(ctx, c, code)
	}
	// 6. The operation.
	op, ok := authz.OperationByName(req.Operation)
	if !ok || !op.BFFDirect || op.Kind != reqtoken.KindReq || op.Service != service ||
		(op.TestOnly && !s.AllowTestOnly) || !slices.ContainsFunc(op.AnyRole, p.HasRole) {
		return s.refuse(ctx, c, p, "forbidden")
	}
	// 7. The call it binds.
	ttl, okTTL := grantTTL(op.Timeout, req.DeadlineMs)
	if req.Subject != "inference.v1."+op.Service+"."+op.Op || req.ReplyPrefix != BFFReplyPrefix ||
		!reqtoken.IsBodyHash(req.BodyHash) || !okTTL {
		return s.refuse(ctx, c, p, "forbidden")
	}
	// 8. Patient ids: none exist yet, so none belongs to the hospital.
	if len(req.PatientIds) > 0 {
		return s.refuse(ctx, c, p, "not_found")
	}
	// 9. Replay.
	if !validRequestID(req.RequestId) {
		return s.refuse(ctx, c, p, "forbidden")
	}
	switch s.seen.add(req.RequestId, now) {
	case replaySeen:
		return s.refuse(ctx, c, p, "forbidden")
	case replayFull:
		s.Metrics.RateLimited.Add(1)
		s.log().Error("grant: replay set full", "service", service, "request_id", c.rid)
		return refusedMsg("rate_limited")
	}
	// 10. Mint, audit, answer.
	rt, claims, err := s.Signer.MintHash(reqtoken.Claims{
		Aud: op.Service, Sub: p.UserID.String(), Hid: p.HospitalID.String(), Roles: slices.Clone(p.Roles),
		Rid: req.RequestId, Kind: reqtoken.KindReq, Subject: req.Subject, ReplyPrefix: BFFReplyPrefix,
		Via: reqtoken.ViaGrant,
	}, req.BodyHash, ttl)
	if err != nil {
		// Mint's errors are fixed values with no claim in them.
		s.seen.remove(req.RequestId) // nothing was granted under it
		s.log().Error("grant: refused to mint", "service", service, "operation", op.Name, "request_id", c.rid, "error", err)
		return s.refuse(ctx, c, p, "forbidden")
	}
	err = authz.Tx(authz.WithPrincipal(ctx, p), s.Pool, func(tx pgx.Tx) error {
		return audit.Write(ctx, tx, audit.Event{
			ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(), RequestID: c.rid,
			Action: "intelligence.granted", Entity: "operation", EntityID: op.Name,
			Payload: map[string]any{"operation": op.Name, "jti": claims.Jti, "bh": claims.BodyHash,
				"patient_ids": []string{}, "service": service},
		})
	})
	if err != nil {
		// No grant without its audit row. Nothing was granted under the
		// request id, so the worker's retry may use it again.
		s.seen.remove(req.RequestId)
		s.Metrics.AuditFailed.Add(1)
		s.Metrics.Refused.Add(1)
		s.log().Error("grant: audit failed, refused", "service", service, "operation", op.Name, "request_id", c.rid)
		return refusedMsg("not_ready")
	}
	s.Metrics.Granted.Add(1)
	s.log().Info("grant: granted", "service", service, "operation", op.Name, "request_id", c.rid,
		"user_id", p.UserID.String(), "hospital_id", p.HospitalID.String(), "jti", claims.Jti, "kid", claims.KeyID)
	return grantedMsg(rt)
}

// refusalPayload is a grant_refused row's payload: ids and codes only.
func refusalPayload(c call, code string) map[string]any {
	p := map[string]any{"reason": code, "service": c.service}
	if c.op != "" {
		p["operation"] = c.op
	}
	return p
}

// refuseEarly refuses before a principal exists (a bad request, token or
// session): audited as mhai_auth, as sign-in failures are, with the user
// and hospital of a verified token if there is one. Refusals of tokens
// that didn't verify are audited at most refusalAuditRate a second per
// service, the rest counted only.
func (s *Server) refuseEarly(ctx context.Context, c call, code string) *nats.Msg {
	code = refusalCode(code)
	s.Metrics.Refused.Add(1)
	var actor, hospital string
	if uid := c.tok.UserID(); uid != uuid.Nil {
		actor = uid.String()
		if hid := c.tok.HospitalID(); hid != uuid.Nil {
			hospital = hid.String()
		}
	}
	s.log().Warn("grant: refused", "service", c.service, "operation", c.op, "reason", code, "request_id", c.rid,
		"user_id", actor)
	// Unverified tokens cost nothing to send, so their refusals are
	// audited at most refusalAuditRate a second per service. A verified
	// token's refusal is always audited: its session and hospital rates
	// already bound it, and a junk flood must not hide it.
	verified := c.tok.UserID() != uuid.Nil
	if verified {
		s.noteVerifiedRefusal(c)
	}
	if !verified && !s.refusalsA.allow(c.service, s.lim.Now()) {
		s.Metrics.AuditSkipped.Add(1)
		return refusedMsg(code)
	}
	err := db.WithAuth(ctx, s.Pool, func(tx pgx.Tx) error {
		return audit.Write(ctx, tx, audit.Event{
			ActorID: actor, HospitalID: hospital, RequestID: c.rid,
			Action: "intelligence.grant_refused", Entity: "operation", EntityID: c.op,
			Payload: refusalPayload(c, code),
		})
	})
	if err != nil {
		s.Metrics.AuditFailed.Add(1)
		s.log().Error("grant: refusal audit failed", "service", c.service, "request_id", c.rid)
	}
	return refusedMsg(code)
}

// refuse refuses once the principal is known: audited through authz.Tx
// under the principal's hospital.
func (s *Server) refuse(ctx context.Context, c call, p authz.Principal, code string) *nats.Msg {
	code = refusalCode(code)
	s.Metrics.Refused.Add(1)
	s.noteVerifiedRefusal(c)
	s.log().Warn("grant: refused", "service", c.service, "operation", c.op, "reason", code, "request_id", c.rid,
		"user_id", p.UserID.String(), "hospital_id", p.HospitalID.String())
	err := authz.Tx(authz.WithPrincipal(ctx, p), s.Pool, func(tx pgx.Tx) error {
		return audit.Write(ctx, tx, audit.Event{
			ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(), RequestID: c.rid,
			Action: "intelligence.grant_refused", Entity: "operation", EntityID: c.op,
			Payload: refusalPayload(c, code),
		})
	})
	if err != nil {
		s.Metrics.AuditFailed.Add(1)
		s.log().Error("grant: refusal audit failed", "service", c.service, "request_id", c.rid)
	}
	return refusedMsg(code)
}

// FloodEvery is the sampling step of the per-session refusal signal: one
// signal (a counter and a log line) each time a session's verified-token
// refusals in a minute reach a multiple of it. The audit rows are not
// capped (a verified refusal is always written); this is how a flood of
// them becomes visible without a per-session metric label.
const FloodEvery = 100

// noteVerifiedRefusal counts a refusal of a verified token, and signals
// when its session has been refused FloodEvery times (or a multiple) within
// a minute. The session id is a uuid and goes in the log only, never in a
// metric label.
func (s *Server) noteVerifiedRefusal(c call) {
	s.Metrics.VerifiedRefused.Add(1)
	if s.flood == nil {
		return
	}
	n := s.flood.add(c.tok.SessionID().String(), s.lim.Now())
	if n > 0 && n%FloodEvery == 0 {
		s.Metrics.RefusalFlood.Add(1)
		s.log().Warn("grant: session refusal flood", "service", c.service, "session_id", c.tok.SessionID().String(),
			"hospital_id", c.tok.HospitalID().String(), "refusals_in_window", n)
	}
}

// DefaultSummaryEvery is how often the grant counts are logged.
const DefaultSummaryEvery = time.Minute

// sampler lets one log line through per interval and counts the rest, so
// a flood of dropped or overloaded requests is one line and a count.
type sampler struct {
	mu         sync.Mutex
	every      time.Duration
	last       time.Time
	suppressed int64
}

func newSampler(every time.Duration) *sampler { return &sampler{every: every} }

// take reports whether to log now, with the events since the last line
// (this one included).
func (l *sampler) take() (int64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.suppressed++
	if !l.last.IsZero() && now.Sub(l.last) < l.every && !now.Before(l.last) {
		return 0, false
	}
	n := l.suppressed
	l.last, l.suppressed = now, 0
	return n, true
}

// summarise logs the counts that changed, every SummaryEvery, until done.
func (s *Server) summarise(done <-chan struct{}) {
	t := time.NewTicker(s.SummaryEvery)
	defer t.Stop()
	var prev [7]int64
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		m := s.Metrics
		cur := [7]int64{m.Granted.Load(), m.Refused.Load(), m.RateLimited.Load(), m.Overloaded.Load(),
			m.DroppedReply.Load(), m.AuditSkipped.Load(), m.AuditFailed.Load()}
		if cur == prev {
			continue
		}
		prev = cur
		s.log().Info("grant: summary", "granted", cur[0], "refused", cur[1], "rate_limited", cur[2],
			"overloaded", cur[3], "dropped_reply", cur[4], "audit_skipped", cur[5], "audit_failed", cur[6],
			"in_flight", m.InFlight.Load())
	}
}
