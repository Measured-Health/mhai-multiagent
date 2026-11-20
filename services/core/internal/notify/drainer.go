// Package notify is core's side of the notifications outbox (notifications
// design §9.1; §15 PR 3). An action writes its outbox row (message_outbox,
// ids only) in its own transaction through internal/delivery; after the
// commit the drainer publishes the row's id on NOTIFY as
// notify.v1.<hospital>.due (Nats-Msg-Id = the id, so a second publish is
// dropped) and marks the row published. A sweep every Every republishes
// what a lost kick or a crash left behind, so a row is never stranded by a
// failed publish. It runs only with CORE_NOTIFY_ENABLED.
//
// The drainer reaches the outbox only through three definers, as
// mhai_notify_outbox (SET LOCAL ROLE; migration 00053): the due ids and
// hospitals (never a link kind, never older than 24 hours), marking them
// published, and the 90-day retention. It sees no recipient, kind or text,
// and publishes ids only. Logs carry counts and closed reasons.
package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/notifypb"
)

const (
	// Every is the sweep: unpublished rows are republished this often.
	Every = 30 * time.Second
	// Batch is how many rows one pass publishes.
	Batch = 100
	// RetentionEvery is how often the 90-day retention runs.
	RetentionEvery = 24 * time.Hour
	// role is the database role the drainer works as.
	role = "mhai_notify_outbox"
	// publishTimeout bounds one publish (the stream's ack).
	publishTimeout = 5 * time.Second
)

// Subject is a due message's subject for hospital h.
func Subject(h uuid.UUID) string { return "notify.v1." + h.String() + ".due" }

// Publisher is what the drainer needs of JetStream.
type Publisher interface {
	PublishMsg(ctx context.Context, m *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error)
}

// Metrics are the drainer's in-process counts.
type Metrics struct {
	Published atomic.Int64 // ids published (a duplicate the stream dropped counts too)
	Failures  atomic.Int64 // failed publishes or database steps (retried next pass)
	Removed   atomic.Int64 // rows the retention deleted
}

// Drainer publishes due outbox rows. Run runs it; Kick asks for a pass now.
type Drainer struct {
	Pool    *pgxpool.Pool
	JS      Publisher
	Log     *slog.Logger
	Every   time.Duration // Every when 0
	Metrics *Metrics
	kick    chan struct{}
}

// New is a drainer ready to Run.
func New(pool *pgxpool.Pool, js Publisher, log *slog.Logger) *Drainer {
	return &Drainer{Pool: pool, JS: js, Log: log, Metrics: &Metrics{}, kick: make(chan struct{}, 1)}
}

func (d *Drainer) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (d *Drainer) metrics() *Metrics {
	if d.Metrics == nil {
		d.Metrics = &Metrics{}
	}
	return d.Metrics
}

// Kick asks for a pass soon; it never blocks (a pending kick is enough).
func (d *Drainer) Kick() {
	if d == nil || d.kick == nil {
		return
	}
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

var errNotReady = errors.New("notify: drainer has no pool or JetStream")

// asRole runs fn as mhai_notify_outbox in a transaction.
func (d *Drainer) asRole(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type due struct{ id, hospital uuid.UUID }

// DrainOnce publishes up to Batch due rows and marks the published ones.
// It returns how many it published. A row whose publish failed stays due
// for the next pass.
func (d *Drainer) DrainOnce(ctx context.Context) (int, error) {
	if d.Pool == nil || d.JS == nil {
		return 0, errNotReady
	}
	var rows []due
	err := d.asRole(ctx, func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `SELECT id, hospital_id FROM mhai_outbox_due($1)`, Batch)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var x due
			if err := r.Scan(&x.id, &x.hospital); err != nil {
				return err
			}
			rows = append(rows, x)
		}
		return r.Err()
	})
	if err != nil {
		d.metrics().Failures.Add(1)
		return 0, err
	}
	var done []uuid.UUID
	for _, x := range rows {
		b, err := proto.Marshal(&notifypb.Due{MessageId: x.id.String()})
		if err != nil {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, publishTimeout)
		_, err = d.JS.PublishMsg(pctx, &nats.Msg{Subject: Subject(x.hospital), Data: b}, jetstream.WithMsgID(x.id.String()))
		cancel()
		if err != nil {
			d.metrics().Failures.Add(1)
			continue
		}
		done = append(done, x.id)
	}
	if len(done) == 0 {
		if len(rows) > 0 {
			d.log().Warn("notify: due messages not published; retried next pass", "due", len(rows))
		}
		return 0, nil
	}
	if err := d.asRole(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT mhai_outbox_published($1)`, done)
		return err
	}); err != nil {
		// Published but not marked: the next pass publishes them again and
		// the stream drops the duplicates (Nats-Msg-Id) within 2 minutes;
		// later, the delivery's unique key does.
		d.metrics().Failures.Add(1)
		return len(done), err
	}
	d.metrics().Published.Add(int64(len(done)))
	if len(done) < len(rows) {
		d.log().Warn("notify: some due messages not published; retried next pass", "published", len(done), "due", len(rows))
	}
	return len(done), nil
}

// Retention deletes outbox rows older than 90 days.
func (d *Drainer) Retention(ctx context.Context) (int, error) {
	if d.Pool == nil {
		return 0, errNotReady
	}
	var n int
	err := d.asRole(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mhai_outbox_retention()`).Scan(&n)
	})
	if err == nil {
		d.metrics().Removed.Add(int64(n))
	}
	return n, err
}

// Run drains on every kick and every Every, and runs the retention daily,
// until ctx ends. Errors are logged by a closed reason and retried.
func (d *Drainer) Run(ctx context.Context) {
	every := d.Every
	if every <= 0 {
		every = Every
	}
	if d.kick == nil {
		d.kick = make(chan struct{}, 1)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	nextRetention := time.Now()
	for {
		for {
			n, err := d.DrainOnce(ctx)
			if err != nil {
				if ctx.Err() == nil {
					d.log().Warn("notify: drain failed; retried next pass", "reason", "internal_error")
				}
				break
			}
			if n < Batch {
				break
			}
		}
		if !time.Now().Before(nextRetention) {
			if n, err := d.Retention(ctx); err != nil {
				if ctx.Err() == nil {
					d.log().Warn("notify: outbox retention failed", "reason", "internal_error")
				}
			} else if n > 0 {
				d.log().Info("notify: outbox retention", "removed", n)
			}
			nextRetention = time.Now().Add(RetentionEvery)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.kick:
		}
	}
}
