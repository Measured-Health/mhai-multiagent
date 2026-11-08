//go:build integration

package calendar_test

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// The SQL twin (migration 00019): mhai_reporting_period_ok agrees with Go
// on every period of 2024–2030 and on 2,000 random non-periods.
func TestCalendarMatchesSQL(t *testing.T) {
	_, pool := testutil.MigratedPostgres(t)
	ctx := context.Background()
	type cand struct {
		kind     string
		from, to time.Time
	}
	var cs []cand
	for d := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC); d.Before(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)); d = d.AddDate(0, 0, 1) {
		for _, k := range []calendar.Kind{calendar.Week, calendar.Month, calendar.Quarter} {
			p := calendar.Of(k, d)
			cs = append(cs, cand{string(k), d, p.To}, cand{string(k), d, d.AddDate(0, 0, 7)})
		}
	}
	r := rand.New(rand.NewSource(7))
	kinds := []string{"week", "month", "quarter", "7d", "30d", "", "WEEK"}
	for i := 0; i < 2000; i++ {
		from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, r.Intn(2555))
		cs = append(cs, cand{kinds[r.Intn(len(kinds))], from, from.AddDate(0, 0, 1+r.Intn(95))})
	}
	kindsArr, froms, tos := make([]string, len(cs)), make([]string, len(cs)), make([]string, len(cs))
	for i, c := range cs {
		kindsArr[i], froms[i], tos[i] = c.kind, c.from.Format(time.DateOnly), c.to.Format(time.DateOnly)
	}
	rows, err := pool.Query(ctx, `SELECT mhai_reporting_period_ok(k, f::date, t::date)
		FROM unnest($1::text[], $2::text[], $3::text[]) WITH ORDINALITY AS u(k, f, t, n) ORDER BY n`, kindsArr, froms, tos)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	i, valid := 0, 0
	for rows.Next() {
		var ok bool
		if err := rows.Scan(&ok); err != nil {
			t.Fatal(err)
		}
		c := cs[i]
		want := calendar.Valid(calendar.Kind(c.kind), c.from, c.to)
		if ok != want {
			t.Fatalf("%s %s–%s: SQL %v, Go %v", c.kind, froms[i], tos[i], ok, want)
		}
		if ok {
			valid++
		}
		i++
	}
	if err := rows.Err(); err != nil || i != len(cs) || valid < 448 { // 336 weeks, 84 months, 28 quarters at least
		t.Fatalf("%d of %d checked, %d valid (%v)", i, len(cs), valid, err)
	}
}
