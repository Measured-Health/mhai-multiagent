//go:build integration

package scan

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
)

// natsImage is core's test NATS (services/core/internal/testutil).
const natsImage = "nats:2.10-alpine"

func startNATS(t *testing.T) jetstream.JetStream {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: natsImage, Cmd: []string{"-js"}, ExposedPorts: []string{"4222/tcp"},
			WaitingFor: wait.ForLog("Server is ready").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start nats: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	host, _ := c.Host(ctx)
	port, err := c.MappedPort(ctx, "4222/tcp")
	if err != nil {
		t.Fatal(err)
	}
	nc, err := nats.Connect("nats://" + host + ":" + port.Port())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	return js
}

// The scan over real JetStream: an uploaded message on DOCS reaches the
// documents_scan durable, the scan runs, the message is acked (gone from
// the work queue) and scanned is published, de-duplicated by its id.
func TestConsumerOverJetStream(t *testing.T) {
	js := startNATS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// The stream and durable as core's bus.EnsureDocsStream makes them.
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: Stream,
		Subjects:  []string{"documents.v1.*.uploaded", "documents.v1.*.scanned"},
		Retention: jetstream.WorkQueuePolicy, Duplicates: 2 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateOrUpdateConsumer(ctx, Stream, jetstream.ConsumerConfig{Durable: Durable,
		FilterSubject: "documents.v1.*.uploaded", AckPolicy: jetstream.AckExplicitPolicy, AckWait: AckWait,
		MaxDeliver: MaxDeliver}); err != nil {
		t.Fatal(err)
	}
	b, _ := proto.Marshal(&docspb.Uploaded{DocumentId: document.String()})
	if _, err := js.Publish(ctx, "documents.v1."+hospital.String()+".uploaded", b); err != nil {
		t.Fatal(err)
	}
	r := newRig()
	r.s.Announce = JSAnnouncer{JS: js}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		(&Consumer{Scanner: r.s}).Run(runCtx, js)
		close(done)
	}()
	st, err := js.Stream(ctx, Stream)
	if err != nil {
		t.Fatal(err)
	}
	for {
		info, err := st.Info(ctx, jetstream.WithSubjectFilter("documents.v1.>"))
		if err != nil {
			t.Fatal(err)
		}
		if info.State.Subjects["documents.v1."+hospital.String()+".scanned"] == 1 &&
			info.State.Subjects["documents.v1."+hospital.String()+".uploaded"] == 0 {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("not scanned: %+v", info.State.Subjects)
		}
		time.Sleep(200 * time.Millisecond)
	}
	stop()
	<-done
	if r.rows.status != "ready" {
		t.Fatalf("row %q", r.rows.status)
	}
	// The announcement: ids and a code, and a second one is a duplicate.
	msg, err := st.GetLastMsgForSubject(ctx, "documents.v1."+hospital.String()+".scanned")
	if err != nil {
		t.Fatal(err)
	}
	var sc docspb.Scanned
	if err := proto.Unmarshal(msg.Data, &sc); err != nil || sc.GetDocumentId() != document.String() ||
		sc.GetOutcome() != OutcomeReady || sc.GetPages() != 2 {
		t.Fatalf("scanned %+v %v", &sc, err)
	}
	if err := (JSAnnouncer{JS: js}).Scanned(ctx, hospital, document, OutcomeReady, "", 2); err != nil {
		t.Fatal(err)
	}
	info, _ := st.Info(ctx, jetstream.WithSubjectFilter("documents.v1.>"))
	if info.State.Subjects["documents.v1."+hospital.String()+".scanned"] != 1 {
		t.Fatalf("duplicate kept: %+v", info.State.Subjects)
	}
}
