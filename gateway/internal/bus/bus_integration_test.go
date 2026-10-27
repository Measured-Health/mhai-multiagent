//go:build integration

package bus_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/gateway/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/gateway/internal/testutil"
)

var sharedNATS *testutil.NATS

func TestMain(m *testing.M) {
	n, stop, err := testutil.NewNATS(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "start nats:", err)
		os.Exit(1)
	}
	sharedNATS = n
	code := m.Run()
	stop()
	os.Exit(code)
}

// svc is a raw client for one service user whose async errors (permission
// violations) are collected.
type svc struct {
	nc   *nats.Conn
	errs chan error
}

func raw(t *testing.T, url, inboxPrefix string) *svc {
	t.Helper()
	s := &svc{errs: make(chan error, 16)}
	nc, err := nats.Connect(url, nats.CustomInboxPrefix(inboxPrefix),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { s.errs <- err }))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	s.nc = nc
	return s
}

func worker(t *testing.T) *svc  { return raw(t, sharedNATS.WorkerURL, bus.WorkerInboxPrefix) }
func gateway(t *testing.T) *svc { return raw(t, sharedNATS.GatewayURL, bus.GatewayInboxPrefix) }

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

func TestWorkerCannotPublishInference(t *testing.T) {
	w := worker(t)
	// A second worker listens: the message must never arrive.
	listener := worker(t)
	got := make(chan struct{}, 1)
	if _, err := listener.nc.Subscribe("inference.v1.cca", func(*nats.Msg) { got <- struct{}{} }); err != nil {
		t.Fatal(err)
	}
	_ = listener.nc.Flush()
	if err := w.nc.Publish("inference.v1.cca", []byte("x")); err != nil {
		t.Fatalf("publish call itself: %v", err)
	}
	w.violation(t, "inference.v1.cca")
	select {
	case <-got:
		t.Fatal("a message the worker was not allowed to publish was delivered")
	case <-time.After(500 * time.Millisecond):
	}
}

func TestWorkerCannotPublishJobs(t *testing.T) {
	w := worker(t)
	_ = w.nc.Publish("jobs.v1.h1.query", []byte("x"))
	w.violation(t, "jobs.v1.h1.query")
}

func TestWorkerHasNoKVAccess(t *testing.T) {
	for _, subj := range []string{
		"$KV.denylist.x", "$JS.API.DIRECT.GET.KV_denylist.$KV.denylist.x",
		"$JS.API.STREAM.INFO.KV_denylist", "$JS.API.STREAM.DELETE.KV_denylist",
	} {
		w := worker(t)
		_ = w.nc.Publish(subj, nil)
		w.violation(t, subj)
	}
	w := worker(t)
	_, _ = w.nc.Subscribe("$KV.>", func(*nats.Msg) {})
	w.violation(t, "$KV.>")
}

func TestWorkerCanReplyAndDead(t *testing.T) {
	w := worker(t)
	if err := w.nc.Publish("dead.v1.h1.query", []byte("x")); err != nil {
		t.Fatal(err)
	}
	w.noViolation(t)
}

func TestInboxIsolation(t *testing.T) {
	w := worker(t)
	for _, subj := range []string{"_INBOX.>", bus.GatewayInboxPrefix + ".>"} {
		_, _ = w.nc.Subscribe(subj, func(*nats.Msg) {})
		w.violation(t, subj)
	}
	for _, subj := range []string{"_INBOX.abc", bus.GatewayInboxPrefix + ".abc"} {
		_ = w.nc.Publish(subj, []byte("forged"))
		w.violation(t, subj)
	}
	// The gateway cannot listen on the worker's inbox either.
	g := gateway(t)
	_, _ = g.nc.Subscribe(bus.WorkerInboxPrefix+".>", func(*nats.Msg) {})
	g.violation(t, bus.WorkerInboxPrefix+".>")
}

func TestDefaultDeny(t *testing.T) {
	w := worker(t)
	_ = w.nc.Publish("unlisted.v1.x", nil)
	w.violation(t, "unlisted.v1.x")
	_, _ = w.nc.Subscribe("unlisted.>", func(*nats.Msg) {})
	w.violation(t, "unlisted.>")

	g := gateway(t)
	_ = g.nc.Publish("unlisted.v1.x", nil)
	g.violation(t, "unlisted.v1.x")
	_ = g.nc.Publish("dead.v1.h1.query", nil)
	g.violation(t, "dead.v1.h1.query")
	_, _ = g.nc.Subscribe("inference.v1.>", func(*nats.Msg) {})
	g.violation(t, "inference.v1.>")
}

func TestGatewayCannotDestroyStreams(t *testing.T) {
	g := gateway(t)
	for _, subj := range []string{"$JS.API.STREAM.DELETE.KV_denylist", "$JS.API.STREAM.PURGE.KV_denylist", "$JS.API.STREAM.DELETE.JOBS"} {
		_ = g.nc.Publish(subj, nil)
		g.violation(t, subj)
	}
}

func TestGatewayCanRequest(t *testing.T) {
	ctx := context.Background()
	w := worker(t)
	sub, err := w.nc.QueueSubscribe("inference.v1.echo", "echo", func(m *nats.Msg) {
		_ = m.Respond(append([]byte("echo:"), m.Data...))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()
	if err := w.nc.Flush(); err != nil {
		t.Fatal(err)
	}

	gnc, _, err := bus.Connect(ctx, sharedNATS.GatewayURL, "", bus.GatewayInboxPrefix)
	if err != nil {
		t.Fatalf("gateway connect: %v", err)
	}
	defer gnc.Close()
	rep, err := gnc.Request("inference.v1.echo", []byte("hi"), 3*time.Second)
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
	nc, _, err := bus.Connect(ctx, bad, "", bus.GatewayInboxPrefix)
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
		_, _, err := bus.Connect(context.Background(), u, "", bus.GatewayInboxPrefix)
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

	if nc, _, err := bus.Connect(ctx, srv.URL, "", bus.GatewayInboxPrefix); err == nil {
		nc.Close()
		t.Fatal("connected to an nkey-only server without a seed")
	}
	nc, _, err := bus.Connect(ctx, srv.URL, srv.SeedPath, bus.GatewayInboxPrefix)
	if err != nil {
		t.Fatalf("connect with seed: %v", err)
	}
	defer nc.Close()
	// Authenticated and allowed to publish: nobody listens, so no responders.
	if _, err := nc.Request("inference.v1.x", nil, time.Second); !errors.Is(err, nats.ErrNoResponders) {
		t.Fatalf("want no responders, got %v", err)
	}
	if _, _, err := bus.Connect(ctx, srv.URL, "/nonexistent/seed", bus.GatewayInboxPrefix); err == nil {
		t.Fatal("missing seed file should fail")
	}
}

func TestOpenBucketsCreatesThreeAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	nc, js, err := bus.Connect(ctx, sharedNATS.GatewayURL, "", bus.GatewayInboxPrefix)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	for i := 0; i < 2; i++ {
		b, err := bus.OpenBuckets(ctx, js)
		if err != nil {
			t.Fatalf("open buckets (round %d): %v", i, err)
		}
		for name, kv := range map[string]jetstream.KeyValue{
			"denylist": b.Denylist, "login": b.LoginAttempts, "mfa": b.MFAAttempts} {
			if kv == nil {
				t.Fatalf("%s bucket is nil", name)
			}
			st, err := kv.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := 15 * time.Minute
			if name == "denylist" {
				want = bus.DenylistTTL // access token life plus a clock-skew margin
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
	}
}

func TestKVTTL(t *testing.T) {
	ctx := context.Background()
	nc, js, err := bus.Connect(ctx, sharedNATS.GatewayURL, "", bus.GatewayInboxPrefix)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	kv, err := bus.OpenBucket(ctx, js, "ttl_test_denylist", 2*time.Second)
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
