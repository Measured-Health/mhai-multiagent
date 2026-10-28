//go:build integration

package intel

import (
	"context"
	"crypto/ed25519"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
)

// received is one message the fake worker got, with what its checks said.
type received struct {
	claims    reqtoken.Claims
	verifyErr error // reqtoken.VerifyAt, checks 1-4 of spec §7.3
	replyErr  error // reqtoken.CheckReply, check 5
	header    nats.Header
	subject   string
	reply     string
	body      []byte
}

// reply is what an answer function sends back; send false sends nothing.
type reply struct {
	status, schema string
	body           []byte
	send           bool
	requestID      string // overrides the echoed Mhai-Request-Id when set
}

// fakeWorker is a Go stand-in for an intelligence worker: it subscribes in
// the service's queue group with the service's own NATS user, verifies every
// message as the Python verifier will (reqtoken.VerifyAt, CheckReply), and
// answers as its answer function says. A message that fails check 5 gets no
// reply at all; one that fails checks 1-4 gets unauthorized.
type fakeWorker struct {
	mu  sync.Mutex
	got []received
	n   atomic.Int32
}

func (w *fakeWorker) received() []received {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]received(nil), w.got...)
}

func startFakeWorker(t *testing.T, url, service, op string, keys map[string]ed25519.PublicKey,
	answer func(reqtoken.Claims, *nats.Msg) reply) *fakeWorker {
	t.Helper()
	nc, _, err := bus.Connect(context.Background(), url, "", bus.WorkerInbox(service), bus.TLS{})
	if err != nil {
		t.Fatalf("fake worker connect: %v", err)
	}
	w := &fakeWorker{}
	sub, err := nc.QueueSubscribe("inference.v1."+service+"."+op, service, func(m *nats.Msg) {
		w.n.Add(1)
		c, verr := reqtoken.VerifyAt(m.Header.Get("Mhai-Request-Token"), keys,
			reqtoken.Expect{Aud: service, Subject: m.Subject, Kind: reqtoken.KindReq}, m.Data, time.Now())
		var rerr error
		if verr == nil {
			rerr = reqtoken.CheckReply(c, m.Reply, false)
		}
		w.mu.Lock()
		w.got = append(w.got, received{claims: c, verifyErr: verr, replyErr: rerr, header: m.Header,
			subject: m.Subject, reply: m.Reply, body: append([]byte(nil), m.Data...)})
		w.mu.Unlock()
		if rerr != nil {
			return
		}
		r := reply{status: "unauthorized", schema: errorSchema, body: errorBody("unauthorized", false), send: true}
		if verr == nil {
			r = answer(c, m)
		}
		if !r.send {
			return
		}
		out := nats.NewMsg(m.Reply)
		out.Header.Set(hdrStatus, r.status)
		out.Header.Set(hdrSchema, r.schema)
		rid := m.Header.Get(hdrRequestID)
		if r.requestID != "" {
			rid = r.requestID
		}
		out.Header.Set(hdrRequestID, rid)
		out.Data = r.body
		_ = m.RespondMsg(out)
	})
	if err != nil {
		nc.Close()
		t.Fatalf("fake worker subscribe: %v", err)
	}
	// Unsubscribe and flush before closing, so the server has dropped this
	// copy from the queue group before the next test sends anything.
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

func errorBody(code string, retryable bool) []byte {
	b, _ := proto.Marshal(&intelpb.Error{Code: code, Retryable: retryable})
	return b
}

// refuse answers every call with code.
func refuse(code string, retryable bool) func(reqtoken.Claims, *nats.Msg) reply {
	return func(reqtoken.Claims, *nats.Msg) reply {
		return reply{status: code, schema: errorSchema, body: errorBody(code, retryable), send: true}
	}
}

// pong echoes a PingRequest's text, with meta.
func pong(meta *intelpb.ResponseMeta) func(reqtoken.Claims, *nats.Msg) reply {
	return func(_ reqtoken.Claims, m *nats.Msg) reply {
		var in intelpb.PingRequest
		if err := proto.Unmarshal(m.Data, &in); err != nil {
			return reply{status: "invalid_request", schema: errorSchema, body: errorBody("invalid_request", false), send: true}
		}
		b, _ := proto.Marshal(&intelpb.PingResponse{Meta: meta, Text: in.Text})
		return reply{status: "ok", schema: "mhai.intelligence.v1.PingResponse", body: b, send: true}
	}
}

// silent never answers.
func silent(reqtoken.Claims, *nats.Msg) reply { return reply{} }
