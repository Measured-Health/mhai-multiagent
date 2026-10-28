//go:build integration

package intel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
)

// The fake worker verifies the token against the subject, the body, aud and
// knd; it carries the caller's identity and nothing from the body;
// Mhai-Request-Id is the rid; the reply goes to core's inbox.
func TestRequestCarriesBoundToken(t *testing.T) {
	c, _ := newClient(t)
	meta := &intelpb.ResponseMeta{ModelVersions: map[string]string{"echo": "1"}}
	w := startFakeWorker(t, sharedNATS.EchoURL, "echo", "ping", signer.PublicKeys(), pong(meta))
	hid := uuid.New()
	caller := clinician(hid)
	var resp intelpb.PingResponse
	// The body names another hospital: the token's hid still comes only
	// from the caller (the worker's scope check then refuses it).
	other := uuid.NewString()
	got, err := c.Request(context.Background(), caller, "echo.ping", &intelpb.PingRequest{Text: "synthetic ping", HospitalId: other}, &resp)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.Text != "synthetic ping" || got.GetModelVersions()["echo"] != "1" {
		t.Fatalf("response %q, meta %v", resp.Text, got)
	}
	rs := w.received()
	if len(rs) != 1 {
		t.Fatalf("worker got %d messages", len(rs))
	}
	r := rs[0]
	if r.verifyErr != nil || r.replyErr != nil {
		t.Fatalf("worker refused: %v / %v", r.verifyErr, r.replyErr)
	}
	cl := r.claims
	if cl.Aud != "echo" || cl.Kind != reqtoken.KindReq || cl.Subject != "inference.v1.echo.ping" || r.subject != cl.Subject {
		t.Errorf("binding: aud %s kind %s nsub %s subject %s", cl.Aud, cl.Kind, cl.Subject, r.subject)
	}
	if cl.Sub != caller.Sub || cl.Hid != hid.String() || cl.Rid != caller.Rid ||
		len(cl.Roles) != 1 || cl.Roles[0] != authz.RoleClinician {
		t.Errorf("identity: %+v", cl)
	}
	if cl.ReplyPrefix != reqtoken.ReplyCore || cl.Via != reqtoken.ViaCore || !strings.HasPrefix(r.reply, reqtoken.ReplyCore) {
		t.Errorf("reply: rpl %q via %q inbox %q", cl.ReplyPrefix, cl.Via, r.reply)
	}
	if cl.KeyID != signer.KeyID {
		t.Errorf("kid %s", cl.KeyID)
	}
	if got := cl.ExpiresAt.Sub(cl.IssuedAt); got != 5*time.Second {
		t.Errorf("lifetime %v, want the operation's 5 s", got)
	}
	if r.header.Get("Mhai-Request-Id") != caller.Rid || r.header.Get("Mhai-Schema") != "mhai.intelligence.v1.PingRequest" {
		t.Errorf("headers %v", r.header)
	}
	// The body the worker verified is the request as marshalled.
	var in intelpb.PingRequest
	if err := proto.Unmarshal(r.body, &in); err != nil || in.HospitalId != other {
		t.Errorf("body %v %v", &in, err)
	}

	// A shorter context deadline shortens the token.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := c.Request(ctx, caller, "echo.ping", &intelpb.PingRequest{Text: "x"}, &resp); err != nil {
		t.Fatal(err)
	}
	if rs := w.received(); len(rs) != 2 || rs[1].claims.ExpiresAt.Sub(rs[1].claims.IssuedAt) > 3*time.Second {
		t.Fatalf("token outlives the context: %v", rs[len(rs)-1].claims.ExpiresAt.Sub(rs[len(rs)-1].claims.IssuedAt))
	}
}

// Core checks the operation's roles before minting: a refused caller never
// reaches NATS.
func TestCoreChecksRolesFirst(t *testing.T) {
	c, _ := newClient(t)
	w := startFakeWorker(t, sharedNATS.EchoURL, "echo", "ping", signer.PublicKeys(), pong(nil))
	hid := uuid.New()
	var resp intelpb.PingResponse
	for name, caller := range map[string]Caller{
		"analyst":          {Sub: uuid.NewString(), Hid: hid.String(), Rid: "r1", Roles: []string{authz.RoleAnalyst}},
		"no roles":         {Sub: uuid.NewString(), Hid: hid.String(), Rid: "r2"},
		"svc:core + roles": {Sub: reqtoken.SvcCore, Hid: hid.String(), Rid: "r3", Roles: []string{authz.RoleClinician}},
		"no hospital":      {Sub: uuid.NewString(), Rid: "r4", Roles: []string{authz.RoleClinician}},
		"bad hospital":     {Sub: uuid.NewString(), Hid: "jobs.v1.>", Rid: "r5", Roles: []string{authz.RoleClinician}},
		"bad subject":      {Sub: "not-a-uuid", Hid: hid.String(), Rid: "r6", Roles: []string{authz.RoleClinician}},
		"no request id":    {Sub: uuid.NewString(), Hid: hid.String(), Roles: []string{authz.RoleClinician}},
	} {
		if _, err := c.Request(context.Background(), caller, "echo.ping", &intelpb.PingRequest{}, &resp); err == nil {
			t.Errorf("%s: allowed", name)
		}
	}
	if _, err := c.Request(context.Background(), clinician(hid), "echo.ping", &intelpb.PingRequest{}, &resp); err != nil {
		t.Fatalf("clinician: %v", err)
	}
	if n := w.n.Load(); n != 1 {
		t.Fatalf("worker got %d messages, want only the allowed one", n)
	}
	// The role check is core's own decision about the user: 403 forbidden.
	_, err := c.Request(context.Background(), Caller{Sub: uuid.NewString(), Hid: hid.String(), Rid: "r", Roles: []string{authz.RoleAnalyst}},
		"echo.ping", &intelpb.PingRequest{}, &resp)
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("analyst: %v", err)
	}
	// Misuse fails closed before NATS: unknown operation, a job operation,
	// the wrong request or response type.
	for name, f := range map[string]func() error{
		"unknown op": func() error {
			_, err := c.Request(context.Background(), clinician(hid), "echo.nope", &intelpb.PingRequest{}, &resp)
			return err
		},
		"job op": func() error {
			_, err := c.Request(context.Background(), clinician(hid), "pipeline.run_query", &intelpb.RunQueryJob{}, &resp)
			return err
		},
		"wrong request type": func() error {
			_, err := c.Request(context.Background(), clinician(hid), "echo.ping", &intelpb.HealthRequest{}, &resp)
			return err
		},
		"wrong response type": func() error {
			_, err := c.Request(context.Background(), clinician(hid), "echo.ping", &intelpb.PingRequest{}, &intelpb.HealthResponse{})
			return err
		},
	} {
		if err := f(); err == nil {
			t.Errorf("%s: allowed", name)
		}
	}
	if n := w.n.Load(); n != 1 {
		t.Fatalf("worker got %d messages after misuse", n)
	}
}

// No reply in time is 503 unavailable, and the call is not retried.
func TestRequestTimeoutIs503(t *testing.T) {
	c, _ := newClient(t)
	w := startFakeWorker(t, sharedNATS.EchoURL, "echo", "ping", signer.PublicKeys(), silent)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	var resp intelpb.PingResponse
	start := time.Now()
	_, err := c.Request(ctx, clinician(uuid.New()), "echo.ping", &intelpb.PingRequest{Text: "x"}, &resp)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v, want ErrTimeout", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("waited %v", time.Since(start))
	}
	if code, ra := Code(err); code != "unavailable" || ra != 0 {
		t.Fatalf("code %s %d", code, ra)
	}
	time.Sleep(700 * time.Millisecond)
	if n := w.n.Load(); n != 1 {
		t.Fatalf("worker got %d messages, want 1 (no retry)", n)
	}
	// A context with under a second left is not sent at all.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if _, err := c.Request(ctx2, clinician(uuid.New()), "echo.ping", &intelpb.PingRequest{}, &resp); !errors.Is(err, ErrTimeout) {
		t.Fatalf("short context: %v", err)
	}
	if n := w.n.Load(); n != 1 {
		t.Fatalf("worker got %d messages", n)
	}
}

// No worker running (NATS no responders) is 503 unavailable. svc:core
// calls carry a hospital too (F10): the health probe names the hospital it
// asks for.
func TestNoRespondersIs503(t *testing.T) {
	c, _ := newClient(t)
	var resp intelpb.HealthResponse
	_, err := c.Request(context.Background(), System(uuid.NewString(), "rid-"+uuid.NewString()), "pipeline.health", &intelpb.HealthRequest{}, &resp)
	if !errors.Is(err, ErrNoResponders) {
		t.Fatalf("got %v, want ErrNoResponders", err)
	}
	if code, _ := Code(err); code != "unavailable" {
		t.Fatalf("code %s", code)
	}
	// svc:core with no hospital is refused before NATS.
	if _, err := c.Request(context.Background(), System("", "rid"), "pipeline.health", &intelpb.HealthRequest{}, &resp); err == nil ||
		errors.Is(err, ErrNoResponders) {
		t.Fatalf("hospital-less svc:core call: %v", err)
	}
}

// overloaded is 503 unavailable with Retry-After: 5.
func TestOverloadedSetsRetryAfter(t *testing.T) {
	c, _ := newClient(t)
	startFakeWorker(t, sharedNATS.EchoURL, "echo", "ping", signer.PublicKeys(), refuse("overloaded", true))
	var resp intelpb.PingResponse
	_, err := c.Request(context.Background(), clinician(uuid.New()), "echo.ping", &intelpb.PingRequest{}, &resp)
	var we *Error
	if !errors.As(err, &we) || we.Code != "overloaded" || !we.Retryable {
		t.Fatalf("got %v", err)
	}
	code, ra := Code(err)
	rec := httptest.NewRecorder()
	httpx.ErrorRetryAfter(rec, httptest.NewRequest(http.MethodGet, "/api/v1/x", nil), code, ra)
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "5" || !strings.Contains(rec.Body.String(), `"code":"unavailable"`) {
		t.Fatalf("%d %q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}

// A reply that doesn't hold together is ErrBadReply (500), never trusted:
// a wrong schema, an echoed request id that isn't ours, an Error body whose
// code isn't the status or isn't in the closed set.
func TestBadRepliesAreInternal(t *testing.T) {
	c, _ := newClient(t)
	hid := uuid.New()
	var resp intelpb.PingResponse
	for name, ans := range map[string]func(reqtoken.Claims, *nats.Msg) reply{
		"wrong schema": func(c reqtoken.Claims, m *nats.Msg) reply {
			r := pong(nil)(c, m)
			r.schema = "mhai.intelligence.v1.HealthResponse"
			return r
		},
		"foreign request id": func(c reqtoken.Claims, m *nats.Msg) reply {
			r := pong(nil)(c, m)
			r.requestID = "rid-someone-else"
			return r
		},
		"status and body disagree": func(reqtoken.Claims, *nats.Msg) reply {
			return reply{status: "not_found", schema: errorSchema, body: errorBody("overloaded", true), send: true}
		},
		"unknown code": func(reqtoken.Claims, *nats.Msg) reply {
			return reply{status: "teapot", schema: errorSchema, body: errorBody("teapot", false), send: true}
		},
		"garbage ok body": func(reqtoken.Claims, *nats.Msg) reply {
			return reply{status: "ok", schema: "mhai.intelligence.v1.PingResponse", body: []byte{0xff, 0xff, 0xff}, send: true}
		},
	} {
		t.Run(name, func(t *testing.T) {
			startFakeWorker(t, sharedNATS.EchoURL, "echo", "ping", signer.PublicKeys(), ans)
			_, err := c.Request(context.Background(), clinician(hid), "echo.ping", &intelpb.PingRequest{}, &resp)
			if err == nil {
				t.Fatal("accepted")
			}
			if code, _ := Code(err); code != "internal_error" {
				t.Fatalf("%v maps to %s", err, code)
			}
		})
	}
	// not_found, as the worker says it, is 404.
	startFakeWorker(t, sharedNATS.EchoURL, "echo", "ping", signer.PublicKeys(), refuse("not_found", false))
	_, err := c.Request(context.Background(), clinician(hid), "echo.ping", &intelpb.PingRequest{}, &resp)
	if code, _ := Code(err); code != "not_found" {
		t.Fatalf("not_found maps to %s (%v)", code, err)
	}
}

// Refusals are logged with ids and the code only.
func TestRefusalLoggedWithIDsOnly(t *testing.T) {
	c, logs := newClient(t)
	startFakeWorker(t, sharedNATS.EchoURL, "echo", "ping", signer.PublicKeys(), refuse("forbidden", false))
	caller := clinician(uuid.New())
	var resp intelpb.PingResponse
	const marker = "synthetic-marker-never-logged"
	if _, err := c.Request(context.Background(), caller, "echo.ping", &intelpb.PingRequest{Text: marker}, &resp); err == nil {
		t.Fatal("accepted")
	}
	out := logs.String()
	if !strings.Contains(out, caller.Rid) || !strings.Contains(out, "forbidden") || !strings.Contains(out, "echo.ping") {
		t.Fatalf("log lacks ids: %s", out)
	}
	if strings.Contains(out, marker) || strings.Contains(out, "eyJ") {
		t.Fatalf("log carries the body or a token: %s", out)
	}
}
