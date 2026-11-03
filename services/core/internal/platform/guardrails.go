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
// Platform admins and auditors, two-step always. Each period's response is
// computed at most once a minute per core copy (the cache holds the guarded
// response, never the raw counts), and each session may ask 6 times a
// minute (429 rate_limited, C31), because the computation scans
// clinical_decisions (P17).
//
// Not recorded in the audit log: the spec rules it out (§6.3, "not audited
// per read": aggregates about no person, polled like health).

import (
	"context"
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
	// GuardrailsCacheTTL is how long one period's response is served
	// before it is computed again.
	GuardrailsCacheTTL = 60 * time.Second
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

// guardCache holds each period's guarded response for GuardrailsCacheTTL.
// One computation per period at a time: a caller waits for the one in
// progress rather than starting another.
type guardCache struct {
	mu      sync.Mutex
	periods map[string]*guardEntry
}

type guardEntry struct {
	mu  sync.Mutex
	at  time.Time
	out *GuardrailsOut
}

var guardrailsCache = &guardCache{periods: map[string]*guardEntry{}}

func (c *guardCache) entry(period string) *guardEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.periods[period]
	if !ok {
		e = &guardEntry{}
		c.periods[period] = e
	}
	return e
}

func (c *guardCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.periods = map[string]*guardEntry{}
}

// get serves period's response, computing it when there is none fresh at
// now (an entry dated after now, another clock's, is not fresh either).
func (c *guardCache) get(period string, now time.Time, compute func() (GuardrailsOut, error)) (GuardrailsOut, error) {
	e := c.entry(period)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.out != nil && !now.Before(e.at) && now.Sub(e.at) < GuardrailsCacheTTL {
		return *e.out, nil
	}
	out, err := compute()
	if err != nil {
		return GuardrailsOut{}, err
	}
	e.out, e.at = &out, now
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
	out, err := guardrailsCache.get(period, now, func() (GuardrailsOut, error) {
		return d.computeGuardrails(r.Context(), period, days, now)
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

// computeGuardrails reads the counts over [now − days, now) and guards
// them: rules 1, 2 and 4 over the table of hospitals and all
// (smallgroup.AcrossRows), rule 1 and 2 over degraded's column, rule 3 for
// each share. Counts that don't add up fail the response (no guess).
func (d Deps) computeGuardrails(ctx context.Context, period string, days int, now time.Time) (GuardrailsOut, error) {
	ctx, cancel := context.WithTimeout(ctx, guardrailsTimeout)
	defer cancel()
	to := now.UTC().Truncate(time.Millisecond)
	from := to.Add(-time.Duration(days) * 24 * time.Hour)
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
			if g.pass < 0 || g.regenerate < 0 || g.escalate < 0 || g.degraded < 0 ||
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

	all := make([]int64, 4)
	table := make([][]int64, len(rows))
	degraded := make([]int64, len(rows))
	var allDegraded int64
	for i, g := range rows {
		table[i] = []int64{g.answers, g.pass, g.regenerate, g.escalate}
		for j, v := range table[i] {
			all[j] += v
		}
		degraded[i] = g.degraded
		allDegraded += g.degraded
	}
	allG, rowsG := smallgroup.AcrossRows(all, table)
	degG := smallgroup.Set(allDegraded, degraded...)
	rates := func(raw []int64, g []smallgroup.Guarded, deg smallgroup.Guarded) Rates {
		return Rates{
			Answers: g[0], Pass: g[1], Regenerate: g[2], Escalate: g[3], Degraded: deg,
			PassShare:       smallgroup.ShareOf(raw[1], raw[0], g[1], g[0]),
			RegenerateShare: smallgroup.ShareOf(raw[2], raw[0], g[2], g[0]),
			EscalateShare:   smallgroup.ShareOf(raw[3], raw[0], g[3], g[0]),
		}
	}
	out := GuardrailsOut{Period: period, From: stamp(from), To: stamp(to), UpdatedAt: stamp(to),
		All: rates(all, allG, degG[0]), Hospitals: make([]GuardrailHospital, 0, len(rows))}
	for i, g := range rows {
		if !audit.ShowableName(g.name) {
			// Left out: every one of its cells is then unknown, which only
			// hides more (ids only in the log).
			d.log().Warn("platform: guardrails row not listed: a name can't be shown", "hospital_id", g.id.String())
			continue
		}
		out.Hospitals = append(out.Hospitals, GuardrailHospital{
			Hospital: Ref{ID: g.id.String(), Name: g.name},
			Rates:    rates(table[i], rowsG[i], degG[i+1]),
		})
	}
	return out, nil
}
