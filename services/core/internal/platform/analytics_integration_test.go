//go:build integration

package platform_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/platform"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// GET /platform/analytics through core's whole chain (Plan 7c Task 6, spec
// §7.1). The stored snapshots here are written by hand as the superuser
// (who passes the insert policy) for fixed periods in 2087–2090, so they
// are the latest of every kind whatever today's date is, and every
// expectation is computed from them: no test depends on the real clock.
// Every name, id and count is synthetic.

// analyticsEnd is the last fixed period: Q1 2090, March 2090 and its last
// week.
var analyticsEnd = time.Date(2090, 3, 22, 0, 0, 0, 0, time.UTC)

type anaFixture struct {
	hosp  []analytics.Ref // A (shown, scale 1), B (shown, scale 2), C (hidden figures)
	rows  map[calendar.Kind][]calendar.Period
	stamp map[string]time.Time // period key → computed_at
}

func anaRow(n int64) analytics.Row {
	return analytics.Row{Questions: smallgroup.Count(12 * n), Answers: smallgroup.Count(10 * n), Pass: smallgroup.Count(5 * n),
		Regenerate: smallgroup.Count(0), Escalate: smallgroup.Count(5 * n), Degraded: smallgroup.Count(0),
		PassShare: smallgroup.Share(5*n, 10*n), RegenerateShare: smallgroup.Share(0, 10*n), EscalateShare: smallgroup.Share(5*n, 10*n),
		Reviews: smallgroup.Count(5 * n), AgreementRate: smallgroup.Share(5*n, 5*n), ActiveClinicians: smallgroup.Count(6 * n),
		QuestionsPerClinician: analytics.RatioOf(2), MedianReviewHours: analytics.HoursOf(float64(n) + 0.5)}
}

func anaHidden() analytics.Row {
	r := anaRow(1)
	h, s := smallgroup.Guarded{Suppressed: true}, smallgroup.GuardedShare{Suppressed: true}
	r.Questions, r.ActiveClinicians, r.Escalate, r.EscalateShare, r.PassShare = h, h, h, s, s
	r.QuestionsPerClinician = analytics.Ratio{Number: analytics.Number{Suppressed: true}}
	r.MedianReviewHours = analytics.Hours{Number: analytics.Number{Suppressed: true}}
	return r
}

func anaKey(p calendar.Period) string { return string(p.Kind) + " " + p.From.Format(time.DateOnly) }

// storeBody inserts p's body (or an unavailable row when b is nil).
func storeBody(t *testing.T, p calendar.Period, b *analytics.Body, computed time.Time) {
	t.Helper()
	from, to := analytics.Dates(p)
	var body any
	state := "unavailable"
	if b != nil {
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		body, state = string(raw), "ok"
	}
	if _, err := super.Exec(context.Background(), `INSERT INTO analytics_snapshots (kind, period_from, period_to, state, body, computed_at)
		VALUES ($1, $2::date, $3::date, $4, $5::jsonb, $6)`, string(p.Kind), from, to, state, body, computed); err != nil {
		t.Fatal(err)
	}
}

// fixedAnalytics stores 14 weeks, 14 months and 10 quarters ending at
// analyticsEnd, and removes them when the test ends.
func fixedAnalytics(t *testing.T) *anaFixture {
	t.Helper()
	f := &anaFixture{rows: map[calendar.Kind][]calendar.Period{}, stamp: map[string]time.Time{}}
	for i, n := range []string{"North", "South", "East"} {
		f.hosp = append(f.hosp, analytics.Ref{ID: uuid.NewString(), Name: fmt.Sprintf("Synthetic Analytics %s %d", n, i)})
	}
	t.Cleanup(func() {
		if _, err := super.Exec(context.Background(), `DELETE FROM analytics_snapshots WHERE period_from >= '2080-01-01'`); err != nil {
			t.Error(err)
		}
	})
	for k, n := range map[calendar.Kind]int{calendar.Week: 14, calendar.Month: 14, calendar.Quarter: 10} {
		for i, p := range calendar.Previous(calendar.Of(k, analyticsEnd), n) {
			from, to := analytics.Dates(p)
			all := anaRow(4)
			all.Questions = smallgroup.Count(int64(100 + i)) // each period's own all cell
			b := analytics.Body{Kind: string(k), From: from, To: to, All: all, Hospitals: []analytics.HospitalRow{
				{Hospital: f.hosp[0], Staff: analytics.StaffFigure(9), Row: anaRow(1)},
				{Hospital: f.hosp[1], Staff: analytics.StaffFigure(3), Row: anaRow(2)},
				{Hospital: f.hosp[2], Staff: analytics.StaffFigure(12), Row: anaHidden()},
			}}
			computed := p.To.Add(17 * time.Minute)
			storeBody(t, p, &b, computed)
			f.rows[k] = append(f.rows[k], p)
			f.stamp[anaKey(p)] = computed
		}
	}
	return f
}

func analyticsGet(t *testing.T, tok, query string) resp {
	t.Helper()
	return get(t, "/platform/analytics"+query, tok)
}

// storedBody is the stored body of p, as JSON maps.
func storedBody(t *testing.T, p calendar.Period) map[string]any {
	t.Helper()
	var body string
	if err := super.QueryRow(context.Background(), `SELECT body::text FROM analytics_snapshots WHERE kind = $1 AND period_from = $2::date`,
		string(p.Kind), p.From.Format(time.DateOnly)).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func storedRows(t *testing.T, body map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, x := range body["hospitals"].([]any) {
		m := x.(map[string]any)
		out[m["hospital"].(map[string]any)["id"].(string)] = m
	}
	return out
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestAnalyticsShape: every area's keys as the contract, alerts null,
// changes {unavailable: true} until 7c-3, the latest stored period, each
// cell the stored one.
func TestAnalyticsShape(t *testing.T) {
	f := fixedAnalytics(t)
	for _, k := range []calendar.Kind{calendar.Week, calendar.Month, calendar.Quarter} {
		r := analyticsGet(t, mint(t, platformAdmin(t)), "?period="+string(k))
		ok(t, string(k), r)
		top := requireKeys(t, string(k), r.body, "period", "from", "to", "changes", "use", "alerts", "quality", "hospitals", "what_changed")
		latest := f.rows[k][len(f.rows[k])-1]
		if top["period"] != string(k) || top["from"] != stampOf(latest.From) || top["to"] != stampOf(latest.To) {
			t.Errorf("%s: %v %v %v, want %v", k, top["period"], top["from"], top["to"], latest)
		}
		if top["alerts"] != nil {
			t.Errorf("%s: alerts %v", k, top["alerts"])
		}
		if c := requireKeys(t, "changes", top["changes"], "unavailable"); c["unavailable"] != true || top["what_changed"] == nil {
			t.Errorf("changes %v", c)
		}
		if r.hdr.Get("Cache-Control") != "no-store" {
			t.Errorf("Cache-Control %q", r.hdr.Get("Cache-Control"))
		}
		stored := storedRows(t, storedBody(t, latest))
		updated := stampOf(f.stamp[anaKey(latest)])
		use := requireKeys(t, "use", top["use"], "updated_at", "rows", "median", "trend")
		quality := requireKeys(t, "quality", top["quality"], "updated_at", "rows", "median", "trend")
		hosp := requireKeys(t, "hospitals", top["hospitals"], "updated_at", "rows")
		for _, a := range []map[string]any{use, quality, hosp} {
			if a["updated_at"] != updated {
				t.Errorf("%s: updated_at %v, want %s", k, a["updated_at"], updated)
			}
		}
		for _, x := range list(t, "use rows", use["rows"]) {
			row := requireKeys(t, "use row", x, "hospital", "questions", "active_clinicians", "questions_per_clinician")
			s := stored[row["hospital"].(map[string]any)["id"].(string)]
			for _, key := range []string{"questions", "active_clinicians", "questions_per_clinician"} {
				if jsonOf(row[key]) != jsonOf(s[key]) {
					t.Errorf("%s use %s: %s, stored %s", k, key, jsonOf(row[key]), jsonOf(s[key]))
				}
			}
			checkGuarded(t, "use questions", row["questions"], false)
		}
		requireKeys(t, "use median", use["median"], "questions", "active_clinicians", "questions_per_clinician")
		for _, x := range list(t, "quality rows", quality["rows"]) {
			row := requireKeys(t, "quality row", x, "hospital", "answers", "needs_review_share", "confidence", "reviews", "agreement_rate", "median_review_hours")
			s := stored[row["hospital"].(map[string]any)["id"].(string)]
			conf := requireKeys(t, "confidence", row["confidence"], "high", "medium", "low")
			for got, want := range map[string]string{
				jsonOf(row["answers"]): jsonOf(s["answers"]), jsonOf(row["needs_review_share"]): jsonOf(s["escalate_share"]),
				jsonOf(conf["high"]): jsonOf(s["pass"]), jsonOf(conf["medium"]): jsonOf(s["regenerate"]), jsonOf(conf["low"]): jsonOf(s["escalate"]),
				jsonOf(row["reviews"]): jsonOf(s["reviews"]), jsonOf(row["agreement_rate"]): jsonOf(s["agreement_rate"]),
				jsonOf(row["median_review_hours"]): jsonOf(s["median_review_hours"]),
			} {
				if got != want {
					t.Errorf("%s quality: %s, stored %s", k, got, want)
				}
			}
			checkGuarded(t, "needs review", row["needs_review_share"], true)
		}
		requireKeys(t, "quality median", quality["median"], "answers", "needs_review_share", "confidence", "reviews", "agreement_rate", "median_review_hours")
		for _, x := range list(t, "hospital rows", hosp["rows"]) {
			row := requireKeys(t, "hospital row", x, "hospital", "size")
			size := requireKeys(t, "size", row["size"], "staff", "active_clinicians", "questions")
			s := stored[row["hospital"].(map[string]any)["id"].(string)]
			if jsonOf(size["staff"]) != jsonOf(s["staff"]) || jsonOf(size["questions"]) != jsonOf(s["questions"]) {
				t.Errorf("%s size %v, stored %v", k, size, s)
			}
		}
		if n := len(list(t, "rows", use["rows"])); n != 3 {
			t.Errorf("%s: %d use rows", k, n)
		}
	}
}

// A1: reads on the 3rd and on the 27th of a month, through routes whose
// clock says so, are byte-identical, updated_at included.
func TestAnalyticsSameAllPeriod(t *testing.T) {
	fixedAnalytics(t)
	read := func(now time.Time) string {
		mux := authz.NewMux(guard)
		platform.Register(mux, platform.Deps{Pool: app, Now: func() time.Time { return now }})
		h := mux.Handler([]string{serviceKey})
		req := httptest.NewRequest(http.MethodGet, httpx.BasePath+"/platform/analytics?period=month", nil)
		req.Header.Set("X-MHAI-Service", serviceKey)
		req.Header.Set("X-MHAI-Audience", "platform")
		req.Header.Set("Authorization", "Bearer "+mint(t, auditor(t)))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%v: %d %s", now, w.Code, w.Body)
		}
		return w.Body.String()
	}
	a := read(time.Date(2090, 4, 3, 9, 0, 0, 0, time.UTC))
	b := read(time.Date(2090, 4, 27, 23, 0, 0, 0, time.UTC))
	if a != b {
		t.Errorf("the 3rd and the 27th differ:\n%s\n%s", a, b)
	}
}

// A3: only week, month and quarter; one period; nothing else in the query.
func TestAnalyticsRefusesRollingPeriod(t *testing.T) {
	tok := mint(t, platformAdmin(t))
	for _, q := range []string{"", "?period=", "?period=7d", "?period=30d", "?period=90d", "?period=Month", "?period=year",
		"?period=week&period=month", "?period=week&hospital=x", "?x=1"} {
		problem(t, "analytics "+q, analyticsGet(t, tok, q), 422, "validation_failed")
	}
}

// A11: no shown count of 1 to 4 anywhere in the JSON; every count and
// share in the rows is guarded.
func TestAnalyticsNoRawSmallCount(t *testing.T) {
	fixedAnalytics(t)
	for _, k := range []string{"week", "month", "quarter"} {
		r := analyticsGet(t, mint(t, auditor(t)), "?period="+k)
		ok(t, k, r)
		if m := rawSmall.FindString(r.raw); m != "" {
			t.Errorf("%s: a small count (%s)", k, m)
		}
		for _, x := range r.body["quality"].(map[string]any)["rows"].([]any) {
			row := x.(map[string]any)
			for _, key := range []string{"answers", "reviews"} {
				checkGuarded(t, k+" "+key, row[key], false)
			}
			for _, key := range []string{"needs_review_share", "agreement_rate"} {
				checkGuarded(t, k+" "+key, row[key], true)
			}
			for _, c := range row["confidence"].(map[string]any) {
				checkGuarded(t, k+" confidence", c, false)
			}
		}
		if !strings.Contains(r.raw, `"suppressed":true`) {
			t.Errorf("%s: nothing hidden: the fixture tests nothing", k)
		}
	}
}

// medianOf is the median of the rows' shown values of key.
func medianOf(rows []any, get func(map[string]any) any) (float64, bool) {
	var v []float64
	for _, x := range rows {
		g := get(x.(map[string]any)).(map[string]any)
		if g["suppressed"] == false && g["value"] != nil {
			v = append(v, g["value"].(float64))
		}
	}
	if len(v) == 0 {
		return 0, false
	}
	sort.Float64s(v)
	if len(v)%2 == 1 {
		return v[len(v)/2], true
	}
	return (v[len(v)/2-1] + v[len(v)/2]) / 2, true
}

// A13: each median is the median of the shown row values.
func TestAnalyticsMedianFromShownOnly(t *testing.T) {
	fixedAnalytics(t)
	r := analyticsGet(t, mint(t, auditor(t)), "?period=month")
	ok(t, "month", r)
	for _, area := range []string{"use", "quality"} {
		a := r.body[area].(map[string]any)
		med := a["median"].(map[string]any)
		for key := range med {
			if key == "confidence" {
				continue
			}
			want, has := medianOf(a["rows"].([]any), func(m map[string]any) any { return m[key] })
			got := med[key].(map[string]any)
			switch {
			case !has && (got["value"] != nil || got["suppressed"] != false):
				t.Errorf("%s %s: %v, want nothing to show", area, key, got)
			case has && (want > 0 && want < 5) && !strings.Contains(key, "share") && !strings.Contains(key, "rate") &&
				!strings.Contains(key, "per_") && !strings.Contains(key, "hours"):
				if got["suppressed"] != true {
					t.Errorf("%s %s: %v, want a count median of %v hidden", area, key, got, want)
				}
			case has && got["value"] != want:
				t.Errorf("%s %s: %v, want %v", area, key, got["value"], want)
			}
		}
	}
}

// F19: before the first published quarter (none stored), from and to are
// null and every area {not_yet, first_from}, the first quarter starting
// at or after the release epoch.
func TestAnalyticsNotYet(t *testing.T) {
	ctx := context.Background()
	type row struct {
		from, to, state string
		body            *string
		computed        time.Time
	}
	var saved []row
	rs, err := super.Query(ctx, `SELECT period_from::text, period_to::text, state, body::text, computed_at FROM analytics_snapshots WHERE kind = 'quarter'`)
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.from, &r.to, &r.state, &r.body, &r.computed); err != nil {
			t.Fatal(err)
		}
		saved = append(saved, r)
	}
	rs.Close()
	if _, err := super.Exec(ctx, `DELETE FROM analytics_snapshots WHERE kind = 'quarter'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, r := range saved {
			if _, err := super.Exec(ctx, `INSERT INTO analytics_snapshots (kind, period_from, period_to, state, body, computed_at)
				VALUES ('quarter', $1::date, $2::date, $3, $4::jsonb, $5)`, r.from, r.to, r.state, r.body, r.computed); err != nil {
				t.Error(err)
			}
		}
	})
	var epoch time.Time
	if err := super.QueryRow(ctx, `SELECT released_at FROM analytics_epoch`).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	first := calendar.Of(calendar.Quarter, epoch)
	if first.From.Before(epoch) {
		first = calendar.Next(first)
	}
	r := analyticsGet(t, mint(t, platformAdmin(t)), "?period=quarter")
	ok(t, "quarter", r)
	if r.body["from"] != nil || r.body["to"] != nil || r.body["alerts"] != nil {
		t.Errorf("not yet: %s", r.raw)
	}
	for _, a := range []string{"use", "quality", "hospitals"} {
		v := requireKeys(t, a, r.body[a], "not_yet", "first_from")
		if v["not_yet"] != true || v["first_from"] != stampOf(first.From) {
			t.Errorf("%s: %v, want first_from %v", a, v, first)
		}
	}
}

// A16, F13: the latest stored period unavailable, or holding a shown 3,
// makes use, quality and hospitals {unavailable: true}; an older period
// unavailable hides only its trend point.
func TestAnalyticsAreaUnavailable(t *testing.T) {
	f := fixedAnalytics(t)
	ctx := context.Background()
	weeks := f.rows[calendar.Week]
	last, older := weeks[len(weeks)-1], weeks[len(weeks)-3]
	if _, err := super.Exec(ctx, `UPDATE analytics_snapshots SET state = 'unavailable', body = NULL WHERE kind = 'week' AND period_from = $1::date`,
		older.From.Format(time.DateOnly)); err != nil {
		t.Fatal(err)
	}
	r := analyticsGet(t, mint(t, platformAdmin(t)), "?period=week")
	ok(t, "week", r)
	tr := r.body["use"].(map[string]any)["trend"].([]any)
	if len(tr) != 12 {
		t.Fatalf("%d trend points", len(tr))
	}
	for i, x := range tr {
		pt := x.(map[string]any)
		hidden := pt["value"].(map[string]any)["suppressed"] == true
		if hidden != (i == 9) || pt["start"] != stampOf(weeks[2+i].From) {
			t.Errorf("point %d: %v", i, pt)
		}
	}
	for name, stmt := range map[string]string{
		"a shown 3":   `UPDATE analytics_snapshots SET body = jsonb_set(body, '{hospitals,0,answers}', '{"value": 3, "suppressed": false}') WHERE kind = 'week' AND period_from = $1::date`,
		"unavailable": `UPDATE analytics_snapshots SET state = 'unavailable', body = NULL WHERE kind = 'week' AND period_from = $1::date`,
	} {
		if _, err := super.Exec(ctx, stmt, last.From.Format(time.DateOnly)); err != nil {
			t.Fatal(err)
		}
		r := analyticsGet(t, mint(t, platformAdmin(t)), "?period=week")
		ok(t, name, r)
		if r.body["from"] != stampOf(last.From) || r.body["alerts"] != nil {
			t.Errorf("%s: %s", name, r.raw)
		}
		for _, a := range []string{"use", "quality", "hospitals", "changes"} {
			if v := requireKeys(t, name+" "+a, r.body[a], "unavailable"); v["unavailable"] != true {
				t.Errorf("%s %s: %v", name, a, v)
			}
		}
		if rawSmall.MatchString(r.raw) {
			t.Errorf("%s: the small count was sent: %s", name, r.raw)
		}
		// The other kinds are untouched.
		ok(t, name+" month", analyticsGet(t, mint(t, platformAdmin(t)), "?period=month"))
	}
}

// A23: 100 parallel reads run no statement on a clinical table; the 31st
// read in a minute of one session is 429, the series counted with it.
func TestAnalyticsReadsNoClinicalTable(t *testing.T) {
	f := fixedAnalytics(t)
	before := callsLike(t, "%clinical_decisions%") + callsLike(t, "%clinical_queries%") + callsLike(t, "%feedback_events%") +
		callsLike(t, "%analytics_period_counts%") + callsLike(t, "%mhai_analytics_%")
	var toks []string
	for i := 0; i < 4; i++ {
		toks = append(toks, mint(t, auditor(t)))
	}
	var wg sync.WaitGroup
	codes := make(chan int, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes <- analyticsGet(t, toks[i%4], "?period="+[]string{"week", "month", "quarter"}[i%3]).code
		}(i)
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 200 {
			t.Errorf("a parallel read: %d", c)
		}
	}
	if d := callsLike(t, "%clinical_decisions%") + callsLike(t, "%clinical_queries%") + callsLike(t, "%feedback_events%") +
		callsLike(t, "%analytics_period_counts%") + callsLike(t, "%mhai_analytics_%") - before; d != 0 {
		t.Errorf("%d statements on a clinical table or the analytics counts", d)
	}
	tok := mint(t, platformAdmin(t))
	for i := 0; i < platform.AnalyticsPerMinute-1; i++ {
		ok(t, fmt.Sprintf("read %d", i+1), analyticsGet(t, tok, "?period=month"))
	}
	ok(t, "30th read, a series", get(t, "/platform/analytics/hospitals/"+f.hosp[0].ID+"/series?measure=needs_review_share&period=month", tok))
	problem(t, "31st read", analyticsGet(t, tok, "?period=month"), 429, "rate_limited")
	problem(t, "32nd read, a series", get(t, "/platform/analytics/hospitals/"+f.hosp[0].ID+"/series?measure=needs_review_share&period=month", tok),
		429, "rate_limited")
}

// A20 (reads): platform admins and auditors; hospital audiences
// not_platform; two-step always; reads only.
func TestAnalyticsRoles(t *testing.T) {
	f := fixedAnalytics(t)
	h := seedHospital(t, "active")
	for _, path := range []string{"/platform/analytics?period=month", "/platform/analytics/hospitals/" + f.hosp[0].ID + "/series?measure=needs_review_share&period=month",
		"/platform/analytics/changes/" + uuid.NewString() + "/series?period=month"} {
		want := 200
		if strings.Contains(path, "/changes/") {
			want = 404
		}
		for _, role := range []string{authz.RolePlatformAdmin, authz.RoleAuditor} {
			if r := get(t, path, mint(t, seedUser(t, uuid.Nil, "", role))); r.code != want {
				t.Errorf("%s %s: %d %s", role, path, r.code, r.raw)
			}
		}
		problem(t, "clinician", get(t, path, mint(t, seedUser(t, h, "", authz.RoleClinician))), 403, "not_platform")
		problem(t, "hospital admin", get(t, path, mint(t, seedUser(t, h, "", authz.RoleHospitalAdmin))), 403, "not_platform")
		problem(t, "no two-step", get(t, path, signIn(t, platformAdmin(t), false).tok), 403, "mfa_required")
		problem(t, "anonymous", get(t, path, ""), 401, "unauthenticated")
		problem(t, "auditor on the clinical audience", do(t, "GET", path, mint(t, auditor(t)), false), 403, "not_platform")
		if r := do(t, "POST", path, mint(t, platformAdmin(t)), true); r.code < 400 {
			t.Errorf("POST %s: %d", path, r.code)
		}
	}
}

// A9 in 7c-2: for the same period, guardrails/periods and the quality area
// show the same cells, shown and hidden.
func TestAnalyticsMatchesGuardrails(t *testing.T) {
	fixedAnalytics(t)
	for _, k := range []string{"week", "month", "quarter"} {
		g := periods(t, mint(t, auditor(t)), "?period="+k)
		ok(t, "guardrails "+k, g)
		a := analyticsGet(t, mint(t, auditor(t)), "?period="+k)
		ok(t, "analytics "+k, a)
		if g.body["from"] != a.body["from"] || g.body["to"] != a.body["to"] {
			t.Fatalf("%s: periods differ: %v %v", k, g.body["from"], a.body["from"])
		}
		rows := map[string]map[string]any{}
		for _, x := range g.body["hospitals"].([]any) {
			m := x.(map[string]any)
			rows[m["hospital"].(map[string]any)["id"].(string)] = m
		}
		qrows := a.body["quality"].(map[string]any)["rows"].([]any)
		if len(qrows) != len(rows) {
			t.Errorf("%s: %d quality rows, %d guardrail rows", k, len(qrows), len(rows))
		}
		for _, x := range qrows {
			q := x.(map[string]any)
			gr := rows[q["hospital"].(map[string]any)["id"].(string)]
			conf := q["confidence"].(map[string]any)
			for got, want := range map[string]string{jsonOf(q["answers"]): jsonOf(gr["answers"]), jsonOf(q["needs_review_share"]): jsonOf(gr["escalate_share"]),
				jsonOf(conf["high"]): jsonOf(gr["pass"]), jsonOf(conf["medium"]): jsonOf(gr["regenerate"]), jsonOf(conf["low"]): jsonOf(gr["escalate"])} {
				if got != want {
					t.Errorf("%s: quality %s, guardrails %s", k, got, want)
				}
			}
		}
	}
}
