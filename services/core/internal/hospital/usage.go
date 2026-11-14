package hospital

// GET /hospital/usage?period=this_month|last_month|last_90_days (frontend
// requirements §5.2, §9.1). Counts only, for the caller's hospital: no
// question text, result, patient or alert id. Periods are in the
// hospital's time zone; `to` is exclusive (now, for the running periods).
//
// confidence maps the HGA decision as the platform's analytics do (PASS
// high, REGENERATE caution, ESCALATE review) and a failed question to
// failed; a question still running counts in consultations only.
//
// alerts is alerts.UsageBlock (EMR plan Task 41) under the small-group
// rule: null when the hospital has no EMR source, {"unavailable": true}
// when the block couldn't be computed (the rest still answers).

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/alerts"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// UsagePeriods are the periods the route takes.
var UsagePeriods = []string{"this_month", "last_month", "last_90_days"}

// SpecialtyCount is one specialty's consultations.
type SpecialtyCount struct {
	Specialty     string `json:"specialty"`
	Consultations int64  `json:"consultations"`
}

// UsageOut is the contract's UsageOut, with core's alert block.
type UsageOut struct {
	Period            string           `json:"period"`
	From              string           `json:"from"`
	To                string           `json:"to"`
	Consultations     int64            `json:"consultations"`
	ActiveClinicians  int64            `json:"active_clinicians"`
	AgreementRate     *float64         `json:"agreement_rate"`
	MedianReviewHours *float64         `json:"median_review_hours"`
	Confidence        map[string]int64 `json:"confidence"`
	BySpecialty       []SpecialtyCount `json:"by_specialty"`
	Alerts            any              `json:"alerts"`
}

// maxBySpecialty bounds by_specialty (largest first).
const maxBySpecialty = 20

// PeriodBounds is [from, to) of period in loc at now.
func PeriodBounds(period string, now time.Time, loc *time.Location) (from, to time.Time, ok bool) {
	n := now.In(loc)
	month := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)
	switch period {
	case "this_month":
		return month, now, true
	case "last_month":
		return month.AddDate(0, -1, 0), month, true
	case "last_90_days":
		day := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
		return day.AddDate(0, 0, -89), now, true
	}
	return time.Time{}, time.Time{}, false
}

func (d Deps) usage(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	period := q.Get("period")
	if len(q) != 1 || len(q["period"]) != 1 {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	out := UsageOut{Period: period, Confidence: map[string]int64{"high": 0, "caution": 0, "review": 0, "failed": 0},
		BySpecialty: []SpecialtyCount{}}
	var pFrom, pTo time.Time
	err := d.tx(ctx, func(tx pgx.Tx) error {
		var tz string
		if err := tx.QueryRow(ctx, `SELECT time_zone FROM hospitals WHERE id = $1`, p.HospitalID).Scan(&tz); err != nil {
			return err
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			loc = time.UTC
		}
		from, to, ok := PeriodBounds(period, d.now(), loc)
		if !ok {
			return errInvalid
		}
		out.From, out.To = stamp(from), stamp(to)
		pFrom, pTo = from, to
		return usageCounts(ctx, tx, p.HospitalID, from, to, &out)
	})
	if err == nil {
		out.Alerts = d.alertBlock(ctx, p.HospitalID, pFrom, pTo)
	}
	if err != nil {
		d.fail(w, r, p, "usage", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

func usageCounts(ctx context.Context, tx pgx.Tx, hid uuid.UUID, from, to time.Time, out *UsageOut) error {
	if err := tx.QueryRow(ctx, `SELECT count(*), count(DISTINCT created_by) FROM clinical_queries
		WHERE hospital_id = $1 AND created_at >= $2 AND created_at < $3`, hid, from, to).
		Scan(&out.Consultations, &out.ActiveClinicians); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT CASE WHEN q.status = 'failed' THEN 'failed'
			WHEN upper(d.hga_decision) = 'PASS' THEN 'high'
			WHEN upper(d.hga_decision) = 'REGENERATE' THEN 'caution'
			WHEN upper(d.hga_decision) = 'ESCALATE' THEN 'review' END AS level, count(*)
		FROM clinical_queries q
		LEFT JOIN clinical_decisions d ON d.query_id = q.id AND d.hospital_id = q.hospital_id
		WHERE q.hospital_id = $1 AND q.created_at >= $2 AND q.created_at < $3
		GROUP BY 1`, hid, from, to)
	if err != nil {
		return err
	}
	for rows.Next() {
		var level *string
		var n int64
		if err := rows.Scan(&level, &n); err != nil {
			rows.Close()
			return err
		}
		if level != nil {
			out.Confidence[*level] = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT category, count(*) FROM clinical_queries
		WHERE hospital_id = $1 AND created_at >= $2 AND created_at < $3 AND category IS NOT NULL AND category <> ''
		GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT $4`, hid, from, to, maxBySpecialty)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s SpecialtyCount
		if err := rows.Scan(&s.Specialty, &s.Consultations); err != nil {
			rows.Close()
			return err
		}
		out.BySpecialty = append(out.BySpecialty, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Agreement: answers agreed with, of those rated (a withdrawn
	// correction isn't a rating).
	if err := tx.QueryRow(ctx, `SELECT CASE WHEN count(*) = 0 THEN NULL
			ELSE (count(*) FILTER (WHERE action = 'validate'))::float8 / count(*) END
		FROM feedback_events WHERE hospital_id = $1 AND created_at >= $2 AND created_at < $3 AND status <> 'withdrawn'`,
		hid, from, to).Scan(&out.AgreementRate); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM decided_at - created_at) / 3600.0)
		FROM feedback_events WHERE hospital_id = $1 AND action = 'correct' AND decided_at >= $2 AND decided_at < $3`,
		hid, from, to).Scan(&out.MedianReviewHours); err != nil {
		return err
	}
	return nil
}

// unavailable is the alert block that couldn't be computed.
type unavailable struct {
	Unavailable bool `json:"unavailable"`
}

// alertBlock is the usage page's alerts: nil with no EMR source, the
// guarded block, or unavailable. It runs in its own transaction, so a
// failure leaves the rest of the reply standing.
func (d Deps) alertBlock(ctx context.Context, hid uuid.UUID, from, to time.Time) any {
	var out any
	err := d.tx(ctx, func(tx pgx.Tx) error {
		var sources int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM emr_hospital_sources()`).Scan(&sources); err != nil {
			return err
		}
		if sources == 0 {
			return nil
		}
		block, err := alerts.UsageBlock(ctx, tx, hid, from, to)
		if err != nil {
			return err
		}
		out = block
		return nil
	})
	if err != nil {
		return unavailable{true}
	}
	return out
}
