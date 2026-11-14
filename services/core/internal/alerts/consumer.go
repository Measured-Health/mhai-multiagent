package alerts

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
)

// The finding consumer: not built yet (failing-first stub).

const ConsumerMaxDeliver = 5

type ConsumerMetrics struct {
	Findings, Raised, Updated, Withdrawn, NotRaised, Skipped, Duplicates, Foreign, Refused, Signals, SourcePaused,
	Retried, DeadLettered, DeadFailed atomic.Int64
}

type Publisher interface {
	Publish(ctx context.Context, subject string, data []byte, msgID string) error
}

type ConsumerMsg interface {
	Subject() string
	Data() []byte
	Ack() error
	NakWithDelay(time.Duration) error
	Term() error
	Deliveries() (uint64, error)
}

type Consumer struct {
	Pool          *pgxpool.Pool
	Pub           Publisher
	AllowNullMRN  bool
	Log           *slog.Logger
	Metrics       *ConsumerMetrics
	handleFinding func(ctx context.Context, hid, fid uuid.UUID) error
}

func (c *Consumer) HandleMsg(ctx context.Context, m ConsumerMsg) { _ = m.Ack() }

func (c *Consumer) HandleFinding(ctx context.Context, hid, fid uuid.UUID) error { return nil }

func (c *Consumer) HandleSignal(ctx context.Context, hid uuid.UUID, s *emrpb.Signal) error { return nil }

func (c *Consumer) Run(ctx context.Context, js jetstream.JetStream) error { return nil }

type JSPublisher struct{ JS jetstream.JetStream }

func (p JSPublisher) Publish(ctx context.Context, subject string, data []byte, msgID string) error { return nil }
