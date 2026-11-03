package platform

// The health sampler (Plan 4 spec §6.1): every 30 s one core copy (the
// holder of a Postgres advisory lock) takes a reading and writes it to the
// health bucket, which GET /platform/health serves. Off the request path,
// with no principal: it reads the database as mhai_platform (the health
// functions, aggregates only) and mhai_jobs (the queue counts), and calls
// pipeline.health as svc:core.
//
// A reading is written only when every figure was read: if the database
// can't be read, nothing is written, the last reading ages past two minutes
// and the route answers 503 ("Couldn't check", never zeros). A worker that
// doesn't answer is a figure: workers and evidence are problem.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intel"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
)

const (
	// SampleEvery is how often the leader takes a reading.
	SampleEvery = 30 * time.Second
	// slowWorker: a pipeline.health answer slower than this is slow.
	slowWorker = time.Second
	// sampleTimeout bounds one reading (pipeline.health's own timeout is
	// 2 s; QueueStats 3 s each).
	sampleTimeout = 15 * time.Second
	// healthLock is the advisory lock that makes one core copy the
	// sampler.
	healthLock = "mhai.platform.health"
	// maxFailures: a leader whose readings fail this many times in a row
	// steps down (closes its lock connection), so a copy that can sample
	// takes over.
	maxFailures = 2
	// stepDownTicks: after stepping down a copy leaves the lock alone this
	// many ticks, so another copy gets it first.
	stepDownTicks = 3
)

// leaderSession bounds how long a frozen leader (its host or network gone
// without a FIN) keeps the lock: the server drops the session, and with it
// the lock, after about 30 s of silence instead of the kernel's default
// (about 2 hours). Session settings, so they need no privilege.
const leaderSession = `SET tcp_keepalives_idle = 15; SET tcp_keepalives_interval = 5; SET tcp_keepalives_count = 3; SET tcp_user_timeout = 30000`

// healthProbeHospital is the hospital the pipeline.health token names. A
// request token always carries one (Plan 3 F10), but the probe is about no
// hospital: this is a fixed name-based (version 5) uuid, which no hospital
// row can have (hospital ids are random, version 4). It is only ever in
// the token, never in the reading.
var healthProbeHospital = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:mhai:platform:health-probe")).String()

// Sampler takes health readings. RunSampler runs one as the leader.
type Sampler struct {
	Pool  *pgxpool.Pool
	Intel *intel.Client
	KV    jetstream.KeyValue
	Log   *slog.Logger
	// Now dates the reading; nil is time.Now.
	Now func() time.Time
	// Sampled, when set, is called with each reading written (tests).
	Sampled func(HealthOut)
}

func (s Sampler) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (s Sampler) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// RunSampler runs the sampler with d's pool, intelligence client, health
// bucket, logger and clock, every every (SampleEvery when 0), until ctx
// ends.
func RunSampler(ctx context.Context, d Deps, every time.Duration) error {
	return Sampler{Pool: d.Pool, Intel: d.Intel, KV: d.Health, Log: d.Log, Now: d.Now}.Run(ctx, every)
}

var errSampler = errors.New("platform: the sampler needs a pool, a health bucket and an intelligence client")

// Run takes a reading every every while it holds the leader lock, until
// ctx ends. A copy that doesn't hold it tries again each tick, so another
// copy takes over within one tick of the leader's end. The lock is a
// session lock on a connection taken out of the pool for as long as this
// copy leads; closing that connection (at the end, or when it fails a
// ping) releases it.
func (s Sampler) Run(ctx context.Context, every time.Duration) error {
	if s.Pool == nil || s.KV == nil || s.Intel == nil {
		return errSampler
	}
	if every <= 0 {
		every = SampleEvery
	}
	var lead *pgx.Conn
	defer func() {
		if lead != nil {
			c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = lead.Close(c)
		}
	}()
	t := time.NewTicker(every)
	defer t.Stop()
	failures, wait := 0, 0
	for {
		if wait > 0 {
			wait--
		} else if lead = s.lead(ctx, lead); lead == nil {
			failures = 0
		} else if _, err := s.Sample(ctx); err == nil {
			failures = 0
		} else if ctx.Err() == nil {
			failures++
			s.log().Error("platform: health reading not taken", "reason", "internal_error", "failures", failures)
			if failures >= maxFailures {
				// Step down: closing the session releases the lock.
				c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				_ = lead.Close(c)
				cancel()
				lead, failures, wait = nil, 0, stepDownTicks
				s.log().Warn("platform: health sampler stepped down after failed readings")
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// lead returns the leader's connection: held, if it still answers, or a
// new one if this copy can take the lock now; nil if another copy leads.
func (s Sampler) lead(ctx context.Context, held *pgx.Conn) *pgx.Conn {
	if held != nil {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := held.Ping(c)
		cancel()
		if err == nil {
			return held
		}
		// The session (and with it the lock) may be gone: let it go.
		c, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = held.Close(c)
		cancel()
		if ctx.Err() == nil {
			s.log().Warn("platform: health sampler lost its lock connection")
		}
		return nil
	}
	pc, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil
	}
	var locked bool
	if err := pc.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, healthLock).Scan(&locked); err != nil || !locked {
		pc.Release()
		return nil
	}
	// Out of the pool: the lock lives as long as this connection, and the
	// pool can't hand it to a request.
	conn := pc.Hijack()
	if _, err := conn.Exec(ctx, leaderSession); err != nil {
		// Without the timeouts a frozen leader could hold the lock for
		// hours: don't lead.
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = conn.Close(c)
		cancel()
		return nil
	}
	return conn
}

// Sample takes one reading and writes it to the health bucket. Run calls it
// as the leader; nothing else should in production.
func (s Sampler) Sample(ctx context.Context) (HealthOut, error) {
	if s.Pool == nil || s.KV == nil || s.Intel == nil {
		return HealthOut{}, errSampler
	}
	ctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()
	at := s.now()
	prev := s.previous(ctx, at)

	workers, evidence := s.workers(ctx)
	figures, err := s.figures(ctx)
	if err != nil {
		return HealthOut{}, err
	}
	guidelines := StatusProblem
	if figures.guidelines {
		guidelines = StatusOK
	}
	statuses := map[string]string{PartApp: StatusOK, PartWorkers: workers, PartEvidence: evidence, PartGuidelines: guidelines}
	h := HealthOut{
		UpdatedAt:  stamp(at),
		Queue:      figures.queue,
		FailedJobs: figures.failed,
		Timing:     figures.timing,
		Errors:     figures.errors,
		// HealthResponse carries no model versions yet (Plan 3's
		// pipeline.proto: models_ready, database_ready), so there is
		// nothing safe to show: none are invented.
		Versions: []HealthVersion{},
		Triage:   HealthTriage{Enabled: false},
	}
	var all []string
	for _, key := range Parts {
		st := statuses[key]
		since := h.UpdatedAt
		if p, ok := prev[key]; ok && p.Status == st && p.Since <= h.UpdatedAt {
			since = p.Since
		}
		h.Parts = append(h.Parts, HealthPart{Key: key, Status: st, Since: since})
		all = append(all, st)
	}
	h.Overall = worst(all...)
	if !h.Valid() {
		return HealthOut{}, errReading
	}
	b, err := json.Marshal(h)
	if err != nil || len(b) > maxReadingBytes {
		return HealthOut{}, errReading
	}
	if _, err := s.KV.Put(ctx, HealthKey, b); err != nil {
		return HealthOut{}, errors.New("platform: health reading not written")
	}
	if s.Sampled != nil {
		s.Sampled(h)
	}
	return h, nil
}

// stamp formats an instant as the reading does: RFC 3339, UTC,
// milliseconds.
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// previous is the last reading's parts, for since: a part keeps its since
// while its status holds, across samples and leaders. A missing or
// unusable reading gives none (every since is now), and so does one taken
// more than HealthMaxAge before at, or after it: across a gap nobody
// watched, the status may have changed and back.
func (s Sampler) previous(ctx context.Context, at time.Time) map[string]HealthPart {
	out := map[string]HealthPart{}
	e, err := s.KV.Get(ctx, HealthKey)
	if err != nil {
		return out
	}
	h, err := decodeReading(e.Value())
	if err != nil {
		return out
	}
	prevAt, _ := parseTime(h.UpdatedAt)
	if prevAt.After(at) || at.Sub(prevAt) > HealthMaxAge {
		return out
	}
	for _, p := range h.Parts {
		out[p.Key] = p
	}
	return out
}

// workers asks pipeline.health as svc:core: ok within a second, slow
// after, problem with no answer, an error answer, or a worker whose
// database isn't ready. evidence is ok only when the worker answered with
// its models ready: HealthResponse has no separate NLI flag yet, so an
// unknown is problem, never ok.
func (s Sampler) workers(ctx context.Context) (workers, evidence string) {
	var resp intelpb.HealthResponse
	started := time.Now()
	_, err := s.Intel.Request(ctx, intel.System(healthProbeHospital, "health-"+uuid.NewString()), "pipeline.health",
		&intelpb.HealthRequest{}, &resp)
	took := time.Since(started)
	if err != nil {
		return StatusProblem, StatusProblem
	}
	workers = StatusOK
	if !resp.GetDatabaseReady() {
		workers = StatusProblem
	} else if took > slowWorker {
		workers = StatusSlow
	}
	evidence = StatusProblem
	if resp.GetModelsReady() {
		evidence = StatusOK
	}
	return workers, evidence
}

// dbFigures are the reading's figures from the database.
type dbFigures struct {
	queue      HealthQueue
	failed     HealthFailed
	timing     HealthTiming
	errors     []HealthErrors
	guidelines bool
}

// figures reads the queue (as mhai_jobs, intel's counts) and the rest
// through the health functions (as mhai_platform, in one snapshot).
func (s Sampler) figures(ctx context.Context) (dbFigures, error) {
	var f dbFigures
	n1, o1, err := intel.QueueStats(ctx, s.Pool)
	if err != nil {
		return f, err
	}
	n2, o2, err := intel.LearningQueueStats(ctx, s.Pool)
	if err != nil {
		return f, err
	}
	f.queue.Waiting = n1 + n2
	if f.queue.Waiting > 0 {
		oldest := int64(max(o1, o2) / time.Second)
		f.queue.OldestWaitS = &oldest
	}
	byKind := map[string]*HealthErrors{}
	for _, k := range intel.ErrorKinds {
		f.errors = append(f.errors, HealthErrors{Kind: k})
	}
	for i := range f.errors {
		byKind[f.errors[i].Kind] = &f.errors[i]
	}
	err = db.WithPlatformSnapshot(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		if f.timing.LastHour, err = timingBlock(ctx, tx, "1 hour"); err != nil {
			return err
		}
		if f.timing.Last24h, err = timingBlock(ctx, tx, "24 hours"); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT error, failed_now, failed_last_hour, failed_last_24h FROM mhai_health_failures()`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var code *string
			var now, hour, day int64
			if err := rows.Scan(&code, &now, &hour, &day); err != nil {
				return err
			}
			c := ""
			if code != nil {
				c = *code
			}
			e := byKind[intel.ErrorKind(c)]
			e.LastHour += hour
			e.Last24h += day
			f.failed.Now += now
			f.failed.Last24h += day
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT mhai_health_guidelines()`).Scan(&f.guidelines)
	})
	if err != nil {
		// A driver error can carry values; the caller logs a fixed reason.
		return dbFigures{}, errors.New("platform: health figures not read")
	}
	return f, nil
}

// timingBlock reads one window's timings: overall always (0 with no
// samples), each step only when it has samples.
func timingBlock(ctx context.Context, tx pgx.Tx, window string) (TimingBlock, error) {
	b := TimingBlock{Steps: []StepTiming{}}
	rows, err := tx.Query(ctx, `SELECT step, p50_ms, p95_ms, samples FROM mhai_health_timings($1)`, window)
	if err != nil {
		return b, err
	}
	defer rows.Close()
	for rows.Next() {
		var step string
		var p50, p95 *int32
		var n int64
		if err := rows.Scan(&step, &p50, &p95, &n); err != nil {
			return b, err
		}
		if n == 0 || p50 == nil || p95 == nil {
			continue
		}
		if step == "overall" {
			b.Overall = Timing{P50ms: int64(*p50), P95ms: int64(*p95)}
			continue
		}
		b.Steps = append(b.Steps, StepTiming{Step: step, P50ms: int64(*p50), P95ms: int64(*p95)})
	}
	return b, rows.Err()
}
