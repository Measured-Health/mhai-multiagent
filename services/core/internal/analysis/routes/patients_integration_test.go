//go:build integration

package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
)

// GET /analysis/patients (plan Task 13) on the route env: 60 synthetic
// subjects in October's first block (20 female, 20 male, 20 other, all
// 40-64 and admitted, all with the env's condition), frozen.

func testKeys(t *testing.T, seed byte) *analysis.Keys {
	t.Helper()
	ks := make([][]byte, 4)
	for i := range ks {
		ks[i] = bytes.Repeat([]byte{seed + byte(i)}, analysis.KeyLen)
	}
	k, err := analysis.NewKeys(ks[0], ks[1], ks[2], ks[3])
	if err != nil {
		t.Fatal(err)
	}
	return k
}

const weekQ = "period=week&start=2026-10-01"

// drillFor is the drill of the affected row list[i] whose field is val,
// read from the served view at query q.
func (e *env) drillFor(t *testing.T, q, list, field, val string) string {
	t.Helper()
	c, b := e.get(t, "/analysis/affected?"+q)
	if c != 200 {
		t.Fatalf("affected %s: %d %s", q, c, b)
	}
	var o map[string]any
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	rows, _ := o[list].([]any)
	for _, r := range rows {
		m := r.(map[string]any)
		if m[field] == val {
			d, _ := m["patients"].(map[string]any)["drill"].(string)
			return d
		}
	}
	t.Fatalf("no %s row %s=%s in %s", list, field, val, b)
	return ""
}

func (e *env) list(t *testing.T, q, drill string) (int, []byte) {
	t.Helper()
	return e.get(t, "/analysis/patients?level=deidentified&drill="+url.QueryEscape(drill)+"&"+q)
}

type listReply struct {
	Level    string
	Count    int
	Patients []map[string]any
}

var pidRe = regexp.MustCompile(`^P-[A-Z2-7]{8}$`)

// P1 (drill half), the rows' keys: the count equals the cell, with that
// many rows, each in the group, carrying exactly the contract's keys.
func TestP1DrillCountEqualsCell(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	drill := e.drillFor(t, weekQ, "by_sex", "sex", "female")
	c, b := e.list(t, weekQ, drill)
	if c != 200 {
		t.Fatalf("list: %d %s", c, b)
	}
	var r listReply
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if r.Level != "deidentified" || r.Count != 20 || len(r.Patients) != 20 {
		t.Fatalf("level %q, count %d, %d rows; want 20", r.Level, r.Count, len(r.Patients))
	}
	seen := map[string]bool{}
	for _, row := range r.Patients {
		var keys []string
		for k := range row {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if strings.Join(keys, ",") != "age_band,conditions,findings,pid,setting,sex" {
			t.Fatalf("row keys %v", keys)
		}
		pid, _ := row["pid"].(string)
		if !pidRe.MatchString(pid) || seen[pid] {
			t.Fatalf("pid %q (repeated: %v)", pid, seen[pid])
		}
		seen[pid] = true
		if row["sex"] != "female" || row["age_band"] != "40-64" || row["setting"] != "admitted" {
			t.Errorf("a row outside the group: %v", row)
		}
		conds, _ := row["conditions"].([]any)
		if len(conds) != 1 || conds[0].(map[string]any)["id"] != e.cond.String() || conds[0].(map[string]any)["name"] != "Synthetic hypertension" {
			t.Errorf("conditions %v", conds)
		}
	}
	// The list is recorded and audited, with codes and counts only.
	var payload string
	if err := super.QueryRow(context.Background(), `SELECT payload::text FROM audit_log WHERE action = 'analysis.patients_viewed'
		AND hospital_id = $1 AND actor_id = $2`, e.h, e.user).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"patient_count": 20`) || !strings.Contains(payload, `"rows": 20`) || strings.Contains(payload, "P-") {
		t.Errorf("payload %s", payload)
	}
}

// No drill on prevalence, on a location row, or under a location filter
// (departure 19); none on a hidden count (A8).
func TestNoDrillWhereRowsDontDescribe(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	for _, p := range []string{"/analysis/prevalence?" + weekQ, "/analysis/affected?" + weekQ + "&location=" + e.loc.String()} {
		c, b := e.get(t, p)
		if c != 200 {
			t.Fatalf("%s: %d %s", p, c, b)
		}
		if bytes.Contains(b, []byte(`"drill":"`)) {
			t.Errorf("%s has a drill", p)
		}
	}
	c, b := e.get(t, "/analysis/affected?"+weekQ)
	if c != 200 {
		t.Fatal(c)
	}
	var o map[string]any
	_ = json.Unmarshal(b, &o)
	for _, r := range o["by_location"].([]any) {
		if d := r.(map[string]any)["patients"].(map[string]any)["drill"]; d != nil {
			t.Errorf("a location row has a drill")
		}
	}
}

// T10: another hospital's, another period's, or another filter set's key
// is 422 too_small; so is a key of another key set (N6), or no such body.
func TestT10ForeignDrillIs422(t *testing.T) {
	super, app := pools(t)
	a := newEnv(t, super, app, true)
	b := newEnv(t, super, app, true)
	drill := a.drillFor(t, weekQ, "by_sex", "sex", "female")
	_ = b.drillFor(t, weekQ, "by_sex", "sex", "female") // B's body is stored
	for name, try := range map[string]func() (int, []byte){
		"hospital B": func() (int, []byte) { return b.list(t, weekQ, drill) },
		"another period": func() (int, []byte) {
			a.drillFor(t, "period=month&start=2026-10-01", "by_sex", "sex", "female")
			return a.list(t, "period=month&start=2026-10-01", drill)
		},
		"other filters": func() (int, []byte) {
			a.drillFor(t, weekQ+"&age_band=40-64", "by_sex", "sex", "female")
			return a.list(t, weekQ+"&age_band=40-64", drill)
		},
		"a forged key": func() (int, []byte) { return a.list(t, weekQ, strings.Repeat("A", 43)) },
		"no key":       func() (int, []byte) { return a.get(t, "/analysis/patients?level=deidentified&"+weekQ) },
	} {
		if c, body := try(); c != 422 || code(body) != "too_small" {
			t.Errorf("%s: %d %s", name, c, body)
		}
	}
	if n := countOf(t, a, `SELECT count(*) FROM analysis_views`); n != 0 {
		t.Errorf("%d lists recorded by refused requests", n)
	}
}

// N6: with a new drill key, old keys give 422.
func TestDrillKeyRotation(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	drill := e.drillFor(t, weekQ, "by_sex", "sex", "female")
	e.hd.d.Keys = testKeys(t, 50)
	if c, b := e.list(t, weekQ, drill); c != 422 || code(b) != "too_small" {
		t.Errorf("an old key after rotation: %d %s", c, b)
	}
	if c, b := e.list(t, weekQ, e.drillFor(t, weekQ, "by_sex", "sex", "female")); c != 200 {
		t.Errorf("a new key: %d %s", c, b)
	}
}

func TestPatientsLevelAndShape(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	drill := e.drillFor(t, weekQ, "by_sex", "sex", "female")
	for q, want := range map[string]string{
		"/analysis/patients?level=named&drill=" + drill + "&" + weekQ:                             "level_not_supported",
		"/analysis/patients?drill=" + drill + "&" + weekQ:                                         "level_not_supported",
		"/analysis/patients?level=deidentified&drill=short&" + weekQ:                              "too_small",
		"/analysis/patients?level=deidentified&drill=" + drill + "&" + weekQ + "&x=1":             "validation_failed",
		"/analysis/patients?level=deidentified&drill=" + drill + "&period=month&start=2026-10-02": "invalid_range",
	} {
		if c, b := e.get(t, q); code(b) != want || c != 422 {
			t.Errorf("%s: %d %s, want %s", q, c, b, want)
		}
	}
	run(t, super, `UPDATE analysis_switch SET position = 'off' WHERE hospital_id = $1`, e.h)
	if c, _ := e.list(t, weekQ, drill); c != 404 {
		t.Errorf("analysis off: %d", c)
	}
}

// T13, T14 for this route: a role removed or two-step reset is refused at
// once, whatever the guard's cache still holds.
func TestPatientsRoleRemovedRefusedAtOnce(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	drill := e.drillFor(t, weekQ, "by_sex", "sex", "female")
	run(t, super, `DELETE FROM user_roles WHERE user_id = $1`, e.user)
	run(t, super, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'clinician')`, e.user)
	if c, b := e.list(t, weekQ, drill); c != 403 || code(b) != "forbidden" {
		t.Errorf("role removed: %d %s", c, b)
	}
}

func TestPatientsMFAResetRefused(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	drill := e.drillFor(t, weekQ, "by_sex", "sex", "female")
	run(t, super, `UPDATE user_mfa SET enabled = false WHERE user_id = $1`, e.user)
	if c, b := e.list(t, weekQ, drill); c != 403 || code(b) != "mfa_required" {
		t.Errorf("two-step reset: %d %s", c, b)
	}
	if n := countOf(t, e, `SELECT count(*) FROM analysis_views`); n != 0 {
		t.Errorf("%d lists recorded", n)
	}
}

// T18: the audit append fails: no rows are returned, nothing is kept.
func TestAuditFailRollsBack(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	drill := e.drillFor(t, weekQ, "by_sex", "sex", "female")
	e.hd.afterOpen = func() error { return errors.New("synthetic audit failure") }
	c, b := e.list(t, weekQ, drill)
	if c < 500 || bytes.Contains(b, []byte("P-")) {
		t.Errorf("with the audit failing: %d %s", c, b)
	}
	if n := countOf(t, e, `SELECT (SELECT count(*) FROM analysis_views) + (SELECT count(*) FROM analysis_patient_opens)
		+ (SELECT count(*) FROM analysis_pseudonyms)`); n != 0 {
		t.Errorf("%d rows kept", n)
	}
}

// T8: the 51st list in a hospital day, the 11th in a minute, and the
// 301st distinct patient in a quarter (departure 11) are each 429, with
// one flagged entry however often they are refused.
func TestT8ListAndCapLimits(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	drill := e.drillFor(t, weekQ, "by_sex", "sex", "female")
	var tick atomic.Int64
	e.eng.Now = func() time.Time { return clockAt.Add(time.Duration(tick.Add(1)) * time.Minute) }
	for i := 0; i < ListsPerDay; i++ {
		if c, b := e.list(t, weekQ, drill); c != 200 {
			t.Fatalf("list %d: %d %s", i+1, c, b)
		}
	}
	for i := 0; i < 2; i++ {
		if c, b := e.list(t, weekQ, drill); c != 429 || code(b) != "rate_limited" {
			t.Errorf("list %d: %d %s", ListsPerDay+1+i, c, b)
		}
	}
	if n := countOf(t, e, `SELECT count(*) FROM audit_log WHERE action = 'analysis.list_limit_reached'`); n != 1 {
		t.Errorf("%d flagged entries for the day's limit, want 1", n)
	}

	m := newEnv(t, super, app, true)
	drill = m.drillFor(t, weekQ, "by_sex", "sex", "female")
	for i := 0; i < ListsPerMinute; i++ {
		if c, b := m.list(t, weekQ, drill); c != 200 {
			t.Fatalf("list %d: %d %s", i+1, c, b)
		}
	}
	if c, b := m.list(t, weekQ, drill); c != 429 || code(b) != "rate_limited" {
		t.Errorf("list %d in a minute: %d %s", ListsPerMinute+1, c, b)
	}

	q := newEnv(t, super, app, true)
	drill = q.drillFor(t, weekQ, "by_sex", "sex", "female")
	run(t, super, `INSERT INTO analysis_patient_budget (hospital_id, user_id, quarter, opened) VALUES ($1, $2, '2026-10-01', 290)`, q.h, q.user)
	for i := 0; i < 2; i++ {
		if c, b := q.list(t, weekQ, drill); c != 429 || code(b) != "rate_limited" {
			t.Errorf("past the patient cap: %d %s", c, b)
		}
	}
	if n := countOf(t, q, `SELECT count(*) FROM audit_log WHERE action = 'analysis.patient_cap_reached'`); n != 1 {
		t.Errorf("%d flagged entries for the cap, want 1", n)
	}
	if n := countOf(t, q, `SELECT count(*) FROM analysis_views`); n != 0 {
		t.Errorf("%d lists past the cap", n)
	}
}

// P7: no drill key matches the pid's shape; no aggregate body holds a pid,
// or a name, MRN or birth-date key.
func TestP7Shapes(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	drill := e.drillFor(t, weekQ, "by_sex", "sex", "female")
	if c, _ := e.list(t, weekQ, drill); c != 200 {
		t.Fatal(c)
	}
	pidIn := regexp.MustCompile(`(^|[^A-Za-z0-9])P-[A-Z0-9]{4,12}([^A-Za-z0-9]|$)`)
	for _, v := range []string{"prevalence", "affected", "co-occurring", "findings", "quality"} {
		_, b := e.get(t, "/analysis/"+v+"?"+weekQ)
		if pidIn.Match(b) {
			t.Errorf("%s holds a pid's shape", v)
		}
		for _, k := range []string{`"name":"Synthetic Admin`, `"mrn"`, `"birth`, `"date_of_birth"`, `"display_name"`, `"pid"`} {
			if bytes.Contains(b, []byte(k)) {
				t.Errorf("%s holds %s", v, k)
			}
		}
	}
	if pidIn.MatchString(drill) {
		t.Error("a drill key holds a pid's shape")
	}
}

// P8: one person's lists of two periods share no pid (they can't be
// linked by pid); the measured linkage is reported.
func TestP8LinkageMeasured(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	pids := func(q string) []string {
		c, b := e.list(t, q, e.drillFor(t, q, "by_age", "band", "40-64"))
		if c != 200 {
			t.Fatalf("%s: %d %s", q, c, b)
		}
		var r listReply
		_ = json.Unmarshal(b, &r)
		var out []string
		for _, row := range r.Patients {
			out = append(out, row["pid"].(string))
		}
		return out
	}
	week, month := pids(weekQ), pids("period=month&start=2026-10-01")
	shared := 0
	for _, p := range week {
		if slices.Contains(month, p) {
			shared++
		}
	}
	if shared != 0 || len(week) != 60 || len(month) != 60 {
		t.Errorf("week and month share %d of %d and %d pids", shared, len(week), len(month))
	}
	// Within a period every list shows age band, sex and setting: each
	// listed patient is a three-attribute profile; across periods none
	// links by pid.
	t.Logf("P8: %d patients profiled (3 attributes) within one period; 0 linked across periods by pid", len(week))
}

// N5: the drill, the pids and the filter values never reach a log line.
func TestPatientsQueryNotLogged(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	drill := e.drillFor(t, weekQ, "by_sex", "sex", "female")
	_, b := e.list(t, weekQ+"&condition="+e.cond.String(), drill)
	e.hd.afterOpen = func() error { return errors.New("synthetic") }
	e.list(t, weekQ, drill)
	var r listReply
	_ = json.Unmarshal(b, &r)
	needles := []string{drill, e.cond.String(), "2026-10-01"}
	for _, row := range r.Patients {
		needles = append(needles, row["pid"].(string))
	}
	for _, s := range needles {
		if strings.Contains(e.logs.String(), s) {
			t.Errorf("a log line holds a drill, pid, filter or period")
		}
	}
}

func countOf(t *testing.T, e *env, q string) int {
	t.Helper()
	var n int
	q = strings.Replace(q, "FROM analysis_views", "FROM analysis_views WHERE hospital_id = '"+e.h.String()+"'", -1)
	q = strings.Replace(q, "FROM analysis_patient_opens", "FROM analysis_patient_opens WHERE hospital_id = '"+e.h.String()+"'", -1)
	q = strings.Replace(q, "FROM analysis_pseudonyms", "FROM analysis_pseudonyms WHERE hospital_id = '"+e.h.String()+"'", -1)
	if strings.Contains(q, "FROM audit_log WHERE") {
		q = strings.Replace(q, "FROM audit_log WHERE", "FROM audit_log WHERE hospital_id = '"+e.h.String()+"' AND", 1)
	}
	if err := e.super.QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
