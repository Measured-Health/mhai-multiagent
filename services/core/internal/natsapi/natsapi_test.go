package natsapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// seenReq is what a handler behind the adapter saw.
type seenReq struct {
	mu       sync.Mutex
	called   int
	method   string
	path     string
	query    string
	header   http.Header
	body     []byte
	audience string
	ip       string
	rid      string
	deadline time.Time
}

func (s *seenReq) handler(status int, out func(w http.ResponseWriter)) http.Handler {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.called++
		s.method, s.path, s.query = r.Method, r.URL.Path, r.URL.RawQuery
		s.header = r.Header.Clone()
		s.body = b
		s.audience = httpx.Audience(r.Context())
		if ip := httpx.ClientIP(r.Context()); ip.IsValid() {
			s.ip = ip.String()
		}
		s.rid = httpx.RequestIDFrom(r.Context())
		s.deadline, _ = r.Context().Deadline()
		s.mu.Unlock()
		if out != nil {
			out(w)
		}
		w.WriteHeader(status)
	})
	rt := httpx.NewRouter()
	for _, m := range methods {
		rt.Handle(m, "/probe", inner)
	}
	return rt.NATSHandler()
}

func newTestServer(h http.Handler) *server {
	return newServer(h, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

func msg(subject, method, path string, body []byte, hdr map[string][]string) *nats.Msg {
	m := nats.NewMsg(subject)
	m.Reply = "_INBOX_bff." + strings.TrimPrefix(subject, "api.v1.") + ".test1"
	if method != "" {
		m.Header.Set(HeaderMethod, method)
	}
	if path != "" {
		m.Header.Set(HeaderPath, path)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			m.Header.Add(k, v)
		}
	}
	m.Data = body
	return m
}

type reply struct {
	status int
	hdr    nats.Header
	body   map[string]any
	raw    string
}

func decode(t *testing.T, m *nats.Msg) reply {
	t.Helper()
	if m == nil {
		t.Fatal("no reply")
	}
	var st int
	if v := m.Header.Get(HeaderStatus); v != "" {
		if err := json.Unmarshal([]byte(v), &st); err != nil {
			t.Fatalf("status header %q", v)
		}
	}
	r := reply{status: st, hdr: m.Header, raw: string(m.Data)}
	_ = json.Unmarshal(m.Data, &r.body)
	return r
}

func TestValidReply(t *testing.T) {
	for _, c := range []struct {
		app, reply string
		ok         bool
	}{
		{"clinical", "_INBOX_bff.clinical.abc", true},
		{"clinical", "_INBOX_bff.clinical.abc.def", true},
		{"platform", "_INBOX_bff.platform.Zx9_-", true},
		{"clinical", "_INBOX_bff.platform.x", false},
		{"platform", "_INBOX_bff.clinical.x", false},
		{"clinical", "_INBOX_bff.clinical", false},
		{"clinical", "_INBOX_bff.clinical.", false},
		{"clinical", "_INBOX_bff.clinical..x", false},
		{"clinical", "_INBOX_bff.clinical.x.", false},
		{"clinical", "_INBOX_bff.clinical.*", false},
		{"clinical", "_INBOX_bff.clinical.>", false},
		{"clinical", "_INBOX_bff.clinical.a*b", false},
		{"clinical", "_INBOX_bff.clinical.a b", false},
		{"clinical", "_INBOX_bff.clinical.a\tb", false},
		{"clinical", "_INBOX_bff.clinical.a\x00b", false},
		{"clinical", "_INBOX_bff.clinical.é", false},
		{"clinical", "_INBOX_bff.clinicalx.y", false},
		{"clinical", "_INBOX_gateway.x", false},
		{"clinical", "_INBOX.x", false},
		{"clinical", "jobs.v1.4b0f8c5e-0000-4000-8000-000000000000.run_query", false},
		{"clinical", "$KV.denylist.x", false},
		{"clinical", "$JS.API.STREAM.DELETE.JOBS", false},
		{"clinical", "", false},
		{"clinical", "_INBOX_bff.clinical." + strings.Repeat("a", 300), false},
		{"admin", "_INBOX_bff.admin.x", false},
	} {
		if got := validReply(c.app, c.reply); got != c.ok {
			t.Errorf("validReply(%q, %q) = %v, want %v", c.app, c.reply, got, c.ok)
		}
	}
}

// A foreign reply subject gets no reply and runs nothing (serveMsg is only
// reached after the check; onMsg is what the subscription calls).
func TestForeignReplyRunsNothing(t *testing.T) {
	var s seenReq
	srv := newTestServer(s.handler(204, nil))
	for _, r := range []string{"_INBOX_gateway.x", "_INBOX_bff.platform.x", "jobs.v1.x.run_query", "$KV.denylist.x", "_INBOX_bff.clinical.*"} {
		m := msg(SubjectClinical, "GET", "/api/v1/probe", nil, nil)
		m.Reply = r
		if srv.accept(AppClinical, m) {
			t.Errorf("%q accepted", r)
		}
	}
	if s.called != 0 || srv.metrics.RefusedReply.Load() != 5 {
		t.Fatalf("called %d, refused %d", s.called, srv.metrics.RefusedReply.Load())
	}
}

// Only the allow-listed headers reach the handler. X-MHAI-Audience is
// taken from the subject, X-Request-ID is core's own, and the BFF key,
// cookies, forwarding and hop-by-hop headers are dropped.
func TestUnlistedHeadersDropped(t *testing.T) {
	var s seenReq
	srv := newTestServer(s.handler(204, nil))
	in := map[string][]string{
		"Authorization":     {"Bearer synthetic-token"},
		"content-type":      {"application/json"}, // any case
		"Idempotency-Key":   {"key-1"},
		"If-None-Match":     {`"v1"`},
		"X-MHAI-Client-IP":  {"198.51.100.4"},
		"traceparent":       {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"},
		"tracestate":        {"vendor=synthetic"},
		"baggage":           {"user=synthetic"},
		"X-MHAI-Service":    {"test-only-bff-service-key-0123456789abcdef"},
		"X-MHAI-Audience":   {"platform"},
		"X-Request-ID":      {"forged-request-id"},
		"Mhai-Request-Id":   {"forged-request-id-2"},
		"Cookie":            {"session=synthetic"},
		"X-Forwarded-For":   {"203.0.113.9"},
		"Forwarded":         {"for=203.0.113.9"},
		"X-Real-IP":         {"203.0.113.9"},
		"Connection":        {"close"},
		"Transfer-Encoding": {"chunked"},
		"Content-Length":    {"999"},
		"Host":              {"evil.example"},
		"X-MHAI-User":       {"00000000-0000-0000-0000-000000000001"},
	}
	r := decode(t, srv.serveMsg(context.Background(), AppClinical, msg(SubjectClinical, "POST", "/api/v1/probe?a=1", []byte(`{"x":1}`), in)))
	if r.status != 204 || s.called != 1 {
		t.Fatalf("status %d called %d %s", r.status, s.called, r.raw)
	}
	want := map[string]string{
		"Authorization": "Bearer synthetic-token", "Content-Type": "application/json", "Idempotency-Key": "key-1",
		"If-None-Match": `"v1"`, "X-Mhai-Client-Ip": "198.51.100.4", "Traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		"Tracestate": "vendor=synthetic",
	}
	for k := range s.header {
		if _, ok := want[k]; !ok {
			t.Errorf("header %s reached the handler", k)
		}
	}
	for k, v := range want {
		if got := s.header.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	if s.audience != httpx.AudienceClinical || s.ip != "198.51.100.4" {
		t.Fatalf("audience %q ip %q", s.audience, s.ip)
	}
	if s.rid == "" || strings.HasPrefix(s.rid, "forged") || r.hdr.Get("X-Request-ID") != s.rid {
		t.Fatalf("request id %q, reply %q", s.rid, r.hdr.Get("X-Request-ID"))
	}
	if s.method != "POST" || s.path != "/api/v1/probe" || s.query != "a=1" || string(s.body) != `{"x":1}` {
		t.Fatalf("request %s %s ? %s %q", s.method, s.path, s.query, s.body)
	}
}

// The audience is the subject's: platform on api.v1.platform, whatever
// header the message carries; a problem there is the platform's {"code"}.
func TestAudienceFromSubjectUnit(t *testing.T) {
	var s seenReq
	srv := newTestServer(s.handler(204, nil))
	decode(t, srv.serveMsg(context.Background(), AppPlatform, msg(SubjectPlatform, "GET", "/api/v1/probe", nil,
		map[string][]string{"X-MHAI-Audience": {"clinical"}})))
	if s.audience != httpx.AudiencePlatform {
		t.Fatalf("audience %q", s.audience)
	}
	r := decode(t, srv.serveMsg(context.Background(), AppPlatform, msg(SubjectPlatform, "TRACE", "/api/v1/probe", nil, nil)))
	if r.raw != `{"code":"method_not_allowed"}` {
		t.Fatalf("platform problem %s", r.raw)
	}
}

// Only the allow-listed response headers go back to the BFF.
func TestResponseHeadersAllowListed(t *testing.T) {
	var s seenReq
	srv := newTestServer(s.handler(200, func(w http.ResponseWriter) {
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Cache-Control", "no-store")
		h.Set("ETag", `"e1"`)
		h.Set("Retry-After", "3")
		h.Set("Idempotent-Replayed", "true")
		h.Set("Deprecation", "@1700000000")
		h.Set("Sunset", "Wed, 01 Jan 2031 00:00:00 GMT")
		h.Set("Link", `</notice>; rel="deprecation"`)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Set-Cookie", "a=b")
		h.Set("X-Internal", "secret")
		h.Set(HeaderStatus, "200")
		h.Set("Mhai-Path", "/x")
	}))
	m := srv.serveMsg(context.Background(), AppClinical, msg(SubjectClinical, "GET", "/api/v1/probe", nil, nil))
	r := decode(t, m)
	if r.status != 200 {
		t.Fatalf("status %d", r.status)
	}
	allowed := map[string]bool{HeaderStatus: true, "Content-Type": true, "Cache-Control": true, "ETag": true, "Retry-After": true,
		"X-Request-ID": true, "Idempotent-Replayed": true, "Deprecation": true, "Sunset": true, "Link": true, "X-Content-Type-Options": true}
	for k := range m.Header {
		if !allowed[k] {
			t.Errorf("response header %s passed", k)
		}
	}
	for k := range allowed {
		if len(m.Header.Values(k)) != 1 {
			t.Errorf("response header %s: %v", k, m.Header.Values(k))
		}
	}
	if m.Header.Get("ETag") != `"e1"` || m.Header.Get("Retry-After") != "3" {
		t.Fatalf("values %v", m.Header)
	}
}

// A handler that writes without WriteHeader is a 200; one that writes
// nothing is a 200 with an empty body.
func TestDefaultStatus(t *testing.T) {
	srv := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("x")) }))
	if r := decode(t, srv.serveMsg(context.Background(), AppClinical, msg(SubjectClinical, "GET", "/api/v1/x", nil, nil))); r.status != 200 || r.raw != "x" {
		t.Fatalf("%d %q", r.status, r.raw)
	}
	srv = newTestServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if r := decode(t, srv.serveMsg(context.Background(), AppClinical, msg(SubjectClinical, "GET", "/api/v1/x", nil, nil))); r.status != 200 || r.raw != "" {
		t.Fatalf("%d %q", r.status, r.raw)
	}
}

// 64 KB is accepted; 64 KB + 1 is 413 before the handler runs (the same
// limit as httpx.MaxBody on HTTP).
func TestBodyLimitOverNATS(t *testing.T) {
	var s seenReq
	srv := newTestServer(s.handler(204, nil))
	r := decode(t, srv.serveMsg(context.Background(), AppClinical, msg(SubjectClinical, "POST", "/api/v1/probe", bytes.Repeat([]byte("a"), httpx.MaxBody), nil)))
	if r.status != 204 || len(s.body) != httpx.MaxBody {
		t.Fatalf("at the limit: %d, %d bytes", r.status, len(s.body))
	}
	r = decode(t, srv.serveMsg(context.Background(), AppClinical, msg(SubjectClinical, "POST", "/api/v1/probe", bytes.Repeat([]byte("a"), httpx.MaxBody+1), nil)))
	if r.status != 413 || r.body["code"] != "payload_too_large" || s.called != 1 {
		t.Fatalf("over the limit: %d %s, called %d", r.status, r.raw, s.called)
	}
	if r.body["request_id"] == "" || r.body["request_id"] != r.hdr.Get("X-Request-ID") {
		t.Fatalf("problem request id %v / %q", r.body["request_id"], r.hdr.Get("X-Request-ID"))
	}
}

func TestMethodAndPathChecked(t *testing.T) {
	var s seenReq
	srv := newTestServer(s.handler(204, nil))
	two := func(m *nats.Msg) *nats.Msg { m.Header.Add(HeaderMethod, "DELETE"); return m }
	twoPaths := func(m *nats.Msg) *nats.Msg { m.Header.Add(HeaderPath, "/api/v1/other"); return m }
	same := func(m *nats.Msg) *nats.Msg { return m }
	for _, c := range []struct {
		method, path string
		edit         func(*nats.Msg) *nats.Msg
		status       int
		code         string
	}{
		{"TRACE", "/api/v1/probe", same, 405, "method_not_allowed"},
		{"CONNECT", "/api/v1/probe", same, 405, "method_not_allowed"},
		{"OPTIONS", "/api/v1/probe", same, 405, "method_not_allowed"},
		{"get", "/api/v1/probe", same, 405, "method_not_allowed"},
		{"", "/api/v1/probe", same, 405, "method_not_allowed"},
		{"GET", "/api/v1/probe", two, 405, "method_not_allowed"},
		{"GET", "/etc/passwd", same, 404, "not_found"},
		{"GET", "", same, 404, "not_found"},
		{"GET", "/api/v1", same, 404, "not_found"},
		{"GET", "/api/docs", same, 404, "not_found"},
		{"GET", "/api/openapi.yaml", same, 404, "not_found"},
		{"GET", "/api/v1/healthz", same, 404, "not_found"},
		{"GET", "http://evil.example/api/v1/probe", same, 404, "not_found"},
		{"GET", "//evil.example/api/v1/probe", same, 404, "not_found"},
		{"GET", "/api/v1/probe#frag", same, 404, "not_found"},
		{"GET", "/api/v1/pro be", same, 404, "not_found"},
		{"GET", "/api/v1/probe\x7f", same, 404, "not_found"},
		{"GET", "/api/v1/pröbe", same, 404, "not_found"},
		{"GET", "/api/v1/../v1/probe", same, 404, "not_found"},
		{"GET", "/api/v1/%2e%2e/v1/probe", same, 404, "not_found"},
		{"GET", "/api/v1/probe?" + strings.Repeat("a", MaxPath), same, 404, "not_found"},
		{"GET", "/api/v1/probe", twoPaths, 404, "not_found"},
	} {
		r := decode(t, srv.serveMsg(context.Background(), AppClinical, c.edit(msg(SubjectClinical, c.method, c.path, nil, nil))))
		if r.status != c.status || r.body["code"] != c.code {
			t.Errorf("%q %q: %d %s", c.method, c.path, r.status, r.raw)
		}
		if ct := r.hdr.Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%q %q: content type %q", c.method, c.path, ct)
		}
	}
	if s.called != 0 {
		t.Fatalf("handler ran %d times", s.called)
	}
	for _, m := range methods {
		if r := decode(t, srv.serveMsg(context.Background(), AppClinical, msg(SubjectClinical, m, "/api/v1/probe", nil, nil))); r.status != 204 {
			t.Errorf("%s: %d %s", m, r.status, r.raw)
		}
	}
	if r := decode(t, srv.serveMsg(context.Background(), AppClinical, msg(SubjectClinical, "GET", "/api/v1/probe?"+strings.Repeat("a", MaxPath-len("/api/v1/probe?")), nil, nil))); r.status != 204 {
		t.Errorf("path at the limit: %d", r.status)
	}
}

// The handler's context ends at Mhai-Timeout-Ms, or at 30 s when the BFF
// sends none, an invalid one or a longer one.
func TestDeadlineEndsHandler(t *testing.T) {
	var mu sync.Mutex
	var ended error
	var deadline time.Time
	srv := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		deadline, _ = r.Context().Deadline()
		mu.Unlock()
		if r.Header.Get("X-Block") == "" { // never set: headers are allow-listed
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}
		mu.Lock()
		ended = r.Context().Err()
		mu.Unlock()
		httpx.Error(w, r, "unavailable")
	}))
	start := time.Now()
	r := decode(t, srv.serveMsg(context.Background(), AppClinical, msg(SubjectClinical, "GET", "/api/v1/x", nil,
		map[string][]string{HeaderTimeout: {"200"}})))
	took := time.Since(start)
	mu.Lock()
	gotErr := ended
	mu.Unlock()
	if !errors.Is(gotErr, context.DeadlineExceeded) || took < 150*time.Millisecond || took > 2*time.Second || r.status != 503 {
		t.Fatalf("ended %v after %v, status %d", gotErr, took, r.status)
	}
	for _, v := range []string{"", "abc", "0", "-5", "999999", "1e3"} {
		hdr := map[string][]string{}
		if v != "" {
			hdr[HeaderTimeout] = []string{v}
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		before := time.Now()
		srv.serveMsg(ctx, AppClinical, msg(SubjectClinical, "GET", "/api/v1/x", nil, hdr))
		mu.Lock()
		d := deadline.Sub(before)
		mu.Unlock()
		if d < MaxTimeout-time.Second || d > MaxTimeout+time.Second {
			t.Errorf("timeout %q: deadline in %v", v, d)
		}
	}
}

// A reply too big for NATS is a 500 internal_error, not a lost answer.
func TestReplyTooLarge(t *testing.T) {
	srv := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), 2048))
	}))
	srv.maxPayload = 1024
	r := decode(t, srv.serveMsg(context.Background(), AppClinical, msg(SubjectClinical, "GET", "/api/v1/x", nil, nil)))
	if r.status != 500 || r.body["code"] != "internal_error" {
		t.Fatalf("%d %s", r.status, r.raw)
	}
}

// A panicking handler is a 500 for the BFF, and the adapter survives.
func TestHandlerPanic(t *testing.T) {
	srv := newTestServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("synthetic") }))
	r := decode(t, srv.serveMsg(context.Background(), AppClinical, msg(SubjectClinical, "GET", "/api/v1/x", nil, nil)))
	if r.status != 500 || r.body["code"] != "internal_error" {
		t.Fatalf("%d %s", r.status, r.raw)
	}
}

// A HEAD reply has the GET's status and headers but no body, as over HTTP.
func TestHeadHasNoBody(t *testing.T) {
	srv := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"a": "b"})
	}))
	r := decode(t, srv.serveMsg(context.Background(), AppClinical, msg(SubjectClinical, "HEAD", "/api/v1/x", nil, nil)))
	if r.status != 200 || r.raw != "" || r.hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %q %v", r.status, r.raw, r.hdr)
	}
}
