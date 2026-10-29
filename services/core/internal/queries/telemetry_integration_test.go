//go:build integration

package queries_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intel"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/telemetry"
)

// TestMarkerNotInSpansOrLogs (A14, core side): a PHI-like marker in the
// question and the patient data, a marker password and the tokens go
// through POST /queries, a failed sign-in and a request/reply round trip
// with an in-memory exporter behind the allow-list. No exported span
// attribute, event, name or link, and no log line, holds any of them; the
// job leaves with a traceparent on the request's trace and no baggage.
func TestMarkerNotInSpansOrLogs(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	shutdown, err := telemetry.SetupWith(exp, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	e := newEnv(t, 10)
	hid := seedHospital(t)
	u := seedUser(t, hid, authz.RoleClinician)
	jobs(t, hid)
	access := mint(t, u)
	pw := "PW-" + marker

	traceID := "4bf92f3577b34da6a3ce929d0e0e4736"
	hdr := map[string]string{
		"traceparent": "00-" + traceID + "-00f067aa0ba902b7-01",
		"baggage":     "patient=" + marker,
	}
	r := e.do(t, "POST", "/queries", access, question("Synthetic question "+marker+"?"), hdr)
	if r.code != http.StatusAccepted {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	id, _ := r.body["query_id"].(string)
	if l := e.do(t, "POST", "/auth/login", "", fmt.Sprintf(`{"email":"synthetic.%s@havana.example","password":%q}`, uniq(), pw), nil); l.code < 400 {
		t.Fatalf("login with a made-up account: %d", l.code)
	}
	if g := e.do(t, "GET", "/queries/"+id, access, "", hdr); g.code != http.StatusOK {
		t.Fatalf("status %d", g.code)
	}

	// The job left on the request's trace; no baggage rode along.
	ms := jobs(t, hid)
	if len(ms) != 1 {
		t.Fatalf("%d jobs", len(ms))
	}
	reqToken := ms[0].Headers().Get("Mhai-Request-Token")
	if reqToken == "" || !strings.Contains(ms[0].Headers().Get("traceparent"), traceID) {
		t.Fatalf("job headers: %v", ms[0].Headers())
	}
	for k := range ms[0].Headers() {
		if strings.EqualFold(k, "baggage") {
			t.Fatal("baggage on the job")
		}
	}

	// A request/reply round trip with the question as the ping text.
	wnc, _, err := bus.Connect(context.Background(), natsSrv.EchoURL, "", bus.WorkerInbox("echo"), bus.TLS{})
	if err != nil {
		t.Fatal(err)
	}
	defer wnc.Close()
	sub, err := wnc.QueueSubscribe("inference.v1.echo.ping", "echo", func(m *nats.Msg) {
		var in intelpb.PingRequest
		_ = proto.Unmarshal(m.Data, &in)
		out := nats.NewMsg(m.Reply)
		out.Header.Set("Mhai-Request-Id", m.Header.Get("Mhai-Request-Id"))
		if in.Text == "refuse" {
			out.Header.Set("Mhai-Status", "forbidden")
			out.Header.Set("Mhai-Schema", "mhai.intelligence.v1.Error")
			out.Data, _ = proto.Marshal(&intelpb.Error{Code: "forbidden"})
		} else {
			out.Header.Set("Mhai-Status", "ok")
			out.Header.Set("Mhai-Schema", "mhai.intelligence.v1.PingResponse")
			out.Data, _ = proto.Marshal(&intelpb.PingResponse{Text: in.Text})
		}
		_ = m.RespondMsg(out)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := wnc.Flush(); err != nil {
		t.Fatal(err)
	}
	client := &intel.Client{NC: gwNC, JS: gwJS, Signer: signer, Log: logx.New(e.logs)}
	var resp intelpb.PingResponse
	caller := intel.Caller{Sub: u.id.String(), Hid: hid.String(), Rid: "rid-" + uniq(), Roles: []string{authz.RoleClinician}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Request(ctx, caller, "echo.ping", &intelpb.PingRequest{Text: marker}, &resp); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	// A refusal is logged (ids and the code) and never shown with the text.
	if _, err := client.Request(ctx, caller, "echo.ping", &intelpb.PingRequest{Text: "refuse"}, &resp); err == nil {
		t.Fatal("expected a refusal")
	}

	spans := exp.GetSpans()
	names := map[string]bool{}
	for _, s := range spans {
		names[s.Name] = true
	}
	for _, want := range []string{"api POST /api/v1/queries", "intelligence.publish run_query", "intelligence.request echo.ping"} {
		if !names[want] {
			t.Errorf("no span %q (have %v)", want, names)
		}
	}
	secrets := []string{marker, access, reqToken, pw}
	for _, s := range spans {
		texts := []string{s.Name, s.Status.Description}
		for _, kv := range s.Attributes {
			texts = append(texts, string(kv.Key), kv.Value.Emit())
		}
		for _, ev := range s.Events {
			texts = append(texts, ev.Name)
			for _, kv := range ev.Attributes {
				texts = append(texts, string(kv.Key), kv.Value.Emit())
			}
		}
		for _, l := range s.Links {
			for _, kv := range l.Attributes {
				texts = append(texts, string(kv.Key), kv.Value.Emit())
			}
		}
		for _, text := range texts {
			for _, secret := range secrets {
				if strings.Contains(text, secret) {
					t.Fatalf("a secret reached span %q: %q", s.Name, text)
				}
			}
		}
	}
	for _, secret := range secrets {
		if strings.Contains(e.logs.String(), secret) {
			t.Fatalf("a secret reached the logs")
		}
	}
	if e.logs.String() == "" {
		t.Fatal("nothing logged: the test proves nothing")
	}
}
