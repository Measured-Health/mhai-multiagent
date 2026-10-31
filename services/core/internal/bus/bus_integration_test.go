//go:build integration

package bus_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

var sharedNATS *testutil.NATS

// devStreams is what compose runs: one replica, the default ack window.
var devStreams = bus.StreamConfig{Replicas: 1, PipelineMaxAckPending: 8}

func TestMain(m *testing.M) {
	ctx := context.Background()
	code := func() int {
		n, stop, err := testutil.NewNATS(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "start nats:", err)
			return 1
		}
		defer stop()
		sharedNATS = n
		// core serve does this at start-up; every test sees the streams.
		nc, js, err := bus.Connect(ctx, n.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{})
		if err != nil {
			fmt.Fprintln(os.Stderr, "connect:", err)
			return 1
		}
		defer nc.Close()
		if err := bus.EnsureStreams(ctx, js, devStreams); err != nil {
			fmt.Fprintln(os.Stderr, "ensure streams:", err)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}

// svc is a raw client for one NATS user whose async errors (permission
// violations) are collected.
type svc struct {
	nc   *nats.Conn
	errs chan error
}

func raw(t *testing.T, url, inboxPrefix string) *svc {
	t.Helper()
	s := &svc{errs: make(chan error, 64)}
	nc, err := nats.Connect(url, nats.CustomInboxPrefix(inboxPrefix),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			select {
			case s.errs <- err:
			default:
			}
		}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	s.nc = nc
	return s
}

func gateway(t *testing.T) *svc { return raw(t, sharedNATS.GatewayURL, bus.GatewayInboxPrefix) }
func pipeline(t *testing.T) *svc {
	return raw(t, sharedNATS.PipelineURL, bus.WorkerInbox("pipeline"))
}
func echo(t *testing.T) *svc { return raw(t, sharedNATS.EchoURL, bus.WorkerInbox("echo")) }
func bffClinical(t *testing.T) *svc {
	return raw(t, sharedNATS.BFFClinicalURL, bus.BFFInbox("clinical"))
}
func bffPlatform(t *testing.T) *svc {
	return raw(t, sharedNATS.BFFPlatformURL, bus.BFFInbox("platform"))
}

// violation waits for a permissions violation that names subject.
func (s *svc) violation(t *testing.T, subject string) {
	t.Helper()
	_ = s.nc.Flush()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case err := <-s.errs:
			if strings.Contains(strings.ToLower(err.Error()), "permissions violation") && strings.Contains(err.Error(), subject) {
				return
			}
		case <-deadline:
			t.Fatalf("no permissions violation naming %q", subject)
		}
	}
}

func (s *svc) noViolation(t *testing.T) {
	t.Helper()
	_ = s.nc.Flush()
	select {
	case err := <-s.errs:
		t.Fatalf("unexpected error: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
}

// cannotPublish checks each subject is refused for the user that mk makes.
func cannotPublish(t *testing.T, mk func(*testing.T) *svc, subjects ...string) {
	t.Helper()
	for _, subj := range subjects {
		s := mk(t)
		_ = s.nc.Publish(subj, []byte("x"))
		s.violation(t, subj)
	}
}

// cannotSubscribe checks each subject is refused, plainly and in a queue.
func cannotSubscribe(t *testing.T, mk func(*testing.T) *svc, subjects ...string) {
	t.Helper()
	for _, subj := range subjects {
		s := mk(t)
		_, _ = s.nc.Subscribe(subj, func(*nats.Msg) {})
		s.violation(t, subj)
	}
}

func canPublish(t *testing.T, mk func(*testing.T) *svc, subjects ...string) {
	t.Helper()
	for _, subj := range subjects {
		s := mk(t)
		if err := s.nc.Publish(subj, []byte("x")); err != nil {
			t.Fatalf("publish %s: %v", subj, err)
		}
		s.noViolation(t)
	}
}

func jobSubject(op string) string { return "jobs.v1." + uuid.NewString() + "." + op }

// --- A2, A16: a service user publishes no requests or jobs, reads nobody else's subjects.

func TestServiceUserCannotPublishRequestsOrJobs(t *testing.T) {
	for _, mk := range []func(*testing.T) *svc{pipeline, echo} {
		cannotPublish(t, mk, "inference.v1.echo.ping", "inference.v1.pipeline.health",
			jobSubject("run_query"), "jobs.v1.global.retention_purge")
	}
}

// The message is not only refused but never delivered to the subscriber.
func TestPipelineCannotPublishInference(t *testing.T) {
	listener := pipeline(t)
	got := make(chan struct{}, 1)
	if _, err := listener.nc.QueueSubscribe("inference.v1.pipeline.>", "pipeline", func(*nats.Msg) { got <- struct{}{} }); err != nil {
		t.Fatal(err)
	}
	_ = listener.nc.Flush()
	w := pipeline(t)
	if err := w.nc.Publish("inference.v1.pipeline.health", []byte("x")); err != nil {
		t.Fatalf("publish call itself: %v", err)
	}
	w.violation(t, "inference.v1.pipeline.health")
	select {
	case <-got:
		t.Fatal("a message the worker was not allowed to publish was delivered")
	case <-time.After(500 * time.Millisecond):
	}
}

func TestServiceUserCannotSubscribeOthers(t *testing.T) {
	cannotSubscribe(t, pipeline, bus.GatewayInboxPrefix+".>", "inference.v1.echo.>", bus.WorkerInbox("echo")+".>",
		"_INBOX_worker.>", "_INBOX.>", "_INBOX_bff.>", "inference.v1.>", "jobs.v1.>", "dead.v1.>",
		"api.v1.>", "authz.v1.grant.>", "$JS.>")
	cannotSubscribe(t, echo, "inference.v1.pipeline.>", bus.WorkerInbox("pipeline")+".>", bus.GatewayInboxPrefix+".>")
}

// A worker subscribes its subjects only in its own queue group, so a
// second, stray subscriber can't take a copy of every request.
func TestServiceSubscribesOnlyInItsQueueGroup(t *testing.T) {
	p := pipeline(t)
	_, _ = p.nc.Subscribe("inference.v1.pipeline.>", func(*nats.Msg) {})
	p.violation(t, "inference.v1.pipeline.>")
	p = pipeline(t)
	_, _ = p.nc.QueueSubscribe("inference.v1.pipeline.>", "echo", func(*nats.Msg) {})
	p.violation(t, "inference.v1.pipeline.>")
	g := gateway(t)
	_, _ = g.nc.Subscribe("api.v1.>", func(*nats.Msg) {})
	g.violation(t, "api.v1.>")
	g = gateway(t)
	_, _ = g.nc.QueueSubscribe("authz.v1.grant.>", "notcore", func(*nats.Msg) {})
	g.violation(t, "authz.v1.grant.>")
	// Core's permission is "api.v1.* core", but NATS 2.10 accepts a
	// subscription to api.v1.> under it (its subscribe check matches the
	// '>' token against '*'). What bounds core's API traffic is the
	// publish side: nobody can publish below api.v1.<app>.
	cannotPublish(t, bffClinical, "api.v1.clinical.x", "api.v1.clinical.>")
	cannotPublish(t, bffPlatform, "api.v1.platform.x")
}

func TestServiceUserHasNoKVAccess(t *testing.T) {
	for _, mk := range []func(*testing.T) *svc{pipeline, echo} {
		cannotPublish(t, mk, "$KV.denylist.x", "$JS.API.DIRECT.GET.KV_denylist.$KV.denylist.x",
			"$JS.API.STREAM.INFO.KV_denylist", "$JS.API.STREAM.DELETE.KV_denylist",
			"$JS.API.STREAM.MSG.GET.KV_denylist", "$JS.API.INFO")
		cannotSubscribe(t, mk, "$KV.>")
	}
}

func TestServiceUserCannotTouchOtherConsumers(t *testing.T) {
	cannotPublish(t, pipeline,
		"$JS.API.CONSUMER.MSG.NEXT.DEAD.core_dead", "$JS.ACK.DEAD.core_dead.x",
		"$JS.API.CONSUMER.CREATE.JOBS.x", "$JS.API.CONSUMER.CREATE.JOBS.pipeline.jobs.v1.>",
		"$JS.API.CONSUMER.DURABLE.CREATE.JOBS.x", "$JS.API.CONSUMER.DELETE.JOBS.pipeline",
		"$JS.API.CONSUMER.INFO.JOBS.other", "$JS.API.CONSUMER.MSG.NEXT.JOBS.other", "$JS.ACK.JOBS.other.x",
		"$JS.API.STREAM.INFO.JOBS", "$JS.API.STREAM.PURGE.JOBS", "$JS.API.STREAM.MSG.DELETE.JOBS")
	// echo has no job consumer at all.
	cannotPublish(t, echo, "$JS.API.CONSUMER.INFO.JOBS.pipeline", "$JS.API.CONSUMER.MSG.NEXT.JOBS.pipeline",
		"$JS.ACK.JOBS.pipeline.x", "dead.v1.global.x")

	// Through the client library: creating a consumer fails.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p := pipeline(t)
	js, err := jetstream.New(p.nc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateOrUpdateConsumer(ctx, bus.JobsStream, jetstream.ConsumerConfig{Durable: "rogue"}); err == nil {
		t.Fatal("pipeline created a consumer")
	}
}

// The pipeline worker can use its own durable consumer: info, fetch and ack.
func TestPipelineUsesItsConsumer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := gateway(t)
	gjs, _ := jetstream.New(g.nc)
	subj := jobSubject("run_query")
	if _, err := gjs.Publish(ctx, subj, []byte("job-1")); err != nil {
		t.Fatalf("gateway publish job: %v", err)
	}
	p := pipeline(t)
	js, _ := jetstream.New(p.nc)
	cons, err := js.Consumer(ctx, bus.JobsStream, bus.PipelineConsumer)
	if err != nil {
		t.Fatalf("consumer info as pipeline: %v", err)
	}
	for {
		batch, err := cons.Fetch(1, jetstream.FetchMaxWait(3*time.Second))
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		var found bool
		for m := range batch.Messages() {
			if err := m.DoubleAck(ctx); err != nil {
				t.Fatalf("ack: %v", err)
			}
			found = m.Subject() == subj
		}
		if batch.Error() != nil {
			t.Fatalf("fetch: %v", batch.Error())
		}
		if found {
			break
		}
	}
	p.noViolation(t)
}

func TestPipelineCanPublishDead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p := pipeline(t)
	js, _ := jetstream.New(p.nc)
	// A JetStream publish: the stream's ack comes back on the worker's inbox.
	if _, err := js.Publish(ctx, "dead.v1."+uuid.NewString()+".run_query", []byte("x")); err != nil {
		t.Fatalf("dead letter: %v", err)
	}
	p.noViolation(t)
}

// --- A26, A27: the BFF users reach only their API subject (and, clinical, bff_direct).

func TestBFFUserCannotReachJobsKVOrGrants(t *testing.T) {
	cannotPublish(t, bffClinical, jobSubject("run_query"), "jobs.v1.global.retention_purge",
		"$KV.denylist.x", "$JS.API.INFO", "$JS.API.STREAM.INFO.JOBS", "$JS.API.CONSUMER.MSG.NEXT.JOBS.pipeline",
		"$JS.ACK.JOBS.pipeline.x", "dead.v1.global.x", "authz.v1.grant.pipeline", "authz.v1.grant.echo",
		"api.v1.platform", "inference.v1.pipeline.health", "inference.v1.echo.other",
		bus.GatewayInboxPrefix+".x", bus.WorkerInbox("echo")+".x")
	canPublish(t, bffClinical, "api.v1.clinical", "inference.v1.echo.ping")

	cannotPublish(t, bffPlatform, "api.v1.clinical", "inference.v1.echo.ping", "inference.v1.pipeline.health",
		jobSubject("run_query"), "$KV.denylist.x", "$JS.API.INFO", "dead.v1.global.x", "authz.v1.grant.echo",
		bus.BFFInbox("clinical")+".x")
	canPublish(t, bffPlatform, "api.v1.platform")
}

func TestBFFUserCannotSubscribeOthers(t *testing.T) {
	cannotSubscribe(t, bffClinical, bus.GatewayInboxPrefix+".>", "_INBOX_worker.>", bus.BFFInbox("platform")+".>",
		"_INBOX_bff.>", "_INBOX.>", "api.v1.>", "api.v1.clinical", "inference.v1.>", "jobs.v1.>", "dead.v1.>",
		"authz.v1.>", "$KV.>", "$JS.>")
	cannotSubscribe(t, bffPlatform, bus.BFFInbox("clinical")+".>", "_INBOX_bff.>", "api.v1.>", "inference.v1.>")
}

// --- A28, A30: no worker publishes the API; each worker only its own grant subject.

func TestWorkerCannotPublishAPI(t *testing.T) {
	for _, mk := range []func(*testing.T) *svc{pipeline, echo} {
		cannotPublish(t, mk, "api.v1.clinical", "api.v1.platform")
	}
	// Core doesn't publish the API either: only the BFF puts a request there.
	cannotPublish(t, gateway, "api.v1.clinical", "api.v1.platform")
}

func TestWorkerGrantSubjectIsItsOwn(t *testing.T) {
	cannotPublish(t, pipeline, "authz.v1.grant.echo", "authz.v1.grant.x")
	canPublish(t, pipeline, "authz.v1.grant.pipeline")
	cannotPublish(t, echo, "authz.v1.grant.pipeline")
	canPublish(t, echo, "authz.v1.grant.echo")
	cannotPublish(t, gateway, "authz.v1.grant.pipeline")
}

// --- allow_responses: core answers the BFF and the workers, once, and never unprompted.

func TestCoreAnswersBFFThroughAllowResponses(t *testing.T) {
	g := gateway(t)
	second := make(chan string, 1)
	if _, err := g.nc.QueueSubscribe("api.v1.*", "core", func(m *nats.Msg) {
		_ = m.Respond([]byte("ok"))
		second <- m.Reply
	}); err != nil {
		t.Fatal(err)
	}
	_ = g.nc.Flush()
	b := bffClinical(t)
	rep, err := b.nc.Request("api.v1.clinical", []byte("GET /me"), 3*time.Second)
	if err != nil || string(rep.Data) != "ok" {
		t.Fatalf("request: %v", err)
	}
	g.noViolation(t)
	// A second answer to the same request is refused (max: 1).
	reply := <-second
	_ = g.nc.Publish(reply, []byte("again"))
	g.violation(t, reply)
	// And core can't write to the BFF's inbox unprompted.
	cannotPublish(t, gateway, bus.BFFInbox("clinical")+".unprompted", bus.BFFInbox("platform")+".unprompted")
}

func TestCoreAnswersGrantThroughAllowResponses(t *testing.T) {
	g := gateway(t)
	if _, err := g.nc.QueueSubscribe("authz.v1.grant.>", "core", func(m *nats.Msg) { _ = m.Respond([]byte("granted")) }); err != nil {
		t.Fatal(err)
	}
	_ = g.nc.Flush()
	p := pipeline(t)
	rep, err := p.nc.Request("authz.v1.grant.pipeline", []byte("grant?"), 3*time.Second)
	if err != nil || string(rep.Data) != "granted" {
		t.Fatalf("grant request: %v", err)
	}
	g.noViolation(t)
	cannotPublish(t, gateway, bus.WorkerInbox("pipeline")+".unprompted", bus.WorkerInbox("echo")+".unprompted")
}

func TestAllowResponsesOnlyOnce(t *testing.T) {
	for name, mk := range map[string]func(*testing.T) *svc{"pipeline": pipeline, "echo": echo} {
		w := mk(t)
		replies := make(chan string, 1)
		if _, err := w.nc.QueueSubscribe("inference.v1."+name+".>", name, func(m *nats.Msg) {
			_ = m.Respond([]byte("first"))
			replies <- m.Reply
		}); err != nil {
			t.Fatal(err)
		}
		_ = w.nc.Flush()
		g := gateway(t)
		if _, err := g.nc.Request("inference.v1."+name+".op", nil, 3*time.Second); err != nil {
			t.Fatalf("%s: request: %v", name, err)
		}
		reply := <-replies
		w.noViolation(t)
		_ = w.nc.Publish(reply, []byte("second"))
		w.violation(t, reply)
	}
}

// The response permission lapses after expires (1m), even unused.
func TestAllowResponsesExpire(t *testing.T) {
	w := echo(t)
	replies := make(chan string, 1)
	if _, err := w.nc.QueueSubscribe("inference.v1.echo.>", "echo", func(m *nats.Msg) { replies <- m.Reply }); err != nil {
		t.Fatal(err)
	}
	_ = w.nc.Flush()
	g := gateway(t)
	inbox := g.nc.NewInbox()
	if _, err := g.nc.SubscribeSync(inbox); err != nil {
		t.Fatal(err)
	}
	if err := g.nc.PublishRequest("inference.v1.echo.ping", inbox, nil); err != nil {
		t.Fatal(err)
	}
	reply := <-replies
	time.Sleep(61 * time.Second)
	_ = w.nc.Publish(reply, []byte("late"))
	w.violation(t, reply)
}

// NATS does not check the reply subject a requester sets: a responder with
// allow_responses publishes its one reply to whatever subject the request
// named, past its allow list and its deny list. This proves the reply is
// delivered (or acted on) for three kinds of target, so every responder
// (core's api.v1 and grant handlers, every worker) must refuse a reply
// subject outside its expected inbox prefix before it answers (README,
// "Responders must check the reply subject"). It is a tripwire: if a NATS
// upgrade starts checking reply subjects, it fails and the note can go.
func TestReplySubjectIsTheResponderToCheck(t *testing.T) {
	// Its own server: the stray job and denylist key stay off the shared one.
	n := freshNATS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	g := raw(t, n.GatewayURL, bus.GatewayInboxPrefix)
	if _, err := g.nc.QueueSubscribe("api.v1.*", "core", func(m *nats.Msg) { _ = m.Respond([]byte("core-answer")) }); err != nil {
		t.Fatal(err)
	}
	_ = g.nc.Flush()
	b := raw(t, n.BFFClinicalURL, bus.BFFInbox("clinical"))

	// (a) Another app's inbox: the platform BFF receives core's answer to a
	// clinical request.
	p := raw(t, n.BFFPlatformURL, bus.BFFInbox("platform"))
	probe := bus.BFFInbox("platform") + ".probe"
	sub, err := p.nc.SubscribeSync(probe)
	if err != nil {
		t.Fatal(err)
	}
	_ = p.nc.Flush()
	if err := b.nc.PublishRequest("api.v1.clinical", probe, nil); err != nil {
		t.Fatal(err)
	}
	if m, err := sub.NextMsg(3 * time.Second); err != nil || string(m.Data) != "core-answer" {
		t.Fatalf("reply to another app's inbox not delivered: %v", err)
	}

	// (b) A job: the BFF, which may not publish jobs.v1, gets one stored on
	// JOBS through core's reply.
	js, _ := jetstream.New(g.nc)
	job := jobSubject("run_query")
	if err := b.nc.PublishRequest("api.v1.clinical", job, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return subjectCount(ctx, t, js, bus.JobsStream, job) == 1 },
		"reply to a jobs.v1 subject not stored on JOBS")

	// (c) A KV write with core's account: a denylist entry appears.
	buckets, err := bus.OpenBuckets(ctx, js, 1)
	if err != nil {
		t.Fatal(err)
	}
	key := "probe" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := b.nc.PublishRequest("api.v1.clinical", "$KV."+bus.DenylistBucket+"."+key, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { _, err := buckets.Denylist.Get(ctx, key); return err == nil },
		"reply to a $KV subject not written")

	// (d) A JetStream API call as core: not refused by NATS (a delete or
	// purge of a real stream would go the same way; this one names none).
	if err := b.nc.PublishRequest("api.v1.clinical", "$JS.API.STREAM.DELETE.NO_SUCH_STREAM", nil); err != nil {
		t.Fatal(err)
	}
	b.noViolation(t)
	g.noViolation(t)
}

func waitFor(t *testing.T, d time.Duration, ok func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// --- Queue groups: two copies of each subscriber share the work.

// share sends n requests on subject from requester and checks two copies
// subscribed in queue share them, each request answered exactly once.
func share(t *testing.T, copies [2]*svc, pattern, queue string, requester *svc, subject string) {
	t.Helper()
	const n = 20
	var mu sync.Mutex
	counts := [2]int{}
	for i, c := range copies {
		i := i
		sub, err := c.nc.QueueSubscribe(pattern, queue, func(m *nats.Msg) {
			mu.Lock()
			counts[i]++
			mu.Unlock()
			_ = m.Respond(m.Data)
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = sub.Unsubscribe() }()
		if err := c.nc.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		body := []byte(fmt.Sprint(i))
		rep, err := requester.nc.Request(subject, body, 3*time.Second)
		if err != nil || !bytes.Equal(rep.Data, body) {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if counts[0]+counts[1] != n || counts[0] == 0 || counts[1] == 0 {
		t.Fatalf("two copies of %s shared %d requests as %v", queue, n, counts)
	}
}

func TestTwoWorkerCopiesShareRequests(t *testing.T) {
	share(t, [2]*svc{pipeline(t), pipeline(t)}, "inference.v1.pipeline.>", "pipeline", gateway(t), "inference.v1.pipeline.health")
	share(t, [2]*svc{echo(t), echo(t)}, "inference.v1.echo.>", "echo", gateway(t), "inference.v1.echo.ping")
}

func TestTwoCoreCopiesShareAPI(t *testing.T) {
	share(t, [2]*svc{gateway(t), gateway(t)}, "api.v1.*", "core", bffClinical(t), "api.v1.clinical")
	share(t, [2]*svc{gateway(t), gateway(t)}, "api.v1.*", "core", bffPlatform(t), "api.v1.platform")
}

func TestTwoCoreCopiesShareGrants(t *testing.T) {
	share(t, [2]*svc{gateway(t), gateway(t)}, "authz.v1.grant.>", "core", pipeline(t), "authz.v1.grant.pipeline")
}

// The BFF calls a bff_direct operation; two echo copies share the calls.
func TestTwoEchoCopiesShareBFFDirectCalls(t *testing.T) {
	share(t, [2]*svc{echo(t), echo(t)}, "inference.v1.echo.>", "echo", bffClinical(t), "inference.v1.echo.ping")
}

// fetchN pulls until it has n messages whose body starts with prefix,
// acking every message it sees, and returns those bodies.
func fetchN(ctx context.Context, t *testing.T, c jetstream.Consumer, prefix string, n int) ([]string, error) {
	var got []string
	for len(got) < n {
		if ctx.Err() != nil {
			return got, ctx.Err()
		}
		batch, err := c.Fetch(1, jetstream.FetchMaxWait(2*time.Second))
		if err != nil {
			return got, err
		}
		for m := range batch.Messages() {
			if strings.HasPrefix(string(m.Data()), prefix) {
				got = append(got, string(m.Data()))
			}
			if err := m.DoubleAck(ctx); err != nil {
				return got, err
			}
		}
	}
	return got, nil
}

// sharePull runs two copies on one durable consumer; each takes half and
// no message reaches both.
func sharePull(t *testing.T, mk func(*testing.T) *svc, stream, consumer, prefix string, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	res := make([][]string, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		s := mk(t)
		js, _ := jetstream.New(s.nc)
		c, err := js.Consumer(ctx, stream, consumer)
		if err != nil {
			t.Fatalf("copy %d: consumer: %v", i, err)
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res[i], errs[i] = fetchN(ctx, t, c, prefix, n/2)
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		if errs[i] != nil {
			t.Fatalf("copy %d: %v", i, errs[i])
		}
		for _, b := range res[i] {
			if seen[b] {
				t.Fatalf("%s delivered to both copies", b)
			}
			seen[b] = true
		}
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct messages, want %d", len(seen), n)
	}
}

func TestTwoPipelineCopiesShareJobs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gjs, _ := jetstream.New(gateway(t).nc)
	prefix := "share-" + uuid.NewString() + "-"
	for i := 0; i < 20; i++ {
		if _, err := gjs.Publish(ctx, jobSubject("run_query"), []byte(fmt.Sprint(prefix, i))); err != nil {
			t.Fatal(err)
		}
	}
	sharePull(t, pipeline, bus.JobsStream, bus.PipelineConsumer, prefix, 20)
}

func TestTwoCoreCopiesShareDeadLetters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pjs, _ := jetstream.New(pipeline(t).nc)
	prefix := "dead-" + uuid.NewString() + "-"
	for i := 0; i < 10; i++ {
		if _, err := pjs.Publish(ctx, "dead.v1."+uuid.NewString()+".run_query", []byte(fmt.Sprint(prefix, i))); err != nil {
			t.Fatal(err)
		}
	}
	sharePull(t, gateway, bus.DeadStream, bus.CoreDeadConsumer, prefix, 10)
}

// --- Gateway: narrowed JetStream rights.

func TestGatewayCannotDestroyStreams(t *testing.T) {
	cannotPublish(t, gateway, "$JS.API.STREAM.DELETE.KV_denylist", "$JS.API.STREAM.PURGE.KV_denylist",
		"$JS.API.STREAM.DELETE.JOBS", "$JS.API.STREAM.PURGE.JOBS", "$JS.API.STREAM.DELETE.DEAD",
		"$JS.API.STREAM.PURGE.DEAD", "$JS.API.STREAM.MSG.DELETE.JOBS", "$JS.API.CONSUMER.DELETE.JOBS.pipeline",
		"$JS.API.CONSUMER.DELETE.DEAD.core_dead")
}

func TestGatewayCannotReadOtherStreams(t *testing.T) {
	cannotPublish(t, gateway, "$JS.API.STREAM.CREATE.OTHER", "$JS.API.STREAM.UPDATE.OTHER", "$JS.API.STREAM.INFO.OTHER",
		"$JS.API.STREAM.MSG.GET.OTHER", "$JS.API.DIRECT.GET.OTHER", "$JS.API.DIRECT.GET.OTHER.x",
		"$JS.API.CONSUMER.CREATE.OTHER.x", "$JS.API.CONSUMER.INFO.OTHER.x", "$JS.API.CONSUMER.MSG.NEXT.OTHER.x",
		"$JS.API.STREAM.CREATE.KV_other", "$JS.API.STREAM.NAMES", "$JS.API.STREAM.LIST",
		// Jobs are the workers' to read; dead letters only through core_dead.
		"$JS.API.STREAM.MSG.GET.JOBS", "$JS.API.CONSUMER.MSG.NEXT.JOBS.pipeline", "$JS.ACK.JOBS.pipeline.x",
		"$JS.API.CONSUMER.MSG.NEXT.DEAD.other", "$JS.API.STREAM.MSG.GET.DEAD",
		// KV subjects of the four buckets only.
		"$KV.other.x", "$KV.KV_denylist.x")
	cannotSubscribe(t, gateway, "jobs.v1.>", "dead.v1.>", "inference.v1.>", bus.WorkerInbox("pipeline")+".>", "_INBOX_bff.>")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	js, _ := jetstream.New(gateway(t).nc)
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "OTHER", Subjects: []string{"other.>"}}); err == nil {
		t.Fatal("gateway created stream OTHER")
	}
	if _, err := js.Stream(ctx, "OTHER"); err == nil {
		t.Fatal("gateway read stream OTHER")
	}
}

func TestDefaultDeny(t *testing.T) {
	for _, mk := range []func(*testing.T) *svc{gateway, pipeline, echo, bffClinical, bffPlatform} {
		cannotPublish(t, mk, "unlisted.v1.x", "_INBOX.abc", "stream.v1.h.r")
		cannotSubscribe(t, mk, "unlisted.>", ">")
	}
	cannotPublish(t, gateway, "dead.v1.h1.query")
}

func TestAnonymousRefused(t *testing.T) {
	if nc, err := nats.Connect("nats://"+sharedNATS.Addr, nats.Timeout(3*time.Second)); err == nil {
		nc.Close()
		t.Fatal("anonymous connection accepted")
	}
}

// --- A18: max_payload.

func TestMaxPayloadRefusesTwoMB(t *testing.T) {
	g := gateway(t)
	if got := g.nc.MaxPayload(); got != 1<<20 {
		t.Fatalf("server max_payload = %d, want 1 MB", got)
	}
	err := g.nc.Publish("inference.v1.echo.ping", make([]byte, 2<<20))
	if !errors.Is(err, nats.ErrMaxPayload) {
		t.Fatalf("2 MB publish: want ErrMaxPayload, got %v", err)
	}
}

// --- Streams.

func TestEnsureStreamsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nc, js, err := bus.Connect(ctx, sharedNATS.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{})
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	for i := 0; i < 2; i++ {
		if err := bus.EnsureStreams(ctx, js, devStreams); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}
	jobs, err := js.Stream(ctx, bus.JobsStream)
	if err != nil {
		t.Fatal(err)
	}
	jc := jobs.CachedInfo().Config
	if len(jc.Subjects) != 1 || jc.Subjects[0] != "jobs.v1.>" || jc.Storage != jetstream.FileStorage ||
		jc.Retention != jetstream.WorkQueuePolicy || jc.Discard != jetstream.DiscardNew || !jc.DiscardNewPerSubject ||
		jc.MaxMsgsPerSubject != 200 || jc.MaxAge != time.Hour || jc.Duplicates != 2*time.Minute || jc.Replicas != 1 ||
		jc.MaxMsgSize != 72<<10 || jc.MaxBytes != 256<<20 {
		t.Fatalf("JOBS config = %+v", jc)
	}
	dead, err := js.Stream(ctx, bus.DeadStream)
	if err != nil {
		t.Fatal(err)
	}
	dc := dead.CachedInfo().Config
	if len(dc.Subjects) != 1 || dc.Subjects[0] != "dead.v1.>" || dc.Storage != jetstream.FileStorage ||
		dc.MaxAge != 30*24*time.Hour || dc.Replicas != 1 || dc.Discard != jetstream.DiscardOld || dc.DiscardNewPerSubject ||
		dc.MaxMsgsPerSubject != 10000 || dc.MaxMsgSize != 4<<10 || dc.MaxBytes != 64<<20 {
		t.Fatalf("DEAD config = %+v", dc)
	}
	pc, err := js.Consumer(ctx, bus.JobsStream, bus.PipelineConsumer)
	if err != nil {
		t.Fatal(err)
	}
	c := pc.CachedInfo().Config
	if c.Durable != "pipeline" || c.FilterSubject != "jobs.v1.*.run_query" || c.AckPolicy != jetstream.AckExplicitPolicy ||
		c.AckWait != 60*time.Second || c.MaxDeliver != 5 || len(c.BackOff) != 0 || c.MaxAckPending != 8 {
		t.Fatalf("pipeline consumer = %+v", c)
	}
	cd, err := js.Consumer(ctx, bus.DeadStream, bus.CoreDeadConsumer)
	if err != nil {
		t.Fatal(err)
	}
	d := cd.CachedInfo().Config
	if d.Durable != "core_dead" || d.AckPolicy != jetstream.AckExplicitPolicy || d.AckWait != 30*time.Second {
		t.Fatalf("core_dead consumer = %+v", d)
	}
}

func TestEnsureStreamsRefusesBadConfig(t *testing.T) {
	ctx := context.Background()
	for _, sc := range []bus.StreamConfig{{Replicas: -1, PipelineMaxAckPending: 8}, {Replicas: 2, PipelineMaxAckPending: 8},
		{Replicas: 1, PipelineMaxAckPending: -1}} {
		if err := bus.EnsureStreams(ctx, nil, sc); err == nil {
			t.Fatalf("%+v accepted", sc)
		}
	}
}

func TestDuplicateMsgIDDropped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	js, _ := jetstream.New(gateway(t).nc)
	subj := jobSubject("dup_test")
	id := uuid.NewString()
	for i := 0; i < 2; i++ {
		ack, err := js.Publish(ctx, subj, []byte("job"), jetstream.WithMsgID(id))
		if err != nil {
			t.Fatal(err)
		}
		if ack.Duplicate != (i == 1) {
			t.Fatalf("publish %d: duplicate = %v", i, ack.Duplicate)
		}
	}
	if n := subjectCount(ctx, t, js, bus.JobsStream, subj); n != 1 {
		t.Fatalf("stored %d messages, want 1", n)
	}
}

func subjectCount(ctx context.Context, t *testing.T, js jetstream.JetStream, stream, subj string) uint64 {
	t.Helper()
	s, err := js.Stream(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx, jetstream.WithSubjectFilter(subj))
	if err != nil {
		t.Fatal(err)
	}
	return info.State.Subjects[subj]
}

// freshNATS starts a server of its own, with the streams, for a test that
// fills a stream: nothing it leaves reaches the shared server's consumers.
func freshNATS(t *testing.T) *testutil.NATS {
	t.Helper()
	ctx := context.Background()
	n, stop, err := testutil.NewNATS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	nc, js, err := bus.Connect(ctx, n.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{})
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if err := bus.EnsureStreams(ctx, js, devStreams); err != nil {
		t.Fatal(err)
	}
	return n
}

// floodSubject publishes n small messages on subj and waits for every ack.
func floodSubject(ctx context.Context, t *testing.T, js jetstream.JetStream, subj string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := js.PublishAsync(subj, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-ctx.Done():
		t.Fatal("async publishes did not complete")
	}
}

// F3: a flood on one hospital's subject is refused at publish; what is
// already queued is kept (DiscardNewPerSubject), and other subjects are
// unaffected.
func TestJobsPerSubjectLimitRefusesNew(t *testing.T) {
	n := freshNATS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	js, _ := jetstream.New(raw(t, n.GatewayURL, bus.GatewayInboxPrefix).nc, jetstream.WithPublishAsyncMaxPending(512))
	subj := jobSubject("flood_test")
	floodSubject(ctx, t, js, subj, bus.JobsMaxPerSubject)
	if got := subjectCount(ctx, t, js, bus.JobsStream, subj); got != bus.JobsMaxPerSubject {
		t.Fatalf("stored %d, want %d", got, bus.JobsMaxPerSubject)
	}
	if _, err := js.Publish(ctx, subj, []byte("one too many")); err == nil {
		t.Fatal("publish past the per-subject limit succeeded")
	}
	if got := subjectCount(ctx, t, js, bus.JobsStream, subj); got != bus.JobsMaxPerSubject {
		t.Fatalf("after the refused publish stored %d", got)
	}
	if _, err := js.Publish(ctx, jobSubject("flood_test"), []byte("another hospital")); err != nil {
		t.Fatalf("another subject refused: %v", err)
	}
}

// A flood of one hospital's dead letters stays within the per-subject
// limit by dropping that subject's oldest (DEAD discards old): the newest
// letter is stored, and another hospital's dead letter gets in.
func TestDeadPerSubjectLimitKeepsOtherHospitals(t *testing.T) {
	n := freshNATS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	js, _ := jetstream.New(raw(t, n.PipelineURL, bus.WorkerInbox("pipeline")).nc, jetstream.WithPublishAsyncMaxPending(512))
	subj := "dead.v1." + uuid.NewString() + ".run_query"
	floodSubject(ctx, t, js, subj, bus.DeadMaxPerSubject)
	if _, err := js.Publish(ctx, subj, []byte("newest")); err != nil {
		t.Fatalf("the newest dead letter was refused: %v", err)
	}
	if _, err := js.Publish(ctx, "dead.v1."+uuid.NewString()+".run_query", []byte("another hospital")); err != nil {
		t.Fatalf("another hospital's dead letter refused: %v", err)
	}
	gjs, _ := jetstream.New(raw(t, n.GatewayURL, bus.GatewayInboxPrefix).nc)
	if got := subjectCount(ctx, t, gjs, bus.DeadStream, subj); got != bus.DeadMaxPerSubject {
		t.Fatalf("stored %d, want %d", got, bus.DeadMaxPerSubject)
	}
	st, err := gjs.Stream(ctx, bus.DeadStream)
	if err != nil {
		t.Fatal(err)
	}
	if st.CachedInfo().State.FirstSeq != 2 {
		t.Fatalf("first seq %d: the oldest letter wasn't the one dropped", st.CachedInfo().State.FirstSeq)
	}
}

// Review round 2 (B): a compromised worker spreads full-size letters over
// made-up hospital subjects until DEAD passes MaxBytes. The oldest are
// dropped, and a genuine dead letter published afterwards is stored.
func TestDeadFloodCannotLockOutRealLetters(t *testing.T) {
	n := freshNATS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	js, _ := jetstream.New(raw(t, n.PipelineURL, bus.WorkerInbox("pipeline")).nc, jetstream.WithPublishAsyncMaxPending(512))
	const body = 4000                  // under DeadMaxMsgSize with the subject and record overhead
	count := bus.DeadMaxBytes/body + 1 // each stored message is more than body bytes, so this crosses the cap
	letter := make([]byte, body)
	for i := 0; i < count; i++ {
		if _, err := js.PublishAsync("dead.v1."+uuid.NewString()+".run_query", letter); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-ctx.Done():
		t.Fatal("async publishes did not complete")
	}
	genuine := "dead.v1." + uuid.NewString() + ".run_query"
	if _, err := js.Publish(ctx, genuine, []byte("genuine")); err != nil {
		t.Fatalf("a genuine dead letter was refused after the flood: %v", err)
	}
	gjs, _ := jetstream.New(raw(t, n.GatewayURL, bus.GatewayInboxPrefix).nc)
	if got := subjectCount(ctx, t, gjs, bus.DeadStream, genuine); got != 1 {
		t.Fatalf("genuine letter stored %d times, want 1", got)
	}
	st, err := gjs.Stream(ctx, bus.DeadStream)
	if err != nil {
		t.Fatal(err)
	}
	state := st.CachedInfo().State
	if state.Bytes > bus.DeadMaxBytes || state.FirstSeq <= 1 {
		t.Fatalf("bytes %d (cap %d), first seq %d: the flood didn't cross the cap or nothing old was dropped",
			state.Bytes, bus.DeadMaxBytes, state.FirstSeq)
	}
}

// Review round 2 (A): one hospital filling its job subject to the
// per-subject limit with near-maximum jobs can't fill the stream: another
// hospital's job is still accepted.
func TestJobsOneHospitalCannotFillStream(t *testing.T) {
	n := freshNATS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	js, _ := jetstream.New(raw(t, n.GatewayURL, bus.GatewayInboxPrefix).nc, jetstream.WithPublishAsyncMaxPending(64))
	big := make([]byte, bus.JobsMaxMsgSize-1024) // near the cap, room for the subject and ack headers
	h1 := jobSubject("run_query")
	for i := 0; i < bus.JobsMaxPerSubject; i++ {
		if _, err := js.PublishAsync(h1, big); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-ctx.Done():
		t.Fatal("async publishes did not complete")
	}
	if got := subjectCount(ctx, t, js, bus.JobsStream, h1); got != bus.JobsMaxPerSubject {
		t.Fatalf("H1 stored %d, want %d", got, bus.JobsMaxPerSubject)
	}
	if _, err := js.Publish(ctx, h1, big); err == nil {
		t.Fatal("H1 queued past its per-subject limit")
	}
	if _, err := js.Publish(ctx, jobSubject("run_query"), big); err != nil {
		t.Fatalf("H2's job refused after H1 filled its subject: %v", err)
	}
}

// Message size caps: a dead letter is ids and a code, a job is at most a
// 64 KiB body plus headers.
func TestStreamsRefuseOversizedMessages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pjs, _ := jetstream.New(pipeline(t).nc)
	dead := "dead.v1." + uuid.NewString() + ".run_query"
	if _, err := pjs.Publish(ctx, dead, make([]byte, 5<<10)); err == nil {
		t.Fatal("a 5 KB dead letter was stored")
	}
	if _, err := pjs.Publish(ctx, dead, make([]byte, 512)); err != nil {
		t.Fatalf("a small dead letter was refused: %v", err)
	}
	gjs, _ := jetstream.New(gateway(t).nc)
	if _, err := gjs.Publish(ctx, jobSubject("size_test"), make([]byte, bus.JobsMaxMsgSize+1)); err == nil {
		t.Fatal("a job over the size cap was stored")
	}
	if _, err := gjs.Publish(ctx, jobSubject("size_test"), make([]byte, 64<<10)); err != nil {
		t.Fatalf("a 64 KiB job was refused: %v", err)
	}
}

// JetStream storage is encrypted: a message body is not on disk in clear.
func TestJetStreamEncryptedAtRest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	js, _ := jetstream.New(gateway(t).nc)
	marker := "plaintext-marker-" + uuid.NewString()
	if _, err := js.Publish(ctx, jobSubject("enc_test"), []byte(marker)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second) // let the file store write the block
	if ok, err := sharedNATS.StoreHasStream(ctx, bus.JobsStream); err != nil || !ok {
		t.Fatalf("no JOBS directory in the store (%v): the check would prove nothing", err)
	}
	found, err := sharedNATS.StoreContains(ctx, marker)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("a job body is stored in clear")
	}
}

// --- Connect.

func TestGatewayCanRequest(t *testing.T) {
	ctx := context.Background()
	w := echo(t)
	sub, err := w.nc.QueueSubscribe("inference.v1.echo.>", "echo", func(m *nats.Msg) {
		_ = m.Respond(append([]byte("echo:"), m.Data...))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()
	if err := w.nc.Flush(); err != nil {
		t.Fatal(err)
	}
	gnc, _, err := bus.Connect(ctx, sharedNATS.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{})
	if err != nil {
		t.Fatalf("gateway connect: %v", err)
	}
	defer gnc.Close()
	rep, err := gnc.Request("inference.v1.echo.ping", []byte("hi"), 3*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if string(rep.Data) != "echo:hi" {
		t.Fatalf("reply = %q", rep.Data)
	}
	w.noViolation(t)
}

func TestConnectBadPassword(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bad := strings.Replace(sharedNATS.GatewayURL, "test-only-gateway-pw", "wrongpwXYZ", 1)
	nc, _, err := bus.Connect(ctx, bad, "", bus.GatewayInboxPrefix, bus.TLS{})
	if err == nil {
		nc.Close()
		t.Fatal("connect with a wrong password succeeded")
	}
	if !errors.Is(err, nats.ErrAuthorization) {
		t.Fatalf("want an authorization error, got %v", err)
	}
	if strings.Contains(err.Error(), "wrongpwXYZ") || strings.Contains(err.Error(), "test-only-gateway-pw") {
		t.Fatalf("error leaks the password: %v", err)
	}
}

func TestConnectMalformedURLDoesNotLeakPassword(t *testing.T) {
	for _, u := range []string{"nats://gateway:SECRETPW@host:abc", "nats://gateway:SECRETPW@[::1", "://gateway:SECRETPW@h"} {
		_, _, err := bus.Connect(context.Background(), u, "", bus.GatewayInboxPrefix, bus.TLS{})
		if err == nil {
			t.Fatalf("%q: expected an error", u)
		}
		if strings.Contains(err.Error(), "SECRETPW") {
			t.Fatalf("error leaks the password: %v", err)
		}
	}
}

func TestConnectWithNkeySeed(t *testing.T) {
	ctx := context.Background()
	srv, stop, err := testutil.NewNATSNkey(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	if nc, _, err := bus.Connect(ctx, srv.URL, "", bus.GatewayInboxPrefix, bus.TLS{}); err == nil {
		nc.Close()
		t.Fatal("connected to an nkey-only server without a seed")
	}
	nc, _, err := bus.Connect(ctx, srv.URL, srv.SeedPath, bus.GatewayInboxPrefix, bus.TLS{})
	if err != nil {
		t.Fatalf("connect with seed: %v", err)
	}
	defer nc.Close()
	// Authenticated and allowed to publish: nobody listens, so no responders.
	if _, err := nc.Request("inference.v1.x", nil, time.Second); !errors.Is(err, nats.ErrNoResponders) {
		t.Fatalf("want no responders, got %v", err)
	}
	if _, _, err := bus.Connect(ctx, srv.URL, "/nonexistent/seed", bus.GatewayInboxPrefix, bus.TLS{}); err == nil {
		t.Fatal("missing seed file should fail")
	}
}

func TestConnectTLS(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	srv, stop, err := testutil.NewNATSTLS(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for name, tc := range map[string]bus.TLS{
		"no TLS":           {},
		"no client cert":   {CAPath: srv.CAPath},
		"wrong CA":         {CAPath: srv.OtherCA, CertPath: srv.CertPath, KeyPath: srv.KeyPath},
		"cert, no CA":      {CertPath: srv.CertPath, KeyPath: srv.KeyPath},
		"cert without key": {CAPath: srv.CAPath, CertPath: srv.CertPath},
		"missing CA file":  {CAPath: dir + "/nope.pem", CertPath: srv.CertPath, KeyPath: srv.KeyPath},
	} {
		nc, _, err := bus.Connect(ctx, srv.URL, "", bus.GatewayInboxPrefix, tc)
		if err == nil {
			nc.Close()
			t.Fatalf("%s: connected", name)
		}
		if strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "test-only-gateway-pw") {
			t.Fatalf("%s: error leaks a path or password: %v", name, err)
		}
	}
	nc, _, err := bus.Connect(ctx, srv.URL, "", bus.GatewayInboxPrefix,
		bus.TLS{CAPath: srv.CAPath, CertPath: srv.CertPath, KeyPath: srv.KeyPath})
	if err != nil {
		t.Fatalf("connect with TLS: %v", err)
	}
	defer nc.Close()
	if !nc.TLSRequired() {
		t.Fatal("connection is not TLS")
	}
	if _, err := nc.Request("inference.v1.x", nil, time.Second); !errors.Is(err, nats.ErrNoResponders) {
		t.Fatalf("want no responders, got %v", err)
	}
}

// --- KV.

func TestOpenBucketsCreatesFourAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	nc, js, err := bus.Connect(ctx, sharedNATS.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{})
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	for i := 0; i < 2; i++ {
		b, err := bus.OpenBuckets(ctx, js, 1)
		if err != nil {
			t.Fatalf("open buckets (round %d): %v", i, err)
		}
		for name, kv := range map[string]jetstream.KeyValue{
			"denylist": b.Denylist, "login": b.LoginAttempts, "mfa": b.MFAAttempts, "ceiling": b.LoginCeiling} {
			if kv == nil {
				t.Fatalf("%s bucket is nil", name)
			}
			st, err := kv.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := 15 * time.Minute
			switch name {
			case "denylist":
				want = bus.DenylistTTL // access token life plus a clock-skew margin
			case "ceiling":
				want = time.Hour // per-login failures across IPs
			}
			if st.TTL() != want {
				t.Fatalf("%s ttl = %v, want %v", name, st.TTL(), want)
			}
		}
		if _, err := b.LoginAttempts.Put(ctx, "0123abcd", []byte("1")); err != nil {
			t.Fatalf("put as gateway: %v", err)
		}
		if v, err := b.LoginAttempts.Get(ctx, "0123abcd"); err != nil || string(v.Value()) != "1" {
			t.Fatalf("get as gateway: %v", err)
		}
		if err := b.LoginAttempts.Delete(ctx, "0123abcd"); err != nil {
			t.Fatalf("delete as gateway: %v", err)
		}
	}
}

// The gateway may open only the four buckets: another name is refused.
func TestGatewayCannotOpenOtherBuckets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	nc, js, err := bus.Connect(ctx, sharedNATS.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{})
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if _, err := bus.OpenBucket(ctx, js, "other_bucket", time.Minute, 1); err == nil {
		t.Fatal("gateway opened a bucket outside the four")
	}
}

func TestKVTTL(t *testing.T) {
	// A fresh server, so the short TTL on a real bucket name touches no
	// other test.
	ctx := context.Background()
	n, stop, err := testutil.NewNATS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	nc, js, err := bus.Connect(ctx, n.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{})
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	kv, err := bus.OpenBucket(ctx, js, bus.DenylistBucket, 2*time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Put(ctx, "jti-abc", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Get(ctx, "jti-abc"); err != nil {
		t.Fatalf("key missing right after put: %v", err)
	}
	time.Sleep(3 * time.Second)
	if _, err := kv.Get(ctx, "jti-abc"); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Fatalf("after 3s want ErrKeyNotFound, got %v", err)
	}
}

// T4 (carried to T15): a server that already has the streams from an
// earlier configuration (JOBS with 10000 per subject and no per-subject
// discard, DEAD discarding new, a pipeline consumer with a BackOff) is
// brought to this one by EnsureStreams in place, keeping what is stored.
// This is what core serve does on a host that ran an earlier build.
func TestEnsureStreamsUpdatesThePreviousConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	n, stop, err := testutil.NewNATS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	nc, js, err := bus.Connect(ctx, n.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{})
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: bus.JobsStream, Subjects: []string{"jobs.v1.>"}, Storage: jetstream.FileStorage,
		Retention: jetstream.WorkQueuePolicy, Discard: jetstream.DiscardNew, MaxMsgsPerSubject: 10000,
		MaxAge: 7 * 24 * time.Hour, Duplicates: 2 * time.Minute, MaxMsgSize: 72 << 10, MaxBytes: 256 << 20, Replicas: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: bus.DeadStream, Subjects: []string{"dead.v1.>"}, Storage: jetstream.FileStorage,
		Retention: jetstream.LimitsPolicy, Discard: jetstream.DiscardNew, MaxMsgsPerSubject: 10000,
		MaxAge: 30 * 24 * time.Hour, MaxMsgSize: 4 << 10, MaxBytes: 64 << 20, Replicas: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateOrUpdateConsumer(ctx, bus.JobsStream, jetstream.ConsumerConfig{
		Durable: bus.PipelineConsumer, FilterSubject: bus.PipelineFilter, AckPolicy: jetstream.AckExplicitPolicy,
		AckWait: 60 * time.Second, MaxDeliver: 5, MaxAckPending: 8,
		BackOff: []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 5 * time.Minute},
	}); err != nil {
		t.Fatal(err)
	}
	// One waiting job and one dead letter from before the update.
	if _, err := js.Publish(ctx, "jobs.v1.00000000-0000-4000-8000-0000000000a1.run_query", []byte("synthetic")); err != nil {
		t.Fatal(err)
	}
	// Only a worker publishes dead letters.
	pnc, err := nats.Connect(n.PipelineURL, nats.CustomInboxPrefix(bus.WorkerInbox("pipeline")))
	if err != nil {
		t.Fatal(err)
	}
	defer pnc.Close()
	pjs, err := jetstream.New(pnc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pjs.Publish(ctx, "dead.v1.00000000-0000-4000-8000-0000000000a1.run_query", []byte("synthetic")); err != nil {
		t.Fatal(err)
	}

	if err := bus.EnsureStreams(ctx, js, devStreams); err != nil {
		t.Fatalf("update from the previous config: %v", err)
	}
	jobs, err := js.Stream(ctx, bus.JobsStream)
	if err != nil {
		t.Fatal(err)
	}
	ji := jobs.CachedInfo()
	// MaxAge goes down from the previous 7 days to an hour in place.
	if jc := ji.Config; jc.MaxMsgsPerSubject != bus.JobsMaxPerSubject || !jc.DiscardNewPerSubject || jc.Discard != jetstream.DiscardNew ||
		jc.MaxAge != time.Hour {
		t.Fatalf("JOBS not updated: %+v", jc)
	}
	if ji.State.Msgs != 1 {
		t.Fatalf("JOBS lost its waiting job: %d messages", ji.State.Msgs)
	}
	dead, err := js.Stream(ctx, bus.DeadStream)
	if err != nil {
		t.Fatal(err)
	}
	di := dead.CachedInfo()
	if dc := di.Config; dc.Discard != jetstream.DiscardOld || dc.MaxMsgsPerSubject != bus.DeadMaxPerSubject {
		t.Fatalf("DEAD not updated: %+v", dc)
	}
	if di.State.Msgs != 1 {
		t.Fatalf("DEAD lost its letter: %d messages", di.State.Msgs)
	}
	pc, err := js.Consumer(ctx, bus.JobsStream, bus.PipelineConsumer)
	if err != nil {
		t.Fatal(err)
	}
	if c := pc.CachedInfo().Config; len(c.BackOff) != 0 || c.AckWait != bus.PipelineAckWait || c.MaxDeliver != bus.PipelineMaxDeliver {
		t.Fatalf("pipeline consumer not updated: %+v", c)
	}
}
