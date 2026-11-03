package platform

// System health, GET /platform/health (Plan 4 spec §6.1; contract
// §14.8.2; mhai-web's apps/platform/lib/api/ops-contract.ts HealthOut).
//
// The route serves the reading the sampler (sampler.go) took, from the
// health KV bucket, and nothing else: a poll costs one KV get, never a
// database statement or a worker call (C30, P17), so a polling client
// can't make core do work. A missing reading, one older than two minutes,
// one dated ahead of this clock, or one that doesn't hold the contract's
// shape is 503 unavailable: the app shows "Couldn't check", never zeros.
//
// No hospital, person, patient or content is anywhere in it: counts,
// percentiles, statuses and times only. Per decision 7 the documents and
// clean_up parts and the clean_up block are not sent until those services
// exist; triage is {enabled: false} until triage exists (§8).
//
// Reads aren't audited (spec §6.1, §6.3: aggregates about no person,
// polled every 30 s; recording each poll would be a database write per
// poll, which C30 forbids).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intel"
)

// The reading's statuses (PartStatus) and part keys (PartKey) this build
// sends: documents and clean_up are never sent (decision 7).
const (
	StatusOK      = "ok"
	StatusSlow    = "slow"
	StatusProblem = "problem"

	PartApp        = "app"
	PartWorkers    = "workers"
	PartEvidence   = "evidence"
	PartGuidelines = "guidelines"
)

// Steps are the timing steps (StepKey), in the contract's order.
var Steps = []string{"privacy", "guidelines", "answer", "evidence"}

// Parts are the parts this build reports, in order.
var Parts = []string{PartApp, PartWorkers, PartEvidence, PartGuidelines}

const (
	// HealthKey is the reading's one key in the health bucket.
	HealthKey = "health.reading"
	// HealthMaxAge: a reading older than this is no reading (contract
	// §14.8.2).
	HealthMaxAge = 2 * time.Minute
	// healthSkew is how far ahead of this clock a reading may be dated
	// (another core copy's clock); further is refused.
	healthSkew = 30 * time.Second
	// maxReadingBytes bounds the stored reading (it is about 2 KB).
	maxReadingBytes = 16 << 10
	// healthReadTimeout bounds the route's KV get.
	healthReadTimeout = 2 * time.Second
)

// HealthOut is the reading (contract §14.8.2), as stored and as served.
type HealthOut struct {
	UpdatedAt  string          `json:"updated_at"`
	Overall    string          `json:"overall"`
	Parts      []HealthPart    `json:"parts"`
	Queue      HealthQueue     `json:"queue"`
	FailedJobs HealthFailed    `json:"failed_jobs"`
	Timing     HealthTiming    `json:"timing"`
	Errors     []HealthErrors  `json:"errors"`
	Versions   []HealthVersion `json:"versions"`
	Triage     HealthTriage    `json:"triage"`
}

// HealthPart is one part's status and since when it has had it.
type HealthPart struct {
	Key    string `json:"key"`
	Status string `json:"status"`
	Since  string `json:"since"`
}

// HealthQueue is the jobs waiting to be picked up; OldestWaitS is null
// when none waits.
type HealthQueue struct {
	Waiting     int64  `json:"waiting"`
	OldestWaitS *int64 `json:"oldest_wait_s"`
}

// HealthFailed is the failed jobs: failed now, and of those, failed in the
// last 24 hours.
type HealthFailed struct {
	Now     int64 `json:"now"`
	Last24h int64 `json:"last_24h"`
}

// HealthTiming is the question timings over the last hour and 24 hours.
type HealthTiming struct {
	LastHour TimingBlock `json:"last_hour"`
	Last24h  TimingBlock `json:"last_24h"`
}

// Timing is a p50 and p95 in milliseconds.
type Timing struct {
	P50ms int64 `json:"p50_ms"`
	P95ms int64 `json:"p95_ms"`
}

// TimingBlock is the overall timing and each step's. A step with no
// samples in the window is left out of Steps.
type TimingBlock struct {
	Overall Timing       `json:"overall"`
	Steps   []StepTiming `json:"steps"`
}

// StepTiming is one step's timing.
type StepTiming struct {
	Step  string `json:"step"`
	P50ms int64  `json:"p50_ms"`
	P95ms int64  `json:"p95_ms"`
}

// HealthErrors is one kind's failed jobs in the last hour and 24 hours.
type HealthErrors struct {
	Kind     string `json:"kind"`
	LastHour int64  `json:"last_hour"`
	Last24h  int64  `json:"last_24h"`
}

// HealthVersion is a part's model version label.
type HealthVersion struct {
	Part  string `json:"part"`
	Label string `json:"label"`
}

// HealthTriage is whether triage is on: false until triage exists.
type HealthTriage struct {
	Enabled bool `json:"enabled"`
}

// versionLabel is a label safe to show (spec §6.1: "the safe pattern").
var versionLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+-]{0,63}$`)

var errReading = errors.New("platform: health reading not usable")

// parseTime reads a reading's instant (RFC 3339, UTC).
func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil && t.Location() == time.UTC
}

// worst is the worst of the statuses: problem over slow over ok.
func worst(statuses ...string) string {
	out := StatusOK
	for _, s := range statuses {
		if s == StatusProblem {
			return StatusProblem
		}
		if s == StatusSlow {
			out = StatusSlow
		}
	}
	return out
}

func validStatus(s string) bool { return s == StatusOK || s == StatusSlow || s == StatusProblem }

func validTiming(t Timing) bool { return t.P50ms >= 0 && t.P95ms >= t.P50ms }

func validBlock(b TimingBlock) bool {
	if !validTiming(b.Overall) || b.Steps == nil {
		return false
	}
	seen := map[string]bool{}
	last := -1
	for _, s := range b.Steps {
		i := slices.Index(Steps, s.Step)
		if i <= last || seen[s.Step] || !validTiming(Timing{s.P50ms, s.P95ms}) {
			return false
		}
		seen[s.Step], last = true, i
	}
	return true
}

// Valid reports whether h holds the contract's shape as this build sends
// it: every enum known, every count non-negative, the four parts in order
// (never documents or clean_up), overall the worst of them, triage off,
// each time UTC and no part's since after updated_at.
func (h HealthOut) Valid() bool {
	at, ok := parseTime(h.UpdatedAt)
	if !ok || !validStatus(h.Overall) || len(h.Parts) != len(Parts) {
		return false
	}
	var statuses []string
	for i, p := range h.Parts {
		since, ok := parseTime(p.Since)
		if !ok || p.Key != Parts[i] || !validStatus(p.Status) || since.After(at) {
			return false
		}
		statuses = append(statuses, p.Status)
	}
	if h.Overall != worst(statuses...) || h.Parts[0].Status != StatusOK {
		return false
	}
	q := h.Queue
	if q.Waiting < 0 || (q.Waiting == 0) != (q.OldestWaitS == nil) || (q.OldestWaitS != nil && *q.OldestWaitS < 0) {
		return false
	}
	if h.FailedJobs.Now < 0 || h.FailedJobs.Last24h < 0 || h.FailedJobs.Last24h > h.FailedJobs.Now {
		return false
	}
	if !validBlock(h.Timing.LastHour) || !validBlock(h.Timing.Last24h) {
		return false
	}
	if len(h.Errors) != len(intel.ErrorKinds) {
		return false
	}
	for i, e := range h.Errors {
		if e.Kind != intel.ErrorKinds[i] || e.LastHour < 0 || e.Last24h < e.LastHour {
			return false
		}
	}
	if h.Versions == nil {
		return false
	}
	for _, v := range h.Versions {
		if !slices.Contains(Parts, v.Part) || !versionLabel.MatchString(v.Label) {
			return false
		}
	}
	return !h.Triage.Enabled
}

// decodeReading parses a stored reading strictly: no unknown field, no
// trailing data, the contract's shape.
func decodeReading(b []byte) (HealthOut, error) {
	var h HealthOut
	if len(b) == 0 || len(b) > maxReadingBytes {
		return h, errReading
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil || dec.More() || !h.Valid() {
		return HealthOut{}, errReading
	}
	return h, nil
}

// ReadHealth returns the stored reading if it is usable at now: present,
// of the contract's shape, at most HealthMaxAge old and not dated more
// than healthSkew ahead. One KV get; nothing else.
func ReadHealth(ctx context.Context, kv jetstream.KeyValue, now time.Time) (HealthOut, string, error) {
	if kv == nil {
		return HealthOut{}, "no_bucket", errReading
	}
	ctx, cancel := context.WithTimeout(ctx, healthReadTimeout)
	defer cancel()
	e, err := kv.Get(ctx, HealthKey)
	if bus.IsNotFound(err) {
		return HealthOut{}, "missing", errReading
	}
	if err != nil {
		return HealthOut{}, "kv_unreadable", errReading
	}
	h, err := decodeReading(e.Value())
	if err != nil {
		return HealthOut{}, "invalid", err
	}
	at, _ := parseTime(h.UpdatedAt)
	if now.Sub(at) > HealthMaxAge {
		return HealthOut{}, "stale", errReading
	}
	if at.Sub(now) > healthSkew {
		return HealthOut{}, "future", errReading
	}
	return h, "", nil
}

func registerHealth(m *authz.Mux, d Deps) {
	read := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin, authz.RoleAuditor}}
	authz.Route(m, http.MethodGet, "/platform/health", read, http.HandlerFunc(d.health))
}

// health is GET /platform/health → the stored reading, or 503
// unavailable. No query parameters.
func (d Deps) health(w http.ResponseWriter, r *http.Request) {
	if _, ok := caller(w, r); !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	h, why, err := ReadHealth(r.Context(), d.Health, d.now())
	if err != nil {
		// why is one of the fixed reasons above; the reading's content is
		// never logged.
		d.log().Warn("platform: no health reading to serve", "request_id", httpx.RequestIDFrom(r.Context()), "reason", why)
		httpx.Error(w, r, "unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, h)
}
