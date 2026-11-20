//go:build integration

package notify_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/notify"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/notifypb"
)

type rig struct {
	super, app *pgxpool.Pool
	js         jetstream.JetStream
	svc        jetstream.JetStream // the notifications user's, which pulls notify_send
	h          uuid.UUID
	u          uuid.UUID
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	pg, super := testutil.MigratedPostgres(t)
	app, err := testutil.AppLogin(ctx, pg, super)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	n, stop, err := testutil.NewNATS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	nc, err := nats.Connect(n.GatewayURL, nats.CustomInboxPrefix(bus.GatewayInboxPrefix))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.EnsureNotifyStream(ctx, js, 1); err != nil {
		t.Fatal(err)
	}
	snc, err := nats.Connect(n.NotificationsURL, nats.CustomInboxPrefix(bus.WorkerInbox("notifications")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(snc.Close)
	svc, err := jetstream.New(snc)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{super: super, app: app, js: js, svc: svc, h: uuid.New()}
	if _, err := super.Exec(ctx, `INSERT INTO hospitals (id, name, short_code) VALUES ($1, 'Synth Drain', $2)`,
		r.h, "DR"+r.h.String()[:6]); err != nil {
		t.Fatal(err)
	}
	if err := super.QueryRow(ctx, `INSERT INTO users (hospital_id, email, display_name) VALUES ($1, $2, 'Synth D') RETURNING id`,
		r.h, "synth-drain-"+r.h.String()[:8]+"@example.test").Scan(&r.u); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) row(t *testing.T, kind string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := r.super.QueryRow(context.Background(), `INSERT INTO message_outbox (hospital_id, recipient_id, kind, channel, created_by)
		VALUES ($1, $2, $3, 'log', $2) RETURNING id`, r.h, r.u, kind).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// fetch pulls what notify_send holds.
func (r *rig) fetch(t *testing.T) []jetstream.Msg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := r.svc.Consumer(ctx, bus.NotifyStream, bus.NotifySendConsumer)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Fetch(50, jetstream.FetchMaxWait(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var out []jetstream.Msg
	for m := range b.Messages() {
		out = append(out, m)
	}
	return out
}

// The drainer, as core's login (mhai_app, SET ROLE mhai_notify_outbox),
// publishes each due row's id once on notify.v1.<hid>.due (the body is the
// id and nothing else), marks it published, and leaves the link kinds
// alone. A second pass publishes nothing; a re-publish of an unmarked row
// is dropped by the stream.
func TestDrainerPublishesDueIDsOnce(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a, b := r.row(t, "mfa_reminder"), r.row(t, "mfa_reset")
	r.row(t, "invite")
	r.row(t, "password_reset")
	d := notify.New(r.app, r.js, nil)
	n, err := d.DrainOnce(ctx)
	if err != nil || n != 2 {
		t.Fatalf("drain: %d %v", n, err)
	}
	if n, err := d.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("second drain: %d %v", n, err)
	}
	// Unmark one: the next pass publishes it again, and the stream drops
	// the duplicate id.
	if _, err := r.super.Exec(ctx, `UPDATE message_outbox SET published_at = NULL WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	if n, err := d.DrainOnce(ctx); err != nil || n != 1 {
		t.Fatalf("republish: %d %v", n, err)
	}
	msgs := r.fetch(t)
	got := map[string]bool{}
	for _, m := range msgs {
		if m.Subject() != notify.Subject(r.h) {
			t.Errorf("subject %s", m.Subject())
		}
		var due notifypb.Due
		if err := proto.Unmarshal(m.Data(), &due); err != nil || len(m.Data()) > 64 {
			t.Errorf("body %x: %v", m.Data(), err)
		}
		got[due.GetMessageId()] = true
		_ = m.Ack()
	}
	if len(msgs) != 2 || !got[a.String()] || !got[b.String()] {
		t.Fatalf("published %v (%d messages), want %s and %s once each", got, len(msgs), a, b)
	}
	var unpublished int
	if err := r.super.QueryRow(ctx, `SELECT count(*) FROM message_outbox WHERE published_at IS NULL`).Scan(&unpublished); err != nil ||
		unpublished != 2 {
		t.Fatalf("unpublished %d (the two link rows), %v", unpublished, err)
	}
	if d.Metrics.Published.Load() != 3 {
		t.Fatalf("published metric %d", d.Metrics.Published.Load())
	}
	if n, err := d.Retention(ctx); err != nil || n != 0 {
		t.Fatalf("retention: %d %v", n, err)
	}
}

// failingJS refuses every publish.
type failingJS struct{}

func (failingJS) PublishMsg(context.Context, *nats.Msg, ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	return nil, errors.New("synthetic: NATS down")
}

// With NATS down nothing is marked published: the next pass retries; Run
// drains on a kick and stops with its context.
func TestDrainerRetriesAndRuns(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	id := r.row(t, "mfa_reminder")
	down := notify.New(r.app, failingJS{}, nil)
	if n, err := down.DrainOnce(ctx); err != nil || n != 0 || down.Metrics.Failures.Load() != 1 {
		t.Fatalf("down: %d %v %d", n, err, down.Metrics.Failures.Load())
	}
	var published bool
	if err := r.super.QueryRow(ctx, `SELECT published_at IS NOT NULL FROM message_outbox WHERE id = $1`, id).Scan(&published); err != nil ||
		published {
		t.Fatalf("marked published with NATS down: %v", err)
	}
	d := notify.New(r.app, r.js, nil)
	d.Every = time.Hour
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { d.Run(runCtx); close(done) }()
	d.Kick()
	deadline := time.Now().Add(10 * time.Second)
	for d.Metrics.Published.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	<-done
	if d.Metrics.Published.Load() != 1 {
		t.Fatalf("run published %d", d.Metrics.Published.Load())
	}
	if _, err := (&notify.Drainer{}).DrainOnce(ctx); err == nil {
		t.Fatal("a drainer without a pool drained")
	}
}
