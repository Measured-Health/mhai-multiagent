//go:build integration

package db_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/letters"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00038 (analysis backend plan Task 18): clearance letters, the
// people they cover, their events and the named-access switch. The rules
// are triggers, for every writer; the superuser seeds fixtures (exempt, as
// it can disable any trigger) and every rule is exercised as mhai_gateway
// or mhai_platform. Synthetic hospitals, accounts and letters only.

// letterWorld is a hospital with analysis on and named access at
// letters_only, an admin, an analyst, a clinician, two eligible platform
// admins (another domain), and a second hospital with its own admin.
type letterWorld struct {
	h, admin, analyst, clin uuid.UUID
	pa, pa2                 uuid.UUID
	h2, admin2              uuid.UUID
}

// platformUser is a synthetic platform account (no hospital) with role
// and two-step as given; domain picks its email's domain.
func platformUser(t *testing.T, pool *pgxpool.Pool, role, domain string, mfa bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO users (hospital_id, email, password_hash, display_name)
		VALUES (NULL, $1, 'synthetic-hash', 'Synthetic Platform Admin') RETURNING id`, uniqueCode("p")+"@"+domain).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`} {
		if _, err := pool.Exec(ctx, q, id, role); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_mfa (user_id, secret_enc, enabled) VALUES ($1, '\x00', $2)`, id, mfa); err != nil {
		t.Fatal(err)
	}
	return id
}

// setNamed writes a named-access row as the superuser (nil: global).
func setNamed(t *testing.T, pool *pgxpool.Pool, h *uuid.UUID, position string) {
	t.Helper()
	ref := any(nil)
	if position != "off" {
		ref = "SO-2026-0003"
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO named_access_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		VALUES ($1, $2, $3, $3, $3) ON CONFLICT ON CONSTRAINT named_access_switch_one
		DO UPDATE SET position = EXCLUDED.position, signoff_clinical = EXCLUDED.signoff_clinical,
			signoff_privacy = EXCLUDED.signoff_privacy, signoff_legal = EXCLUDED.signoff_legal`, h, position, ref); err != nil {
		t.Fatal(err)
	}
}

func newLetterWorld(t *testing.T, pool *pgxpool.Pool) letterWorld {
	t.Helper()
	w := letterWorld{h: onHospital(t, pool, "Africa/Lagos"), h2: onHospital(t, pool, "Africa/Lagos")}
	setNamed(t, pool, nil, "on")
	setNamed(t, pool, &w.h, "letters_only")
	setNamed(t, pool, &w.h2, "letters_only")
	w.admin = staffer(t, pool, w.h, staffOpts{roles: []string{"hospital_admin"}})
	w.analyst = staffer(t, pool, w.h, staffOpts{roles: []string{"clinician", "analyst"}})
	w.clin = staffer(t, pool, w.h, staffOpts{roles: []string{"clinician"}})
	w.admin2 = staffer(t, pool, w.h2, staffOpts{roles: []string{"hospital_admin"}})
	w.pa = platformUser(t, pool, "platform_admin", "measured-health.example", true)
	w.pa2 = platformUser(t, pool, "platform_admin", "measured-health.example", true)
	return w
}

func hospitalToday(t *testing.T, pool *pgxpool.Pool, h uuid.UUID) time.Time {
	t.Helper()
	var d time.Time
	if err := pool.QueryRow(context.Background(), `SELECT (now() AT TIME ZONE time_zone)::date FROM hospitals WHERE id = $1`, h).Scan(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

// createLetter uploads a letter as the admin, with its entries, and
// returns its id.
func createLetter(t *testing.T, pool *pgxpool.Pool, w letterWorld, staff, platform []uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	today := hospitalToday(t, pool, w.h)
	var id uuid.UUID
	err := db.WithHospitalUser(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT mhai_clearance_create('Synthetic Signer', 'Synthetic Title', $1, $2, NULL, $3, $4)`,
			today, today.AddDate(0, 6, 0), staff, platform).Scan(&id); err != nil {
			return err
		}
		_, err := letters.AuditPending(ctx, tx, "req-letter")
		return err
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return id
}

// seedLetter inserts a letter in status as the superuser (exempt), with
// the columns that status needs; past ends it yesterday at the hospital.
func seedLetter(t *testing.T, pool *pgxpool.Pool, h, by uuid.UUID, status string, past bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	today := hospitalToday(t, pool, h)
	end := today.AddDate(0, 1, 0)
	if past {
		end = today.AddDate(0, 0, -1)
	}
	cols := map[string]string{
		"checking": "",
		"refused":  ", refused_reason = 'unreadable'",
		"waiting":  ", pages = 1, sha256 = sha256('x')",
		"active":   ", pages = 1, sha256 = sha256('x'), verified_by = $3, verified_at = now()",
		"rejected": ", pages = 1, sha256 = sha256('x'), rejected_at = now(), reject_reason = 'other'",
		"ended":    ", pages = 1, sha256 = sha256('x'), verified_by = $3, verified_at = now(), ended_at = now()",
		"revoked":  ", pages = 1, sha256 = sha256('x'), revoked_at = now(), revoke_reason = 'other', revoked_side = 'hospital'",
	}[status]
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO clearances (id, hospital_id, signed_by, signer_title, letter_date, end_date, uploaded_by)
		VALUES ($1, $2, 'Synthetic Signer', 'Synthetic Title', $4::date - 60, $4, $3)`, id, h, by, end); err != nil {
		t.Fatal(err)
	}
	args := []any{id}
	if strings.Contains(cols, "$3") {
		args = append(args, by)
	}
	if _, err := tx.Exec(ctx, `UPDATE clearances SET status = '`+status+`'`+strings.ReplaceAll(cols, "$3", "$2")+` WHERE id = $1`, args...); err != nil {
		t.Fatalf("seed %s: %v", status, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return id
}

// asActor runs fn as role (mhai_gateway or mhai_platform) with hospital h
// (uuid.Nil: none) and user u, with a test-only grant and policy that let
// the role UPDATE clearances and clearance_covered directly, so the
// triggers alone decide (T15 "even in SQL"). Always rolled back.
func asActor(ctx context.Context, pool *pgxpool.Pool, role string, h, u uuid.UUID, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{
		`GRANT SELECT, UPDATE, DELETE ON clearances, clearance_covered TO ` + role,
		`CREATE POLICY t15_test ON clearances TO ` + role + ` USING (true) WITH CHECK (true)`,
		`CREATE POLICY t15_test ON clearance_covered TO ` + role + ` USING (true) WITH CHECK (true)`,
		`SET LOCAL ROLE ` + role,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}
	hs := ""
	if h != uuid.Nil {
		hs = h.String()
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true), set_config('mhai.user_id', $2, true)`, hs, u.String()); err != nil {
		return err
	}
	return fn(tx)
}

// T15: verification by a hospital user, a covered platform admin, a
// platform admin on the hospital's side, or an auditor is refused by the
// trigger, even by a direct UPDATE; an eligible platform admin passes.
func TestT15TriggerRefusesWrongVerifier(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newLetterWorld(t, pool)
	letter := seedLetter(t, pool, w.h, w.admin, "waiting", false)
	if _, err := pool.Exec(ctx, `INSERT INTO clearance_covered (hospital_id, clearance_id, user_id, side) VALUES ($1, $2, $3, 'platform')`,
		w.h, letter, w.pa); err != nil {
		t.Fatal(err)
	}
	// The hospital's accounts use example.test: a platform admin there is on its side.
	linked := platformUser(t, pool, "platform_admin", "example.test", true)
	auditor := platformUser(t, pool, "auditor", "measured-health.example", true)
	noMFA := platformUser(t, pool, "platform_admin", "measured-health.example", false)
	for _, set := range []string{`status = 'active'`, `status = 'rejected', reject_reason = 'other'`,
		`status = 'revoked', revoke_reason = 'other'`} {
		for _, c := range []struct {
			name, role string
			h, u       uuid.UUID
			want       string
		}{
			{"a hospital admin", "mhai_gateway", w.h, w.admin, "forbidden"},
			{"a covered platform admin", "mhai_platform", uuid.Nil, w.pa, "same_person"},
			{"a platform admin on the hospital's side", "mhai_platform", uuid.Nil, linked, "same_side"},
			{"an auditor", "mhai_platform", uuid.Nil, auditor, "forbidden"},
			{"a platform admin without two-step", "mhai_platform", uuid.Nil, noMFA, "forbidden"},
			{"an eligible platform admin", "mhai_platform", uuid.Nil, w.pa2, ""},
		} {
			if c.role == "mhai_gateway" && strings.HasPrefix(set, "status = 'revoked'") {
				continue // the hospital side may revoke (TestClearanceTransitionsTable)
			}
			err := asActor(ctx, pool, c.role, c.h, c.u, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE clearances SET `+set+` WHERE id = $1`, letter)
				return err
			})
			got := ""
			if err != nil {
				got = refusal(err)
			}
			want := c.want
			if strings.HasPrefix(set, "status = 'revoked'") && strings.HasPrefix(want, "same_") {
				want = "" // revoking only takes access away: any eligible platform admin
			}
			if got != want {
				t.Errorf("%s by %s: want %q, got %q", set, c.name, want, got)
			}
		}
	}
	// Without the test grant, neither role may write a letter at all.
	for _, role := range []string{"mhai_gateway", "mhai_platform"} {
		requireDenied(t, asRole(ctx, pool, role, `UPDATE clearances SET status = 'active' WHERE false`), role+" updating clearances")
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM clearances WHERE id = $1`, letter).Scan(&status); err != nil || status != "waiting" {
		t.Errorf("the letter is %s (%v), want waiting", status, err)
	}
}

// Every pair of statuses: only the design's transitions pass, each only
// for its actor; everything else is refused.
func TestClearanceTransitionsTable(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newLetterWorld(t, pool)
	statuses := []string{"checking", "refused", "waiting", "active", "rejected", "ended", "revoked"}
	set := map[string]string{
		"checking": "",
		"refused":  ", refused_reason = 'unreadable'",
		"waiting":  ", pages = 1, sha256 = sha256('y')",
		"active":   "",
		"rejected": ", reject_reason = 'other'",
		"ended":    "",
		"revoked":  ", revoke_reason = 'other'",
	}
	allowed := map[string]bool{
		"mhai_gateway checking>waiting": true, "mhai_gateway checking>refused": true,
		"mhai_gateway waiting>revoked": true, "mhai_gateway active>revoked": true, "mhai_gateway active>ended": true,
		"mhai_platform waiting>active": true, "mhai_platform waiting>rejected": true,
		"mhai_platform waiting>revoked": true, "mhai_platform active>revoked": true, "mhai_platform active>ended": true,
	}
	for _, from := range statuses {
		for _, to := range statuses {
			if from == to {
				continue
			}
			letter := seedLetter(t, pool, w.h, w.admin, from, from == "active" && to == "ended")
			for _, actor := range []struct {
				role string
				h, u uuid.UUID
			}{{"mhai_gateway", w.h, w.admin}, {"mhai_platform", uuid.Nil, w.pa2}} {
				err := asActor(ctx, pool, actor.role, actor.h, actor.u, func(tx pgx.Tx) error {
					// The documents outcome's mark, as its function sets it.
					if _, err := tx.Exec(ctx, `SELECT set_config('mhai.clearance_outcome', $1, true)`, letter.String()); err != nil {
						return err
					}
					_, err := tx.Exec(ctx, `UPDATE clearances SET status = '`+to+`'`+set[to]+` WHERE id = $1`, letter)
					return err
				})
				k := actor.role + " " + from + ">" + to
				if allowed[k] {
					if err != nil {
						t.Errorf("%s: refused: %v", k, err)
					}
					continue
				}
				if r := refusal(err); r != "conflict" && r != "forbidden" {
					t.Errorf("%s: want conflict or forbidden, got %s", k, r)
				}
			}
		}
	}
	// An active letter whose last day is today doesn't end.
	letter := seedLetter(t, pool, w.h, w.admin, "active", false)
	err := asActor(ctx, pool, "mhai_gateway", w.h, w.admin, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE clearances SET status = 'ended' WHERE id = $1`, letter)
		return err
	})
	if refusal(err) != "conflict" {
		t.Errorf("ending a current letter: %v", err)
	}
	// The letter's facts never change; nothing is deleted.
	for _, q := range []string{`UPDATE clearances SET end_date = end_date + 1 WHERE id = $1`,
		`UPDATE clearances SET signed_by = 'Someone Else' WHERE id = $1`,
		`UPDATE clearances SET hospital_id = '` + w.h2.String() + `' WHERE id = $1`,
		`DELETE FROM clearances WHERE id = $1`} {
		err := asActor(ctx, pool, "mhai_gateway", w.h, w.admin, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, letter)
			return err
		})
		if refusal(err) != "conflict" && pgCode(err) != permissionDenied {
			t.Errorf("%s: want refused, got %v", q, err)
		}
	}
}

// The insert rules, through the gateway's function: the gates, the
// uploader, the dates, the people, renews, and demo set by the trigger.
func TestClearanceInsertRules(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newLetterWorld(t, pool)
	today := hospitalToday(t, pool, w.h)
	other := seedLetter(t, pool, w.h2, w.admin2, "active", false)
	foreign := staffer(t, pool, w.h2, staffOpts{roles: []string{"hospital_admin"}})
	noMFA := platformUser(t, pool, "platform_admin", "measured-health.example", false)
	var many []uuid.UUID
	for range 21 {
		many = append(many, staffer(t, pool, w.h, staffOpts{roles: []string{"hospital_admin"}}))
	}
	create := func(h, by uuid.UUID, letterDate, end time.Time, renews *uuid.UUID, staff, platform []uuid.UUID) error {
		return asPerson(ctx, pool, h, by, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT mhai_clearance_create('Synthetic Signer', 'Synthetic Title', $1, $2, $3, $4, $5)`,
				letterDate, end, renews, staff, platform)
			return err
		})
	}
	ok := today.AddDate(0, 6, 0)
	for _, c := range []struct {
		name            string
		by              uuid.UUID
		letterDate, end time.Time
		renews          *uuid.UUID
		staff, platform []uuid.UUID
		want            string
	}{
		{"a clinician uploading", w.clin, today, ok, nil, []uuid.UUID{w.admin}, nil, "forbidden"},
		{"an end date of today", w.admin, today, today, nil, []uuid.UUID{w.admin}, nil, "end_date_too_late"},
		{"an end date past 12 months", w.admin, today, today.AddDate(0, 12, 1), nil, []uuid.UUID{w.admin}, nil, "end_date_too_late"},
		{"a letter dated tomorrow", w.admin, today.AddDate(0, 0, 1), ok, nil, []uuid.UUID{w.admin}, nil, "invalid"},
		{"a stale letter", w.admin, today.AddDate(-1, 0, -1), ok, nil, []uuid.UUID{w.admin}, nil, "invalid"},
		{"another hospital's letter renewed", w.admin, today, ok, &other, []uuid.UUID{w.admin}, nil, "invalid"},
		{"a clinician covered", w.admin, today, ok, nil, []uuid.UUID{w.clin}, nil, "not_eligible"},
		{"another hospital's admin covered", w.admin, today, ok, nil, []uuid.UUID{foreign}, nil, "not_eligible"},
		{"a platform admin without two-step", w.admin, today, ok, nil, nil, []uuid.UUID{noMFA}, "not_eligible_platform"},
		{"a hospital account as platform", w.admin, today, ok, nil, nil, []uuid.UUID{w.analyst}, "not_eligible_platform"},
		{"nobody", w.admin, today, ok, nil, nil, nil, "invalid"},
		{"21 people", w.admin, today, ok, nil, many, nil, "invalid"},
	} {
		if got := refusal(create(w.h, c.by, c.letterDate, c.end, c.renews, c.staff, c.platform)); got != c.want {
			t.Errorf("%s: want %s, got %s", c.name, c.want, got)
		}
	}
	// The gates.
	setNamed(t, pool, &w.h, "off")
	if got := refusal(create(w.h, w.admin, today, ok, nil, []uuid.UUID{w.admin}, nil)); got != "named_access_off" {
		t.Errorf("named access off: %s", got)
	}
	setNamed(t, pool, &w.h, "letters_only")
	setSwitch(t, pool, &w.h, "off", "")
	if got := refusal(create(w.h, w.admin, today, ok, nil, []uuid.UUID{w.admin}, nil)); got != "not_found" {
		t.Errorf("analysis off: %s", got)
	}
	setSwitch(t, pool, &w.h, "on", "SO-2026-0002")
	// No direct insert.
	requireDenied(t, asRole(ctx, pool, "mhai_gateway", `INSERT INTO clearances (hospital_id, signed_by, signer_title, letter_date, end_date, uploaded_by)
		VALUES (gen_random_uuid(), 'a', 'b', current_date, current_date + 1, gen_random_uuid())`), "gateway inserting a letter")
	// A good letter: checking, version 1, not a demo letter here.
	id := createLetter(t, pool, w, []uuid.UUID{w.admin, w.analyst}, []uuid.UUID{w.pa})
	var status string
	var version int
	var demo bool
	if err := pool.QueryRow(ctx, `SELECT status, version, demo FROM clearances WHERE id = $1`, id).Scan(&status, &version, &demo); err != nil {
		t.Fatal(err)
	}
	if status != "checking" || version != 1 || demo {
		t.Errorf("new letter: %s v%d demo %v", status, version, demo)
	}
	// The demo outcome is refused for it.
	err := asPerson(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT mhai_clearance_demo_ready($1, 2, sha256('z'))`, id)
		return err
	})
	if refusal(err) != "unavailable" {
		t.Errorf("demo outcome for a live letter: %v", err)
	}
	// On the demo hospital the trigger sets demo.
	dh := demoHospital(t, pool)
	setSwitch(t, pool, &dh, "on", "DEMO-SYNTHETIC")
	setNamed(t, pool, &dh, "letters_only")
	dadmin := staffer(t, pool, dh, staffOpts{roles: []string{"hospital_admin"}})
	dw := letterWorld{h: dh, admin: dadmin}
	did := createLetter(t, pool, dw, []uuid.UUID{dadmin}, nil)
	if err := pool.QueryRow(ctx, `SELECT demo FROM clearances WHERE id = $1`, did).Scan(&demo); err != nil || !demo {
		t.Errorf("demo hospital's letter: demo %v (%v)", demo, err)
	}
}

// Narrowing only removes, never the last person, at the version seen;
// nobody is re-added or deleted.
func TestNarrowOnlyRemoves(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newLetterWorld(t, pool)
	id := createLetter(t, pool, w, []uuid.UUID{w.admin, w.analyst}, nil)
	narrow := func(version int, remove []uuid.UUID, audited bool) error {
		return db.WithHospitalUser(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT mhai_clearance_narrow($1, $2, $3, 'person_left', NULL)`, id, version, remove); err != nil {
				return err
			}
			if audited {
				_, err := letters.AuditPending(ctx, tx, "req-narrow")
				return err
			}
			return nil
		})
	}
	if got := refusal(narrow(1, []uuid.UUID{w.admin, w.analyst}, true)); got != "would_empty" {
		t.Errorf("removing everyone: %s", got)
	}
	if got := refusal(narrow(7, []uuid.UUID{w.analyst}, true)); got != "conflict" {
		t.Errorf("a stale version: %s", got)
	}
	if got := refusal(narrow(1, []uuid.UUID{w.clin}, true)); got != "invalid" {
		t.Errorf("removing someone not covered: %s", got)
	}
	if err := narrow(1, []uuid.UUID{w.analyst}, false); pgCode(err) != "23514" {
		t.Errorf("an unaudited narrowing: want 23514, got %v", err)
	}
	if err := narrow(1, []uuid.UUID{w.analyst}, true); err != nil {
		t.Fatalf("narrow: %v", err)
	}
	var version int
	if err := pool.QueryRow(ctx, `SELECT version FROM clearances WHERE id = $1`, id).Scan(&version); err != nil || version != 2 {
		t.Errorf("version %d (%v), want 2", version, err)
	}
	if got := refusal(narrow(2, []uuid.UUID{w.analyst}, true)); got != "invalid" {
		t.Errorf("removing someone already removed: %s", got)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM audit_log WHERE action = 'clearance.narrowed' AND entity_id = $1
		AND payload->>'person' = $2 AND payload->>'reason' = 'person_left' AND actor_id = $3`, id.String(), w.analyst.String(), w.admin); n != 1 {
		t.Errorf("%d narrowed entries, want 1", n)
	}
	// Re-adding or deleting, even directly: refused.
	for _, q := range []string{`UPDATE clearance_covered SET removed_at = NULL, removed_reason = NULL WHERE clearance_id = $1`,
		`UPDATE clearance_covered SET user_id = gen_random_uuid() WHERE clearance_id = $1`} {
		err := asActor(ctx, pool, "mhai_gateway", w.h, w.admin, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, id)
			return err
		})
		if refusal(err) != "conflict" {
			t.Errorf("%s: %v", q, err)
		}
	}
	requireDenied(t, asRole(ctx, pool, "mhai_gateway", `DELETE FROM clearance_covered WHERE false`), "gateway deleting people")
	requireDenied(t, asRole(ctx, pool, "mhai_gateway", `INSERT INTO clearance_covered (hospital_id, clearance_id, user_id, side)
		VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'hospital')`), "gateway adding people")
}

// No letter change commits without its entry; an entry for another event
// doesn't count.
func TestClearanceEventsAudited(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newLetterWorld(t, pool)
	today := hospitalToday(t, pool, w.h)
	for _, write := range []func(tx pgx.Tx, id uuid.UUID) error{
		func(pgx.Tx, uuid.UUID) error { return nil },
		func(tx pgx.Tx, id uuid.UUID) error {
			return audit.Write(ctx, tx, audit.Event{ActorID: w.admin.String(), HospitalID: w.h.String(), Action: "clearance.uploaded",
				Entity: "clearance", EntityID: id.String(), Payload: map[string]any{"event_id": uuid.NewString()}})
		},
	} {
		err := db.WithHospitalUser(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT mhai_clearance_create('Synthetic Signer', 'Synthetic Title', $1, $2, NULL, $3, NULL)`,
				today, today.AddDate(0, 3, 0), []uuid.UUID{w.admin}).Scan(&id); err != nil {
				return err
			}
			return write(tx, id)
		})
		if pgCode(err) != "23514" {
			t.Errorf("an upload without its entry: want 23514, got %v", err)
		}
	}
	id := createLetter(t, pool, w, []uuid.UUID{w.admin}, nil)
	if n := countRows(t, pool, `SELECT count(*) FROM audit_log a JOIN clearance_events e ON a.payload->>'event_id' = e.id::text
		WHERE e.clearance_id = $1 AND a.action = 'clearance.uploaded' AND a.actor_id = $2 AND a.hospital_id = $3`, id, w.admin, w.h); n != 1 {
		t.Errorf("%d uploaded entries, want 1", n)
	}
	// Nobody writes an event directly.
	requireDenied(t, asRole(ctx, pool, "mhai_gateway", `INSERT INTO clearance_events (hospital_id, clearance_id, kind)
		VALUES (gen_random_uuid(), gen_random_uuid(), 'ended')`), "gateway writing an event")
}

// The named-access switch: the position is the lower of the two rows,
// off when either is missing; a hospital row is never above the global;
// the writers.
func TestNamedAccessSwitch(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	t.Cleanup(func() { setNamed(t, pool, nil, "on") })
	h := onHospital(t, pool, "UTC")
	position := func() string {
		var p string
		err := asPerson(ctx, pool, h, uuid.New(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT mhai_named_access_position($1)`, h).Scan(&p)
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	setNamed(t, pool, nil, "on")
	if p := position(); p != "off" {
		t.Errorf("no hospital row: %s", p)
	}
	setNamed(t, pool, &h, "letters_only")
	if p := position(); p != "letters_only" {
		t.Errorf("letters_only under on: %s", p)
	}
	setNamed(t, pool, nil, "off")
	if p := position(); p != "off" {
		t.Errorf("global off: %s", p)
	}
	// Another hospital's position is off to the gateway.
	var p string
	if err := asPerson(ctx, pool, uuid.New(), uuid.New(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mhai_named_access_position($1)`, h).Scan(&p)
	}); err != nil || p != "off" {
		t.Errorf("another hospital's position: %s (%v)", p, err)
	}
	// above_global.
	setNamed(t, pool, nil, "letters_only")
	if _, err := pool.Exec(ctx, `UPDATE named_access_switch SET position = 'on' WHERE hospital_id = $1`, h); pgCode(err) != "23514" {
		t.Errorf("a hospital above the global: want 23514, got %v", err)
	}
	// Writers: never the gateway; the platform never with demo references;
	// the provisioning role only lowers.
	requireDenied(t, asRole(ctx, pool, "mhai_gateway", `UPDATE named_access_switch SET position = 'off'`), "gateway lowering")
	requireDenied(t, asRole(ctx, pool, "mhai_provision", `UPDATE named_access_switch SET position = 'on' WHERE hospital_id IS NULL`),
		"provisioning raising")
	// The demo raise: only a demo hospital under a demo analysis switch.
	raise := func(h *uuid.UUID) (bool, error) {
		var was bool
		err := asRoleIn(ctx, pool, "mhai_provision", uuid.Nil, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT mhai_named_access_demo_raise($1)`, h).Scan(&was)
		})
		return was, err
	}
	if _, err := raise(&h); pgCode(err) != "22023" {
		t.Errorf("demo raise of a real hospital: want 22023, got %v", err)
	}
	setNamed(t, pool, nil, "on")
	dh := demoHospital(t, pool)
	setSwitch(t, pool, &dh, "on", "DEMO-SYNTHETIC")
	// The platform never writes demo references, even for the demo.
	requireDenied(t, asRole(ctx, pool, "mhai_platform", `INSERT INTO named_access_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		VALUES ('`+dh.String()+`', 'letters_only', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC')`), "platform writing demo references")
	if was, err := raise(&dh); err != nil || !was {
		t.Fatalf("demo raise: %v %v", was, err)
	}
	if was, err := raise(&dh); err != nil || was {
		t.Errorf("second demo raise: %v %v", was, err)
	}
}

// Grants: the gateway's functions are its own, the helpers nobody's, the
// due list the job's; every function pins its search_path; the rules'
// owner has no login and no members.
func TestClearanceGrants(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	var bad []string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(array_agg(p.proname || ':' || r.role ORDER BY (p.proname || ':' || r.role) COLLATE "C"), '{}') FROM pg_proc p,
		unnest(ARRAY['mhai_gateway', 'mhai_platform', 'mhai_provision', 'mhai_auth', 'mhai_analysis_job', 'mhai_jobs']) r(role)
		WHERE (p.proname LIKE 'mhai_clearance%' OR p.proname LIKE 'mhai_named_access%')
		  AND has_function_privilege(r.role, p.oid, 'EXECUTE')`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	want := "mhai_clearance_check:mhai_gateway,mhai_clearance_create:mhai_gateway,mhai_clearance_demo_ready:mhai_gateway," +
		"mhai_clearance_due:mhai_analysis_job,mhai_clearance_gate:mhai_gateway,mhai_clearance_names:mhai_gateway," +
		"mhai_clearance_narrow:mhai_gateway,mhai_clearance_nightly:mhai_gateway," +
		"mhai_clearance_pending:mhai_gateway,mhai_clearance_release_check:mhai_gateway,mhai_clearance_reserve_check:mhai_gateway," +
		"mhai_clearance_revoke:mhai_gateway,mhai_clearance_today:mhai_gateway,mhai_named_access_demo_raise:mhai_provision," +
		"mhai_named_access_position:mhai_gateway,mhai_named_access_position:mhai_provision," +
		"mhai_named_access_rank:mhai_provision,mhai_named_access_within_global:mhai_provision"
	if got := strings.Join(bad, ","); got != want {
		t.Errorf("EXECUTE grants:\n%s\nwant\n%s", got, want)
	}
	var loose []string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(array_agg(proname::text), '{}') FROM pg_proc
		WHERE (proname LIKE 'mhai_clearance%' OR proname LIKE 'mhai_named_access%')
		  AND NOT (proconfig @> ARRAY['search_path=pg_catalog, pg_temp'])`).
		Scan(&loose); err != nil || len(loose) != 0 {
		t.Errorf("functions without a pinned search_path: %v (%v)", loose, err)
	}
	var login, members bool
	if err := pool.QueryRow(ctx, `SELECT rolcanlogin OR rolsuper OR rolbypassrls,
		EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.roleid = r.oid) FROM pg_roles r WHERE rolname = 'mhai_clearance_rules'`).
		Scan(&login, &members); err != nil || login || members {
		t.Errorf("mhai_clearance_rules: login %v members %v (%v)", login, members, err)
	}
	for _, role := range []string{"mhai_platform", "mhai_auth", "mhai_provision", "mhai_jobs", "mhai_analysis_job"} {
		requireDenied(t, asRole(ctx, pool, role, `SELECT count(*) FROM clearances`), role+" reading letters")
	}
}

func TestMigration00038DownUp(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	policies := func() string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT string_agg(polname || ':' || COALESCE(pg_get_expr(polwithcheck, polrelid), ''), ';' ORDER BY polname)
			FROM pg_policy WHERE polrelid = 'audit_log'::regclass`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 37); err != nil {
		t.Fatalf("down to 37: %v", err)
	}
	before := policies()
	var left int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_class WHERE relname IN ('clearances', 'clearance_covered', 'clearance_events', 'named_access_switch', 'clearance_check_refusals',
			'audit_log_clearance_event_idx'))
		+ (SELECT count(*) FROM pg_proc WHERE proname LIKE 'mhai_clearance%' OR proname LIKE 'mhai_named_access%')
		+ (SELECT count(*) FROM pg_policy WHERE polname IN ('clearance_rules', 'provision_named_access_append'))
		+ (SELECT count(*) FROM unnest(ARRAY['users', 'user_mfa', 'user_roles', 'audit_log', 'hospitals', 'analysis_switch']) t
		   WHERE has_any_column_privilege('mhai_clearance_rules', t, 'SELECT'))
		+ (SELECT count(*) WHERE has_function_privilege('mhai_clearance_rules', 'mhai_analysis_position(uuid)', 'EXECUTE'))`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("after down, %d objects or grants of 00038 remain", left)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 37); err != nil {
		t.Fatalf("down again: %v", err)
	}
	if after := policies(); after != before {
		t.Errorf("the audit appends changed across up and down:\n%s\nwant\n%s", after, before)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up a third time: %v", err)
	}
}
