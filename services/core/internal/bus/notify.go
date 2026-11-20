package bus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// The NOTIFY stream and its durable (notifications design §7, §9; §15
// PR 3). Core creates them only when CORE_NOTIFY_ENABLED is on (serve's
// ensureNotify); off, no stream exists and core publishes nothing. Only
// core (gateway) publishes notify.v1.<hid|platform>.due, a message id and
// nothing else; the notifications user may only pull and ack notify_send.
const (
	NotifyStream = "NOTIFY"
	// NotifySendConsumer: the notifications service's sender.
	NotifySendConsumer = "notify_send"
	// NotifySubjects: notify.v1.<hospital id | platform>.due.
	NotifySubjects = "notify.v1.*.due"

	// NotifyMaxPerSubject bounds what one hospital can queue; past it a
	// publish is refused (DiscardNewPerSubject), so one hospital can't fill
	// the stream for the others, and the drainer retries.
	NotifyMaxPerSubject = 10000
	NotifyMaxAge        = 3 * 24 * time.Hour
	NotifyDuplicates    = 2 * time.Minute // Nats-Msg-Id = the message id
	// NotifyMaxMsgSize: one id (notifypb.Due), plus headers.
	NotifyMaxMsgSize = 1 << 10
	// NotifyMaxBytes caps the stream: ids only, so it is small.
	NotifyMaxBytes = 16 << 20

	// The sender's durable: a delivery holds its lease 2 minutes, so a
	// message not acked by then is redelivered and the claim (reserve, then
	// check) decides who sends. Retries and holds are naks with a delay
	// (up to 6 hours, for 24 hours), so MaxDeliver leaves room for them and
	// the rate limits' hourly holds; at most 16 messages in flight.
	NotifySendAckWait       = 2 * time.Minute
	NotifySendMaxDeliver    = 200
	NotifySendMaxAckPending = 16
)

// EnsureNotifyStream creates NOTIFY and notify_send, or updates them to
// this configuration: a work queue on notify.v1.*.due, file storage, 3
// days, bounded per subject with discard-new, duplicates dropped within 2
// minutes.
func EnsureNotifyStream(ctx context.Context, js jetstream.JetStream, replicas int) error {
	r, err := replicaCount(replicas)
	if err != nil {
		return err
	}
	if js == nil {
		return errors.New("bus: no JetStream")
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:                 NotifyStream,
		Subjects:             []string{NotifySubjects},
		Storage:              jetstream.FileStorage,
		Retention:            jetstream.WorkQueuePolicy,
		Discard:              jetstream.DiscardNew,
		DiscardNewPerSubject: true,
		MaxMsgsPerSubject:    NotifyMaxPerSubject,
		MaxMsgSize:           NotifyMaxMsgSize,
		MaxBytes:             NotifyMaxBytes,
		MaxAge:               NotifyMaxAge,
		Duplicates:           NotifyDuplicates,
		Replicas:             r,
	}); err != nil {
		return fmt.Errorf("bus: ensure stream NOTIFY: %w", err)
	}
	if _, err := js.CreateOrUpdateConsumer(ctx, NotifyStream, jetstream.ConsumerConfig{
		Durable:       NotifySendConsumer,
		FilterSubject: NotifySubjects,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       NotifySendAckWait,
		MaxDeliver:    NotifySendMaxDeliver,
		MaxAckPending: NotifySendMaxAckPending,
	}); err != nil {
		return fmt.Errorf("bus: ensure consumer NOTIFY/%s: %w", NotifySendConsumer, err)
	}
	return nil
}
