//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/notifypb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/ctl"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/deliver"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/testpg"
)

// natsImage is core's test NATS (services/core/internal/testutil).
const natsImage = "nats:2.10-alpine"

func startNATS(t *testing.T) string {
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
	return "nats://" + host + ":" + port.Port()
}

// syncBuf is a log sink safe to read while the service writes to it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// The service end to end: its real login (notify_app, a member of
// mhai_notify_app only) on core's migrations, NATS, and a health request
// signed as core signs it. It answers database_ready true and its channel;
// a due message on NOTIFY goes out once through the log channel without
// the address in any log; a superuser login stops it at start.
func TestServeAnswersHealth(t *testing.T) {
	superURL, super := testpg.Migrated(t)
	dbURL := testpg.Login(t, superURL, super, "notify_app", "mhai_notify_app")
	natsURL := startNATS(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(t.TempDir(), "request-keys.json")
	if err := reqtoken.WriteTrusted(keys, map[string]ed25519.PublicKey{reqtoken.KeyID(pub): pub}); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"APP_ENV": "development", "NOTIFY_NATS_URL": natsURL, "NOTIFY_NATS_PASSWORD": devPassword(),
		"NOTIFY_DATABASE_URL": dbURL, "NOTIFY_TRUSTED_KEYS_PATH": keys, "NOTIFY_CHANNELS": "log"}
	// The NOTIFY stream and durable as core's bus.EnsureNotifyStream makes
	// them (CORE_NOTIFY_ENABLED), and one due outbox row.
	setup, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Close()
	js, err := jetstream.New(setup)
	if err != nil {
		t.Fatal(err)
	}
	sctx, scancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer scancel()
	if _, err := js.CreateStream(sctx, jetstream.StreamConfig{Name: deliver.Stream, Subjects: []string{"notify.v1.*.due"},
		Retention: jetstream.WorkQueuePolicy, Duplicates: 2 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateOrUpdateConsumer(sctx, deliver.Stream, jetstream.ConsumerConfig{Durable: deliver.Durable,
		FilterSubject: "notify.v1.*.due", AckPolicy: jetstream.AckExplicitPolicy, AckWait: deliver.Lease}); err != nil {
		t.Fatal(err)
	}
	hid := testpg.Hospital(t, super)
	var u, msgID uuid.UUID
	if err := super.QueryRow(sctx, `INSERT INTO users (hospital_id, email, display_name) VALUES ($1, $2, 'Synth E2E')
		RETURNING id`, hid, "synth-e2e-"+hid.String()[:8]+"@example.test").Scan(&u); err != nil {
		t.Fatal(err)
	}
	if err := super.QueryRow(sctx, `INSERT INTO message_outbox (hospital_id, recipient_id, kind, channel, created_by)
		VALUES ($1, $2, 'mfa_reminder', 'log', $2) RETURNING id`, hid, u).Scan(&msgID); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &syncBuf{}
	up := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- serveUntil(ctx, func(k string) string { return env[k] }, logs, func() { close(up) }) }()
	select {
	case <-up:
	case err := <-done:
		t.Fatalf("serve: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("not serving")
	}

	nc, err := nats.Connect(natsURL, nats.CustomInboxPrefix("_INBOX_gateway"))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	signer := reqtoken.Signer{Key: priv, KeyID: reqtoken.KeyID(pub)}
	c := reqtoken.Claims{Aud: "notifications", Sub: reqtoken.SvcCore, Hid: uuid.NewString(), Rid: uuid.NewString(),
		Kind: reqtoken.KindReq, Subject: "notify.v1.ctl.health", ReplyPrefix: reqtoken.ReplyCore, Via: reqtoken.ViaCore}
	tok, _, err := signer.Mint(c, nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	m := nats.NewMsg("notify.v1.ctl.health")
	m.Header.Set(ctl.HeaderToken, tok)
	m.Header.Set(ctl.HeaderSchema, "mhai.notify.v1.HealthRequest")
	m.Header.Set(ctl.HeaderRequestID, c.Rid)
	rep, err := nc.RequestMsg(m, 5*time.Second)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	var h notifypb.HealthReply
	if err := proto.Unmarshal(rep.Data, &h); err != nil || !h.GetDatabaseReady() || len(h.GetChannels()) != 1 {
		t.Fatalf("health reply: %v %v", &h, err)
	}
	// A due message on NOTIFY is delivered once through the log channel
	// and recorded sent.
	body, _ := proto.Marshal(&notifypb.Due{MessageId: msgID.String()})
	if _, err := js.Publish(sctx, "notify.v1."+hid.String()+".due", body, jetstream.WithMsgID(msgID.String())); err != nil {
		t.Fatal(err)
	}
	status := ""
	for deadline := time.Now().Add(20 * time.Second); status != "sent" && time.Now().Before(deadline); {
		_ = super.QueryRow(sctx, `SELECT status FROM message_deliveries WHERE message_id = $1`, msgID).Scan(&status)
		time.Sleep(100 * time.Millisecond)
	}
	if status != "sent" || !strings.Contains(logs.String(), "message delivered to the log channel") ||
		strings.Contains(logs.String(), "synth-e2e-") {
		t.Fatalf("delivery %q; logs: %s", status, logs.String())
	}
	// The same token again: no reply.
	if _, err := nc.RequestMsg(m, time.Second); err == nil {
		t.Fatal("a replayed token was answered")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("stop: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "notifications serving") || !strings.Contains(out, `"database_ready":true`) ||
		strings.Contains(out, devPassword()) || strings.Contains(out, "postgres://") || strings.Contains(out, tok[:20]) {
		t.Fatalf("logs: %s", out)
	}

	// A superuser login is refused at start, by its variable and reason.
	env["NOTIFY_DATABASE_URL"] = superURL
	err = serveUntil(context.Background(), func(k string) string { return env[k] }, &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "NOTIFY_DATABASE_URL") || !strings.Contains(err.Error(), "superuser") ||
		strings.Contains(err.Error(), "postgres://") {
		t.Fatalf("superuser: %v", err)
	}
}
