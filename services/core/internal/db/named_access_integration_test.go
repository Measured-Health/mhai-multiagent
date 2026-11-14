//go:build integration

package db_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00042 (analysis backend plan Task 21): who is covered, named
// lists, their keys, notes and the named budget. The superuser seeds the
// fixtures (exempt from the letter rules, as it can disable any trigger):
// active letters exist only after the platform's verify (Plan 11b).
// Synthetic hospitals, accounts, letters and subjects only.

// namedWorld is newDrill's hospital (frozen April 2026 periods, n female
// subjects with an encrypted-looking reference each) with named access on
// globally and for it, an admin and an analyst, both eligible.
type namedWorld struct {
	drill
	admin, analyst uuid.UUID
}

func newNamedWorld(t *testing.T, pool *pgxpool.Pool, n int) namedWorld {
	t.Helper()
	w := namedWorld{drill: newDrill(t, pool, n)}
	setNamed(t, pool, nil, "on")
	setNamed(t, pool, &w.h, "on")
	w.admin = staffer(t, pool, w.h, staffOpts{roles: []string{"hospital_admin"}})
	w.analyst = staffer(t, pool, w.h, staffOpts{roles: []string{"clinician", "analyst"}})
	ref := make([]byte, 48)
	_, _ = rand.Read(ref)
	if _, err := pool.Exec(context.Background(), `UPDATE analysis_subjects SET ref_enc = $2 WHERE hospital_id = $1`, w.h, ref); err != nil {
		t.Fatal(err)
	}
	return w
}

// activeLetter is an active, verified letter of h ending in days days at
// the hospital, covering the people given (seeded by the superuser).
func activeLetter(t *testing.T, pool *pgxpool.Pool, h uuid.UUID, days int, people ...uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := seedLetter(t, pool, h, people[0], "active", false)
	if _, err := pool.Exec(ctx, `UPDATE clearances SET end_date = (now() AT TIME ZONE 'Africa/Lagos')::date + $2::int WHERE id = $1`, id, days); err != nil {
		t.Fatal(err)
	}
	for _, p := range people {
		if _, err := pool.Exec(ctx, `INSERT INTO clearance_covered (hospital_id, clearance_id, user_id, side) VALUES ($1, $2, $3, 'hospital')`,
			h, id, p); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

type coverRow struct {
	named     bool
	until     *time.Time
	clearance *uuid.UUID
	reason    *string
}

func cover(t *testing.T, pool *pgxpool.Pool, h, user, asked uuid.UUID) (coverRow, error) {
	t.Helper()
	var c coverRow
	err := asPerson(context.Background(), pool, h, user, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT named, until, clearance_id, reason FROM mhai_named_cover($1)`, asked).
			Scan(&c.named, &c.until, &c.clearance, &c.reason)
	})
	return c, err
}

func reasonOf(c coverRow) string {
	if c.reason == nil {
		return ""
	}
	return *c.reason
}

func p0001(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "P0001" {
		return pe.Message
	}
	return ""
}

// T11-T14 in SQL: the position, the role and account, each letter's end
// state, and the session's own person only.
func TestNamedCoverRules(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newNamedWorld(t, pool, 6)

	// Nobody is covered without a letter.
	if c, err := cover(t, pool, w.h, w.admin, w.admin); err != nil || c.named || reasonOf(c) != "none" {
		t.Fatalf("no letter: %+v %v", c, err)
	}
	letter := activeLetter(t, pool, w.h, 30, w.admin, w.analyst)
	c, err := cover(t, pool, w.h, w.admin, w.admin)
	if err != nil || !c.named || c.clearance == nil || *c.clearance != letter || c.reason != nil {
		t.Fatalf("covered: %+v %v", c, err)
	}
	if c.until == nil || time.Until(*c.until) > 30*time.Minute+time.Second {
		t.Errorf("until %v: want at most 30 minutes on", c.until)
	}
	// Another person's cover is never asked.
	if _, err := cover(t, pool, w.h, w.admin, w.analyst); p0001(err) != "forbidden" {
		t.Errorf("another person's cover: %v", err)
	}
	// A letter ending today ends at the hospital's midnight: until is
	// the earlier of that and 30 minutes on.
	soon := activeLetter(t, pool, w.h, 0, staffer(t, pool, w.h, staffOpts{roles: []string{"hospital_admin"}}))
	var ownerOf uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT user_id FROM clearance_covered WHERE clearance_id = $1`, soon).Scan(&ownerOf); err != nil {
		t.Fatal(err)
	}
	if c, err := cover(t, pool, w.h, ownerOf, ownerOf); err != nil || !c.named || c.until == nil {
		t.Fatalf("ends today: %+v %v", c, err)
	} else {
		var midnight time.Time
		if err := pool.QueryRow(ctx, `SELECT ((now() AT TIME ZONE 'Africa/Lagos')::date + 1)::timestamp AT TIME ZONE 'Africa/Lagos'`).Scan(&midnight); err != nil {
			t.Fatal(err)
		}
		if c.until.After(midnight) {
			t.Errorf("until %v after the hospital's midnight %v", c.until, midnight)
		}
	}

	// T11: letters_only is below on.
	setNamed(t, pool, &w.h, "letters_only")
	if c, err := cover(t, pool, w.h, w.admin, w.admin); err != nil || c.named || reasonOf(c) != "switched_off" {
		t.Errorf("letters_only: %+v %v", c, err)
	}
	setNamed(t, pool, &w.h, "on")

	// T13, T14: the role, two-step and account, at once.
	for name, fix := range map[string]string{
		"role removed":  `DELETE FROM user_roles WHERE user_id = $1 AND role = 'analyst'`,
		"two-step off":  `UPDATE user_mfa SET enabled = false WHERE user_id = $1`,
		"account ended": `UPDATE users SET is_active = false WHERE id = $1`,
	} {
		p := staffer(t, pool, w.h, staffOpts{roles: []string{"clinician", "analyst"}})
		activeLetter(t, pool, w.h, 30, p)
		if c, err := cover(t, pool, w.h, p, p); err != nil || !c.named {
			t.Fatalf("%s, before: %+v %v", name, c, err)
		}
		if _, err := pool.Exec(ctx, fix, p); err != nil {
			t.Fatal(err)
		}
		if c, err := cover(t, pool, w.h, p, p); err != nil || c.named || reasonOf(c) != "role_or_account" {
			t.Errorf("%s: %+v %v", name, c, err)
		}
	}

	// T12: revoked, removed from the letter, and ended: each at once, with
	// its reason.
	revoked := staffer(t, pool, w.h, staffOpts{roles: []string{"hospital_admin"}})
	l := activeLetter(t, pool, w.h, 30, revoked)
	if _, err := pool.Exec(ctx, `UPDATE clearances SET status = 'revoked', revoked_at = now(), revoke_reason = 'other',
		revoked_side = 'platform' WHERE id = $1`, l); err != nil {
		t.Fatal(err)
	}
	if c, err := cover(t, pool, w.h, revoked, revoked); err != nil || c.named || reasonOf(c) != "revoked" {
		t.Errorf("revoked: %+v %v", c, err)
	}
	removed := staffer(t, pool, w.h, staffOpts{roles: []string{"hospital_admin"}})
	l = activeLetter(t, pool, w.h, 30, removed, w.admin)
	if _, err := pool.Exec(ctx, `UPDATE clearance_covered SET removed_at = now(), removed_reason = 'person_left'
		WHERE clearance_id = $1 AND user_id = $2`, l, removed); err != nil {
		t.Fatal(err)
	}
	if c, err := cover(t, pool, w.h, removed, removed); err != nil || c.named || reasonOf(c) != "removed_from_letter" {
		t.Errorf("removed: %+v %v", c, err)
	}
	ended := staffer(t, pool, w.h, staffOpts{roles: []string{"hospital_admin"}})
	activeLetter(t, pool, w.h, -1, ended)
	if c, err := cover(t, pool, w.h, ended, ended); err != nil || c.named || reasonOf(c) != "ended" {
		t.Errorf("ended yesterday at the hospital: %+v %v", c, err)
	}

	// Another hospital's letter never covers: the person is not its.
	other := onHospital(t, pool, "Africa/Lagos")
	setNamed(t, pool, &other, "on")
	otherAdmin := staffer(t, pool, other, staffOpts{roles: []string{"hospital_admin"}})
	activeLetter(t, pool, other, 30, otherAdmin)
	if c, err := cover(t, pool, w.h, otherAdmin, otherAdmin); err != nil || c.named || reasonOf(c) != "role_or_account" {
		t.Errorf("a person of another hospital: %+v %v", c, err)
	}
	// Analysis off: not_found before anything.
	if _, err := pool.Exec(ctx, `UPDATE analysis_switch SET position = 'off' WHERE hospital_id = $1`, other); err != nil {
		t.Fatal(err)
	}
	if _, err := cover(t, pool, other, otherAdmin, otherAdmin); p0001(err) != "not_found" {
		t.Errorf("analysis off: %v", err)
	}
}

// A revocation that starts while a view holds its letter waits for the
// view's commit; the next cover sees it.
func TestNamedCoverRevokeWaits(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newNamedWorld(t, pool, 6)
	letter := activeLetter(t, pool, w.h, 30, w.admin)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{`SET LOCAL ROLE mhai_gateway`} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true), set_config('mhai.user_id', $2, true)`,
		w.h.String(), w.admin.String()); err != nil {
		t.Fatal(err)
	}
	var named bool
	if err := tx.QueryRow(ctx, `SELECT named FROM mhai_named_cover($1)`, w.admin).Scan(&named); err != nil || !named {
		t.Fatalf("cover: %v %v", named, err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := pool.Exec(ctx, `UPDATE clearances SET status = 'revoked', revoked_at = now(), revoke_reason = 'other',
			revoked_side = 'platform' WHERE id = $1`, letter)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the revocation didn't wait for the view: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if c, err := cover(t, pool, w.h, w.admin, w.admin); err != nil || c.named || reasonOf(c) != "revoked" {
		t.Errorf("after the revocation: %+v %v", c, err)
	}
}

// The named budget: 20 parallel admissions of one person, exactly 5 true.
func TestNamedBudgetReserveRace(t *testing.T) {
	_, pool := shared(t)
	w := newNamedWorld(t, pool, 6)
	var mu sync.Mutex
	var wg sync.WaitGroup
	yes := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var ok bool
			err := asPerson(context.Background(), pool, w.h, w.admin, func(tx pgx.Tx) error {
				return tx.QueryRow(context.Background(), `SELECT mhai_analysis_named_admit(5)`).Scan(&ok)
			})
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			if ok {
				yes++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if yes != 5 {
		t.Errorf("%d admitted, want 5", yes)
	}
	// The limit is at most 5, whatever the caller asks.
	err := asPerson(context.Background(), pool, w.h, w.analyst, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `SELECT mhai_analysis_named_admit(6)`)
		return err
	})
	if pgCode(err) != "22023" {
		t.Errorf("a limit of 6: %v", err)
	}
}

func namedPayload(view uuid.UUID, clearance uuid.UUID, n, replays int) map[string]any {
	return map[string]any{"view": "affected", "period": "week:2026-04-01", "filters": map[string]any{}, "patient_count": n,
		"rows": n, "purpose": "audit", "replays": replays, "clearance_id": clearance.String()}
}

type namedOpen struct {
	view, clearance uuid.UUID
	rows            int
	refs            int
	key, body       []byte
}

// openNamed opens a named list of the drill's female group in its week as
// user, with its entry unless noAudit.
func openNamed(ctx context.Context, pool *pgxpool.Pool, w namedWorld, user uuid.UUID, noAudit bool) (namedOpen, error) {
	return openNamedOpt(ctx, pool, w, user, noAudit, false)
}

// openNamedOpt is openNamed without the budget's reservation when
// noReserve.
func openNamedOpt(ctx context.Context, pool *pgxpool.Pool, w namedWorld, user uuid.UUID, noAudit, noReserve bool) (namedOpen, error) {
	o := namedOpen{view: uuid.New()}
	k, b := sha256.Sum256([]byte("synthetic-key-"+o.view.String())), sha256.Sum256([]byte("synthetic-body"))
	o.key, o.body = k[:], b[:]
	err := asPerson(ctx, pool, w.h, user, func(tx pgx.Tx) error {
		if !noReserve {
			if _, err := tx.Exec(ctx, `SELECT mhai_analysis_named_admit(5)`); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `SELECT ref_enc, clearance_id FROM mhai_analysis_open_named($1, $2, '{}'::jsonb, $3::jsonb, 'affected',
			$4, $5, 300, 'audit', NULL, $6, $7)`, o.view, w.week, femaleGroup, make([]byte, 32), len(w.subjects), o.key, o.body)
		if err != nil {
			return err
		}
		for rows.Next() {
			var ref []byte
			if err := rows.Scan(&ref, &o.clearance); err != nil {
				rows.Close()
				return err
			}
			o.rows++
			if len(ref) > 0 {
				o.refs++
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if noAudit {
			return nil
		}
		return audit.Write(ctx, tx, audit.Event{ActorID: user.String(), HospitalID: w.h.String(), Action: "analysis.named_viewed",
			Entity: "analysis_view", EntityID: o.view.String(), Payload: namedPayload(o.view, o.clearance, o.rows, 0)})
	})
	return o, err
}

// A named list is recorded with its letter, purpose and named_until, its
// rows carry the references, and it is refused at commit without its
// analysis.named_viewed entry; nobody uncovered opens one.
func TestOpenNamedRecordsAndAudits(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newNamedWorld(t, pool, 6)
	if _, err := openNamed(ctx, pool, w, w.admin, false); p0001(err) != "not_cleared" {
		t.Fatalf("no letter: %v", err)
	}
	letter := activeLetter(t, pool, w.h, 30, w.admin)
	// The budget is the database's: no reservation in this transaction, no list.
	if _, err := openNamedOpt(ctx, pool, w, w.admin, false, true); p0001(err) != "not_reserved" {
		t.Fatalf("without a reservation: %v", err)
	}
	if _, err := openNamed(ctx, pool, w, w.admin, true); pgCode(err) != "23514" {
		t.Fatalf("without its entry: %v", err)
	}
	o, err := openNamed(ctx, pool, w, w.admin, false)
	if err != nil {
		t.Fatal(err)
	}
	if o.rows != 6 || o.refs != 6 || o.clearance != letter {
		t.Fatalf("rows %d, refs %d, letter %v", o.rows, o.refs, o.clearance)
	}
	var level, purpose string
	var cl uuid.UUID
	var until time.Time
	if err := pool.QueryRow(ctx, `SELECT level, purpose, clearance_id, named_until FROM analysis_views WHERE id = $1`, o.view).
		Scan(&level, &purpose, &cl, &until); err != nil {
		t.Fatal(err)
	}
	if level != "named" || purpose != "audit" || cl != letter || time.Until(until) > 30*time.Minute+time.Second {
		t.Errorf("view: %s %s %v %v", level, purpose, cl, until)
	}
	// A de-identified list can't take a letter, nor a named one go without.
	if _, err := pool.Exec(ctx, `UPDATE analysis_views SET clearance_id = NULL WHERE id = $1`, o.view); pgCode(err) != "23514" {
		t.Errorf("a named list without its letter: %v", err)
	}
	// The analyst isn't on the letter.
	if _, err := openNamed(ctx, pool, w, w.analyst, false); p0001(err) != "not_cleared" {
		t.Errorf("not on the letter: %v", err)
	}
	// The cap is shared with de-identified lists: the subjects already
	// opened cost nothing; a cap below them refuses.
	var opened int
	if err := pool.QueryRow(ctx, `SELECT opened FROM analysis_patient_budget WHERE hospital_id = $1 AND user_id = $2`, w.h, w.admin).
		Scan(&opened); err != nil || opened != 6 {
		t.Errorf("opened %d (%v), want 6", opened, err)
	}
}

func replay(ctx context.Context, pool *pgxpool.Pool, w namedWorld, user uuid.UUID, o namedOpen, body []byte, audited bool) (int, error) {
	n := 0
	err := asPerson(ctx, pool, w.h, user, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT mhai_analysis_named_admit(5)`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT replays, clearance_id FROM mhai_analysis_named_replay($1, $2, $3, '{}'::jsonb, $4::jsonb, $5, $6, 300)`,
			o.key, body, w.week, femaleGroup, make([]byte, 32), len(w.subjects))
		if err != nil {
			return err
		}
		var cl uuid.UUID
		rows2 := 0
		for rows.Next() {
			if err := rows.Scan(&n, &cl); err != nil {
				rows.Close()
				return err
			}
			rows2++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if !audited {
			return nil
		}
		return audit.Write(ctx, tx, audit.Event{ActorID: user.String(), HospitalID: w.h.String(), Action: "analysis.named_viewed",
			Entity: "analysis_view", EntityID: o.view.String(), Payload: namedPayload(o.view, cl, rows2, n)})
	})
	return n, err
}

func keyState(ctx context.Context, pool *pgxpool.Pool, w namedWorld, user uuid.UUID, key, body []byte) string {
	var s string
	_ = asPerson(ctx, pool, w.h, user, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM mhai_analysis_named_key($1, $2)`, key, body).Scan(&s)
	})
	return s
}

// A replay is refused at commit without its own entry; another person's
// key or another body is a mismatch; after named_until it has expired.
func TestNamedReplayAuditedAndBounded(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newNamedWorld(t, pool, 6)
	activeLetter(t, pool, w.h, 30, w.admin, w.analyst)
	o, err := openNamed(ctx, pool, w, w.admin, false)
	if err != nil {
		t.Fatal(err)
	}
	other := sha256.Sum256([]byte("another synthetic body"))
	for _, c := range []struct {
		user uuid.UUID
		key  []byte
		body []byte
		want string
	}{
		{w.admin, o.key, o.body, "replay"},
		{w.analyst, o.key, o.body, "mismatch"},
		{w.admin, o.key, other[:], "mismatch"},
		{w.admin, make([]byte, 32), o.body, "new"},
	} {
		if got := keyState(ctx, pool, w, c.user, c.key, c.body); got != c.want {
			t.Errorf("key state %q, want %q", got, c.want)
		}
	}
	if _, err := replay(ctx, pool, w, w.admin, o, o.body, false); pgCode(err) != "23514" {
		t.Fatalf("a replay without its entry: %v", err)
	}
	if n, err := replay(ctx, pool, w, w.admin, o, o.body, true); err != nil || n != 1 {
		t.Fatalf("replay: %d %v", n, err)
	}
	if _, err := replay(ctx, pool, w, w.analyst, o, o.body, true); p0001(err) != "idempotency_mismatch" {
		t.Errorf("another person's replay: %v", err)
	}
	if _, err := replay(ctx, pool, w, w.admin, o, other[:], true); p0001(err) != "idempotency_mismatch" {
		t.Errorf("another body: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE named_view_keys SET named_until = now() - interval '1 second' WHERE view_id = $1`, o.view); err != nil {
		t.Fatal(err)
	}
	if got := keyState(ctx, pool, w, w.admin, o.key, o.body); got != "expired" {
		t.Errorf("after named_until: %q", got)
	}
	if _, err := replay(ctx, pool, w, w.admin, o, o.body, true); p0001(err) != "expired" {
		t.Errorf("replay after named_until: %v", err)
	}
	// The sweep drops the key a day after named_until, never the list.
	if _, err := pool.Exec(ctx, `UPDATE named_view_keys SET named_until = now() - interval '25 hours' WHERE view_id = $1`, o.view); err != nil {
		t.Fatal(err)
	}
	if err := db.WithAnalysisJob(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT mhai_analysis_sweep($1, now())`, w.h)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var keys, views int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM named_view_keys WHERE view_id = $1), (SELECT count(*) FROM analysis_views WHERE id = $1)`,
		o.view).Scan(&keys, &views); err != nil || keys != 0 || views != 1 {
		t.Errorf("after the sweep: %d keys, %d lists (%v)", keys, views, err)
	}
}

// No app role reads or writes the named tables; only the gateway runs the
// request functions.
func TestNamedTablesGrants(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for _, role := range []string{"mhai_gateway", "mhai_platform", "mhai_auth", "mhai_provision", "mhai_jobs", "mhai_analysis_job", "mhai_worker"} {
		for _, table := range []string{"named_view_keys", "named_view_notes", "named_view_budget"} {
			requireDenied(t, asRole(ctx, pool, role, `SELECT count(*) FROM `+table), role+" reading "+table)
		}
	}
	var got []string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(array_agg(p.proname || ':' || r.role ORDER BY (p.proname || ':' || r.role) COLLATE "C"), '{}') FROM pg_proc p,
		unnest(ARRAY['mhai_gateway', 'mhai_platform', 'mhai_provision', 'mhai_auth', 'mhai_analysis_job', 'mhai_jobs', 'mhai_worker']) r(role)
		WHERE (p.proname LIKE 'mhai_analysis_named%' OR p.proname IN ('mhai_named_cover', 'mhai_analysis_open_named', 'mhai_named_replay_audited'))
		  AND has_function_privilege(r.role, p.oid, 'EXECUTE')`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := "mhai_analysis_named_admit:mhai_gateway,mhai_analysis_named_key:mhai_gateway,mhai_analysis_named_replay:mhai_gateway," +
		"mhai_analysis_open_named:mhai_gateway,mhai_named_cover:mhai_gateway"
	if s := strings.Join(got, ","); s != want {
		t.Errorf("EXECUTE grants:\n%s\nwant\n%s", s, want)
	}
	var loose []string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(array_agg(proname::text), '{}') FROM pg_proc
		WHERE (proname LIKE 'mhai_analysis_named%' OR proname IN ('mhai_named_cover', 'mhai_analysis_open_named', 'mhai_named_replay_audited',
		       'mhai_named_access_state', 'mhai_named_access_demo_on'))
		  AND NOT (proconfig @> ARRAY['search_path=pg_catalog, pg_temp'])`).Scan(&loose); err != nil || len(loose) != 0 {
		t.Errorf("functions without a pinned search_path: %v (%v)", loose, err)
	}
}

func TestMigration00042DownUp(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.MigrateUpTo(ctx, pg.URL, 41); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	state := func() string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT
			(SELECT string_agg(polname || ':' || COALESCE(pg_get_expr(polqual, polrelid), '') || ':' || COALESCE(pg_get_expr(polwithcheck, polrelid), ''), ';' ORDER BY polname)
			 FROM pg_policy WHERE polrelid IN ('audit_log'::regclass, 'analysis_subjects'::regclass))
			|| '|' || (SELECT string_agg(conname || ':' || pg_get_constraintdef(oid), ';' ORDER BY conname)
			 FROM pg_constraint WHERE conrelid IN ('analysis_views'::regclass, 'analysis_drill_flags'::regclass))
			|| '|' || has_column_privilege('mhai_analysis_reader', 'audit_log', 'payload', 'SELECT')::text
			|| '|' || has_any_column_privilege('mhai_analysis_reader', 'analysis_subjects', 'SELECT')::text
			|| '|' || md5(pg_get_functiondef('mhai_analysis_sweep(uuid, timestamptz)'::regprocedure))
			|| '|' || md5(pg_get_functiondef('mhai_analysis_drill_flag(text, timestamptz)'::regprocedure))
			|| '|' || md5(pg_get_functiondef('mhai_analysis_view_audited()'::regprocedure))
			|| '|' || has_function_privilege('mhai_auth', 'mhai_named_access_position(uuid)', 'EXECUTE')::text`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := state()
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 41); err != nil {
		t.Fatalf("down to 41: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_class WHERE relname IN ('named_view_keys', 'named_view_notes', 'named_view_budget'))
		+ (SELECT count(*) FROM pg_proc WHERE proname LIKE 'mhai_analysis_named%' OR proname IN ('mhai_named_cover',
		   'mhai_analysis_open_named', 'mhai_named_replay_audited', 'mhai_named_access_state', 'mhai_named_access_demo_on'))`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("after down, %d objects of 00042 remain", left)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 41); err != nil {
		t.Fatalf("down again: %v", err)
	}
	if after := state(); after != before {
		t.Errorf("the state before 00042 changed across up and down:\n%s\nwant\n%s", after, before)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up a third time: %v", err)
	}
}
