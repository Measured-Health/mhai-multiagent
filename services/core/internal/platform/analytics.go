package platform

// Platform analytics (Plan 7c spec §7.1, §7.2, §10; rulings R41a to
// R41a-5).
//
// GET /platform/analytics?period=week|month|quarter, and the series
// GET /platform/analytics/hospitals/{id}/series?measure&period and
// GET /platform/analytics/changes/{id}/series?period.
//
// The routes read stored snapshots (analytics_snapshots, the guarded
// output of the scheduler), the release epoch and What changed's stored
// rows (analytics_change_runs, analytics_changes, analytics_follow_ups,
// with the names of hospitals and authors), as mhai_platform, and nothing
// else: no request computes a figure or touches a clinical table
// or the frozen counts (invariant I2). The render is internal/analytics's,
// which takes the stored rows as its only input; no clock is read, so
// every read of a period is identical (A1). Platform admins and auditors,
// two-step always; 30 reads a minute per session across the three routes
// (429 rate_limited). Not recorded in the audit log (aggregates about no
// person, R39).

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

const (
	// AnalyticsPerMinute is the reads one session may make a minute, the
	// three analytics routes together.
	AnalyticsPerMinute = 30
	// analyticsScope is the idempotency_keys scope of the read counter; no
	// Idempotency-Key scope (a user id) takes this form.
	analyticsScope = "platform.analytics.rate"
	// analyticsTimeout bounds one read.
	analyticsTimeout = 10 * time.Second
)

func registerAnalytics(m *authz.Mux, d Deps) {
	read := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin, authz.RoleAuditor}}
	authz.Route(m, http.MethodGet, "/platform/analytics", read, http.HandlerFunc(d.analyticsRead))
	authz.Route(m, http.MethodGet, "/platform/analytics/hospitals/{id}/series", read, http.HandlerFunc(d.hospitalSeries))
	authz.Route(m, http.MethodGet, "/platform/analytics/changes/{id}/series", read, http.HandlerFunc(d.changeSeries))
}

// analyticsKind reads the query: exactly the keys given, period a calendar
// kind.
func analyticsKind(r *http.Request, keys []string) (calendar.Kind, map[string]string, bool) {
	v, err := params(r, keys)
	if err != nil {
		return "", nil, false
	}
	for _, k := range keys {
		if v[k] == "" {
			return "", nil, false
		}
	}
	k, err := calendar.ParseKind(v["period"])
	if err != nil {
		return "", nil, false
	}
	return k, v, true
}

// analyticsStart checks the caller and counts the read; false when the
// answer is already written.
func (d Deps) analyticsStart(w http.ResponseWriter, r *http.Request) bool {
	_, ok := d.analyticsCaller(w, r)
	return ok
}

// analyticsCaller is analyticsStart that also returns the caller.
func (d Deps) analyticsCaller(w http.ResponseWriter, r *http.Request) (authz.Principal, bool) {
	p, ok := caller(w, r)
	if !ok {
		return p, false
	}
	allowed, err := d.reserveRate(r, p, analyticsScope, "platform.analytics.rate.v1", AnalyticsPerMinute)
	switch {
	case err != nil:
		d.log().Error("platform: analytics refused: rate counter unavailable", "request_id", httpx.RequestIDFrom(r.Context()),
			"reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return p, false
	case !allowed:
		httpx.Error(w, r, "rate_limited")
		return p, false
	}
	return p, true
}

// analyticsFail logs ids and a fixed reason (never a figure) and answers
// internal_error.
func (d Deps) analyticsFail(w http.ResponseWriter, r *http.Request, what string) {
	d.log().Error("platform: analytics "+what+" failed", "request_id", httpx.RequestIDFrom(r.Context()), "reason", "internal_error")
	httpx.Error(w, r, "internal_error")
}

// analyticsDegraded logs that a stored body couldn't be read (served as
// unavailable or hidden): the kind and a fixed reason.
func (d Deps) analyticsDegraded(r *http.Request, k calendar.Kind) {
	d.log().Warn("platform: analytics stored snapshot unreadable", "request_id", httpx.RequestIDFrom(r.Context()),
		"kind", string(k), "reason", "unreadable_snapshot")
}

// analyticsRead is GET /platform/analytics?period.
func (d Deps) analyticsRead(w http.ResponseWriter, r *http.Request) {
	k, _, ok := analyticsKind(r, []string{"period"})
	if !ok {
		httpx.Error(w, r, "validation_failed")
		return
	}
	p, ok := d.analyticsCaller(w, r)
	if !ok {
		return
	}
	rows, epoch, err := d.storedSnapshots(r.Context(), k)
	if err != nil {
		d.analyticsFail(w, r, "read")
		return
	}
	out, degraded := analytics.Render(k, rows, epoch)
	if degraded {
		d.analyticsDegraded(r, k)
	}
	// What changed is the same for every period: the newest weekly
	// evaluation (spec §7.3). Its failure leaves the other areas (F13).
	run, changes, err := d.openChanges(r.Context())
	if err != nil {
		d.log().Error("platform: analytics changes read failed", "request_id", httpx.RequestIDFrom(r.Context()), "reason", "internal_error")
		out.WhatChanged = analytics.Unavailable{Unavailable: true}
	} else {
		var cdeg bool
		out.WhatChanged, cdeg = analytics.RenderChanges(run, changes, p.UserID.String())
		if cdeg {
			d.log().Warn("platform: analytics stored change unreadable", "request_id", httpx.RequestIDFrom(r.Context()),
				"reason", "unreadable_change")
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

// storedSnapshots reads the newest stored rows of kind k (as many as a
// trend covers) and the release epoch, in one transaction as
// mhai_platform. A row whose dates aren't a period of k is left out.
func (d Deps) storedSnapshots(ctx context.Context, k calendar.Kind) ([]analytics.Snapshot, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, analyticsTimeout)
	defer cancel()
	var out []analytics.Snapshot
	var epoch time.Time
	err := authz.Tx(ctx, d.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT released_at FROM analytics_epoch`).Scan(&epoch); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT period_from, period_to, state, body::text, computed_at FROM analytics_snapshots
			WHERE kind = $1 ORDER BY period_from DESC LIMIT $2`, string(k), analytics.TrendLength(k))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var from, to, computed time.Time
			var state string
			var body *string
			if err := rows.Scan(&from, &to, &state, &body, &computed); err != nil {
				return err
			}
			p := calendar.Of(k, from.UTC())
			if !p.From.Equal(from.UTC()) || !p.To.Equal(to.UTC()) {
				continue
			}
			s := analytics.Snapshot{Period: p, State: state, ComputedAt: computed}
			if body != nil {
				s.Body = []byte(*body)
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, epoch, err
}

// changeSelect reads open changes with their hospital and follow-up; the
// caller adds the WHERE.
const changeSelect = `SELECT c.id, c.hospital_id, h.name, c.area, c.measure, c.direction, c.serious, c.current::text,
		c.previous::text, c.summary, c.version, f.by_user, u.display_name, f.at, f.note, f.version
	FROM analytics_changes c
	LEFT JOIN hospitals h ON h.id = c.hospital_id
	LEFT JOIN analytics_follow_ups f ON f.change_id = c.id
	LEFT JOIN users u ON u.id = f.by_user`

// scanChange reads one changeSelect row.
func scanChange(row pgx.Row) (analytics.ChangeRow, error) {
	var c analytics.ChangeRow
	var id uuid.UUID
	var hid, by *uuid.UUID
	var hname, byName, note *string
	var cur, prev string
	var at *time.Time
	var fver *int
	if err := row.Scan(&id, &hid, &hname, &c.Area, &c.Measure, &c.Direction, &c.Serious, &cur, &prev, &c.Summary, &c.Version,
		&by, &byName, &at, &note, &fver); err != nil {
		return c, err
	}
	c.ID, c.Current, c.Previous = id.String(), []byte(cur), []byte(prev)
	if hid != nil {
		c.Hospital = &analytics.Ref{ID: hid.String()}
		if hname != nil {
			c.Hospital.Name = *hname
		}
	}
	if by != nil && at != nil && fver != nil {
		f := &analytics.FollowUpRow{By: analytics.Ref{ID: by.String()}, At: *at, Note: note, Version: *fver}
		if byName != nil {
			f.By.Name = *byName
		}
		c.FollowUp = f
	}
	return c, nil
}

// openChanges reads the newest run and its open changes, as mhai_platform:
// stored rows only. No run yet is a nil run.
func (d Deps) openChanges(ctx context.Context) (*analytics.ChangesRun, []analytics.ChangeRow, error) {
	ctx, cancel := context.WithTimeout(ctx, analyticsTimeout)
	defer cancel()
	var run *analytics.ChangesRun
	var out []analytics.ChangeRow
	err := authz.Tx(ctx, d.Pool, func(tx pgx.Tx) error {
		var wf, wt, mf, mt, at time.Time
		err := tx.QueryRow(ctx, `SELECT week_from, week_to, month_from, month_to, evaluated_at FROM analytics_change_runs
			ORDER BY week_from DESC LIMIT 1`).Scan(&wf, &wt, &mf, &mt, &at)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		wk, mo := calendar.Of(calendar.Week, wf.UTC()), calendar.Of(calendar.Month, mf.UTC())
		if !wk.To.Equal(wt.UTC()) || !mo.To.Equal(mt.UTC()) || !mo.Equal(analytics.ComparedMonth(wk)) {
			return errBadRun
		}
		run = &analytics.ChangesRun{Week: wk, Month: mo, EvaluatedAt: at}
		rows, err := tx.Query(ctx, changeSelect+` WHERE c.closed_week IS NULL AND c.last_week = $1::date ORDER BY c.id`,
			wk.From.Format(time.DateOnly))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanChange(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return run, out, err
}

// errBadRun is a stored run whose dates aren't a week and its compared
// month.
var errBadRun = errors.New("platform: not a stored change run")
