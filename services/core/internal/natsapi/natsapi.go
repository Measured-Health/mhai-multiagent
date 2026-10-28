// Package natsapi serves the frontend contract over NATS (spec §17.1).
//
// The BFFs (bff_clinical, bff_platform) send one HTTP request per message on
// api.v1.clinical or api.v1.platform. Core subscribes both in queue group
// core, so each message goes to one copy of core. The adapter turns the
// message into an *http.Request and runs it through the same http.Handler
// as HTTP (authz.Mux.NATSHandler): request id, body limit, principal and
// access token, route rule, idempotency, the RLS transaction and audit.
// Nothing of that chain is reimplemented here. What the adapter adds:
//
//  1. the reply subject must be the sending app's own inbox
//     (_INBOX_bff.<app>.<literal tokens>), else no reply at all: NATS lets
//     the requester choose the reply subject and allow_responses lets core
//     publish there once with its own permissions (services/nats/README.md,
//     "Responders must check the reply subject");
//  2. Mhai-Method and Mhai-Path checked (405, 404), the body at most
//     httpx.MaxBody (413);
//  3. only RequestHeaders pass in, only ResponseHeaders pass out;
//  4. the audience is the subject's (httpx.ViaNATS), never a header;
//  5. a deadline per message (30 s, or the BFF's Mhai-Timeout-Ms if less)
//     and a bounded pool (past it, 503 unavailable at once).
package natsapi

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// The apps, their subjects and core's queue group.
const (
	AppClinical     = httpx.AudienceClinical
	AppPlatform     = httpx.AudiencePlatform
	SubjectClinical = "api.v1." + AppClinical
	SubjectPlatform = "api.v1." + AppPlatform
	Queue           = "core"
)

// Limits.
const (
	// DefaultConcurrency is CORE_NATS_API_CONCURRENCY's default.
	DefaultConcurrency = 256
	// MaxPath bounds Mhai-Path (path and query), in bytes.
	MaxPath = 2048
	// MaxTimeout is a message's deadline; Mhai-Timeout-Ms can only shorten it.
	MaxTimeout = 30 * time.Second
	// MaxReply bounds the reply subject. nats.go's are about 60 bytes.
	MaxReply = 256
	// defaultMaxPayload is NATS max_payload (services/nats/nats.conf), used
	// when the connection doesn't report one.
	defaultMaxPayload = 1 << 20
)

// methods are the HTTP methods the API uses; anything else is 405.
var methods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// Metrics counts what the adapter did, for a later metrics endpoint
// (Plan 3 Task 14) and the tests.
type Metrics struct {
	RefusedReply atomic.Int64 // messages dropped for their reply subject
	Overflow     atomic.Int64 // messages answered 503 because the pool was full
	Served       atomic.Int64 // messages run through the handler
	InFlight     atomic.Int64 // messages being handled now
}

// Option configures Serve.
type Option func(*server)

// WithConcurrency bounds the messages handled at once (n < 1: the default).
func WithConcurrency(n int) Option {
	return func(s *server) {
		if n >= 1 {
			s.sem = make(chan struct{}, n)
		}
	}
}

// WithMetrics counts into m.
func WithMetrics(m *Metrics) Option {
	return func(s *server) {
		if m != nil {
			s.metrics = m
		}
	}
}

type server struct {
	h          http.Handler
	log        *slog.Logger
	sem        chan struct{}
	metrics    *Metrics
	maxPayload int64

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func newServer(h http.Handler, log *slog.Logger, opts []Option) *server {
	s := &server{h: h, log: log, sem: make(chan struct{}, DefaultConcurrency), metrics: &Metrics{}, maxPayload: defaultMaxPayload}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Serve subscribes api.v1.clinical and api.v1.platform in queue group core
// and answers each message with h, normally authz.Mux.NATSHandler. stop
// unsubscribes and waits for the messages in hand (each ends by its
// deadline); it also runs when ctx ends.
func Serve(ctx context.Context, nc *nats.Conn, h http.Handler, log *slog.Logger, opts ...Option) (stop func(), err error) {
	s := newServer(h, log, opts)
	if mp := nc.MaxPayload(); mp > 0 {
		s.maxPayload = mp
	}
	var subs []*nats.Subscription
	unsubscribe := func() {
		for _, sub := range subs {
			_ = sub.Unsubscribe()
		}
	}
	for _, app := range []string{AppClinical, AppPlatform} {
		sub, err := nc.QueueSubscribe("api.v1."+app, Queue, s.onMsg(app))
		if err != nil {
			unsubscribe()
			return nil, err
		}
		subs = append(subs, sub)
	}
	if err := nc.Flush(); err != nil {
		unsubscribe()
		return nil, err
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			unsubscribe()
			_ = nc.FlushTimeout(time.Second)
			s.mu.Lock()
			s.closed = true
			s.mu.Unlock()
			s.wg.Wait()
		})
	}
	context.AfterFunc(ctx, stop)
	return stop, nil
}

// onMsg is the subscription callback for app. It checks the reply
// subject, takes a slot in the pool (or answers 503 at once) and handles
// the message on its own goroutine.
func (s *server) onMsg(app string) nats.MsgHandler {
	return func(m *nats.Msg) {
		if !s.accept(app, m) {
			return
		}
		select {
		case s.sem <- struct{}{}:
		default:
			s.metrics.Overflow.Add(1)
			s.log.Warn("natsapi: busy, answered unavailable", "app", app)
			s.respond(app, m, s.problem(app, m.Reply, "unavailable"))
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			<-s.sem
			return // stopping: the BFF times out and retries on another copy
		}
		s.wg.Add(1)
		s.mu.Unlock()
		s.metrics.InFlight.Add(1)
		go func() {
			defer func() {
				s.metrics.InFlight.Add(-1)
				<-s.sem
				s.wg.Done()
			}()
			s.metrics.Served.Add(1)
			s.respond(app, m, s.serveMsg(context.Background(), app, m))
		}()
	}
}

// accept reports whether m may be answered: it arrived on api.v1.<app> and
// its reply subject is that app's own inbox. A refusal sends nothing, runs
// nothing, and is logged and counted with the app only: the reply subject
// is attacker-chosen and never logged.
func (s *server) accept(app string, m *nats.Msg) bool {
	if m.Subject == "api.v1."+app && validReply(app, m.Reply) {
		return true
	}
	s.metrics.RefusedReply.Add(1)
	s.log.Error("natsapi: reply subject refused", "app", app)
	return false
}

// validReply reports whether reply is _INBOX_bff.<app>. followed by one or
// more literal tokens: non-empty, printable ASCII, no '*', '>' or space.
// Anything else (another app's inbox, core's own inbox, jobs.v1.…, $KV.…,
// $JS.API.…, a wildcard) would make core's one reply an arbitrary publish
// with core's permissions.
func validReply(app, reply string) bool {
	if app != AppClinical && app != AppPlatform {
		return false
	}
	return bus.ReplyUnder(bus.BFFInbox(app)+".", reply, MaxReply)
}

func (s *server) respond(app string, m *nats.Msg, reply *nats.Msg) {
	if err := m.RespondMsg(reply); err != nil {
		s.log.Warn("natsapi: reply failed", "app", app, "request_id", reply.Header.Get("X-Request-ID"), "error", err.Error())
	}
}

// serveMsg turns m into an HTTP request on app's audience, runs it through
// the handler and returns the reply (its Subject is m.Reply).
func (s *server) serveMsg(ctx context.Context, app string, m *nats.Msg) *nats.Msg {
	method, ok := single(m.Header, HeaderMethod)
	if !ok || !knownMethod(method) {
		return s.problem(app, m.Reply, "method_not_allowed")
	}
	target, ok := single(m.Header, HeaderPath)
	if !ok || !validPath(target) {
		return s.problem(app, m.Reply, "not_found")
	}
	if len(m.Data) > httpx.MaxBody {
		return s.problem(app, m.Reply, "payload_too_large")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout(m.Header))
	defer cancel()
	var body io.Reader = http.NoBody
	if len(m.Data) > 0 {
		body = bytes.NewReader(m.Data)
	}
	r, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return s.problem(app, m.Reply, "not_found")
	}
	r.RequestURI = target
	copyRequestHeaders(r.Header, m.Header)
	if r, err = httpx.ViaNATS(r, app); err != nil {
		return s.problem(app, m.Reply, "internal_error")
	}
	rec := newRecorder(s.maxPayload)
	if !s.run(app, rec, r) {
		return s.problem(app, m.Reply, "internal_error")
	}
	out := nats.NewMsg(m.Reply)
	out.Header.Set(HeaderStatus, strconv.Itoa(rec.statusCode()))
	replyHeaders(out.Header, rec.final())
	if method != http.MethodHead { // as net/http, a HEAD reply has no body
		out.Data = rec.body.Bytes()
	}
	if rec.overflow || msgSize(out) > s.maxPayload {
		s.log.Error("natsapi: reply over the NATS payload limit", "app", app, "request_id", out.Header.Get("X-Request-ID"))
		return s.problem(app, m.Reply, "internal_error")
	}
	return out
}

// run calls the handler, turning a panic into false (a 500 for the BFF)
// so one bad request can't stop the adapter.
func (s *server) run(app string, w http.ResponseWriter, r *http.Request) (ok bool) {
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("natsapi: handler panicked", "app", app, "request_id", httpx.RequestIDFrom(r.Context()))
			ok = false
		}
	}()
	s.h.ServeHTTP(w, r)
	return true
}

// problem is the reply for an error code, built as the chain builds one:
// under its own request id, as a problem body or, on the platform
// audience, {"code"}.
func (s *server) problem(app, reply, code string) *nats.Msg {
	r, _ := http.NewRequest(http.MethodGet, httpx.BasePath+"/", http.NoBody)
	if vr, err := httpx.ViaNATS(r, app); err == nil {
		r = vr
	}
	rec := newRecorder(s.maxPayload)
	httpx.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { httpx.Error(w, r, code) })).ServeHTTP(rec, r)
	out := nats.NewMsg(reply)
	out.Header.Set(HeaderStatus, strconv.Itoa(rec.statusCode()))
	replyHeaders(out.Header, rec.final())
	out.Data = rec.body.Bytes()
	return out
}

func knownMethod(m string) bool {
	for _, k := range methods {
		if m == k {
			return true
		}
	}
	return false
}

// validPath accepts an origin-form target (path and optional query) under
// /api/v1/, at most MaxPath bytes of printable ASCII, with no fragment.
// Unclean paths ("..", "//") pass here and get the router's own 404, as
// over HTTP.
func validPath(p string) bool {
	if len(p) > MaxPath || !strings.HasPrefix(p, httpx.BasePath+"/") {
		return false
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c <= ' ' || c >= 0x7f || c == '#' {
			return false
		}
	}
	u, err := url.ParseRequestURI(p)
	return err == nil && u.Scheme == "" && u.Host == "" && u.Opaque == "" && strings.HasPrefix(u.Path, httpx.BasePath+"/")
}

// timeout is the message's deadline: MaxTimeout, or Mhai-Timeout-Ms when
// it is a positive whole number of milliseconds below it.
func timeout(h nats.Header) time.Duration {
	v, ok := single(h, HeaderTimeout)
	if !ok {
		return MaxTimeout
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || int64(n) >= MaxTimeout.Milliseconds() {
		return MaxTimeout
	}
	return time.Duration(n) * time.Millisecond
}

// msgSize is an upper bound of m's size on the wire (headers and body).
func msgSize(m *nats.Msg) int64 {
	n := int64(len(m.Data)) + 16
	for k, vs := range m.Header {
		for _, v := range vs {
			n += int64(len(k) + len(v) + 4)
		}
	}
	return n
}

// recorder is the http.ResponseWriter for one message: status, headers as
// of WriteHeader (as net/http sends them) and the body, capped at limit.
type recorder struct {
	hdr      http.Header
	sent     http.Header
	status   int
	body     bytes.Buffer
	limit    int64
	overflow bool
}

func newRecorder(limit int64) *recorder { return &recorder{hdr: http.Header{}, limit: limit} }

func (r *recorder) Header() http.Header { return r.hdr }

func (r *recorder) WriteHeader(code int) {
	if r.status != 0 || code < 200 || code > 999 {
		return
	}
	r.status = code
	r.sent = r.hdr.Clone()
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	if int64(r.body.Len()+len(b)) > r.limit {
		r.overflow = true
		return len(b), nil
	}
	return r.body.Write(b)
}

func (r *recorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// final is the headers sent: those at WriteHeader, or the current ones if
// the handler never wrote.
func (r *recorder) final() http.Header {
	if r.sent != nil {
		return r.sent
	}
	return r.hdr
}
