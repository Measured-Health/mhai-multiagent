//go:build integration

package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// The aggregate routes on a migrated database, through the handlers with a
// verified principal in the context (the guard's role and two-step checks
// are authz's, pinned by the Rule test). Synthetic hospitals and facts;
// pinned dates (facts in October 2026, frozen at 6 November, the engine's
// clock 12 November), all in the database's past.

var (
	freezeAt = time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)
	clockAt  = time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC)
)

type env struct {
	super, app *pgxpool.Pool
	h          uuid.UUID
	cond, loc  uuid.UUID
	mux        *http.ServeMux
	logs       *bytes.Buffer
	session    string
	user       uuid.UUID
}

func pools(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	pg, super := testutil.MigratedPostgres(t)
	app, err := testutil.AppLogin(context.Background(), pg, super)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return super, app
}

func run(t *testing.T, p *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	if _, err := p.Exec(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

// newEnv is a demo hospital, switched on (DEMO-SYNTHETIC) when on, with
// 60 synthetic subjects in October's first week, frozen.
func newEnv(t *testing.T, super, app *pgxpool.Pool, on bool) *env {
	t.Helper()
	e := &env{super: super, app: app, h: uuid.New(), cond: uuid.New(), loc: uuid.New(), logs: &bytes.Buffer{},
		session: uuid.NewString(), user: uuid.New()}
	run(t, super, `INSERT INTO hospitals (id, name, short_code, status, time_zone, is_demo)
		VALUES ($1, 'Synthetic Demo Hospital', $2, 'active', 'Africa/Lagos', true)`, e.h, "Y"+e.h.String()[:8])
	run(t, super, `INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		VALUES (NULL, 'on', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC') ON CONFLICT DO NOTHING`)
	if on {
		run(t, super, `INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
			VALUES ($1, 'on', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC')`, e.h)
	}
	run(t, super, `INSERT INTO analysis_locations (hospital_id, id, label, setting) VALUES ($1, $2, 'Synthetic Ward', 'admitted')`, e.h, e.loc)
	run(t, super, `INSERT INTO analysis_conditions (hospital_id, id, icd10, name) VALUES ($1, $2, 'I10', 'Synthetic hypertension')`, e.h, e.cond)
	for i := 0; i < 60; i++ {
		s := uuid.New()
		ref := make([]byte, 32)
		copy(ref, s[:])
		sex := []string{"female", "male", "other"}[i%3]
		run(t, super, `INSERT INTO analysis_subjects (hospital_id, subject_id, ref_hmac, created_day) VALUES ($1, $2, $3, '2026-09-01')`, e.h, s, ref)
		for _, d := range []string{"2026-09-03", "2026-10-02"} {
			run(t, super, `INSERT INTO analysis_facts (hospital_id, subject_id, day, kind, condition_id, age_band, sex, setting, location_id, period_late)
				VALUES ($1, $2, $3::date, 'diagnosis', $4, '40-64', $5, 'admitted', $6, false)`, e.h, s, d, e.cond, sex, e.loc)
		}
		run(t, super, `INSERT INTO analysis_first_seen (hospital_id, subject_id, condition_id, first_day, first_late)
			VALUES ($1, $2, $3, '2026-09-03', false)`, e.h, s, e.cond)
	}
	if on {
		ctx := context.Background()
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{`SET LOCAL ROLE mhai_analysis_job`, `SELECT set_config('mhai.analysis.engine', '1', true)`} {
			if _, err := tx.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(ctx, `SELECT mhai_analysis_freeze($1, $2)`, e.h, freezeAt); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	eng := analysis.NewEngine(app, nil)
	eng.Now = func() time.Time { return clockAt }
	eng.Budget = time.Minute
	log := slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng.Log = log
	h := &handlers{d: Deps{Pool: app, Engine: eng, Log: log}, lim: newLimiter(AggregateReads, AggregateWindow, nil)}
	e.mux = http.NewServeMux()
	e.mux.Handle("GET "+httpx.BasePath+"/analysis/filters", http.HandlerFunc(h.filters))
	e.mux.Handle("GET "+httpx.BasePath+"/analysis/{view}", http.HandlerFunc(h.views))
	return e
}

// get serves path as the env's admin (one session).
func (e *env) get(t *testing.T, path string) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, httpx.BasePath+path, nil)
	ctx := authz.WithPrincipal(r.Context(), authz.Principal{UserID: e.user, HospitalID: e.h,
		Roles: []string{authz.RoleHospitalAdmin}, MFA: true, SessionID: e.session})
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r.WithContext(ctx))
	return w.Code, w.Body.Bytes()
}

func code(body []byte) string {
	var p struct{ Code string }
	_ = json.Unmarshal(body, &p)
	return p.Code
}

func TestAnalysisOffIs404(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, false)
	for _, p := range []string{"/analysis/filters", "/analysis/prevalence?period=month&start=2026-10-01",
		"/analysis/prevalence?nonsense=1", "/analysis/unknown", "/analysis/filters?period=year"} {
		if c, b := e.get(t, p); c != http.StatusNotFound || code(b) != "not_found" {
			t.Errorf("%s with analysis off: %d %s", p, c, b)
		}
	}
}

func TestPeriodsAndRanges(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	cases := []struct {
		path string
		want int
		code string
	}{
		{"/analysis/prevalence?from=2026-10-01&to=2026-11-30", 422, "invalid_range"}, // two months
		{"/analysis/prevalence?from=2026-10-05&to=2026-11-04", 422, "invalid_range"}, // a rolling 30 days
		{"/analysis/prevalence?from=2026-10-05&to=2026-10-11", 422, "invalid_range"}, // an ISO week
		{"/analysis/prevalence?period=week&start=2026-10-02", 422, "invalid_range"},  // not a block's first day
		{"/analysis/prevalence?period=month&start=2024-01-01", 422, "invalid_range"}, // older than 24 months
		{"/analysis/prevalence", 422, "invalid_range"},                               // no period
		{"/analysis/prevalence?period=week&start=2026-11-01", 503, "not_ready"},      // ended, not frozen
		{"/analysis/nonsense?period=month&start=2026-10-01", 404, "not_found"},       // unknown view
		{"/analysis/prevalence?period=month&start=2026-10-01&sex=female&age_band=40-64&setting=admitted", 422, "too_many_filters"},
		{"/analysis/prevalence?period=month&start=2026-10-01&sex=nobody", 422, "validation_failed"},
		{"/analysis/prevalence?period=month&start=2026-10-01&extra=1", 422, "validation_failed"},
	}
	for _, c := range cases {
		got, b := e.get(t, c.path)
		if got != c.want || code(b) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.path, got, code(b), c.want, c.code)
		}
	}
	// from/to exactly a frozen month answers the same bytes as the period
	// (apart from nothing: updated_at is the stored body's).
	c1, b1 := e.get(t, "/analysis/prevalence?period=month&start=2026-10-01")
	c2, b2 := e.get(t, "/analysis/prevalence?from=2026-10-01&to=2026-10-31")
	if c1 != 200 || c2 != 200 || !bytes.Equal(b1, b2) {
		t.Errorf("period %d and from/to %d differ:\n%s\n%s", c1, c2, b1, b2)
	}
}

// Review focus 1, T4: each history point under filters is the earlier
// period's own stored body, as published.
func TestHistoryUnderFiltersMatchesStoredBody(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	c, b := e.get(t, "/analysis/prevalence?period=month&start=2026-10-01&sex=female")
	if c != 200 {
		t.Fatalf("%d %s", c, b)
	}
	var v struct {
		History []struct {
			Start                string
			Cases, New, Patients json.RawMessage
		}
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.History) != disclose.HistoryPoints {
		t.Fatalf("%d history points", len(v.History))
	}
	last := v.History[len(v.History)-1]
	if last.Start != "2026-09-01" {
		t.Fatalf("the last point is %s", last.Start)
	}
	c, sb := e.get(t, "/analysis/prevalence?period=month&start=2026-09-01&sex=female")
	if c != 200 {
		t.Fatal(c)
	}
	var sep struct {
		Patients json.RawMessage
		Total    struct{ Cases, New json.RawMessage }
	}
	if err := json.Unmarshal(sb, &sep); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(sep.Patients, last.Patients) || !jsonEqual(sep.Total.Cases, last.Cases) || !jsonEqual(sep.Total.New, last.New) {
		t.Errorf("September's point %s/%s/%s, its own body %s/%s/%s", last.Patients, last.Cases, last.New,
			sep.Patients, sep.Total.Cases, sep.Total.New)
	}
	if strings.Contains(string(b), `"total_over`) {
		t.Error("a total over the history")
	}
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && fmt.Sprint(x) == fmt.Sprint(y)
}

// T10: hospital B's session sees only B: with B off, 404; with B on and
// nothing frozen, nothing of A's.
func TestOtherHospitalGetsNothing(t *testing.T) {
	super, app := pools(t)
	a := newEnv(t, super, app, true)
	if c, _ := a.get(t, "/analysis/prevalence?period=month&start=2026-10-01"); c != 200 {
		t.Fatal(c)
	}
	b := newEnv(t, super, app, false)
	if c, _ := b.get(t, "/analysis/prevalence?period=month&start=2026-10-01"); c != 404 {
		t.Errorf("B off: %d", c)
	}
	run(t, super, `INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		VALUES ($1, 'on', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC')`, b.h)
	c, body := b.get(t, "/analysis/prevalence?period=month&start=2026-10-01")
	if c != 503 || code(body) != "not_ready" {
		t.Errorf("B on, nothing frozen: %d %s", c, body)
	}
	_, fl := b.get(t, "/analysis/filters")
	if strings.Contains(string(fl), a.cond.String()) || strings.Contains(string(fl), a.loc.String()) {
		t.Error("B's filters list A's dictionary")
	}
}

// No route sends a shown count of 1 to 4.
func TestNoShownOneToFourAnywhere(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	for _, v := range disclose.Views {
		for _, q := range []string{"period=week&start=2026-10-01", "period=month&start=2026-10-01&sex=other", "period=quarter&start=2026-07-01"} {
			c, b := e.get(t, "/analysis/"+string(v)+"?"+q)
			if c != 200 {
				t.Fatalf("%s %s: %d %s", v, q, c, b)
			}
			var o any
			if err := json.Unmarshal(b, &o); err != nil {
				t.Fatal(err)
			}
			if p := smallShown(o, ""); p != "" {
				t.Errorf("%s %s shows a small count at %s", v, q, p)
			}
		}
	}
}

func smallShown(o any, at string) string {
	switch x := o.(type) {
	case map[string]any:
		if v, ok := x["value"].(float64); ok && x["suppressed"] == false && v >= 1 && v <= 4 && v == float64(int(v)) &&
			!strings.Contains(at, "share") && !strings.Contains(at, "agreement_rate") && !strings.Contains(at, "median") {
			return at
		}
		for k, v := range x {
			if p := smallShown(v, at+"."+k); p != "" {
				return p
			}
		}
	case []any:
		for i, v := range x {
			if p := smallShown(v, fmt.Sprintf("%s[%d]", at, i)); p != "" {
				return p
			}
		}
	}
	return ""
}

func TestAggregateRateLimit(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	for i := 0; i < AggregateReads; i++ {
		if c, b := e.get(t, "/analysis/filters"); c != 200 {
			t.Fatalf("read %d: %d %s", i+1, c, b)
		}
	}
	if c, b := e.get(t, "/analysis/filters"); c != 429 || code(b) != "rate_limited" {
		t.Errorf("read %d: %d %s", AggregateReads+1, c, b)
	}
}

// analysis.viewed once per session, filter set and period, with codes only.
func TestViewedAuditedOncePerSessionFilterPeriod(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	for _, p := range []string{"prevalence?period=month&start=2026-10-01", "affected?period=month&start=2026-10-01",
		"prevalence?period=month&start=2026-10-01&sex=female", "prevalence?period=month&start=2026-10-01"} {
		if c, b := e.get(t, "/analysis/"+p); c != 200 {
			t.Fatalf("%s: %d %s", p, c, b)
		}
	}
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'analysis.viewed' AND hospital_id = $1`, e.h).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d analysis.viewed entries, want 2 (two filter sets)", n)
	}
	var payload string
	if err := super.QueryRow(context.Background(), `SELECT payload::text FROM audit_log WHERE action = 'analysis.viewed'
		AND hospital_id = $1 AND payload->'filters' ? 'sex'`, e.h).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"female"`) || !strings.Contains(payload, `month:2026-10-01`) {
		t.Errorf("payload %s", payload)
	}
}

// N5: no filter value or period reaches a log line, on success or failure.
func TestAnalysisQueryNotLogged(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	e.get(t, "/analysis/prevalence?period=month&start=2026-10-01&condition="+e.cond.String())
	e.get(t, "/analysis/prevalence?period=month&start=2026-10-01&location="+e.loc.String()+"&sex=other&age_band=80%2B")
	run(t, super, `UPDATE analysis_switch SET position = 'off' WHERE hospital_id = $1`, e.h)
	e.get(t, "/analysis/quality?period=week&start=2026-10-08&location="+e.loc.String())
	for _, s := range []string{e.cond.String(), e.loc.String(), "2026-10-01", "2026-10-08", "80+"} {
		if strings.Contains(e.logs.String(), s) {
			t.Errorf("the logs hold %q", s)
		}
	}
}

// The filters route: periods are the frozen ones within 24 months; without
// a period, the latest frozen month's lists (departure 15).
func TestFiltersRoute(t *testing.T) {
	super, app := pools(t)
	e := newEnv(t, super, app, true)
	c, b := e.get(t, "/analysis/filters")
	if c != 200 {
		t.Fatalf("%d %s", c, b)
	}
	var f struct {
		UpdatedAt  *string             `json:"updated_at"`
		Periods    map[string][]string `json:"periods"`
		Conditions []struct{ ID string }
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	months := f.Periods["month"]
	if len(months) == 0 || months[len(months)-1] != "2026-10-01" || months[0] < "2024-11-12" {
		t.Errorf("months %v", months)
	}
	for _, w := range f.Periods["week"] {
		if w >= "2026-11-01" {
			t.Errorf("an unfrozen week %s offered", w)
		}
	}
	if f.UpdatedAt == nil || len(f.Conditions) != 1 || f.Conditions[0].ID != e.cond.String() {
		t.Errorf("lists %s", b)
	}
	if c, b := e.get(t, "/analysis/filters?period=week&start=2026-10-01"); c != 200 {
		t.Errorf("a named week: %d %s", c, b)
	}
	if c, _ := e.get(t, "/analysis/filters?period=week&start=2026-11-01"); c != 503 {
		t.Errorf("an unfrozen week: %d", c)
	}
}
