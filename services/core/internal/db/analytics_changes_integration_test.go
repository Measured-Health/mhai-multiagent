//go:build integration

package db_test

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00022 (Plan 7c Tasks 8 and 9): What changed and follow-ups.
// Every id, name and value is synthetic; the periods are fixed.

// shownJSON is a shown value as stored.
const shownJSON = `{"value": 20, "suppressed": false}`

var quotedAction = regexp.MustCompile(`'([a-z][a-z0-9_.]*)'::text`)

// appendPolicy is a policy's WITH CHECK on audit_log, and the actions it
// lists, sorted.
func appendPolicy(t *testing.T, pool *pgxpool.Pool, name string) (string, []string) {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT with_check FROM pg_policies WHERE tablename = 'audit_log' AND policyname = $1`,
		name).Scan(&s); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var out []string
	for _, m := range quotedAction.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return s, out
}

// TestMigration00022DownUp: Up rebuilds platform_append from main's
// (00021's) list with the three follow-up actions added, conditions kept,
// and touches no other append policy; Down restores main's policy exactly
// and drops the three tables; Up again works.
func TestMigration00022DownUp(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.MigrateUpTo(ctx, pg.URL, 21); err != nil {
		t.Fatalf("up to 21: %v", err)
	}
	pool, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	main, mainActions := appendPolicy(t, pool, "platform_append")
	provision, _ := appendPolicy(t, pool, "provision_append")
	if len(mainActions) != 15 || !strings.Contains(main, "hospital_id IS NULL") {
		t.Fatalf("main's platform_append isn't 00020's: %s", main)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
	up, upActions := appendPolicy(t, pool, "platform_append")
	want := append(append([]string{}, mainActions...), "analytics.follow_up_edited", "analytics.follow_up_undone", "analytics.followed_up")
	sort.Strings(want)
	if strings.Join(upActions, ",") != strings.Join(want, ",") {
		t.Errorf("after up, the actions are\n %v\nwant main's plus the three:\n %v", upActions, want)
	}
	// The conditions are main's: only the action list differs.
	strip := func(s string) string { return regexp.MustCompile(`ARRAY\[[^\]]*\]`).ReplaceAllString(s, "ARRAY[]") }
	if strip(up) != strip(main) {
		t.Errorf("after up, the conditions changed:\n up:   %s\n main: %s", strip(up), strip(main))
	}
	if p, _ := appendPolicy(t, pool, "provision_append"); p != provision {
		t.Errorf("up changed provision_append: %s", p)
	}
	var tables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname IN ('analytics_change_runs', 'analytics_changes', 'analytics_follow_ups')
		AND relowner = 'mhai_migrate'::regrole AND relrowsecurity AND relforcerowsecurity`).Scan(&tables); err != nil || tables != 3 {
		t.Errorf("after up, %d tables owned by mhai_migrate with RLS forced: %v", tables, err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 21); err != nil {
		t.Fatalf("down to 21: %v", err)
	}
	if down, _ := appendPolicy(t, pool, "platform_append"); down != main {
		t.Errorf("after down, platform_append is not main's:\n down: %s\n main: %s", down, main)
	}
	if p, _ := appendPolicy(t, pool, "provision_append"); p != provision {
		t.Errorf("down changed provision_append: %s", p)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_class WHERE relname IN ('analytics_change_runs', 'analytics_changes', 'analytics_follow_ups', 'analytics_changes_open'))
		+ (SELECT count(*) FROM pg_policies WHERE tablename IN ('analytics_change_runs', 'analytics_changes', 'analytics_follow_ups'))`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("after down, %d objects of 00022 remain", left)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

func asPlatform(ctx context.Context, pool *pgxpool.Pool, q string, args ...any) (int64, error) {
	var n int64
	err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, q, args...)
		n = tag.RowsAffected()
		return err
	})
	return n, err
}

func asJobExec(ctx context.Context, pool *pgxpool.Pool, q string, args ...any) (int64, error) {
	var n int64
	err := asJob(ctx, pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, q, args...)
		n = tag.RowsAffected()
		return err
	})
	return n, err
}

// insertChange inserts an open change as the scheduler's role.
func insertChange(t *testing.T, pool *pgxpool.Pool, h any, measure, dir string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	area := "use"
	if measure == "needs_review_share" || measure == "agreement_rate" {
		area = "quality"
	}
	err := asJob(context.Background(), pool, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `INSERT INTO analytics_changes (hospital_id, area, measure, direction, current, previous,
			summary, opened_week, last_week) VALUES ($1, $2, $3, $4, $5::jsonb, $5::jsonb, 'Synthetic summary', '2024-02-01', '2024-02-01')
			RETURNING id`, h, area, measure, dir, shownJSON).Scan(&id)
	})
	if err != nil {
		t.Fatalf("insert change: %v", err)
	}
	return id
}

// The grants and policies of 00022 (§8.1): who may write what, the one
// open change per key, the shown values only, the note's backstop, and the
// run's policy.
func TestAnalyticsChangesGrants(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("CH"))
	admin := seedUser(t, pool, nil)
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM analytics_follow_ups WHERE change_id IN (SELECT id FROM analytics_changes WHERE opened_week = '2024-02-01')`,
			`DELETE FROM analytics_changes WHERE opened_week = '2024-02-01'`,
			`DELETE FROM analytics_change_runs WHERE week_from >= '2024-02-01' AND week_from < '2024-03-01' OR week_from >= '2090-01-01'`,
			`DELETE FROM analytics_snapshots WHERE period_from >= '2024-01-01' AND period_from < '2024-03-01' OR period_from >= '2090-01-01'`} {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Error(err)
			}
		}
	})

	id := insertChange(t, pool, h, "questions", "rising")
	// One open change per key, a hospital's and the all-hospitals one.
	err := asJob(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO analytics_changes (hospital_id, area, measure, direction, current, previous, summary,
			opened_week, last_week) VALUES ($1, 'use', 'questions', 'rising', $2::jsonb, $2::jsonb, 'x', '2024-02-08', '2024-02-08')`, h, shownJSON)
		return err
	})
	if pgCode(err) != uniqueViolation {
		t.Errorf("a second open change for the key: %v", err)
	}
	insertChange(t, pool, nil, "questions", "falling")
	err = asJob(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO analytics_changes (area, measure, direction, current, previous, summary,
			opened_week, last_week) VALUES ('use', 'questions', 'falling', $1::jsonb, $1::jsonb, 'x', '2024-02-08', '2024-02-08')`, shownJSON)
		return err
	})
	if pgCode(err) != uniqueViolation {
		t.Errorf("a second open all-hospitals change: %v", err)
	}
	// Values are shown values only; serious stays false; the vocabularies.
	for name, cols := range map[string][2]string{
		"hidden current":       {`{"value": null, "suppressed": true}`, shownJSON},
		"a raw key":            {`{"value": 20, "suppressed": false, "raw": 3}`, shownJSON},
		"a negative previous":  {shownJSON, `{"value": -1, "suppressed": false}`},
		"a string value":       {`{"value": "20", "suppressed": false}`, shownJSON},
		"suppressed not false": {`{"value": 20, "suppressed": null}`, shownJSON},
	} {
		_, err := asJobExec(ctx, pool, `INSERT INTO analytics_changes (area, measure, direction, current, previous, summary,
			opened_week, last_week) VALUES ('quality', 'needs_review_share', 'worse', $1::jsonb, $2::jsonb, 'x', '2024-02-01', '2024-02-01')`, cols[0], cols[1])
		if pgCode(err) != checkViolation {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, q := range map[string]string{
		"serious":                `INSERT INTO analytics_changes (area, measure, direction, serious, current, previous, summary, opened_week, last_week) VALUES ('quality', 'needs_review_share', 'worse', true, '` + shownJSON + `', '` + shownJSON + `', 'x', '2024-02-01', '2024-02-01')`,
		"a quality direction":    `INSERT INTO analytics_changes (area, measure, direction, current, previous, summary, opened_week, last_week) VALUES ('use', 'questions', 'worse', '` + shownJSON + `', '` + shownJSON + `', 'x', '2024-02-01', '2024-02-01')`,
		"median review time":     `INSERT INTO analytics_changes (area, measure, direction, current, previous, summary, opened_week, last_week) VALUES ('quality', 'median_review_hours', 'worse', '` + shownJSON + `', '` + shownJSON + `', 'x', '2024-02-01', '2024-02-01')`,
		"active clinicians (C9)": `INSERT INTO analytics_changes (area, measure, direction, current, previous, summary, opened_week, last_week) VALUES ('use', 'active_clinicians', 'rising', '` + shownJSON + `', '` + shownJSON + `', 'x', '2024-02-01', '2024-02-01')`,
		"per clinician (C9)":     `INSERT INTO analytics_changes (area, measure, direction, current, previous, summary, opened_week, last_week) VALUES ('use', 'questions_per_clinician', 'rising', '` + shownJSON + `', '` + shownJSON + `', 'x', '2024-02-01', '2024-02-01')`,
		"a summary over 200":     `INSERT INTO analytics_changes (area, measure, direction, current, previous, summary, opened_week, last_week) VALUES ('quality', 'needs_review_share', 'worse', '` + shownJSON + `', '` + shownJSON + `', repeat('x', 201), '2024-02-01', '2024-02-01')`,
	} {
		if _, err := asJobExec(ctx, pool, q); pgCode(err) != checkViolation && pgCode(err) != permissionDenied {
			t.Errorf("%s: %v", name, err)
		}
	}

	// The routes' role: reads, and moves an open change's version only.
	if _, err := asPlatform(ctx, pool, `INSERT INTO analytics_changes (area, measure, direction, current, previous, summary, opened_week, last_week)
		VALUES ('quality', 'needs_review_share', 'worse', '`+shownJSON+`', '`+shownJSON+`', 'x', '2024-02-01', '2024-02-01')`); pgCode(err) != permissionDenied {
		t.Errorf("platform inserts a change: %v", err)
	}
	if _, err := asPlatform(ctx, pool, `UPDATE analytics_changes SET current = '{"value": 1, "suppressed": false}' WHERE id = $1`, id); pgCode(err) != permissionDenied {
		t.Errorf("platform rewrites a value: %v", err)
	}
	if _, err := asPlatform(ctx, pool, `DELETE FROM analytics_changes WHERE id = $1`, id); pgCode(err) != permissionDenied {
		t.Errorf("platform deletes a change: %v", err)
	}
	if n, err := asPlatform(ctx, pool, `UPDATE analytics_changes SET version = version + 1 WHERE id = $1`, id); err != nil || n != 1 {
		t.Errorf("platform moves an open change's version: %d %v", n, err)
	}
	// The scheduler's role: updates and closes, never the version, never a
	// follow-up, never reopens.
	if _, err := asJobExec(ctx, pool, `UPDATE analytics_changes SET version = 9 WHERE id = $1`, id); pgCode(err) != permissionDenied {
		t.Errorf("the scheduler moves a version: %v", err)
	}
	if n, err := asJobExec(ctx, pool, `UPDATE analytics_changes SET closed_week = '2024-02-08' WHERE id = $1`, id); err != nil || n != 1 {
		t.Fatalf("the scheduler closes a change: %d %v", n, err)
	}
	if n, err := asJobExec(ctx, pool, `UPDATE analytics_changes SET closed_week = NULL WHERE id = $1`, id); err != nil || n != 0 {
		t.Errorf("the scheduler reopened a closed change: %d %v", n, err)
	}
	if n, err := asPlatform(ctx, pool, `UPDATE analytics_changes SET version = version + 1 WHERE id = $1`, id); err != nil || n != 0 {
		t.Errorf("platform wrote a closed change: %d %v", n, err)
	}
	for _, q := range []string{`SELECT count(*) FROM analytics_follow_ups`, `INSERT INTO analytics_follow_ups (change_id, by_user) VALUES ('` + id.String() + `', '` + admin.String() + `')`} {
		if _, err := asJobExec(ctx, pool, q); pgCode(err) != permissionDenied {
			t.Errorf("the scheduler: %s: %v", q, err)
		}
	}

	// The note's backstop (core checks first): length and separators.
	open := insertChange(t, pool, h, "agreement_rate", "better")
	for name, note := range map[string]string{
		"U+2028":     "Synthetic\u2028note",
		"U+2029":     "Synthetic\u2029note",
		"a bidi":     "Synthetic \u202enote",
		"an isolate": "Synthetic \u2066note",
		"a ZWSP":     "Synthetic\u200bnote",
		"501":        strings.Repeat("n", 501),
		"empty":      "",
	} {
		if _, err := asPlatform(ctx, pool, `INSERT INTO analytics_follow_ups (change_id, by_user, note) VALUES ($1, $2, $3)`, open, admin, note); pgCode(err) != checkViolation {
			t.Errorf("note %s: %v", name, err)
		}
	}
	if _, err := asPlatform(ctx, pool, `INSERT INTO analytics_follow_ups (change_id, by_user, note) VALUES ($1, $2, $3)`, open, admin,
		"Synthetic line one\nline two"); err != nil {
		t.Errorf("a newline refused: %v", err)
	}

	// The run: a finished week with a stored snapshot, by the scheduler
	// only, compared with the previous calendar month.
	for _, q := range []string{
		`INSERT INTO analytics_snapshots (kind, period_from, period_to, state) VALUES ('week', '2024-02-08', '2024-02-15', 'unavailable')`,
		`INSERT INTO analytics_snapshots (kind, period_from, period_to, state) VALUES ('week', '2090-02-08', '2090-02-15', 'unavailable')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	run := `INSERT INTO analytics_change_runs (week_from, week_to, month_from, month_to) VALUES ($1::date, $2::date, $3::date, $4::date)`
	if _, err := asPlatform(ctx, pool, run, "2024-02-08", "2024-02-15", "2024-01-01", "2024-02-01"); pgCode(err) != permissionDenied {
		t.Errorf("platform writes a run: %v", err)
	}
	if _, err := asJobExec(ctx, pool, run, "2024-02-15", "2024-02-22", "2024-01-01", "2024-02-01"); pgCode(err) != permissionDenied {
		t.Errorf("a run for a week with no snapshot: %v", err)
	}
	if _, err := asJobExec(ctx, pool, run, "2090-02-08", "2090-02-15", "2090-01-01", "2090-02-01"); pgCode(err) != permissionDenied {
		t.Errorf("a run for an unfinished week: %v", err)
	}
	if _, err := asJobExec(ctx, pool, run, "2024-02-08", "2024-02-15", "2023-12-01", "2024-01-01"); pgCode(err) != checkViolation {
		t.Errorf("a run against another month: %v", err)
	}
	// No snapshot can exist for it, so the policy refuses it before the
	// CHECK would.
	if _, err := asJobExec(ctx, pool, run, "2024-02-09", "2024-02-16", "2024-01-01", "2024-02-01"); pgCode(err) != checkViolation &&
		pgCode(err) != permissionDenied {
		t.Errorf("a run on a rolling week: %v", err)
	}
	if n, err := asJobExec(ctx, pool, run, "2024-02-08", "2024-02-15", "2024-01-01", "2024-02-01"); err != nil || n != 1 {
		t.Errorf("the run: %d %v", n, err)
	}
	if _, err := asJobExec(ctx, pool, `DELETE FROM analytics_change_runs`); pgCode(err) != permissionDenied {
		t.Errorf("the scheduler deletes a run: %v", err)
	}
}
