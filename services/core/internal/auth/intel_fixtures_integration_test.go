//go:build integration && test

package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
)

// echoWorker answers inference.v1.echo.ping as the echo user, with the
// worker code next() names ("ok" for a pong with one audit fact), and
// records the Mhai-Request-Id of each call.
type echoWorker struct {
	mu   sync.Mutex
	rids []string
}

func startEcho(t *testing.T, next func() string, factEntity string) *echoWorker {
	t.Helper()
	nc, _, err := bus.Connect(context.Background(), echoURL, "", bus.WorkerInbox("echo"), bus.TLS{})
	if err != nil {
		t.Fatal(err)
	}
	w := &echoWorker{}
	sub, err := nc.QueueSubscribe("inference.v1.echo.ping", "echo", func(m *nats.Msg) {
		w.mu.Lock()
		w.rids = append(w.rids, m.Header.Get("Mhai-Request-Id"))
		w.mu.Unlock()
		out := nats.NewMsg(m.Reply)
		out.Header.Set("Mhai-Request-Id", m.Header.Get("Mhai-Request-Id"))
		code := next()
		out.Header.Set("Mhai-Status", code)
		if code == "ok" {
			var in intelpb.PingRequest
			_ = proto.Unmarshal(m.Data, &in)
			p, _ := structpb.NewStruct(nil) // echo.pinged's schema: no payload keys
			out.Header.Set("Mhai-Schema", "mhai.intelligence.v1.PingResponse")
			out.Data, _ = proto.Marshal(&intelpb.PingResponse{Text: in.Text, Meta: &intelpb.ResponseMeta{AuditFacts: []*intelpb.AuditFact{
				{Action: "intelligence.echo.pinged", Entity: "echo", EntityId: factEntity, Payload: p},
			}}})
		} else {
			out.Header.Set("Mhai-Schema", "mhai.intelligence.v1.Error")
			out.Data, _ = proto.Marshal(&intelpb.Error{Code: code})
		}
		_ = m.RespondMsg(out)
	})
	if err != nil {
		nc.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sub.Unsubscribe()
		_ = nc.Flush()
		nc.Close()
	})
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return w
}

// A17: a worker's unauthorized, forbidden and invalid_request look the same
// to the user: one fixed 500 internal_error, identical apart from the
// request id. Each is audited as intelligence.refused with its reason, and
// the worker saw Mhai-Request-Id = the response's X-Request-ID.
func TestRefusalsLookTheSame(t *testing.T) {
	e := newEnv(t)
	at, hid := e.clinicalToken(t, "clinician")
	codes := []string{"unauthorized", "forbidden", "invalid_request"}
	var mu sync.Mutex
	i := 0
	w := startEcho(t, func() string {
		mu.Lock()
		defer mu.Unlock()
		c := codes[i%len(codes)]
		i++
		return c
	}, "")
	var first resp
	var firstBody map[string]any
	for n, code := range codes {
		r := e.do(t, req{method: http.MethodGet, path: "/_test/echo", bearer: at})
		rid := r.hdr.Get("X-Request-ID")
		if r.code != 500 || r.body["code"] != "internal_error" || r.body["request_id"] != rid || rid == "" {
			t.Fatalf("%s: %d %s", code, r.code, r.raw)
		}
		body := map[string]any{}
		for k, v := range r.body {
			if k != "request_id" {
				body[k] = v
			}
		}
		h := r.hdr.Clone()
		h.Del("X-Request-ID")
		if n == 0 {
			first, firstBody = r, body
			first.hdr = h
		} else {
			if !equalHeaders(h, first.hdr) {
				t.Errorf("%s: headers %v, first %v", code, h, first.hdr)
			}
			bj, _ := json.Marshal(body)
			fj, _ := json.Marshal(firstBody)
			if string(bj) != string(fj) {
				t.Errorf("%s: body %s, first %s", code, bj, fj)
			}
		}
		w.mu.Lock()
		got := w.rids[len(w.rids)-1]
		w.mu.Unlock()
		if got != rid {
			t.Errorf("%s: worker saw Mhai-Request-Id %q, response X-Request-ID %q", code, got, rid)
		}
		var reason, op, actor, hosp string
		if err := super.QueryRow(context.Background(), `SELECT payload->>'reason', payload->>'operation', actor_id::text, hospital_id::text
			FROM audit_log WHERE action = 'intelligence.refused' AND request_id = $1`, rid).Scan(&reason, &op, &actor, &hosp); err != nil {
			t.Fatalf("%s: no intelligence.refused audit: %v", code, err)
		}
		if reason != code || op != "echo.ping" || hosp != hid.String() {
			t.Errorf("%s: audit reason %s op %s hospital %s", code, reason, op, hosp)
		}
	}
}

// The answer path: a worker's audit facts are appended with the caller's
// actor, hospital and request id.
func TestEchoFixtureAppendsFacts(t *testing.T) {
	e := newEnv(t)
	at, hid := e.clinicalToken(t, "clinician")
	entity := uuid.NewString()
	startEcho(t, func() string { return "ok" }, entity)
	r := e.do(t, req{method: http.MethodGet, path: "/_test/echo", bearer: at})
	if r.code != 200 || r.body["text"] != "synthetic ping" {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	var actor, hosp, rid string
	if err := super.QueryRow(context.Background(), `SELECT actor_id::text, hospital_id::text, request_id FROM audit_log
		WHERE action = 'intelligence.echo.pinged' AND entity_id = $1`, entity).Scan(&actor, &hosp, &rid); err != nil {
		t.Fatal(err)
	}
	if hosp != hid.String() || rid != r.hdr.Get("X-Request-ID") || actor == "" {
		t.Fatalf("fact row actor %s hospital %s rid %s", actor, hosp, rid)
	}
}

func equalHeaders(a, b http.Header) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !slices.Equal(v, b[k]) {
			return false
		}
	}
	return true
}
