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

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// The cold budget (design §11; plan Task 11), measured on the build host
// with ANALYSIS_LONG=1: a synthetic hospital of 200,000 subjects, the
// worst cold request (a quarter with two filters, every parent and child
// cold). Every request answers within the budget (plus scheduling slack):
// stored and served, or 503 not_ready while the body is finished in the
// background. The PR reports the latencies and how long until the body is
// served.
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
	q3 := calendar.Period{Kind: calendar.Quarter, From: day("2026-07-01"), To: day("2026-10-01")}
	p := w.period(t, e, q3)
	var f disclose.Filters
	f[disclose.DimSex], f[disclose.DimAgeBand] = "female", "40-64"
	var lat []time.Duration
	began := time.Now()
	for i := 0; ; i++ {
		t0 := time.Now()
		_, err := e.Body(as(w.h), p, f)
		lat = append(lat, time.Since(t0))
		if err == nil {
			break
		}
		if !errors.Is(err, ErrNotReady) {
			t.Fatalf("request %d: %v", i+1, err)
		}
		if time.Since(began) > 30*time.Minute {
			t.Fatal("not served within 30 minutes")
		}
	}
	slices.Sort(lat)
	p50, p99 := lat[len(lat)/2], lat[(len(lat)*99)/100]
	t.Logf("cold quarter, two filters: served after %v and %d requests (%d contexts computed); request latency p50 %v, p99 %v, max %v",
		time.Since(began), len(lat), computed.Load(), p50, p99, lat[len(lat)-1])
	if lat[len(lat)-1] > disclose.ColdBody+500*time.Millisecond {
		t.Errorf("a request took %v: over the %v budget", lat[len(lat)-1], disclose.ColdBody)
	}
	t0 := time.Now()
	if _, err := e.Body(as(w.h), p, f); err != nil || time.Since(t0) > disclose.ColdBody {
		t.Errorf("the stored body: %v in %v", err, time.Since(t0))
	}
	t.Logf("served stored in %v", time.Since(t0))
}
