package bus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// The EMR stream and its durables (EMR design §3.2, plan Task 5 and
// departure 18). Core creates them, as it does JOBS and DEAD; the emr user
// may only read and ack its own two durables. Created only when
// CORE_EMR_ENABLED is on (serve's ensureEMR).
const (
	EMRStream = "EMR"
	// EMRNormaliseConsumer: the emr service's normaliser, on fetched raw
	// records.
	EMRNormaliseConsumer = "emr_normalise"
	// EMRRulesConsumer: the emr service's rules, on canonical records.
	EMRRulesConsumer = "emr_rules"
	// CoreEMRConsumer: core's finding and signal consumer.
	CoreEMRConsumer = "core_emr"

	// EMRMaxPerSubject bounds what one hospital can queue per event kind;
	// past it a publish is refused (DiscardNewPerSubject) and the emr
	// service's sweeper retries from its rows (design §15).
	EMRMaxPerSubject = 50000
	EMRMaxAge        = 7 * 24 * time.Hour
	EMRDuplicates    = 2 * time.Minute // Nats-Msg-Id = the row id
	// EMRMaxMsgSize: ids only (at most 500 record ids a message), plus
	// headers.
	EMRMaxMsgSize = 64 << 10
	// EMRMaxBytes caps the stream, so with JOBS and DEAD it still leaves
	// the KV buckets room in the demo's 512 MB store.
	EMRMaxBytes = 64 << 20

	EMRAckWait    = 60 * time.Second
	EMRMaxDeliver = 5
)

// EnsureEMRStream creates the EMR stream and its three durables, or
// updates them to this configuration. A work queue: each subject has one
// consumer, and an acked message is gone; the per-subject bound refuses a
// flood at publish instead of dropping queued events.
func EnsureEMRStream(ctx context.Context, js jetstream.JetStream, replicas int) error {
	r, err := replicaCount(replicas)
	if err != nil {
		return err
	}
	if js == nil {
		return errors.New("bus: no JetStream")
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:                 EMRStream,
		Subjects:             []string{"emr.v1.*.fetched", "emr.v1.*.finding", "emr.v1.*.signal", "records.v1.>"},
		Storage:              jetstream.FileStorage,
		Retention:            jetstream.WorkQueuePolicy,
		Discard:              jetstream.DiscardNew,
		DiscardNewPerSubject: true,
		MaxMsgsPerSubject:    EMRMaxPerSubject,
		MaxMsgSize:           EMRMaxMsgSize,
		MaxBytes:             EMRMaxBytes,
		MaxAge:               EMRMaxAge,
		Duplicates:           EMRDuplicates,
		Replicas:             r,
	}); err != nil {
		return fmt.Errorf("bus: ensure stream EMR: %w", err)
	}
	for _, c := range []jetstream.ConsumerConfig{
		{Durable: EMRNormaliseConsumer, FilterSubject: "emr.v1.*.fetched"},
		{Durable: EMRRulesConsumer, FilterSubject: "records.v1.*.created"},
		{Durable: CoreEMRConsumer, FilterSubjects: []string{"emr.v1.*.finding", "emr.v1.*.signal"}},
	} {
		c.AckPolicy, c.AckWait, c.MaxDeliver = jetstream.AckExplicitPolicy, EMRAckWait, EMRMaxDeliver
		if _, err := js.CreateOrUpdateConsumer(ctx, EMRStream, c); err != nil {
			return fmt.Errorf("bus: ensure consumer EMR/%s: %w", c.Durable, err)
		}
	}
	return nil
}
