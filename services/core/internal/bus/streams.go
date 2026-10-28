package bus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Stream and consumer names (spec §9.2, plan departure 6). The NATS
// permissions in services/nats/nats.conf name each of them, so a rename is
// a config change too.
const (
	JobsStream       = "JOBS"      // jobs.v1.<hospital_id|global>.<type>
	DeadStream       = "DEAD"      // dead.v1.<hospital_id|global>.<type>
	PipelineConsumer = "pipeline"  // the pipeline worker's durable on JOBS
	CoreDeadConsumer = "core_dead" // core's durable on DEAD

	// PipelineFilter is the pipeline consumer's subjects: run_query jobs
	// of every hospital.
	PipelineFilter = "jobs.v1.*.run_query"

	// JobsMaxPerSubject bounds what one hospital can queue per job type.
	// Past it a publish is refused (DiscardNewPerSubject) and core answers
	// 503; queued jobs are never dropped to make room.
	//
	// It is sized so one subject can't fill the stream for everyone:
	// JobsMaxPerSubject × JobsMaxMsgSize = 200 × 72 KiB ≈ 14 MiB, at most
	// JobsMaxBytes/16 (a test holds it). 200 waiting jobs is far more than
	// a hospital needs: core lets each hospital have only 10 questions in
	// flight (Task 5, decision 8), and that is the main fairness control;
	// this cap is the backstop at the stream. Each further job type adds
	// one subject per hospital, so recheck the sum when types are added.
	JobsMaxPerSubject = 200
	JobsMaxAge        = 7 * 24 * time.Hour
	JobsDuplicates    = 2 * time.Minute // Nats-Msg-Id window (the query id)
	// JobsMaxMsgSize bounds one job message, headers included: the 64 KiB
	// job-body limit, plus 8 KiB for the headers (the request token is at
	// most 4 KiB; Nats-Msg-Id, Mhai-Schema and traceparent are small).
	JobsMaxMsgSize = 72 << 10
	// JobsMaxBytes caps the whole stream. Jobs carry ids (well under 8 KiB
	// with the token), so 256 MiB holds tens of thousands of waiting jobs.
	JobsMaxBytes = 256 << 20

	DeadMaxAge = 30 * 24 * time.Hour
	// DeadMaxMsgSize: a dead letter is ids, the type, attempts and a code
	// (a few hundred bytes).
	DeadMaxMsgSize = 4 << 10
	// DeadMaxPerSubject bounds one hospital's dead letters of one type;
	// past it the oldest of that subject is dropped (DEAD discards old).
	DeadMaxPerSubject = 10000
	// DeadMaxBytes caps the whole stream, so a flood from a compromised
	// worker credential (every worker may publish dead.v1.>) can't fill the
	// store that JOBS and the KV buckets (the denylist) need. Past it the
	// oldest letters are dropped, never the new ones: a flood can push real
	// dead letters out early, but can't lock them out. The job row in
	// Postgres keeps the failure either way; DEAD is the notice, not the
	// record.
	DeadMaxBytes = 64 << 20

	// StreamsMaxBytes is what JOBS and DEAD reserve in the server's
	// max_file_store (services/nats/nats.conf: 1 GB; the demo: 512 MB).
	StreamsMaxBytes = JobsMaxBytes + DeadMaxBytes

	PipelineAckWait    = 60 * time.Second
	PipelineMaxDeliver = 5
	CoreDeadAckWait    = 30 * time.Second

	// DefaultPipelineMaxAckPending is JOBS_PIPELINE_MAX_ACK_PENDING's
	// default: the worker count times its concurrency.
	DefaultPipelineMaxAckPending = 8
)

// StreamConfig is what EnsureStreams takes from the environment.
type StreamConfig struct {
	Replicas              int // NATS_JS_REPLICAS: 1 (dev) or 3 (production, R3); 0 means 1
	PipelineMaxAckPending int // JOBS_PIPELINE_MAX_ACK_PENDING; 0 means the default
}

var (
	errReplicas      = errors.New("bus: replicas must be 1, 3 or 5")
	errMaxAckPending = errors.New("bus: pipeline max ack pending must be positive")
)

// replicaCount checks a replica count; 0 (unset) means 1.
func replicaCount(n int) (int, error) {
	switch n {
	case 0:
		return 1, nil
	case 1, 3, 5:
		return n, nil
	}
	return 0, errReplicas
}

// EnsureStreams' errors wrap the JetStream API error, which names a code
// and the stream, never a credential.
//
// EnsureStreams creates the JOBS and DEAD streams and their durable
// consumers, or updates them to this configuration. core serve runs it at
// start-up; running it again with the same configuration changes nothing.
//
//   - JOBS is a work queue (F3): a message is removed once acked, so only
//     waiting jobs count against the per-subject limit, and a flood is
//     refused at publish instead of dropping the oldest. Work-queue streams
//     allow one consumer per subject, so every job type has exactly one
//     durable, shared by all copies of its service.
//   - The pipeline consumer has no BackOff (plan departure 4): the worker
//     chooses each retry's delay with Nak(delay).
//   - DEAD keeps dead letters 30 days, for core's consumer and for review.
//   - Both are bounded in message size, per subject and in bytes, so
//     neither can fill the store the KV buckets share. JOBS discards new
//     (a flood is refused at publish; one hospital's subject holds at most
//     1/16 of the stream); DEAD discards old (a flood evicts old letters
//     instead of refusing new ones).
func EnsureStreams(ctx context.Context, js jetstream.JetStream, sc StreamConfig) error {
	var err error
	if sc.Replicas, err = replicaCount(sc.Replicas); err != nil {
		return err
	}
	if sc.PipelineMaxAckPending == 0 {
		sc.PipelineMaxAckPending = DefaultPipelineMaxAckPending
	}
	if sc.PipelineMaxAckPending < 0 {
		return errMaxAckPending
	}
	if js == nil {
		return errors.New("bus: no JetStream")
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:                 JobsStream,
		Subjects:             []string{"jobs.v1.>"},
		Storage:              jetstream.FileStorage,
		Retention:            jetstream.WorkQueuePolicy,
		Discard:              jetstream.DiscardNew,
		DiscardNewPerSubject: true,
		MaxMsgsPerSubject:    JobsMaxPerSubject,
		MaxMsgSize:           JobsMaxMsgSize,
		MaxBytes:             JobsMaxBytes,
		MaxAge:               JobsMaxAge,
		Duplicates:           JobsDuplicates,
		Replicas:             sc.Replicas,
	}); err != nil {
		return fmt.Errorf("bus: ensure stream JOBS: %w", err)
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:              DeadStream,
		Subjects:          []string{"dead.v1.>"},
		Storage:           jetstream.FileStorage,
		Retention:         jetstream.LimitsPolicy,
		Discard:           jetstream.DiscardOld,
		MaxMsgsPerSubject: DeadMaxPerSubject,
		MaxMsgSize:        DeadMaxMsgSize,
		MaxBytes:          DeadMaxBytes,
		MaxAge:            DeadMaxAge,
		Replicas:          sc.Replicas,
	}); err != nil {
		return fmt.Errorf("bus: ensure stream DEAD: %w", err)
	}
	if _, err := js.CreateOrUpdateConsumer(ctx, JobsStream, jetstream.ConsumerConfig{
		Durable:       PipelineConsumer,
		FilterSubject: PipelineFilter,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       PipelineAckWait,
		MaxDeliver:    PipelineMaxDeliver,
		MaxAckPending: sc.PipelineMaxAckPending,
	}); err != nil {
		return fmt.Errorf("bus: ensure consumer JOBS/pipeline: %w", err)
	}
	if _, err := js.CreateOrUpdateConsumer(ctx, DeadStream, jetstream.ConsumerConfig{
		Durable:   CoreDeadConsumer,
		AckPolicy: jetstream.AckExplicitPolicy,
		AckWait:   CoreDeadAckWait,
	}); err != nil {
		return fmt.Errorf("bus: ensure consumer DEAD/core_dead: %w", err)
	}
	return nil
}
