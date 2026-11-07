//go:build integration

package platform_test

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/scheduler"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/platform"
)

// The guardrail statistics through core's whole chain: calendar periods
// from stored snapshots on GET /platform/guardrails/periods (Plan 7c Task
// 5, spec §9, R41a-4), and the deprecated GET /platform/guardrails, its
// rolling windows fully hidden (R41a-3). Every name, id and count here is
// synthetic.

// rawSmall is a count of 1 to 4 sent as a value.
var rawSmall = regexp.MustCompile(`"value":\s*[1-4]\s*[,}]`)

var rateKeys = []string{"answers", "pass", "regenerate", "escalate", "degraded", "pass_share", "regenerate_share", "escalate_share"}

// published is the latest finished month, its weeks published by the real
// scheduler once, with hospitals of every size (counts of 1 to 4 included)
// seeded before.
var calFixture struct {
	once  sync.Once
	month calendar.Period
	hosp  []uuid.UUID
}

func seedQuestionsAt(t *testing.T, h, by uuid.UUID, at time.Time, decision string, degraded bool, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		q := uuid.New()
		if _, err := super.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, created_by, created_at)
			VALUES ($1, $2, 'SYNTHETIC question', 'routine', $3, $4)`, q, h, by, at); err != nil {
			t.Fatal(err)
		}
		if _, err := super.Exec(ctx, `INSERT INTO clinical_decisions (id, hospital_id, query_id, hga_decision, degraded, response_text, created_at)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, 'SYNTHETIC answer', $5)`, h, q, decision, degraded, at.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
}

func publish(t *testing.T) {
	t.Helper()
	calFixture.once.Do(func() {
		ctx := context.Background()
		m := calendar.Latest(calendar.Month, time.Now())
		calFixture.month = m
		if _, err := super.Exec(ctx, `UPDATE analytics_epoch SET released_at = $1`, m.From); err != nil {
			t.Fatal(err)
		}
		for _, c := range [][4]int{{1, 0, 0, 1}, {3, 2, 1, 2}, {4, 4, 4, 4}, {0, 1, 0, 0}, {6, 3, 0, 2}, {2, 0, 7, 1}, {12, 4, 1, 3}, {20, 9, 8, 0}, {15, 6, 5, 5}} {
			h := seedHospital(t, "active")
			if _, err := super.Exec(ctx, `UPDATE hospitals SET created_at = $2 WHERE id = $1`, h, m.From.AddDate(0, -1, 0)); err != nil {
				t.Fatal(err)
			}
			u := seedUser(t, h, "", authz.RoleClinician)
			calFixture.hosp = append(calFixture.hosp, h)
			for _, w := range calendar.Children(m) {
				at := w.From.Add(26 * time.Hour)
				seedQuestionsAt(t, h, u.id, at, "PASS", false, c[0])
				seedQuestionsAt(t, h, u.id, at, "REGENERATE", false, c[1])
				seedQuestionsAt(t, h, u.id, at, "ESCALATE", false, c[2])
				seedQuestionsAt(t, h, u.id, at, "ESCALATE", true, c[3])
			}
		}
		s := scheduler.Scheduler{Pool: app, Builder: scheduler.Builder{Src: scheduler.DB{Pool: app}}}
		if _, err := s.Tick(ctx); err != nil {
			t.Fatalf("scheduler: %v", err)
		}
	})
}

func guardrails(t *testing.T, tok, query string) resp {
	t.Helper()
	return get(t, "/platform/guardrails"+query, tok)
}

// periods is GET /platform/guardrails/periods.
func periods(t *testing.T, tok, query string) resp {
	t.Helper()
	return get(t, "/platform/guardrails/periods"+query, tok)
}

// stored is the stored body of the latest period of kind k, and its period.
func stored(t *testing.T, k calendar.Kind) (map[string]any, calendar.Period, time.Time) {
	t.Helper()
	var from, computed time.Time
	var body string
	if err := super.QueryRow(context.Background(), `SELECT period_from, body::text, computed_at FROM analytics_snapshots
		WHERE kind = $1 AND state = 'ok' ORDER BY period_from DESC LIMIT 1`, string(k)).Scan(&from, &body, &computed); err != nil {
		t.Fatalf("%s: %v", k, err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	return m, calendar.Of(k, from.UTC()), computed
}

// hospitalRow is the response's row for hid, or nil.
func hospitalRow(t *testing.T, r resp, hid uuid.UUID) map[string]any {
	t.Helper()
	for _, x := range r.body["hospitals"].([]any) {
		row := x.(map[string]any)
		if row["hospital"].(map[string]any)["id"] == hid.String() {
			return row
		}
	}
	return nil
}

// checkGuarded: v is {value, suppressed}; a hidden value is null; a shown
// count is 0 or at least 5 (C19), a share between 0 and 1.
func checkGuarded(t *testing.T, what string, v any, share bool) {
	t.Helper()
	m := requireKeys(t, what, v, "value", "suppressed")
	sup, ok := m["suppressed"].(bool)
	if !ok {
		t.Fatalf("%s: suppressed %v", what, m["suppressed"])
	}
	if sup && m["value"] != nil {
		t.Errorf("%s: hidden with a value %v", what, m["value"])
	}
	if m["value"] == nil {
		if !share && !sup {
			t.Errorf("%s: a count with nothing to show", what)
		}
		return
	}
	n := m["value"].(float64)
	switch {
	case share && (n < 0 || n > 1):
		t.Errorf("%s: share %v", what, n)
	case !share && (n != float64(int64(n)) || (n >= 1 && n <= 4) || n < 0):
		t.Errorf("%s: count %v sent", what, n)
	}
}

func checkRates(t *testing.T, what string, v any, extra ...string) map[string]any {
	t.Helper()
	m := requireKeys(t, what, v, append(append([]string{}, rateKeys...), extra...)...)
	for _, k := range rateKeys {
		checkGuarded(t, what+"."+k, m[k], strings.HasSuffix(k, "_share"))
	}
	return m
}

func stampOf(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// callsLike is the statements run by the app's roles whose text matches.
func callsLike(t *testing.T, pattern string) int64 {
	t.Helper()
	var n int64
	if err := super.QueryRow(context.Background(), `SELECT COALESCE(sum(s.calls), 0)::bigint FROM pg_stat_statements s
		JOIN pg_roles r ON r.oid = s.userid WHERE r.rolname <> current_user AND s.query ILIKE $1`, pattern).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGuardrailsShape(t *testing.T) {
	publish(t)
	for _, role := range []string{authz.RolePlatformAdmin, authz.RoleAuditor} {
		for _, k := range []calendar.Kind{calendar.Week, calendar.Month} {
			r := periods(t, mint(t, seedUser(t, uuid.Nil, "", role)), "?period="+string(k))
			ok(t, role+" "+string(k), r)
			top := requireKeys(t, string(k), r.body, "period", "from", "to", "updated_at", "all", "hospitals")
			_, p, computed := stored(t, k)
			if top["period"] != string(k) || top["from"] != stampOf(p.From) || top["to"] != stampOf(p.To) || top["updated_at"] != stampOf(computed) {
				t.Errorf("%s: %v %v %v %v, want %v (computed %v)", k, top["period"], top["from"], top["to"], top["updated_at"], p, computed)
			}
			if k == calendar.Month && !p.Equal(calFixture.month) {
				t.Errorf("month %v, want the latest %v", p, calFixture.month)
			}
			checkRates(t, string(k)+" all", top["all"])
			for i, x := range list(t, "hospitals", top["hospitals"]) {
				row := checkRates(t, fmt.Sprint(k, " row ", i), x, "hospital")
				requireKeys(t, "hospital", row["hospital"], "id", "name")
			}
			if hospitalRow(t, r, calFixture.hosp[len(calFixture.hosp)-2]) == nil {
				t.Errorf("%s: a seeded hospital is missing", k)
			}
			if !regexp.MustCompile(`"pass_share":\{"value":(null|[01]\.\d{4})`).MatchString(r.raw) {
				t.Errorf("%s: shares not sent with four decimals: %s", k, r.raw)
			}
		}
	}
	// The query: period, and nothing else; each route its own values
	// (R41a-4: the old route keeps its enum).
	tok := mint(t, platformAdmin(t))
	for _, q := range []string{"", "?period=", "?period=1d", "?period=7D", "?period=Week", "?period=year", "?period=week&period=month",
		"?period=week&hospital=x", "?x=1", "?period=7d", "?period=30d"} {
		problem(t, "periods query "+q, periods(t, tok, q), 422, "validation_failed")
	}
	for _, q := range []string{"", "?period=", "?period=week", "?period=month", "?period=quarter", "?period=7d&x=1", "?period=7d&period=30d"} {
		problem(t, "guardrails query "+q, guardrails(t, tok, q), 422, "validation_failed")
	}
	// Platform accounts only, two-step always, on both routes.
	h := calFixture.hosp[0]
	for _, c := range []struct{ path, q string }{{"/platform/guardrails/periods", "?period=week"}, {"/platform/guardrails", "?period=7d"}} {
		problem(t, "clinician", get(t, c.path+c.q, mint(t, seedUser(t, h, "", authz.RoleClinician))), 403, "not_platform")
		problem(t, "hospital admin", get(t, c.path+c.q, mint(t, seedUser(t, h, "", authz.RoleHospitalAdmin))), 403, "not_platform")
		problem(t, "no two-step", get(t, c.path+c.q, signIn(t, platformAdmin(t), false).tok), 403, "mfa_required")
		problem(t, "anonymous", get(t, c.path+c.q, ""), 401, "unauthenticated")
		problem(t, "auditor on the clinical audience", do(t, "GET", c.path+c.q, mint(t, auditor(t)), false), 403, "not_platform")
		if got := do(t, "POST", c.path+c.q, tok, true); got.code < 400 {
			t.Errorf("POST %s: %d", c.path, got.code)
		}
	}
}

// week, month and quarter serve the latest stored period; a kind with
// nothing published is 503 not_ready (F19); a stored body that breaks rule
// 1 is never served.
func TestGuardrailsCalendarPeriods(t *testing.T) {
	publish(t)
	ctx := context.Background()
	tok := mint(t, platformAdmin(t))
	var quarters int
	if err := super.QueryRow(ctx, `SELECT count(*) FROM analytics_snapshots WHERE kind = 'quarter'`).Scan(&quarters); err != nil {
		t.Fatal(err)
	}
	if quarters == 0 {
		problem(t, "no quarter published", periods(t, tok, "?period=quarter"), 503, "not_ready")
	}
	// A quarter, stored by hand (the superuser passes the insert policy):
	// the month's body, relabelled.
	body, _, _ := stored(t, calendar.Month)
	q := calendar.Latest(calendar.Quarter, time.Now())
	body["kind"], body["from"], body["to"] = "quarter", q.From.Format(time.DateOnly), q.To.Format(time.DateOnly)
	raw, _ := json.Marshal(body)
	if _, err := super.Exec(ctx, `INSERT INTO analytics_snapshots (kind, period_from, period_to, state, body) VALUES ('quarter', $1::date, $2::date, 'ok', $3::jsonb)`,
		q.From.Format(time.DateOnly), q.To.Format(time.DateOnly), string(raw)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = super.Exec(ctx, `DELETE FROM analytics_snapshots WHERE kind = 'quarter' AND period_from = $1::date`, q.From.Format(time.DateOnly))
	}()
	r := periods(t, tok, "?period=quarter")
	ok(t, "quarter", r)
	if r.body["from"] != stampOf(q.From) || r.body["to"] != stampOf(q.To) {
		t.Errorf("quarter %v–%v, want %v", r.body["from"], r.body["to"], q)
	}
	// A stored shown 3 (put there by hand) is refused: 500, no body.
	if _, err := super.Exec(ctx, `UPDATE analytics_snapshots SET body = jsonb_set(body, '{all,answers}', '{"value": 3, "suppressed": false}')
		WHERE kind = 'quarter' AND period_from = $1::date`, q.From.Format(time.DateOnly)); err != nil {
		t.Fatal(err)
	}
	bad := periods(t, mint(t, platformAdmin(t)), "?period=quarter")
	problem(t, "a stored small count", bad, 500, "internal_error")
	if rawSmall.MatchString(bad.raw) {
		t.Errorf("the small count was sent: %s", bad.raw)
	}
	// Stored unavailable: 503.
	if _, err := super.Exec(ctx, `UPDATE analytics_snapshots SET state = 'unavailable', body = NULL WHERE kind = 'quarter' AND period_from = $1::date`,
		q.From.Format(time.DateOnly)); err != nil {
		t.Fatal(err)
	}
	problem(t, "an unavailable quarter", periods(t, mint(t, platformAdmin(t)), "?period=quarter"), 503, "not_ready")
}

// A9: for the same period, the route shows the stored body's QG cells, the
// same ones shown and hidden, for all and for every hospital.
func TestGuardrailsMatchesQuality(t *testing.T) {
	publish(t)
	for _, k := range []calendar.Kind{calendar.Week, calendar.Month} {
		body, _, _ := stored(t, k)
		r := periods(t, mint(t, auditor(t)), "?period="+string(k))
		ok(t, string(k), r)
		same := func(what string, got, want map[string]any) {
			for _, key := range rateKeys {
				g, _ := json.Marshal(got[key])
				w, _ := json.Marshal(want[key])
				if string(g) != string(w) {
					t.Errorf("%s %s %s: route %s, stored %s", k, what, key, g, w)
				}
			}
		}
		same("all", r.body["all"].(map[string]any), body["all"].(map[string]any))
		rows := map[string]map[string]any{}
		for _, x := range body["hospitals"].([]any) {
			m := x.(map[string]any)
			rows[m["hospital"].(map[string]any)["id"].(string)] = m
		}
		got := r.body["hospitals"].([]any)
		if len(got) != len(rows) {
			t.Errorf("%s: %d rows, stored %d", k, len(got), len(rows))
		}
		for _, x := range got {
			m := x.(map[string]any)
			id := m["hospital"].(map[string]any)["id"].(string)
			same(id, m, rows[id])
		}
	}
}

// A10: the rolling values carry Deprecation, Sunset (90 days on) and Link,
// errors included; every count and share is hidden, all included; nothing
// touches guardrail_snapshots; after the sunset they are 422. The calendar
// values carry no deprecation.
func TestGuardrailsRollingDeprecated(t *testing.T) {
	publish(t)
	before := callsLike(t, "%guardrail_snapshots%")
	beforeCounts := callsLike(t, "%mhai_guardrail_counts%")
	u := platformAdmin(t)
	tok := mint(t, u)
	wantDep := "@" + strconv.FormatInt(platform.RollingDeprecatedAt.Unix(), 10)
	wantSunset := platform.RollingSunset.UTC().Format(time.RFC1123)
	wantSunset = strings.Replace(wantSunset, "UTC", "GMT", 1)
	deprecated := func(what string, r resp) {
		t.Helper()
		if r.hdr.Get("Deprecation") != wantDep || r.hdr.Get("Sunset") != wantSunset ||
			!strings.Contains(r.hdr.Get("Link"), "</docs/notices/guardrails-rolling-windows>; rel=\"deprecation\"") {
			t.Errorf("%s: Deprecation %q Sunset %q Link %q", what, r.hdr.Get("Deprecation"), r.hdr.Get("Sunset"), r.hdr.Get("Link"))
		}
	}
	var active int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM hospitals WHERE status = 'active'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"7d", "30d", "90d"} {
		r := guardrails(t, tok, "?period="+p)
		ok(t, p, r)
		deprecated(p, r)
		if m := regexp.MustCompile(`"value":[^n]|"suppressed":false`).FindString(r.raw); m != "" {
			t.Errorf("%s: something shown (%s): %s", p, m, r.raw)
		}
		checkRates(t, p+" all", r.body["all"])
		if n := len(r.body["hospitals"].([]any)); n == 0 || n > active {
			t.Errorf("%s: %d rows, %d active hospitals", p, n, active)
		}
		if r.body["period"] != p {
			t.Errorf("period %v", r.body["period"])
		}
	}
	// Errors carry the headers too: the rate limit, on a rolling value.
	for i := 0; i < platform.GuardrailsPerMinute; i++ {
		guardrails(t, tok, "?period=7d")
	}
	limited := guardrails(t, tok, "?period=7d")
	problem(t, "past the limit", limited, 429, "rate_limited")
	deprecated("429", limited)
	cal := periods(t, mint(t, u), "?period=month")
	ok(t, "month", cal)
	if cal.hdr.Get("Deprecation") != "" || cal.hdr.Get("Sunset") != "" {
		t.Errorf("the periods route is marked deprecated: %v", cal.hdr)
	}
	// The old route is deprecated whole: a calendar value there is 422, with
	// the headers.
	wrong := guardrails(t, mint(t, u), "?period=month")
	problem(t, "a calendar value on the old route", wrong, 422, "validation_failed")
	deprecated("422", wrong)
	if d := callsLike(t, "%guardrail_snapshots%") - before; d != 0 {
		t.Errorf("%d statements on guardrail_snapshots", d)
	}
	if d := callsLike(t, "%mhai_guardrail_counts%") - beforeCounts; d != 0 {
		t.Errorf("%d calls of mhai_guardrail_counts", d)
	}
	// After the sunset, the values are gone.
	restore := platform.SetGuardrailsClock(func() time.Time { return platform.RollingSunset.Add(time.Minute) })
	defer restore()
	gone := guardrails(t, mint(t, u), "?period=30d")
	problem(t, "after the sunset", gone, 422, "validation_failed")
	ok(t, "month after the sunset", periods(t, mint(t, u), "?period=month"))
}

// A23 for guardrails: a calendar read runs no statement on a clinical
// table, only the stored snapshot.
func TestGuardrailsCalendarNoComputation(t *testing.T) {
	publish(t)
	before := callsLike(t, "%clinical_decisions%") + callsLike(t, "%clinical_queries%")
	for _, k := range []string{"week", "month", "week", "month"} {
		ok(t, k, periods(t, mint(t, auditor(t)), "?period="+k))
	}
	if d := callsLike(t, "%clinical_decisions%") + callsLike(t, "%clinical_queries%") - before; d != 0 {
		t.Errorf("%d statements on a clinical table", d)
	}
}

// C19 over the calendar values: hospitals with counts of 1 to 4
// everywhere; no "value" of 1 to 4 in the JSON, and no shown count is one.
func TestGuardrailsNoRawSmallCount(t *testing.T) {
	publish(t)
	for _, k := range []string{"week", "month"} {
		r := periods(t, mint(t, platformAdmin(t)), "?period="+k)
		ok(t, k, r)
		if m := rawSmall.FindString(r.raw); m != "" {
			t.Errorf("%s: a small count was sent (%s)", k, m)
		}
		checkRates(t, k+" all", r.body["all"])
		hidden := 0
		for i, x := range r.body["hospitals"].([]any) {
			m := checkRates(t, fmt.Sprint(k, " row ", i), x, "hospital")
			if m["answers"].(map[string]any)["suppressed"] == true {
				hidden++
			}
		}
		if hidden == 0 {
			t.Errorf("%s: no answers hidden: the seed tests nothing", k)
		}
	}
}

// C31: past 6 a minute per session, 429 rate_limited; another session is
// counted on its own.
func TestGuardrailsRateLimited(t *testing.T) {
	publish(t)
	u := platformAdmin(t)
	tok := mint(t, u)
	for i := 0; i < platform.GuardrailsPerMinute; i++ {
		ok(t, fmt.Sprintf("read %d", i+1), periods(t, tok, "?period=month"))
	}
	problem(t, "7th read", periods(t, tok, "?period=month"), 429, "rate_limited")
	problem(t, "8th read, the other route", guardrails(t, tok, "?period=7d"), 429, "rate_limited")
	ok(t, "another session", periods(t, mint(t, u), "?period=month"))
}
