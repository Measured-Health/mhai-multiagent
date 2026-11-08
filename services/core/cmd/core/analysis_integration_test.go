//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// core analysis seed-demo and switch off, as the provisioning login, on
// pinned clocks. Every hospital and fact is synthetic.

func analysisEnv(t *testing.T) (super *pgxpool.Pool, provURL string, app *pgxpool.Pool) {
	t.Helper()
	pg, super := testutil.MigratedPostgres(t)
	_, provURL, err := testutil.ProvisionLogin(context.Background(), pg, super)
	if err != nil {
		t.Fatal(err)
	}
	app, err = testutil.AppLogin(context.Background(), pg, super)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return super, provURL, app
}

// analysisHospital seeds a hospital marked as the synthetic demo.
func analysisHospital(t *testing.T, super *pgxpool.Pool, code string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := super.QueryRow(context.Background(), `INSERT INTO hospitals (name, short_code, status, time_zone, is_demo)
		VALUES ('Synthetic Demo Hospital', $1, 'active', 'Africa/Lagos', true) RETURNING id`, code).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func pinned(at time.Time) func(context.Context) (time.Time, error) {
	return func(context.Context) (time.Time, error) { return at, nil }
}

func scalar(t *testing.T, super *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := super.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// N2: a global switch with a real sign-off is never changed by the CLI;
// nothing is written.
func TestSeedDemoRefusesRealSwitch(t *testing.T) {
	super, provURL, _ := analysisEnv(t)
	h := analysisHospital(t, super, "DEMO1")
	if _, err := super.Exec(context.Background(), `INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		VALUES (NULL, 'off', 'SO-2026-0001', 'SO-2026-0001', 'SO-2026-0001')`); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := analysisCmd(context.Background(), []string{"seed-demo", "demo1", "--months", "3"}, provURL, &out, pinned(time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)))
	if err == nil {
		t.Fatal("seeded under a signed-off global switch")
	}
	if n := scalar(t, super, `SELECT (SELECT count(*) FROM analysis_switch WHERE hospital_id = $1)
		+ (SELECT count(*) FROM analysis_subjects WHERE hospital_id = $1)
		+ (SELECT count(*) FROM analysis_switch WHERE hospital_id IS NULL AND position = 'on')
		+ (SELECT count(*) FROM audit_log WHERE action LIKE 'analysis.%')`, h); n != 0 {
		t.Fatalf("%d rows written", n)
	}
}

// Review item 2: seed-demo refuses a hospital not created as the demo;
// nothing is written, not even the global switch.
func TestSeedDemoRefusesRealHospital(t *testing.T) {
	super, provURL, _ := analysisEnv(t)
	var real uuid.UUID
	if err := super.QueryRow(context.Background(), `INSERT INTO hospitals (name, short_code, status, time_zone)
		VALUES ('Synthetic Hospital', 'REAL1', 'active', 'Africa/Lagos') RETURNING id`).Scan(&real); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := analysisCmd(context.Background(), []string{"seed-demo", "real1", "--months", "2"}, provURL, &out,
		pinned(time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC))); err == nil {
		t.Fatal("seeded a hospital that isn't the demo")
	}
	if n := scalar(t, super, `SELECT (SELECT count(*) FROM analysis_switch)
		+ (SELECT count(*) FROM analysis_subjects WHERE hospital_id = $1)
		+ (SELECT count(*) FROM audit_log WHERE action LIKE 'analysis.%')`, real); n != 0 {
		t.Fatalf("%d rows written", n)
	}
}

// core hospitals create --demo marks the hospital as the demo.
func TestHospitalsCreateDemo(t *testing.T) {
	super, provURL, _ := analysisEnv(t)
	var out bytes.Buffer
	if err := seed(context.Background(), []string{"hospitals", "create", "--name", "Synthetic Demo", "--short-code", "demo9",
		"--time-zone", "Africa/Lagos", "--demo"}, provURL, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, super, `SELECT count(*) FROM hospitals WHERE short_code = 'DEMO9' AND is_demo`); n != 1 {
		t.Fatalf("demo hospitals %d", n)
	}
}

// Seed 24 months, then the nightly run on a pinned clock freezes every
// finished block, month and quarter, each parent with all its children.
func TestSeedDemoThenNightlyFreezes(t *testing.T) {
	super, provURL, app := analysisEnv(t)
	h := analysisHospital(t, super, "DEMO2")
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	if err := analysisCmd(context.Background(), []string{"seed-demo", h.String(), "--seed", "11", "--months", "24"}, provURL, &out, pinned(now)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "analysis demo "+h.String()+" ") {
		t.Fatalf("output %q", out.String())
	}
	if n := scalar(t, super, `SELECT count(*) FROM analysis_switch WHERE position = 'on' AND signoff_clinical = 'DEMO-SYNTHETIC'
		AND (hospital_id IS NULL OR hospital_id = $1)`, h); n != 2 {
		t.Fatalf("%d demo switch rows on, want the global and the hospital's", n)
	}
	if n := scalar(t, super, `SELECT count(*) FROM analysis_subjects WHERE hospital_id = $1 AND ref_enc IS NULL`, h); n < 100 {
		t.Fatalf("%d seeded subjects", n)
	}
	n := analysis.NewNightly(app, nil, nil)
	n.Clock = pinnedClock(now)
	ran, err := n.Tick(context.Background())
	if err != nil || len(ran) != 1 || ran[0] != h {
		t.Fatalf("nightly ran %v (%v)", ran, err)
	}
	// Expected: from the quarter 24 months back, every block whose end + 4
	// days (local midnight) is past now; months and quarters with all their
	// children.
	loc, _ := time.LoadLocation("Africa/Lagos")
	from := calendar.Of(calendar.Quarter, now.In(loc).AddDate(0, -24, 0)).From
	want := map[string]bool{}
	for w := calendar.Of(calendar.Week, from); ; w = calendar.Next(w) {
		end := time.Date(w.To.Year(), w.To.Month(), w.To.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 3)
		if end.After(now) {
			break
		}
		want["week "+w.From.Format(time.DateOnly)] = true
		if m, _ := calendar.Parent(w); m.To.Equal(w.To) {
			want["month "+m.From.Format(time.DateOnly)] = true
			if q, _ := calendar.Parent(m); q.To.Equal(m.To) {
				want["quarter "+q.From.Format(time.DateOnly)] = true
			}
		}
	}
	rows, err := super.Query(context.Background(), `SELECT level || ' ' || start::text FROM analysis_periods WHERE hospital_id = $1`, h)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		got[k] = true
	}
	rows.Close()
	if len(got) != len(want) {
		t.Errorf("%d periods frozen, want %d", len(got), len(want))
	}
	for k := range want {
		if !got[k] {
			t.Errorf("%s not frozen", k)
		}
	}
	// A second seed is refused.
	if err := analysisCmd(context.Background(), []string{"seed-demo", h.String()}, provURL, &out, pinned(now)); err == nil {
		t.Error("a second seed was accepted")
	}
	// The raise is audited with a closed payload.
	var payload string
	if err := super.QueryRow(context.Background(), `SELECT payload::text FROM audit_log WHERE action = 'analysis.switch_raised'
		AND payload ? 'hospital_id'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(payload), &p)
	if p["hospital_id"] != h.String() || p["via"] != "cli" || p["refs"] != "demo" || len(p) != 3 {
		t.Errorf("raise payload %v", p)
	}
}

type pinnedClock time.Time

func (c pinnedClock) Now(context.Context) (time.Time, error) { return time.Time(c), nil }

// Lowering the global switch turns every hospital off at once, and is
// audited with a closed payload.
func TestSwitchOffCLI(t *testing.T) {
	super, provURL, _ := analysisEnv(t)
	h := analysisHospital(t, super, "DEMO3")
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	if err := analysisCmd(context.Background(), []string{"seed-demo", "DEMO3", "--months", "2"}, provURL, &out, pinned(now)); err != nil {
		t.Fatal(err)
	}
	position := func() string {
		var p string
		if err := super.QueryRow(context.Background(), `SELECT mhai_analysis_position($1)`, h).Scan(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if position() != "on" {
		t.Fatal("not on after the seed")
	}
	if err := analysisCmd(context.Background(), []string{"switch", "off", "--global"}, provURL, &out, nil); err != nil {
		t.Fatal(err)
	}
	if position() != "off" {
		t.Fatal("still on after the global was lowered")
	}
	if err := analysisCmd(context.Background(), []string{"switch", "off", "demo3"}, provURL, &out, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := super.Query(context.Background(), `SELECT payload::text, hospital_id IS NULL, actor_id IS NULL FROM audit_log
		WHERE action = 'analysis.switch_lowered' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var payloads []map[string]any
	for rows.Next() {
		var raw string
		var noHospital, noActor bool
		if err := rows.Scan(&raw, &noHospital, &noActor); err != nil {
			t.Fatal(err)
		}
		if !noHospital || !noActor {
			t.Errorf("a lowering row has a hospital or an actor")
		}
		var p map[string]any
		_ = json.Unmarshal([]byte(raw), &p)
		payloads = append(payloads, p)
	}
	if len(payloads) != 2 || payloads[0]["via"] != "cli" || len(payloads[0]) != 1 ||
		payloads[1]["hospital_id"] != h.String() || len(payloads[1]) != 2 {
		t.Errorf("lowering payloads %v", payloads)
	}
	// The demo can be raised again by the CLI (it is still demo-only).
	if n := scalar(t, super, `SELECT count(*) FROM analysis_switch WHERE position = 'on'`); n != 0 {
		t.Errorf("%d rows still on", n)
	}
}
