package main

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/grant"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intel"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/natsapi"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/telemetry"
)

// jobTypeLabel is the one job type with a queue today; labels are the job
// type only, never a hospital, user or query id.
var jobTypeLabel = attribute.String("type", "run_query")

// registerMetrics exposes core's in-process counters as OpenTelemetry
// metrics (read at each collection), Prometheus-style names, low-cardinality
// labels. With telemetry off, none of it does anything.
func registerMetrics(a *app, nm *natsapi.Metrics, gm *grant.Metrics) {
	count := func(name, help string, read func() int64, attrs ...attribute.KeyValue) {
		if err := telemetry.ObserveCount(name, help, read, attrs...); err != nil {
			a.log.Warn("core: a metric was not registered", "metric", name)
		}
	}
	count("natsapi_refused_replies_total", "NATS API messages dropped for a bad reply subject.", nm.RefusedReply.Load)
	count("natsapi_overflow_total", "NATS API messages answered unavailable because the pool was full.", nm.Overflow.Load)
	count("grant_refusals_total", "Grant requests refused (not the rate or in-flight bounds).", gm.Refused.Load)
	count("grant_verified_refusals_total", "Grant refusals of a verified token; its rate shows a flood the audit does not cap.", gm.VerifiedRefused.Load)
	count("grant_refusal_flood_total", "Times one session reached a multiple of grant.FloodEvery refusals in a minute.", gm.RefusalFlood.Load)
	count("grant_audit_skipped_total", "Unverified-token refusals counted but not audited (past the per-service rate).", gm.AuditSkipped.Load)
	count("grant_audit_failed_total", "Grant audit writes that failed.", gm.AuditFailed.Load)
	count("grant_dropped_replies_total", "Grant requests dropped for a bad subject or reply subject.", gm.DroppedReply.Load)
	count("grant_rate_limited_total", "Grant requests refused by the session or hospital rate.", gm.RateLimited.Load)
	count("grant_overloaded_total", "Grant requests refused because too many were in flight.", gm.Overloaded.Load)
	count("grant_granted_total", "Grants given.", gm.Granted.Load)
	im := a.intel.Metrics
	count("jobs_dead_letters_total", "Dead letters read from workers.", im.DeadReceived.Load, jobTypeLabel)
	count("intelligence_dead_ignored_total", "Dead letters that changed nothing (malformed, replayed, or naming no job).", im.DeadIgnored.Load)
	count("intelligence_dead_refused_total", "Jobs a worker refused at the boundary (alert).", im.DeadRefused.Load)

	pool := a.pool
	var mu sync.Mutex
	var depth, oldest float64
	var read time.Time
	// One query feeds both gauges per collection.
	stats := func(ctx context.Context) (float64, float64) {
		mu.Lock()
		defer mu.Unlock()
		if pool == nil || time.Since(read) < time.Second {
			return depth, oldest
		}
		read = time.Now()
		if n, d, err := intel.QueueStats(ctx, pool); err == nil {
			depth, oldest = float64(n), d.Seconds()
		}
		return depth, oldest
	}
	if err := telemetry.Observe("jobs_queue_depth", "Queued run_query jobs.", false, func(ctx context.Context) []telemetry.Sample {
		depth, _ := stats(ctx)
		return []telemetry.Sample{{Value: depth, Attrs: []attribute.KeyValue{jobTypeLabel}}}
	}); err != nil {
		a.log.Warn("core: a metric was not registered", "metric", "jobs_queue_depth")
	}
	if err := telemetry.Observe("jobs_oldest_wait_seconds", "Age of the oldest queued run_query job.", false, func(ctx context.Context) []telemetry.Sample {
		_, oldest := stats(ctx)
		return []telemetry.Sample{{Value: oldest, Attrs: []attribute.KeyValue{jobTypeLabel}}}
	}); err != nil {
		a.log.Warn("core: a metric was not registered", "metric", "jobs_oldest_wait_seconds")
	}
}
