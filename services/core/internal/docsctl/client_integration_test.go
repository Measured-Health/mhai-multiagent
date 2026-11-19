//go:build integration

package docsctl_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/docsctl"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docops"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// Core's client against the real nats.conf, with a stand-in documents
// responder signed in as the documents user: the token it receives is
// core's (aud documents, svc:core, the hospital as hid, bound to the
// subject and body), the reply comes back, and page_image's reply of the
// JPEG's full size fits NATS's payload. A refused request (no answer) is a
// timeout; no responder at all is ErrNoResponders. Synthetic ids only.
func TestClientAgainstNATS(t *testing.T) {
	ctx := context.Background()
	n, stop, err := testutil.NewNATS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := reqtoken.Signer{Key: priv, KeyID: reqtoken.KeyID(priv.Public().(ed25519.PublicKey))}
	core, err := nats.Connect(n.GatewayURL, nats.CustomInboxPrefix(bus.GatewayInboxPrefix))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	c := &docsctl.Client{NC: core, Signer: signer}
	hospital := uuid.New()

	// No documents service yet.
	if err := c.Request(ctx, hospital, "rid-1", "health", &docspb.HealthRequest{}, &docspb.HealthReply{}); !errors.Is(err, docsctl.ErrNoResponders) {
		t.Fatalf("no responder: %v", err)
	}

	docs, err := nats.Connect(n.DocumentsURL, nats.CustomInboxPrefix("_INBOX_worker.documents"))
	if err != nil {
		t.Fatal(err)
	}
	defer docs.Close()
	got := make(chan reqtoken.Claims, 4)
	sub, err := docs.QueueSubscribe("documents.v1.ctl.>", "documents", func(m *nats.Msg) {
		cl, err := reqtoken.VerifyAt(m.Header.Get(docsctl.HeaderToken), signer.PublicKeys(),
			reqtoken.Expect{Aud: "documents", Subject: m.Subject, Kind: reqtoken.KindReq}, m.Data, time.Now())
		if err != nil {
			return // refused: no answer
		}
		got <- cl
		op, _ := docops.Lookup(m.Subject)
		var rep proto.Message = &docspb.HealthReply{DatabaseReady: true}
		if op.Op == "page_image" {
			rep = &docspb.PageImageReply{Outcome: "ok", Jpeg: make([]byte, docops.MaxPageJPEG), Width: 1600, Height: 2200}
		}
		b, _ := proto.Marshal(rep)
		out := nats.NewMsg(m.Reply)
		out.Data = b
		out.Header.Set(docsctl.HeaderSchema, op.Response)
		out.Header.Set(docsctl.HeaderRequestID, cl.Rid)
		_ = m.RespondMsg(out)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	_ = docs.Flush()

	var h docspb.HealthReply
	if err := c.Request(ctx, hospital, "rid-2", "health", &docspb.HealthRequest{}, &h); err != nil || !h.GetDatabaseReady() {
		t.Fatalf("health: %v %v", err, h.GetDatabaseReady())
	}
	cl := <-got
	if cl.Aud != "documents" || cl.Sub != reqtoken.SvcCore || cl.Hid != hospital.String() || cl.Rid != "rid-2" ||
		len(cl.Roles) != 0 || cl.StepUp {
		t.Fatalf("claims: %+v", cl)
	}
	var p docspb.PageImageReply
	if err := c.Request(ctx, hospital, "rid-3", "page_image", &docspb.PageImageRequest{DocumentId: uuid.NewString(), N: 1,
		ViewerName: "Synthetic Viewer", At: time.Now().Unix()}, &p); err != nil || len(p.GetJpeg()) != docops.MaxPageJPEG {
		t.Fatalf("page_image: %v %d", err, len(p.GetJpeg()))
	}
	<-got

	// A responder that refuses (another core's key) never answers.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	stranger := &docsctl.Client{NC: core, Signer: reqtoken.Signer{Key: other, KeyID: reqtoken.KeyID(other.Public().(ed25519.PublicKey))}}
	tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := stranger.Request(tctx, hospital, "rid-4", "health", &docspb.HealthRequest{}, &docspb.HealthReply{}); !errors.Is(err, docsctl.ErrTimeout) {
		t.Fatalf("refused: %v", err)
	}
}
