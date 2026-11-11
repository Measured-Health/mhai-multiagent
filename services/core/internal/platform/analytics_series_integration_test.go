//go:build integration

package platform_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// The series routes through core's whole chain (Plan 7c Task 7, spec
// §7.2), over the fixed stored periods of analytics_integration_test.go.

func series(t *testing.T, tok, id, query string) resp {
	t.Helper()
	return get(t, "/platform/analytics/hospitals/"+id+"/series"+query, tok)
}

// 12 weeks, 12 months or 8 quarters, oldest first, each the hospital's
// stored cell; median over the other hospitals' shown values, or null.
func TestHospitalSeriesPoints(t *testing.T) {
	f := fixedAnalytics(t)
	for k, n := range map[calendar.Kind]int{calendar.Week: 12, calendar.Month: 12, calendar.Quarter: 8} {
		for _, h := range f.hosp {
			for _, m := range []struct{ measure, stored string }{{"questions_per_clinician", "questions_per_clinician"}, {"needs_review_share", "escalate_share"}} {
				r := series(t, mint(t, auditor(t)), h.ID, "?measure="+m.measure+"&period="+string(k))
				ok(t, string(k)+" "+m.measure, r)
				top := requireKeys(t, "series", r.body, "measure", "hospital", "points")
				if top["measure"] != m.measure {
					t.Errorf("measure %v", top["measure"])
				}
				if hr := requireKeys(t, "hospital", top["hospital"], "id", "name"); hr["id"] != h.ID || hr["name"] != h.Name {
					t.Errorf("hospital %v", hr)
				}
				pts := list(t, "points", top["points"])
				if len(pts) != n {
					t.Fatalf("%s: %d points, want %d", k, len(pts), n)
				}
				all := f.rows[k]
				want := all[len(all)-n:]
				for i, x := range pts {
					pt := requireKeys(t, "point", x, "start", "value", "median")
					if pt["start"] != stampOf(want[i].From) {
						t.Errorf("%s point %d starts %v, want %v", k, i, pt["start"], want[i])
					}
					rows := storedRows(t, storedBody(t, want[i]))
					if jsonOf(pt["value"]) != jsonOf(rows[h.ID][m.stored]) {
						t.Errorf("%s point %d: %s, stored %s", k, i, jsonOf(pt["value"]), jsonOf(rows[h.ID][m.stored]))
					}
					var others []any
					for id, row := range rows {
						if id != h.ID {
							others = append(others, row)
						}
					}
					med, has := medianOf(others, func(r map[string]any) any { return r[m.stored] })
					switch {
					case !has && pt["median"] != nil:
						t.Errorf("%s point %d median %v, want null", k, i, pt["median"])
					case has && pt["median"] != med:
						t.Errorf("%s point %d median %v, want %v", k, i, pt["median"], med)
					}
				}
			}
		}
	}
}

// F10: the two served measures; the Critical ones and anything else 422;
// the query takes measure and period, each once.
func TestHospitalSeriesMeasures(t *testing.T) {
	f := fixedAnalytics(t)
	tok := mint(t, platformAdmin(t))
	id := f.hosp[0].ID
	for _, m := range []string{"questions_per_clinician", "needs_review_share"} {
		ok(t, m, series(t, tok, id, "?measure="+m+"&period=week"))
	}
	for _, q := range []string{"?measure=critical_median_ack_minutes&period=week", "?measure=critical_within_target&period=week",
		"?measure=questions&period=week", "?measure=needs_review_share", "?period=week", "?measure=needs_review_share&period=30d",
		"?measure=needs_review_share&period=week&x=1", "?measure=needs_review_share&measure=questions_per_clinician&period=week", ""} {
		problem(t, "series "+q, series(t, tok, id, q), 422, "validation_failed")
	}
}

// A22: a random uuid as a hospital or a change is 404, and so is an id that
// isn't a canonical uuid, or a real hospital in no stored period.
func TestSeriesUnknownIds(t *testing.T) {
	f := fixedAnalytics(t)
	tok := mint(t, platformAdmin(t))
	known := seedHospital(t, "active")
	for _, id := range []string{uuid.NewString(), known.String(), strings.ToUpper(f.hosp[0].ID), "not-a-uuid", "1"} {
		problem(t, "hospital "+id, series(t, tok, id, "?measure=needs_review_share&period=month"), 404, "not_found")
	}
	for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
		problem(t, "change "+id, get(t, "/platform/analytics/changes/"+id+"/series?period=month", tok), 404, "not_found")
	}
}

// The change series (Plan 7c-3): a hospital's change gives the hospital's
// stored cell of the change's measure for each stored period, with the
// other hospitals' median; a change across all hospitals gives the all
// cells, hospital null and no median (F14). Unknown and closed changes are
// 404; the query is checked first.
func TestChangeSeries(t *testing.T) {
	fx := fixedAnalytics(t)
	// The fixture's hospital A as a hospital row, so a change can name it.
	a := fx.hosp[0]
	if _, err := super.Exec(context.Background(), `INSERT INTO hospitals (id, name, short_code, status) VALUES ($1, $2, $3, 'active')`,
		a.ID, a.Name, "H"+uniq()); err != nil {
		t.Fatal(err)
	}
	f := seedChanges(t, uuid.MustParse(a.ID), a.Name)
	tok := mint(t, auditor(t))
	for k, n := range map[calendar.Kind]int{calendar.Week: 12, calendar.Month: 12, calendar.Quarter: 8} {
		r := get(t, "/platform/analytics/changes/"+f.hospChange.String()+"/series?period="+string(k), tok)
		ok(t, string(k), r)
		top := requireKeys(t, "series", r.body, "change_id", "measure", "hospital", "points")
		if top["change_id"] != f.hospChange.String() || top["measure"] != "questions" || top["hospital"].(map[string]any)["id"] != a.ID {
			t.Errorf("%s: %v", k, top)
		}
		pts := list(t, "points", top["points"])
		if len(pts) != n {
			t.Fatalf("%s: %d points", k, len(pts))
		}
		latest := fx.rows[k][len(fx.rows[k])-1]
		stored := storedRows(t, storedBody(t, latest))
		last := pts[len(pts)-1].(map[string]any)
		if jsonOf(last["value"]) != jsonOf(stored[a.ID]["questions"]) || last["start"] != stampOf(latest.From) {
			t.Errorf("%s: last point %v, stored %v", k, last, stored[a.ID]["questions"])
		}
		// The others: B's 24 shown, C hidden: 24.
		if last["median"] != float64(24) {
			t.Errorf("%s: median %v", k, last["median"])
		}
		r = get(t, "/platform/analytics/changes/"+f.allChng.String()+"/series?period="+string(k), tok)
		ok(t, string(k)+" all", r)
		top = requireKeys(t, "series", r.body, "change_id", "measure", "hospital", "points")
		if top["change_id"] != f.allChng.String() || top["measure"] != "needs_review_share" || top["hospital"] != nil {
			t.Errorf("%s all: %v", k, top)
		}
		all := storedBody(t, latest)["all"].(map[string]any)
		pts = list(t, "points", top["points"])
		last = pts[len(pts)-1].(map[string]any)
		if jsonOf(last["value"]) != jsonOf(all["escalate_share"]) || last["median"] != nil {
			t.Errorf("%s all: last point %v", k, last)
		}
	}
	problem(t, "closed", get(t, "/platform/analytics/changes/"+f.closed.String()+"/series?period=month", tok), 404, "not_found")
	id := uuid.NewString()
	for _, k := range []string{"week", "month", "quarter"} {
		problem(t, k, get(t, "/platform/analytics/changes/"+id+"/series?period="+k, tok), 404, "not_found")
	}
	for _, q := range []string{"", "?period=7d", "?period=week&measure=x"} {
		problem(t, "change series "+q, get(t, "/platform/analytics/changes/"+id+"/series"+q, tok), 422, "validation_failed")
	}
}
