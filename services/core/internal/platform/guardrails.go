package platform

// Guardrail statistics, GET /platform/guardrails?period=7d|30d|90d (Plan 4
// spec §6.3, new: not in the frontend contract, added as New, ADR 0012).
//
// The HGA decision counts of every active hospital over the period, and
// across all of them, from mhai_guardrail_counts() (00017: aggregates
// only, mhai_platform only). Every value is guarded by the small-group
// rules (internal/smallgroup) before it is encoded: a raw 1 to 4 never
// reaches the response writer, nor anything that could be worked out from
// what is shown (C19, C20).
//
// Platform admins and auditors, two-step always. The window is whole UTC
// days ending at the start of today (ruling R42; the previous day's until
// 00:15 UTC), and each period's day is one snapshot in guardrail_snapshots
// (R43, 00017), computed once by whichever copy asks first and served by
// every copy all day (the snapshot and the in-process cache hold the
// guarded response, never the raw counts); each session may ask 6 times a minute (429 rate_limited, C31),
// because the computation scans clinical_decisions (P17).
//
// Not recorded in the audit log: the spec rules it out (§6.3, "not audited
// per read": aggregates about no person, polled like health).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

const (
	// GuardrailsPerMinute is the reads one session may make a minute (C31).
	GuardrailsPerMinute = 6
	// guardrailsScope is the idempotency_keys scope of the read rate
	// counter; no Idempotency-Key scope (a user id) takes this form.
	guardrailsScope = "platform.guardrails.rate"
	// guardrailsTimeout bounds one computation.
	guardrailsTimeout = 10 * time.Second
)

// guardrailPeriods are the periods and their length in days.
var guardrailPeriods = map[string]int{"7d": 7, "30d": 30, "90d": 90}

// Rates is one row's guarded figures (spec §6.3): answers = pass +
// regenerate + escalate, degraded on its own, and the three shares of
// answers.
type Rates struct {
	Answers         smallgroup.Guarded      `json:"answers"`
	Pass            smallgroup.Guarded      `json:"pass"`
	Regenerate      smallgroup.Guarded      `json:"regenerate"`
	Escalate        smallgroup.Guarded      `json:"escalate"`
	Degraded        smallgroup.Guarded      `json:"degraded"`
	PassShare       smallgroup.GuardedShare `json:"pass_share"`
	RegenerateShare smallgroup.GuardedShare `json:"regenerate_share"`
	EscalateShare   smallgroup.GuardedShare `json:"escalate_share"`
}

// GuardrailHospital is one active hospital's row: {hospital, ...Rates}.
type GuardrailHospital struct {
	Hospital Ref `json:"hospital"`
	Rates
}

// GuardrailsOut is the response.
type GuardrailsOut struct {
	Period    string              `json:"period"`
	From      string              `json:"from"`
	To        string              `json:"to"`
	UpdatedAt string              `json:"updated_at"`
	All       Rates               `json:"all"`
	Hospitals []GuardrailHospital `json:"hospitals"`
}

// guardCache holds each period's guarded response for the UTC day it runs
// to (rulings R42, R43), in front of the stored snapshot: the window is
// whole days, and every copy serves the one stored row for the day, so
// every read of a period within a day is byte-identical, and a client
// can't difference two reads. One computation per period at
// a time: a caller waits for the one in progress (or gives up when its own
// request ends) rather than starting another.
type guardCache struct {
	mu      sync.Mutex
	periods map[string]*guardEntry
}

type guardEntry struct {
	lock chan struct{} // held while computing; one slot
	to   time.Time
	out  *GuardrailsOut
}

var guardrailsCache = &guardCache{periods: map[string]*guardEntry{}}

func (c *guardCache) entry(period string) *guardEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.periods[period]
	if !ok {
		e = &guardEntry{lock: make(chan struct{}, 1)}
		c.periods[period] = e
	}
	return e
}

func (c *guardCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.periods = map[string]*guardEntry{}
}

// get serves period's response for the day ending at to, computing it when
// the entry holds another day's (or none). ctx bounds only the wait.
func (c *guardCache) get(ctx context.Context, period string, to time.Time, compute func() (GuardrailsOut, error)) (GuardrailsOut, error) {
	e := c.entry(period)
	select {
	case e.lock <- struct{}{}:
	case <-ctx.Done():
		return GuardrailsOut{}, ctx.Err()
	}
	defer func() { <-e.lock }()
	if e.out != nil && e.to.Equal(to) {
		return *e.out, nil
	}
	out, err := compute()
	if err != nil {
		return GuardrailsOut{}, err
	}
	e.out, e.to = &out, to
	return out, nil
}

// GuardrailsGrace is how long after midnight UTC a day's figures wait
// before they are computed (R43): longer than any write transaction, so a
// decision committed after midnight but dated before it is in them. Until
// then the previous day's figures are served.
const GuardrailsGrace = 15 * time.Minute

// guardClock replaces the route's clock in tests (nil: Deps.now).
var guardClock func() time.Time

// guardWindow is the period's window (R42, R43): whole UTC days, ending at
// the start of the UTC day GuardrailsGrace before now. So from 00:00 to
// 00:15 UTC the figures to yesterday's midnight are served, and from 00:15
// those to today's.
func guardWindow(now time.Time, days int) (from, to time.Time) {
	to = now.UTC().Add(-GuardrailsGrace).Truncate(24 * time.Hour)
	return to.AddDate(0, 0, -days), to
}

// maxSnapshotBytes bounds a stored snapshot read back.
const maxSnapshotBytes = 4 << 20

// snapshot is period's figures for the day ending at to (R43): the stored
// row if there is one; otherwise computed, inserted if no other copy got
// there first (ON CONFLICT DO NOTHING), and the stored row read back. Every
// copy serves the stored row, so a day's figures are one snapshot whoever
// asks, whatever changed since (a late decision, a hospital deactivated).
func (d Deps) snapshot(ctx context.Context, period string, from, to time.Time) (GuardrailsOut, error) {
	ctx, cancel := context.WithTimeout(ctx, guardrailsTimeout)
	defer cancel()
	day := to.Format(time.DateOnly)
	read := func() ([]byte, error) {
		var body []byte
		err := authz.Tx(ctx, d.Pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT body::text FROM guardrail_snapshots WHERE period = $1 AND day = $2::date`, period, day).Scan(&body)
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return body, err
	}
	body, err := read()
	if err != nil {
		return GuardrailsOut{}, err
	}
	if body == nil {
		out, err := d.computeGuardrails(ctx, period, from, to)
		if err != nil {
			return GuardrailsOut{}, err
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return GuardrailsOut{}, err
		}
		if err := authz.Tx(ctx, d.Pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO guardrail_snapshots (period, day, body) VALUES ($1, $2::date, $3::jsonb)
				ON CONFLICT (period, day) DO NOTHING`, period, day, string(raw))
			return err
		}); err != nil {
			return GuardrailsOut{}, err
		}
		if body, err = read(); err != nil {
			return GuardrailsOut{}, err
		}
		if body == nil {
			return GuardrailsOut{}, errSnapshot
		}
	}
	return decodeSnapshot(body, period, from, to)
}

var errSnapshot = errors.New("platform: guardrail snapshot not usable")

// decodeSnapshot reads a stored snapshot strictly: no unknown field, every
// value a guarded one (smallgroup's decoders refuse a shown 1 to 4), the
// period and window it was stored for, four-column rows.
func decodeSnapshot(b []byte, period string, from, to time.Time) (GuardrailsOut, error) {
	var out GuardrailsOut
	if len(b) == 0 || len(b) > maxSnapshotBytes {
		return out, errSnapshot
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil || dec.More() {
		return GuardrailsOut{}, errSnapshot
	}
	if out.Period != period || out.From != stamp(from) || out.To != stamp(to) || out.UpdatedAt != stamp(to) || out.Hospitals == nil {
		return GuardrailsOut{}, errSnapshot
	}
	for _, h := range out.Hospitals {
		if !canonicalUUID(h.Hospital.ID) || !audit.ShowableName(h.Hospital.Name) {
			return GuardrailsOut{}, errSnapshot
		}
	}
	return out, nil
}

func registerGuardrails(m *authz.Mux, d Deps) {
	read := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin, authz.RoleAuditor}}
	authz.Route(m, http.MethodGet, "/platform/guardrails", read, http.HandlerFunc(d.guardrails))
}

// guardrails is GET /platform/guardrails?period → GuardrailsOut.
func (d Deps) guardrails(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	v, err := params(r, []string{"period"})
	days, known := guardrailPeriods[v["period"]]
	if err != nil || !known {
		httpx.Error(w, r, "validation_failed")
		return
	}
	period := v["period"]
	allowed, err := d.reserveRate(r, p, guardrailsScope, "platform.guardrails.rate.v1", GuardrailsPerMinute)
	switch {
	case err != nil:
		d.log().Error("platform: guardrails refused: rate counter unavailable", "request_id", httpx.RequestIDFrom(r.Context()),
			"reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return
	case !allowed:
		httpx.Error(w, r, "rate_limited")
		return
	}
	now := d.now()
	if guardClock != nil {
		now = guardClock().UTC()
	}
	from, to := guardWindow(now, days)
	// The in-process cache reads through to the stored snapshot. The work
	// outlives a caller who goes away (others may be waiting for it),
	// bounded by its own timeout.
	out, err := guardrailsCache.get(r.Context(), period, to, func() (GuardrailsOut, error) {
		return d.snapshot(context.WithoutCancel(r.Context()), period, from, to)
	})
	if err != nil {
		// No count is ever in the log: ids and a fixed reason only.
		d.log().Error("platform: guardrails failed", "request_id", httpx.RequestIDFrom(r.Context()),
			"user_id", p.UserID.String(), "reason", "internal_error")
		httpx.Error(w, r, "internal_error")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

// guardRow is one hospital's raw counts. They never leave this file.
type guardRow struct {
	id                                            uuid.UUID
	name                                          string
	answers, pass, regenerate, escalate, degraded int64
}

var errCounts = errors.New("platform: guardrail counts don't add up")

// computeGuardrails reads the counts over [from, to) and guards them.
// Counts that don't add up (or a degraded count over its answers) fail the
// response (no guess). updated_at is to: the figures run to the start of
// today, UTC.
func (d Deps) computeGuardrails(ctx context.Context, period string, from, to time.Time) (GuardrailsOut, error) {
	var rows []guardRow
	err := authz.Tx(ctx, d.Pool, func(tx pgx.Tx) error {
		q, err := tx.Query(ctx, `SELECT g.hospital_id, h.name, g.answers, g.pass, g.regenerate, g.escalate, g.degraded
			FROM mhai_guardrail_counts($1, $2) g
			JOIN hospitals h ON h.id = g.hospital_id AND h.status = 'active'
			ORDER BY h.name, h.id`, from, to)
		if err != nil {
			return err
		}
		defer q.Close()
		for q.Next() {
			var g guardRow
			if err := q.Scan(&g.id, &g.name, &g.answers, &g.pass, &g.regenerate, &g.escalate, &g.degraded); err != nil {
				return err
			}
			if g.pass < 0 || g.regenerate < 0 || g.escalate < 0 || g.degraded < 0 || g.degraded > g.answers ||
				g.answers != g.pass+g.regenerate+g.escalate {
				return errCounts
			}
			rows = append(rows, g)
		}
		return q.Err()
	})
	if err != nil {
		return GuardrailsOut{}, err
	}
	return d.guardOut(period, from, to, rows)
}

// guardOut guards rows' raw counts into the response. No rows (no active
// hospital) is a response with an all row of zeros and no hospitals.
func (d Deps) guardOut(period string, from, to time.Time, rows []guardRow) (GuardrailsOut, error) {
	table := make([][]int64, len(rows))
	degraded := make([]int64, len(rows))
	all := make([]int64, 4)
	for i, g := range rows {
		table[i] = []int64{g.answers, g.pass, g.regenerate, g.escalate}
		for j, v := range table[i] {
			all[j] += v
		}
		degraded[i] = g.degraded
	}
	// Rules 1, 2 and 4 over the hospitals and all; degraded within answers
	// and as its own column (smallgroup.Table); rule 3 for each share.
	t := smallgroup.Table(4, table, degraded)
	if len(t.All) != 4 || len(t.Rows) != len(rows) || len(t.Subset) != len(rows)+1 {
		return GuardrailsOut{}, errCounts
	}
	for _, r := range t.Rows {
		if len(r) != 4 {
			return GuardrailsOut{}, errCounts
		}
	}
	rates := func(raw []int64, g []smallgroup.Guarded, deg smallgroup.Guarded) Rates {
		return Rates{
			Answers: g[0], Pass: g[1], Regenerate: g[2], Escalate: g[3], Degraded: deg,
			PassShare:       smallgroup.ShareOf(raw[1], raw[0], g[1], g[0]),
			RegenerateShare: smallgroup.ShareOf(raw[2], raw[0], g[2], g[0]),
			EscalateShare:   smallgroup.ShareOf(raw[3], raw[0], g[3], g[0]),
		}
	}
	out := GuardrailsOut{Period: period, From: stamp(from), To: stamp(to), UpdatedAt: stamp(to),
		All: rates(all, t.All, t.Subset[0]), Hospitals: make([]GuardrailHospital, 0, len(rows))}
	for i, g := range rows {
		if !audit.ShowableName(g.name) {
			// Left out: every one of its cells is then unknown, which only
			// hides more (ids only in the log).
			d.log().Warn("platform: guardrails row not listed: a name can't be shown", "hospital_id", g.id.String())
			continue
		}
		out.Hospitals = append(out.Hospitals, GuardrailHospital{
			Hospital: Ref{ID: g.id.String(), Name: g.name},
			Rates:    rates(table[i], t.Rows[i], t.Subset[i+1]),
		})
	}
	return out, nil
}
