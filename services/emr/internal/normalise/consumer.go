package normalise

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/sink"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
)

// The durable core creates for the normaliser, and its stream.
const (
	Stream      = "EMR"
	Durable     = "emr_normalise"
	MaxDeliver  = 5 // the durable's limit: the 5th failed delivery dead-letters
	fetchBatch  = 10
	fetchWait   = 5 * time.Second
	SweepAfter  = 2 * time.Minute
	SweepEvery  = time.Minute
	msgDeadline = 50 * time.Second // inside the durable's 60 s ack wait
)

// Msg is what Handle needs of a JetStream message.
type Msg interface {
	Subject() string
	Data() []byte
	Ack() error
	NakWithDelay(time.Duration) error
	Term() error
	Deliveries() uint64
}

// HandleMsg settles one emr.v1.<hid>.fetched message. A malformed subject
// or body, or another hospital's ids, is acked with nothing done (counted).
// A failure is retried with a growing delay; the 5th delivery that fails
// dead-letters dead.v1.<hid>.emr_normalise (ids and a code only) and ends
// the message.
func (n *Normaliser) HandleMsg(ctx context.Context, m Msg) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), msgDeadline)
	defer cancel()
	seg := strings.Split(m.Subject(), ".")
	hid, err := uuid.Parse(segment(seg, 2))
	if len(seg) != 4 || seg[0] != "emr" || seg[1] != "v1" || seg[3] != "fetched" || err != nil || hid.String() != seg[2] {
		n.metrics().Refused.Add(1)
		_ = m.Ack()
		return
	}
	var f emrpb.Fetched
	if proto.Unmarshal(m.Data(), &f) != nil || len(f.RawIds) == 0 || len(f.RawIds) > sink.MaxBatch {
		n.metrics().Refused.Add(1)
		_ = m.Ack()
		return
	}
	ids := make([]uuid.UUID, 0, len(f.RawIds))
	for _, s := range f.RawIds {
		id, err := uuid.Parse(s)
		if err != nil {
			n.metrics().Refused.Add(1)
			_ = m.Ack()
			return
		}
		ids = append(ids, id)
	}
	if err := n.Handle(ctx, hid, ids); err != nil {
		if m.Deliveries() >= MaxDeliver {
			n.deadLetter(ctx, hid, ids[0])
			_ = m.Term()
			return
		}
		_ = m.NakWithDelay(time.Duration(m.Deliveries()) * 5 * time.Second)
		return
	}
	_ = m.Ack()
}

func segment(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return ""
}

func (n *Normaliser) deadLetter(ctx context.Context, hid, batch uuid.UUID) {
	b, err := proto.Marshal(&emrpb.DeadLetter{Stage: "normalise", BatchId: batch.String(), ErrorCode: "normalise_failed"})
	if err == nil {
		err = n.Pub.Publish(ctx, "dead.v1."+hid.String()+".emr_normalise", b, "emr_normalise:"+batch.String())
	}
	if err != nil {
		n.log().Error("normalise: dead letter not sent", "batch", batch.String())
		return
	}
	n.log().Warn("normalise: batch dead-lettered", "batch", batch.String())
}

// jsMsg adapts a jetstream.Msg.
type jsMsg struct{ jetstream.Msg }

func (m jsMsg) Deliveries() uint64 {
	md, err := m.Metadata()
	if err != nil {
		return MaxDeliver
	}
	return md.NumDelivered
}

// JSPublisher publishes to JetStream with a de-duplication id.
type JSPublisher struct{ JS jetstream.JetStream }

// Publish publishes data to subject with Nats-Msg-Id msgID.
func (p JSPublisher) Publish(ctx context.Context, subject string, data []byte, msgID string) error {
	_, err := p.JS.PublishMsg(ctx, &nats.Msg{Subject: subject, Data: data}, jetstream.WithMsgID(msgID))
	return err
}

// Run pulls the normaliser's durable until ctx ends. Until core has made
// the stream (CORE_EMR_ENABLED), it waits.
func (n *Normaliser) Run(ctx context.Context, js jetstream.JetStream) error {
	var cons jetstream.Consumer
	for ctx.Err() == nil {
		c, err := js.Consumer(ctx, Stream, Durable)
		if err == nil {
			cons = c
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
	for ctx.Err() == nil {
		batch, err := cons.Fetch(fetchBatch, jetstream.FetchMaxWait(fetchWait))
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		for m := range batch.Messages() {
			n.HandleMsg(ctx, jsMsg{m})
		}
	}
	return nil
}

// Sweep announces raw records stored but never announced (a refused
// publish, design §15) once they are SweepAfter old: hospitals from the
// scheduler's view of enabled connections, rows under each hospital.
func (n *Normaliser) Sweep(ctx context.Context) error {
	hospitals, err := enabledHospitals(ctx, n.Pool)
	if err != nil {
		return err
	}
	var errs []error
	for _, hid := range hospitals {
		if err := ClearExcluded(ctx, n.Pool, hid); err != nil {
			errs = append(errs, err)
		}
		var ids []uuid.UUID
		err := dbx.WithHospital(ctx, n.Pool, dbx.EMR, hid, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT id FROM emr.raw_records WHERE hospital_id = $1 AND status = 'pending'
				AND published_at IS NULL AND received_at < $2 ORDER BY received_at LIMIT 5000`, hid, n.now().Add(-SweepAfter))
			if err != nil {
				return err
			}
			ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			return err
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for len(ids) > 0 {
			k := min(len(ids), sink.MaxBatch)
			if err := PublishFetched(ctx, n.Pub, hid, ids[:k]); err != nil {
				errs = append(errs, err)
				break
			}
			if err := sink.MarkPublished(ctx, n.Pool, hid, ids[:k]); err != nil {
				errs = append(errs, err)
				break
			}
			ids = ids[k:]
		}
	}
	return errors.Join(errs...)
}

// ClearExcluded clears the payload of every raw record of hospital hid that
// names an excluded patient (directly, or through a merge), and marks a
// pending one rejected as excluded: a record stored or left behind before
// the exclusion reached it (a dead-lettered batch, a failed normalise)
// keeps nothing (design §10.6; race review 3).
func ClearExcluded(ctx context.Context, pool *pgxpool.Pool, hid uuid.UUID) error {
	return dbx.WithHospital(ctx, pool, dbx.EMR, hid, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE emr.raw_records r
			SET payload = NULL,
			    reject_code = CASE WHEN r.status = 'pending' THEN 'excluded' ELSE r.reject_code END,
			    status = CASE WHEN r.status = 'pending' THEN 'rejected' ELSE r.status END,
			    normalised_at = coalesce(r.normalised_at, now())
			FROM emr.patient_links l
			JOIN emr.patients p ON p.id = l.patient_id AND p.hospital_id = l.hospital_id
			LEFT JOIN emr.patients q ON q.id = p.merged_into AND q.hospital_id = p.hospital_id
			WHERE r.hospital_id = $1 AND l.hospital_id = $1 AND r.payload IS NOT NULL
			  AND l.connection_id = r.connection_id
			  AND l.source_patient_id = CASE WHEN r.resource_kind = 'patient' THEN r.source_id ELSE r.payload ->> 'patient' END
			  AND (p.status = 'excluded' OR q.status = 'excluded')`, hid)
		return err
	})
}

// RunSweep sweeps every SweepEvery until ctx ends.
func (n *Normaliser) RunSweep(ctx context.Context) {
	t := time.NewTicker(SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := n.Sweep(ctx); err != nil {
				n.log().Warn("normalise: sweep incomplete")
			}
		}
	}
}

// PublishFetched announces raw ids on emr.v1.<hid>.fetched (the sink's
// Publish).
func PublishFetched(ctx context.Context, pub interface {
	Publish(context.Context, string, []byte, string) error
}, hid uuid.UUID, ids []uuid.UUID) error {
	msg := &emrpb.Fetched{RawIds: make([]string, len(ids))}
	for i, id := range ids {
		msg.RawIds[i] = id.String()
	}
	b, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	return pub.Publish(ctx, "emr.v1."+hid.String()+".fetched", b, ids[0].String())
}

// enabledHospitals is the hospitals with an enabled connection, read as the
// scheduler's role (ids only, across hospitals).
func enabledHospitals(ctx context.Context, pool *pgxpool.Pool) ([]uuid.UUID, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_emr_sched`); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT hospital_id FROM public.emr_connections WHERE enabled`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}
