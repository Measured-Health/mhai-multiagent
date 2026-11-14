package alerts

// Usage counts for alerts (EMR design §14.2, plan Task 41): a hospital's
// alerts in a period by severity, category, route and handling, every
// count under the small-group rule (smallgroup.Set per breakdown, so a
// hidden part can't be worked out from its total and the shown parts). The
// counts outlive the patient: an alert keeps its codes and times after an
// exclusion or the 13-month retention clears its patient. /hospital/usage
// will serve the block when that route is built (not in the EMR plan).

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// The breakdowns' closed keys, in the order they are guarded.
var (
	UsageSeverities = []string{"critical", "urgent", "info"}
	UsageCategories = []string{"blood_chemistry", "blood_count", "kidney_function", "blood_gases_lactate", "heart_markers",
		"blood_sugar", "vital_signs", "infection_markers"}
	// UsageRoutes: on the hospital's queue, or sent to named clinicians.
	UsageRoutes = []string{"queue", "named"}
	// UsageHandling: still new; acknowledged; dismissed by a clinician;
	// withdrawn (by the source or an exclusion) before anyone handled it.
	UsageHandling = []string{"open", "acknowledged", "dismissed", "withdrawn"}
)

// UsageOut is the alert block of a hospital's Usage page.
type UsageOut struct {
	Total      smallgroup.Guarded            `json:"total"`
	BySeverity map[string]smallgroup.Guarded `json:"by_severity"`
	ByCategory map[string]smallgroup.Guarded `json:"by_category"`
	ByRoute    map[string]smallgroup.Guarded `json:"by_route"`
	ByHandling map[string]smallgroup.Guarded `json:"by_handling"`
}

// UsageCounts are the raw counts behind a UsageOut; they never leave the
// service.
type UsageCounts struct {
	Total                               int64
	Severity, Category, Route, Handling map[string]int64
}

var errUsagePeriod = errors.New("alerts: usage period")

// UsageBlock counts hospital hid's alerts created in [from, to) in tx (the
// caller's role reads alerts under RLS, its hospital set) and guards them.
func UsageBlock(ctx context.Context, tx pgx.Tx, hid uuid.UUID, from, to time.Time) (UsageOut, error) {
	if hid == uuid.Nil || !from.Before(to) {
		return UsageOut{}, errUsagePeriod
	}
	c := UsageCounts{Severity: map[string]int64{}, Category: map[string]int64{}, Route: map[string]int64{},
		Handling: map[string]int64{}}
	rows, err := tx.Query(ctx, `SELECT severity, category,
		CASE WHEN routed_queue THEN 'queue' ELSE 'named' END,
		CASE WHEN status = 'new' THEN 'open'
		     WHEN status = 'handled' THEN 'acknowledged'
		     WHEN handled_by IS NOT NULL THEN 'dismissed'
		     ELSE 'withdrawn' END,
		count(*)
		FROM alerts WHERE hospital_id = $1 AND created_at >= $2 AND created_at < $3
		GROUP BY 1, 2, 3, 4`, hid, from, to)
	if err != nil {
		return UsageOut{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var sev, cat, route, handling string
		var n int64
		if err := rows.Scan(&sev, &cat, &route, &handling, &n); err != nil {
			return UsageOut{}, err
		}
		c.Total += n
		c.Severity[sev] += n
		c.Category[cat] += n
		c.Route[route] += n
		c.Handling[handling] += n
	}
	if err := rows.Err(); err != nil {
		return UsageOut{}, err
	}
	return GuardUsage(c), nil
}

// GuardUsage applies the small-group rule to c: each breakdown is a set
// (its total and its parts), and the total is hidden in every breakdown
// once any one hides it, so no breakdown shows what another hides.
func GuardUsage(c UsageCounts) UsageOut {
	type breakdown struct {
		keys []string
		raw  map[string]int64
		out  *map[string]smallgroup.Guarded
	}
	var o UsageOut
	bs := []breakdown{{UsageSeverities, c.Severity, &o.BySeverity}, {UsageCategories, c.Category, &o.ByCategory},
		{UsageRoutes, c.Route, &o.ByRoute}, {UsageHandling, c.Handling, &o.ByHandling}}
	guard := func(b breakdown, hideTotal bool) []smallgroup.Guarded {
		parts := make([]int64, len(b.keys))
		forced := make([]bool, len(b.keys)+1)
		forced[0] = hideTotal
		for i, k := range b.keys {
			parts[i] = b.raw[k]
		}
		return smallgroup.SetForced(c.Total, parts, forced)
	}
	hideTotal := false
	for _, b := range bs {
		if !guard(b, false)[0].Shown() && c.Total != 0 {
			hideTotal = true
		}
	}
	for _, b := range bs {
		g := guard(b, hideTotal)
		m := make(map[string]smallgroup.Guarded, len(b.keys))
		for i, k := range b.keys {
			m[k] = g[i+1]
		}
		*b.out = m
		o.Total = g[0]
	}
	return o
}
