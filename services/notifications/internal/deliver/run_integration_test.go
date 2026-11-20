//go:build integration

package deliver

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/notifypb"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/channel"
)

func startJS(t *testing.T) jetstream.JetStream {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "nats:2.10-alpine", Cmd: []string{"-js"}, ExposedPorts: []string{"4222/tcp"},
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

// Run pulls notify_send over real JetStream once core has made it: a due
// message is claimed, sent and acked (gone from the work queue). A dead
// letter goes onto DEAD as ids and a code, once per message.
func TestRunAndDeadLetterOverJetStream(t *testing.T) {
	js := startJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: Stream, Subjects: []string{"notify.v1.*.due"},
		Retention: jetstream.WorkQueuePolicy, Duplicates: 2 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateOrUpdateConsumer(ctx, Stream, jetstream.ConsumerConfig{Durable: Durable,
		FilterSubject: "notify.v1.*.due", AckPolicy: jetstream.AckExplicitPolicy, AckWait: Lease}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "DEAD", Subjects: []string{"dead.v1.>"}}); err != nil {
		t.Fatal(err)
	}
	r := newRig(Claimed)
	r.s.Now = nil
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { r.s.Run(runCtx, js); close(done) }()
	h, id := uuid.New(), uuid.New()
	b, _ := proto.Marshal(&notifypb.Due{MessageId: id.String()})
	if _, err := js.Publish(ctx, "notify.v1."+h.String()+".due", b, jetstream.WithMsgID(id.String())); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for r.s.metrics().Sent.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	<-done
	if r.s.metrics().Sent.Load() != 1 || r.ch.sends != 1 {
		t.Fatalf("sent %d", r.s.metrics().Sent.Load())
	}
	st, _ := js.Stream(ctx, Stream)
	if info, err := st.Info(ctx); err != nil || info.State.Msgs != 0 {
		t.Fatalf("the due message stays: %+v %v", info, err)
	}
	dead := JSDead{JS: js}
	for i := 0; i < 2; i++ {
		if err := dead.Dead(ctx, h, id, "provider_down", 9); err != nil {
			t.Fatal(err)
		}
	}
	ds, _ := js.Stream(ctx, "DEAD")
	m, err := ds.GetLastMsgForSubject(ctx, "dead.v1."+h.String()+".notify_send")
	if err != nil {
		t.Fatal(err)
	}
	var dl notifypb.DeadLetter
	if err := proto.Unmarshal(m.Data, &dl); err != nil || dl.GetMessageId() != id.String() || dl.GetOutcome() != "provider_down" ||
		dl.GetAttempts() != 9 {
		t.Fatalf("%v %v", &dl, err)
	}
	if info, _ := ds.Info(ctx); info.State.Msgs != 1 {
		t.Fatalf("%d dead letters, want 1", info.State.Msgs)
	}
	_ = channel.Accepted
}
