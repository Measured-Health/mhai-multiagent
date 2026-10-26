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

func TestWorkerCannotPublishInference(t *testing.T) {
	ctx := context.Background()
	violations := make(chan error, 4)
	nc, err := nats.Connect(sharedNATS.WorkerURL,
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { violations <- err }))
	if err != nil {
		t.Fatalf("connect as worker: %v", err)
	}
	defer nc.Close()
	_ = ctx
	if err := nc.Publish("inference.v1.cca", []byte("x")); err != nil {
		t.Fatalf("publish call itself: %v", err)
	}
	_ = nc.Flush()
	select {
	case err := <-violations:
		if !errors.Is(err, nats.ErrPermissionViolation) && !strings.Contains(strings.ToLower(err.Error()), "permissions violation") {
			t.Fatalf("want permissions violation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no permissions violation for worker publishing inference.v1.cca")
	}
}

func TestWorkerCanReplyAndDead(t *testing.T) {
	violations := make(chan error, 4)
	nc, err := nats.Connect(sharedNATS.WorkerURL,
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { violations <- err }))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if err := nc.Publish("dead.v1.h1.query", []byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	select {
	case err := <-violations:
		t.Fatalf("worker publishing dead.v1 was refused: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestGatewayCanRequest(t *testing.T) {
	ctx := context.Background()
	wnc, err := nats.Connect(sharedNATS.WorkerURL)
	if err != nil {
		t.Fatalf("worker connect: %v", err)
	}
	defer wnc.Close()
	sub, err := wnc.QueueSubscribe("inference.v1.echo", "echo", func(m *nats.Msg) {
		_ = m.Respond(append([]byte("echo:"), m.Data...))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()
	if err := wnc.Flush(); err != nil {
		t.Fatal(err)
	}

	gnc, _, err := bus.Connect(ctx, sharedNATS.GatewayURL, "")
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
}

func TestConnectFailsWithBadPassword(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bad := strings.Replace(sharedNATS.GatewayURL, "test-only-gateway-pw", "wrong", 1)
	if nc, _, err := bus.Connect(ctx, bad, ""); err == nil {
		nc.Close()
		t.Fatal("connect with a wrong password succeeded")
	}
}

func TestOpenBucketsCreatesThreeAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	nc, js, err := bus.Connect(ctx, sharedNATS.GatewayURL, "")
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
		}
		st, err := b.Denylist.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st.TTL() != 15*time.Minute {
			t.Fatalf("denylist ttl = %v", st.TTL())
		}
		if _, err := b.LoginAttempts.Put(ctx, "0123abcd", []byte("1")); err != nil {
			t.Fatalf("put as gateway: %v", err)
		}
	}
}

func TestKVTTL(t *testing.T) {
	ctx := context.Background()
	nc, js, err := bus.Connect(ctx, sharedNATS.GatewayURL, "")
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
