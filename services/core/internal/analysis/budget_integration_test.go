//go:build integration

package analysis

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// The cold budget (design §11; plan Task 11), measured on the build host
// with ANALYSIS_LONG=1: a synthetic hospital of 200,000 subjects, the
// worst cold request (a quarter with two filters, every parent and child
// cold), per level. Every request answers within the budget (plus
// scheduling slack): stored and served, or 503 while the body is finished
// in the background, or 503 when it can't be (the 5 s statement timeout):
// never anything unstored. The PR reports the numbers.
func TestColdFamilyBudget(t *testing.T) {
	if os.Getenv("ANALYSIS_LONG") != "1" {
		t.Skip("ANALYSIS_LONG=1: the 200,000-subject budget run")
	}
	super, app := migrated(t)
	w := newWorld(t, super, app)
	start := time.Now()
	conds := make([]string, 0, 10)
	for i := 0; i < 7; i++ {
		var id string
		if err := super.QueryRow(context.Background(), `INSERT INTO analysis_conditions (hospital_id, icd10, name)
			VALUES ($1, $2, 'Synthetic budget condition') RETURNING id::text`, w.h, "Z"+string(rune('0'+i))+"0").Scan(&id); err != nil {
			t.Fatal(err)
		}
		conds = append(conds, id)
	}
	all := append([]string{w.conds[0].String(), w.conds[1].String(), w.conds[2].String()}, conds...)
	exec(t, super, `INSERT INTO analysis_subjects (hospital_id, subject_id, ref_hmac, created_day)
		SELECT $1::uuid, md5($1::uuid::text || g)::uuid, sha256(convert_to($1::uuid::text || g, 'UTF8')), '2026-07-01'
		FROM generate_series(1, 200000) g`, w.h)
	exec(t, super, `INSERT INTO analysis_facts (hospital_id, subject_id, day, kind, condition_id, age_band, sex, setting, location_id, source_id, period_late)
		SELECT $1::uuid, md5($1::uuid::text || g)::uuid, '2026-07-01'::date + ((g * 7 + k * 13) % 92),
		       'diagnosis', ($2::uuid[])[1 + ((g * k) % 10) * ((g * k) % 10) % 10],
		       (ARRAY['0-17', '18-39', '40-64', '65-79', '80+'])[1 + g % 5],
		       (ARRAY['female', 'male', 'other', 'not_recorded'])[1 + g % 4], 'admitted', $3::uuid, (ARRAY[$4::uuid, $5::uuid])[1 + g % 2], false
		FROM generate_series(1, 200000) g, generate_series(1, 3) k`, w.h, all, w.loc, w.src, w.src2)
	exec(t, super, `INSERT INTO analysis_first_seen (hospital_id, subject_id, condition_id, first_day, first_late)
		SELECT hospital_id, subject_id, condition_id, min(day), false FROM analysis_facts
		WHERE hospital_id = $1 GROUP BY hospital_id, subject_id, condition_id`, w.h)
	w.freezeAt(t, time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC))
	t.Logf("seeded 200,000 subjects and 600,000 facts in %v", time.Since(start))

	e := w.engine()
	e.Budget = disclose.ColdBody
	var computed atomic.Int64
	e.OnCompute = func() { computed.Add(1) }
	var f disclose.Filters
	f[disclose.DimSex], f[disclose.DimAgeBand] = "female", "40-64"

	// The raw read alone, per level, without the statement timeout (for the
	// report: the 5 s timeout is what a request gets).
	for _, per := range []calendar.Period{
		{Kind: calendar.Week, From: day("2026-07-01"), To: day("2026-07-08")},
		{Kind: calendar.Month, From: day("2026-07-01"), To: day("2026-08-01")},
		{Kind: calendar.Quarter, From: day("2026-07-01"), To: day("2026-10-01")}} {
		p := w.period(t, e, per)
		for _, ff := range []disclose.Filters{{}, f} {
			t0 := time.Now()
			if err := gateway(t, w.app, w.h, func(tx pgx.Tx) error {
				if _, err := tx.Exec(context.Background(), `SET LOCAL statement_timeout = 0`); err != nil {
					return err
				}
				_, err := readCells(context.Background(), tx, p, ff)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			t.Logf("cells %s, %d filters: %v", per.Kind, ff.Count(), time.Since(t0))
		}
	}

	// Each level's worst cold request (two filters, every parent and child
	// cold): every request answers within the budget; the body is served,
	// or the engine fails closed (503) and nothing is served unstored.
	for _, per := range []calendar.Period{
		{Kind: calendar.Week, From: day("2026-07-08"), To: day("2026-07-15")},
		{Kind: calendar.Month, From: day("2026-08-01"), To: day("2026-09-01")},
		{Kind: calendar.Quarter, From: day("2026-07-01"), To: day("2026-10-01")}} {
		p := w.period(t, e, per)
		computed.Store(0)
		var lat []time.Duration
		began := time.Now()
		var last error
		for {
			t0 := time.Now()
			_, err := e.Body(as(w.h), p, f)
			lat = append(lat, time.Since(t0))
			last = err
			if !errors.Is(err, ErrNotReady) || time.Since(began) > 20*time.Minute {
				break
			}
		}
		slices.Sort(lat)
		p50, p99 := lat[len(lat)/2], lat[(len(lat)*99)/100]
		outcome := "served"
		if last != nil {
			outcome = "failed closed (" + last.Error() + ")"
		}
		t.Logf("cold %s, two filters: %s after %v and %d requests (%d contexts computed); request latency p50 %v, p99 %v, max %v",
			per.Kind, outcome, time.Since(began), len(lat), computed.Load(), p50, p99, lat[len(lat)-1])
		if lat[len(lat)-1] > disclose.ColdBody+500*time.Millisecond {
			t.Errorf("%s: a request took %v, over the %v budget", per.Kind, lat[len(lat)-1], disclose.ColdBody)
		}
		if last != nil && !errors.Is(last, ErrFailed) && !errors.Is(last, ErrNotReady) {
			t.Errorf("%s: %v is not a fail-closed refusal", per.Kind, last)
		}
		if last == nil {
			t0 := time.Now()
			if _, err := e.Body(as(w.h), p, f); err != nil || time.Since(t0) > disclose.ColdBody {
				t.Errorf("%s: the stored body: %v in %v", per.Kind, err, time.Since(t0))
			}
		}
	}
}
