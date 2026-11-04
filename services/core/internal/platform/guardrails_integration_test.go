//go:build integration

package platform_test

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/platform"
)

// Plan 4 Task 13: GET /platform/guardrails through core's whole chain
// (spec §6.3; C19, C20 end to end, C31). Every name, id and count here is
// synthetic.

// rawSmall is a count of 1 to 4 sent as a value.
var rawSmall = regexp.MustCompile(`"value":\s*[1-4]\s*[,}]`)

var rateKeys = []string{"answers", "pass", "regenerate", "escalate", "degraded", "pass_share", "regenerate_share", "escalate_share"}

// startOfToday is where every window ends (rulings R42, R43): the start of
// the UTC day 15 minutes ago.
func startOfToday() time.Time {
	return time.Now().UTC().Add(-platform.GuardrailsGrace).Truncate(24 * time.Hour)
}

// freshGuardrails empties the in-process cache and the stored snapshots,
// so a test sees what it seeded.
func freshGuardrails(t *testing.T) {
	t.Helper()
	platform.ResetGuardrailsCache()
	if _, err := super.Exec(context.Background(), `DELETE FROM guardrail_snapshots`); err != nil {
		t.Fatal(err)
	}
}

// seedDecisions adds n decisions of hid, ago before the start of today
// (UTC), as the superuser.
func seedDecisions(t *testing.T, hid uuid.UUID, decision string, degraded bool, n int, ago time.Duration) {
	t.Helper()
	seedDecisionsAt(t, hid, decision, degraded, n, startOfToday().Add(-ago))
}

func seedDecisionsAt(t *testing.T, hid uuid.UUID, decision string, degraded bool, n int, at time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := super.Exec(context.Background(), `INSERT INTO clinical_decisions (id, hospital_id, hga_decision, degraded, response_text, created_at)
			VALUES (gen_random_uuid(), $1, $2, $3, 'SYNTHETIC answer', $4)`, hid, decision, degraded, at); err != nil {
			t.Fatal(err)
		}
	}
}

func guardrails(t *testing.T, tok, query string) resp {
	t.Helper()
	return get(t, "/platform/guardrails"+query, tok)
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
		checkGuarded(t, what+"."+k, m[k], len(k) > 6 && k[len(k)-6:] == "_share")
	}
	return m
}

func shownValue(m map[string]any, k string) (float64, bool) {
	g := m[k].(map[string]any)
	if g["value"] == nil {
		return 0, false
	}
	return g["value"].(float64), true
}

func TestGuardrailsShape(t *testing.T) {
	freshGuardrails(t)
	h := seedHospital(t, "active")
	seedDecisions(t, h, "PASS", false, 10, time.Hour)
	seedDecisions(t, h, "REGENERATE", true, 6, 2*time.Hour)
	seedDecisions(t, h, "ESCALATE", true, 5, 3*time.Hour)
	// Today's decisions are not in any window until tomorrow.
	seedDecisionsAt(t, h, "PASS", false, 3, time.Now())
	raw := map[string]float64{"answers": 21, "pass": 10, "regenerate": 6, "escalate": 5, "degraded": 11}
	for _, role := range []string{authz.RolePlatformAdmin, authz.RoleAuditor} {
		for period, days := range map[string]int{"7d": 7, "30d": 30, "90d": 90} {
			r := guardrails(t, mint(t, seedUser(t, uuid.Nil, "", role)), "?period="+period)
			ok(t, role+" "+period, r)
			top := requireKeys(t, period, r.body, "period", "from", "to", "updated_at", "all", "hospitals")
			if top["period"] != period {
				t.Errorf("period %v", top["period"])
			}
			from, err1 := time.Parse(time.RFC3339Nano, top["from"].(string))
			to, err2 := time.Parse(time.RFC3339Nano, top["to"].(string))
			at, err3 := time.Parse(time.RFC3339Nano, top["updated_at"].(string))
			if err1 != nil || err2 != nil || err3 != nil || to.Sub(from) != time.Duration(days)*24*time.Hour ||
				!to.Equal(startOfToday()) || !at.Equal(to) {
				t.Errorf("%s: from %v to %v updated_at %v", period, top["from"], top["to"], top["updated_at"])
			}
			checkRates(t, period+" all", top["all"])
			for i, x := range list(t, "hospitals", top["hospitals"]) {
				row := checkRates(t, fmt.Sprint(period, " row ", i), x, "hospital")
				requireKeys(t, "hospital", row["hospital"], "id", "name")
			}
			row := hospitalRow(t, r, h)
			if row == nil {
				t.Fatalf("%s: the seeded hospital is missing", period)
			}
			for k, want := range raw {
				if v, shown := shownValue(row, k); shown && v != want {
					t.Errorf("%s: %s shown as %v, want %v", period, k, v, want)
				}
			}
			if s, shown := shownValue(row, "pass_share"); shown && s != 0.4762 {
				t.Errorf("%s: pass_share %v", period, s)
			}
			if r.raw == "" || !regexp.MustCompile(`"pass_share":\{"value":(null|[01]\.\d{4})`).MatchString(r.raw) {
				t.Errorf("%s: shares not sent with four decimals: %s", period, r.raw)
			}
		}
	}

	// The query: period, and nothing else.
	tok := mint(t, platformAdmin(t))
	for _, q := range []string{"", "?period=", "?period=1d", "?period=7D", "?period=7d&period=30d", "?period=7d&hospital=x", "?x=1"} {
		problem(t, "query "+q, guardrails(t, tok, q), 422, "validation_failed")
	}
	// Platform accounts only, two-step always.
	clin := mint(t, seedUser(t, h, "", authz.RoleClinician))
	problem(t, "clinician", guardrails(t, clin, "?period=7d"), 403, "not_platform")
	problem(t, "hospital admin", guardrails(t, mint(t, seedUser(t, h, "", authz.RoleHospitalAdmin)), "?period=7d"), 403, "not_platform")
	problem(t, "no two-step", guardrails(t, signIn(t, platformAdmin(t), false).tok, "?period=7d"), 403, "mfa_required")
	problem(t, "anonymous", guardrails(t, "", "?period=7d"), 401, "unauthenticated")
	problem(t, "auditor on the clinical audience", do(t, "GET", "/platform/guardrails?period=7d", mint(t, auditor(t)), false), 403, "not_platform")
	if got := do(t, "POST", "/platform/guardrails?period=7d", tok, true); got.code < 400 {
		t.Errorf("POST: %d", got.code)
	}
}

// Active hospitals only: a deactivated or set-up hospital has no row and
// isn't in all; all is the sum over active hospitals when shown.
func TestGuardrailsActiveHospitalsOnly(t *testing.T) {
	freshGuardrails(t)
	active := seedHospital(t, "active")
	gone := seedHospital(t, "deactivated")
	setup := seedHospital(t, "setup_incomplete")
	seedDecisions(t, active, "PASS", false, 8, time.Hour)
	seedDecisions(t, gone, "PASS", false, 9, time.Hour)
	seedDecisions(t, setup, "ESCALATE", false, 7, time.Hour)
	r := guardrails(t, mint(t, platformAdmin(t)), "?period=7d")
	ok(t, "guardrails", r)
	if hospitalRow(t, r, active) == nil {
		t.Error("the active hospital is missing")
	}
	if hospitalRow(t, r, gone) != nil || hospitalRow(t, r, setup) != nil {
		t.Error("a hospital that isn't active has a row")
	}
	from, err1 := time.Parse(time.RFC3339Nano, r.body["from"].(string))
	to, err2 := time.Parse(time.RFC3339Nano, r.body["to"].(string))
	if err1 != nil || err2 != nil {
		t.Fatalf("from %v to %v", r.body["from"], r.body["to"])
	}
	var want, wantActive int64
	if err := super.QueryRow(context.Background(), `SELECT count(*), count(*) FILTER (WHERE h.status = 'active')
		FROM clinical_decisions d JOIN hospitals h ON h.id = d.hospital_id
		WHERE d.created_at >= $1 AND d.created_at < $2`, from, to).Scan(&want, &wantActive); err != nil {
		t.Fatal(err)
	}
	all := r.body["all"].(map[string]any)
	if v, shown := shownValue(all, "answers"); !shown || int64(v) != wantActive || int64(v) == want {
		t.Errorf("all.answers %v (shown %v), want the active hospitals' %d, not every hospital's %d", v, shown, wantActive, want)
	}
	var activeRows int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM hospitals WHERE status = 'active'`).Scan(&activeRows); err != nil {
		t.Fatal(err)
	}
	if n := len(r.body["hospitals"].([]any)); n != activeRows {
		t.Errorf("%d rows, %d active hospitals", n, activeRows)
	}
}

// C31: past 6 a minute per session, 429 rate_limited; another session is
// counted on its own.
func TestGuardrailsRateLimited(t *testing.T) {
	u := platformAdmin(t)
	tok := mint(t, u)
	for i := 0; i < platform.GuardrailsPerMinute; i++ {
		ok(t, fmt.Sprintf("read %d", i+1), guardrails(t, tok, "?period=30d"))
	}
	problem(t, "7th read", guardrails(t, tok, "?period=30d"), 429, "rate_limited")
	problem(t, "8th read, another period", guardrails(t, tok, "?period=7d"), 429, "rate_limited")
	ok(t, "another session", guardrails(t, mint(t, u), "?period=30d"))
}

// R43: a period's day is one stored snapshot. A second core copy (a fresh
// in-process cache) serves the same bytes, though decisions were added and
// a hospital deactivated after the snapshot; with the snapshot gone, a
// fresh computation shows the change.
func TestGuardrailsOneSnapshotPerDay(t *testing.T) {
	freshGuardrails(t)
	h := seedHospital(t, "active")
	gone := seedHospital(t, "active")
	seedDecisions(t, h, "PASS", false, 20, time.Hour)
	seedDecisions(t, gone, "PASS", false, 30, time.Hour)
	tok := mint(t, platformAdmin(t))
	first := guardrails(t, tok, "?period=90d")
	ok(t, "first", first)
	if hospitalRow(t, first, gone) == nil {
		t.Fatal("the hospital to deactivate is missing")
	}
	seedDecisions(t, h, "PASS", false, 20, time.Hour) // a late commit, dated yesterday
	if _, err := super.Exec(context.Background(), `UPDATE hospitals SET status = 'deactivated' WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	second := guardrails(t, tok, "?period=90d")
	ok(t, "second, same copy", second)
	platform.ResetGuardrailsCache() // another core copy
	third := guardrails(t, mint(t, auditor(t)), "?period=90d")
	ok(t, "third, another copy", third)
	if first.raw != second.raw || first.raw != third.raw {
		t.Errorf("one day's reads differ:\n%s\n%s\n%s", first.raw, second.raw, third.raw)
	}
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM guardrail_snapshots WHERE period = '90d'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d stored snapshots for 90d (%v), want 1", n, err)
	}
	freshGuardrails(t)
	fourth := guardrails(t, tok, "?period=90d")
	ok(t, "fourth, after the snapshot is gone", fourth)
	a1, shown1 := shownValue(hospitalRow(t, first, h), "answers")
	a4, shown4 := shownValue(hospitalRow(t, fourth, h), "answers")
	if !shown1 || !shown4 || a1 != 20 || a4 != 40 {
		t.Errorf("answers: first %v (shown %v), recomputed %v (shown %v); want 20 then 40", a1, shown1, a4, shown4)
	}
	if hospitalRow(t, fourth, gone) != nil {
		t.Error("a recomputed day lists the deactivated hospital")
	}
}

// R43: from 00:00 to 00:15 UTC the previous day's figures are served (and
// computed if missing); from 00:15, today's.
func TestGuardrailsGraceServesPreviousDay(t *testing.T) {
	freshGuardrails(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	tok := mint(t, platformAdmin(t))
	for _, c := range []struct {
		at     time.Duration
		period string
		want   time.Time
	}{
		{5 * time.Minute, "7d", today.AddDate(0, 0, -1)},
		{20 * time.Minute, "7d", today},
		// Fix round 3: 90 days to yesterday's midnight starts just over 91
		// days before the database's clock, and is computed (not stored yet).
		{5 * time.Minute, "90d", today.AddDate(0, 0, -1)},
	} {
		restore := platform.SetGuardrailsClock(func() time.Time { return today.Add(c.at) })
		r := guardrails(t, tok, "?period="+c.period)
		restore()
		ok(t, c.period+" at "+c.at.String(), r)
		if r.body["to"] != c.want.Format("2006-01-02T15:04:05.000Z") || r.body["updated_at"] != r.body["to"] {
			t.Errorf("at %v past midnight: to %v updated_at %v, want %v", c.at, r.body["to"], r.body["updated_at"], c.want)
		}
	}
	var days []string
	if err := super.QueryRow(context.Background(), `SELECT array_agg(day::text ORDER BY day) FROM guardrail_snapshots WHERE period = '7d'`).Scan(&days); err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 || days[0] != today.AddDate(0, 0, -1).Format(time.DateOnly) || days[1] != today.Format(time.DateOnly) {
		t.Errorf("stored days %v", days)
	}
}

// A stored snapshot that breaks rule 1 (a shown 3, put there by hand) is
// never served: 500, no body.
func TestGuardrailsBadSnapshotRefused(t *testing.T) {
	freshGuardrails(t)
	tok := mint(t, platformAdmin(t))
	r := guardrails(t, tok, "?period=30d")
	ok(t, "first", r)
	if _, err := super.Exec(context.Background(), `UPDATE guardrail_snapshots
		SET body = jsonb_set(body, '{all,answers}', '{"value": 3, "suppressed": false}') WHERE period = '30d'`); err != nil {
		t.Fatal(err)
	}
	platform.ResetGuardrailsCache()
	bad := guardrails(t, tok, "?period=30d")
	problem(t, "a stored small count", bad, 500, "internal_error")
	if rawSmall.MatchString(bad.raw) {
		t.Errorf("the small count was sent: %s", bad.raw)
	}
}

// C19 end to end: hospitals with counts of 1 to 4 everywhere; the JSON
// never holds a "value" of 1 to 4, and no shown count is one.
func TestGuardrailsNoRawSmallCount(t *testing.T) {
	freshGuardrails(t)
	for _, c := range [][4]int{{1, 0, 0, 1}, {3, 2, 1, 2}, {4, 4, 4, 4}, {0, 1, 0, 0}, {6, 3, 0, 2}, {2, 0, 7, 1}, {12, 4, 1, 3}} {
		h := seedHospital(t, "active")
		seedDecisions(t, h, "PASS", false, c[0], time.Hour)
		seedDecisions(t, h, "REGENERATE", false, c[1], time.Hour)
		seedDecisions(t, h, "ESCALATE", false, c[2], time.Hour)
		// degraded ones, as extra decisions
		seedDecisions(t, h, "ESCALATE", true, c[3], 2*time.Hour)
	}
	for _, period := range []string{"7d", "30d", "90d"} {
		r := guardrails(t, mint(t, platformAdmin(t)), "?period="+period)
		ok(t, period, r)
		if m := rawSmall.FindString(r.raw); m != "" {
			t.Errorf("%s: a small count was sent (%s)", period, m)
		}
		checkRates(t, period+" all", r.body["all"])
		for i, x := range r.body["hospitals"].([]any) {
			checkRates(t, fmt.Sprint(period, " row ", i), x, "hospital")
		}
		var parsed platformGuardrails
		if err := json.Unmarshal([]byte(r.raw), &parsed); err != nil {
			t.Fatalf("%s: %v", period, err)
		}
	}
}

// platformGuardrails is the response as a client would decode it.
type platformGuardrails struct {
	Period    string           `json:"period"`
	All       map[string]any   `json:"all"`
	Hospitals []map[string]any `json:"hospitals"`
}
