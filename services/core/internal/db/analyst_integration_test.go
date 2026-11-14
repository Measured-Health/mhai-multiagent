//go:build integration

package db_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00030 (analysis backend plan Task 15): the analyst rules in
// triggers on user_roles, for every writer, and the commit check that
// every grant and removal is audited. Synthetic hospitals and accounts;
// the superuser seeds them (exempt from the rules, as it can disable any
// trigger), and every rule is exercised as mhai_gateway or mhai_provision.

// staffer is a synthetic account of hospital h.
type staffOpts struct {
	roles    []string
	inactive bool
	noMFA    bool
}

func staffer(t *testing.T, pool *pgxpool.Pool, h uuid.UUID, o staffOpts) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO users (hospital_id, email, password_hash, display_name, is_active)
		VALUES ($1, $2, 'synthetic-hash', 'Synthetic Staff', $3) RETURNING id`,
		h, uniqueCode("s")+"@example.test", !o.inactive).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, r := range o.roles {
		if _, err := pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, id, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_mfa (user_id, secret_enc, enabled) VALUES ($1, '\x00', $2)`, id, !o.noMFA); err != nil {
		t.Fatal(err)
	}
	return id
}

// analystWorld is a switched-on hospital with an active admin and an
// eligible person (clinician, active, two-step), and a second hospital
// with its own admin.
type analystWorld struct {
	h, admin, person uuid.UUID
	h2, admin2       uuid.UUID
}

func newAnalystWorld(t *testing.T, pool *pgxpool.Pool) analystWorld {
	t.Helper()
	w := analystWorld{h: onHospital(t, pool, "Africa/Lagos"), h2: onHospital(t, pool, "Africa/Lagos")}
	w.admin = staffer(t, pool, w.h, staffOpts{roles: []string{"hospital_admin"}})
	w.person = staffer(t, pool, w.h, staffOpts{roles: []string{"clinician"}})
	w.admin2 = staffer(t, pool, w.h2, staffOpts{roles: []string{"hospital_admin"}})
	return w
}

func isAnalyst(t *testing.T, pool *pgxpool.Pool, u uuid.UUID) bool {
	t.Helper()
	return countRows(t, pool, `SELECT count(*) FROM user_roles WHERE user_id = $1 AND role = 'analyst'`, u) == 1
}

func refusal(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "P0001" {
		return pe.Message
	}
	return fmt.Sprintf("not a refusal: %v", err)
}

// T26 (SQL): every refusal, by a direct INSERT as mhai_gateway and through
// mhai_grant_analyst.
func TestT26TriggerRefusals(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		setup func(w analystWorld) (h, grantor, person uuid.UUID)
		want  string
	}{
		{"self", func(w analystWorld) (uuid.UUID, uuid.UUID, uuid.UUID) {
			self := staffer(t, pool, w.h, staffOpts{roles: []string{"clinician", "hospital_admin"}})
			return w.h, self, self
		}, "self_change"},
		{"another hospital's admin", func(w analystWorld) (uuid.UUID, uuid.UUID, uuid.UUID) {
			return w.h, w.admin2, w.person
		}, "forbidden"},
		{"a grantor who isn't an admin", func(w analystWorld) (uuid.UUID, uuid.UUID, uuid.UUID) {
			return w.h, staffer(t, pool, w.h, staffOpts{roles: []string{"clinician"}}), w.person
		}, "forbidden"},
		{"an inactive admin", func(w analystWorld) (uuid.UUID, uuid.UUID, uuid.UUID) {
			return w.h, staffer(t, pool, w.h, staffOpts{roles: []string{"hospital_admin"}, inactive: true}), w.person
		}, "forbidden"},
		{"no grantor", func(w analystWorld) (uuid.UUID, uuid.UUID, uuid.UUID) {
			return w.h, uuid.Nil, w.person
		}, "forbidden"},
		{"no clinician", func(w analystWorld) (uuid.UUID, uuid.UUID, uuid.UUID) {
			return w.h, w.admin, staffer(t, pool, w.h, staffOpts{roles: []string{"hospital_admin"}})
		}, "not_eligible"},
		{"no two-step", func(w analystWorld) (uuid.UUID, uuid.UUID, uuid.UUID) {
			return w.h, w.admin, staffer(t, pool, w.h, staffOpts{roles: []string{"clinician"}, noMFA: true})
		}, "not_eligible"},
		{"an inactive person", func(w analystWorld) (uuid.UUID, uuid.UUID, uuid.UUID) {
			return w.h, w.admin, staffer(t, pool, w.h, staffOpts{roles: []string{"clinician"}, inactive: true})
		}, "not_eligible"},
		{"the switch off", func(w analystWorld) (uuid.UUID, uuid.UUID, uuid.UUID) {
			setSwitch(t, pool, &w.h, "off", "")
			return w.h, w.admin, w.person
		}, "analysis_off"},
		{"no switch row", func(w analystWorld) (uuid.UUID, uuid.UUID, uuid.UUID) {
			if _, err := pool.Exec(ctx, `DELETE FROM analysis_switch WHERE hospital_id = $1`, w.h); err != nil {
				t.Fatal(err)
			}
			return w.h, w.admin, w.person
		}, "analysis_off"},
		{"an 11th analyst", func(w analystWorld) (uuid.UUID, uuid.UUID, uuid.UUID) {
			for range 10 {
				staffer(t, pool, w.h, staffOpts{roles: []string{"clinician", "analyst"}})
			}
			return w.h, w.admin, w.person
		}, "analyst_cap"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newAnalystWorld(t, pool)
			h, grantor, person := c.setup(w)
			for _, how := range []string{
				`INSERT INTO user_roles (user_id, role) VALUES ($1, 'analyst')`,
				`SELECT mhai_grant_analyst($1)`,
			} {
				err := asPerson(ctx, pool, h, grantor, func(tx pgx.Tx) error {
					_, err := tx.Exec(ctx, how, person)
					return err
				})
				if got := refusal(err); got != c.want {
					t.Errorf("%s: want %s, got %s", how, c.want, got)
				}
				if isAnalyst(t, pool, person) {
					t.Fatalf("%s: refused, yet the person is an analyst", how)
				}
			}
		})
	}

	// Another hospital's person: not_found, by a direct INSERT (the rules
	// run before the row's policy would refuse it) and by the function.
	w := newAnalystWorld(t, pool)
	err := asPerson(ctx, pool, w.h2, w.admin2, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'analyst')`, w.person)
		return err
	})
	if got := refusal(err); got != "not_found" {
		t.Errorf("direct insert across hospitals: want not_found, got %s", got)
	}
	err = asPerson(ctx, pool, w.h2, w.admin2, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT mhai_grant_analyst($1)`, w.person)
		return err
	})
	if got := refusal(err); got != "not_found" {
		t.Errorf("grant across hospitals: want not_found, got %s", got)
	}
	// No UPDATE makes, moves or changes an analyst row.
	analyst := staffer(t, pool, w.h, staffOpts{roles: []string{"clinician", "analyst"}})
	for _, c := range []struct {
		q string
		u uuid.UUID
	}{
		{`UPDATE user_roles SET role = 'analyst' WHERE user_id = $1 AND role = 'clinician'`, w.person},
		{`UPDATE user_roles SET role = 'hospital_admin' WHERE user_id = $1 AND role = 'analyst'`, analyst},
	} {
		err := asPerson(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, c.q, c.u)
			return err
		})
		if got := refusal(err); got != "not_eligible" {
			t.Errorf("%s: want not_eligible, got %s", c.q, got)
		}
	}
	if isAnalyst(t, pool, w.person) || !isAnalyst(t, pool, analyst) {
		t.Error("an UPDATE changed an analyst row")
	}
}

// Every grant and removal commits only with its audit entry, in the same
// transaction (design §6.3; plan departure 25).
func TestGrantAuditedInSameTransaction(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newAnalystWorld(t, pool)

	// Unaudited, by the function or a direct INSERT: the commit is refused.
	for _, q := range []string{`SELECT mhai_grant_analyst($1)`, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'analyst')`} {
		err := asPerson(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, w.person)
			return err
		})
		if pgCode(err) != "23514" || isAnalyst(t, pool, w.person) {
			t.Errorf("%s without its entry: want the commit refused (23514), got %v", q, err)
		}
	}
	// An entry for another change doesn't count.
	err := asPerson(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT mhai_grant_analyst($1)`, w.person); err != nil {
			return err
		}
		return auditWrite(ctx, tx, w.admin.String(), w.h.String(), "analyst.granted", w.person.String(), uuid.NewString(), "app", "")
	})
	if pgCode(err) != "23514" || isAnalyst(t, pool, w.person) {
		t.Errorf("a grant with another change's entry: want 23514, got %v", err)
	}

	// Through the Go path: granted, audited, once.
	var changed bool
	err = db.WithHospitalUser(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
		var err error
		changed, err = analysis.GrantAnalyst(ctx, tx, w.person, "req-grant")
		return err
	})
	if err != nil || !changed || !isAnalyst(t, pool, w.person) {
		t.Fatalf("grant: changed %v, %v", changed, err)
	}
	var actor, hospital, entity, entityID, via, changeID string
	if err := pool.QueryRow(ctx, `SELECT actor_id::text, hospital_id::text, entity, entity_id, payload->>'via', payload->>'change_id'
		FROM audit_log WHERE action = 'analyst.granted' AND entity_id = $1`, w.person.String()).
		Scan(&actor, &hospital, &entity, &entityID, &via, &changeID); err != nil {
		t.Fatal(err)
	}
	if actor != w.admin.String() || hospital != w.h.String() || entity != "user" || via != "app" {
		t.Errorf("entry: actor %s hospital %s entity %s via %s", actor, hospital, entity, via)
	}
	if countRows(t, pool, `SELECT count(*) FROM analyst_changes WHERE id::text = $1 AND user_id = $2 AND by_user = $3 AND change = 'granted'`,
		changeID, w.person, w.admin) != 1 {
		t.Error("the entry names no change of this grant")
	}
	// Granting again changes nothing and writes nothing.
	err = db.WithHospitalUser(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
		var err error
		changed, err = analysis.GrantAnalyst(ctx, tx, w.person, "req-again")
		return err
	})
	if err != nil || changed {
		t.Errorf("second grant: changed %v, %v", changed, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM audit_log WHERE action = 'analyst.granted' AND entity_id = $1`, w.person.String()); n != 1 {
		t.Errorf("%d grant entries, want 1", n)
	}

	// Removal: only by an admin of the hospital; audited.
	clin := staffer(t, pool, w.h, staffOpts{roles: []string{"clinician"}})
	err = db.WithHospitalUser(ctx, pool, w.h, clin, func(tx pgx.Tx) error {
		_, err := analysis.RemoveAnalyst(ctx, tx, w.person, "req-x")
		return err
	})
	if !errors.Is(err, analysis.ErrAnalystForbidden) || !isAnalyst(t, pool, w.person) {
		t.Errorf("removal by a clinician: %v", err)
	}
	err = db.WithHospitalUser(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
		changed, err = analysis.RemoveAnalyst(ctx, tx, w.person, "req-remove")
		return err
	})
	if err != nil || !changed || isAnalyst(t, pool, w.person) {
		t.Fatalf("removal: changed %v, %v", changed, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM audit_log WHERE action = 'analyst.removed' AND entity_id = $1
		AND actor_id = $2 AND payload->>'reason' = 'removed'`, w.person.String(), w.admin); n != 1 {
		t.Errorf("%d removal entries, want 1", n)
	}
	// An unaudited direct DELETE is refused at commit too.
	staffAnalyst := staffer(t, pool, w.h, staffOpts{roles: []string{"clinician", "analyst"}})
	err = asPerson(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM user_roles WHERE user_id = $1 AND role = 'analyst'`, staffAnalyst)
		return err
	})
	if pgCode(err) != "23514" || !isAnalyst(t, pool, staffAnalyst) {
		t.Errorf("unaudited removal: want 23514, got %v", err)
	}
}

// auditWrite appends one analyst entry naming change id c.
func auditWrite(ctx context.Context, tx pgx.Tx, actor, h, action, person, c, via, reason string) error {
	p := map[string]any{"change_id": c, "via": via}
	if reason != "" {
		p["reason"] = reason
	}
	return audit.Write(ctx, tx, audit.Event{ActorID: actor, HospitalID: h, Action: action, Entity: "user", EntityID: person, Payload: p})
}

// N11: removing clinician removes analyst, by DELETE or by UPDATE, and the
// removal is audited (reason clinician_removed) before commit.
func TestRemovingClinicianRemovesAnalyst(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for _, q := range []string{
		`DELETE FROM user_roles WHERE user_id = $1 AND role = 'clinician'`,
		`DELETE FROM user_roles WHERE user_id = $1`,
		`UPDATE user_roles SET role = 'hospital_admin' WHERE user_id = $1 AND role = 'clinician'`,
	} {
		w := newAnalystWorld(t, pool)
		u := staffer(t, pool, w.h, staffOpts{roles: []string{"clinician", "analyst"}})
		// Without the entries the whole change is refused.
		err := asPerson(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, u)
			return err
		})
		if pgCode(err) != "23514" || !isAnalyst(t, pool, u) {
			t.Errorf("%s unaudited: want 23514, got %v", q, err)
		}
		var n int
		err = db.WithHospitalUser(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, q, u); err != nil {
				return err
			}
			var err error
			n, err = analysis.AuditAnalystChanges(ctx, tx, "req-n11")
			return err
		})
		if err != nil || n != 1 || isAnalyst(t, pool, u) {
			t.Fatalf("%s: %d entries, analyst %v, %v", q, n, isAnalyst(t, pool, u), err)
		}
		if c := countRows(t, pool, `SELECT count(*) FROM audit_log WHERE action = 'analyst.removed' AND entity_id = $1
			AND payload->>'reason' = 'clinician_removed' AND actor_id = $2 AND hospital_id = $3`, u.String(), w.admin, w.h); c != 1 {
			t.Errorf("%s: %d clinician_removed entries, want 1", q, c)
		}
	}
}

// The trigger leaves 00003's other rules alone: clinical roles come and
// go with no analyst change and no audit demanded, and the pairing rules
// still refuse platform roles on a hospital account.
func TestPlatformRolesUntouched(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newAnalystWorld(t, pool)
	u := staffer(t, pool, w.h, staffOpts{})
	err := asPerson(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
		for _, q := range []string{
			`INSERT INTO user_roles (user_id, role) VALUES ($1, 'clinician')`,
			`INSERT INTO user_roles (user_id, role) VALUES ($1, 'hospital_admin')`,
			`DELETE FROM user_roles WHERE user_id = $1 AND role = 'hospital_admin'`,
			`DELETE FROM user_roles WHERE user_id = $1 AND role = 'clinician'`,
		} {
			if _, err := tx.Exec(ctx, q, u); err != nil {
				return fmt.Errorf("%s: %w", q, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM analyst_changes WHERE user_id = $1`, u); n != 0 {
		t.Errorf("%d analyst changes for clinical roles", n)
	}
	err = asPerson(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'platform_admin')`, u)
		return err
	})
	requireDenied(t, err, "gateway granting platform_admin")
}

// The cap holds under concurrency: 8 analysts, 6 parallel grants, exactly
// 2 succeed.
func TestAnalystCapRace(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newAnalystWorld(t, pool)
	for range 8 {
		staffer(t, pool, w.h, staffOpts{roles: []string{"clinician", "analyst"}})
	}
	var people []uuid.UUID
	for range 6 {
		people = append(people, staffer(t, pool, w.h, staffOpts{roles: []string{"clinician"}}))
	}
	var wg sync.WaitGroup
	errs := make([]error, len(people))
	for i, p := range people {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = db.WithHospitalUser(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
				_, err := analysis.GrantAnalyst(ctx, tx, p, "req-race")
				return err
			})
		}()
	}
	wg.Wait()
	ok, capped := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, analysis.ErrAnalystCap):
			capped++
		default:
			t.Errorf("grant: %v", err)
		}
	}
	n := countRows(t, pool, `SELECT count(*) FROM user_roles r JOIN users u ON u.id = r.user_id WHERE u.hospital_id = $1 AND r.role = 'analyst'`, w.h)
	if ok != 2 || capped != 4 || n != 10 {
		t.Errorf("%d granted, %d capped, %d analysts; want 2, 4, 10", ok, capped, n)
	}
}

// The CLI's path, as mhai_provision: the lookup refuses an admin of
// another hospital, and the grant's entry is by the --by admin.
func TestProvisionGrantPath(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newAnalystWorld(t, pool)
	var personEmail, adminEmail, admin2Email string
	for _, p := range []struct {
		id  uuid.UUID
		out *string
	}{{w.person, &personEmail}, {w.admin, &adminEmail}, {w.admin2, &admin2Email}} {
		if err := pool.QueryRow(ctx, `SELECT email::text FROM users WHERE id = $1`, p.id).Scan(p.out); err != nil {
			t.Fatal(err)
		}
	}
	asProvision := func(fn func(tx pgx.Tx) error) error {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_provision`); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	err := asProvision(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT * FROM mhai_analyst_cli_users($1, $2)`, personEmail, admin2Email)
		return err
	})
	if refusal(err) != "forbidden" {
		t.Errorf("an admin of another hospital: %v", err)
	}
	err = asProvision(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT * FROM mhai_analyst_cli_users($1, $2)`, "nobody-"+uniqueCode("x")+"@example.test", adminEmail)
		return err
	})
	if refusal(err) != "not_found" {
		t.Errorf("an unknown person: %v", err)
	}
	// operator_id alone isn't trusted from the gateway: as the gateway the
	// grantor is the token's user.
	err = asPerson(ctx, pool, w.h, uuid.Nil, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.operator_id', $1, true)`, w.admin.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT mhai_grant_analyst($1)`, w.person)
		return err
	})
	if refusal(err) != "forbidden" {
		t.Errorf("the gateway with only an operator id: %v", err)
	}
	err = asProvision(func(tx pgx.Tx) error {
		var person, hospital, by uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT person, hospital_id, by_user FROM mhai_analyst_cli_users($1, $2)`, personEmail, adminEmail).
			Scan(&person, &hospital, &by); err != nil {
			return err
		}
		if person != w.person || hospital != w.h || by != w.admin {
			return fmt.Errorf("lookup %s %s %s", person, hospital, by)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.operator_id', $1, true)`, by.String()); err != nil {
			return err
		}
		_, err := analysis.GrantAnalyst(ctx, tx, person, "")
		return err
	})
	if err != nil || !isAnalyst(t, pool, w.person) {
		t.Fatalf("provision grant: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM audit_log WHERE action = 'analyst.granted' AND entity_id = $1
		AND actor_id = $2 AND hospital_id = $3 AND payload->>'via' = 'cli'`, w.person.String(), w.admin, w.h); n != 1 {
		t.Errorf("%d CLI grant entries, want 1", n)
	}
	// The provisioning role writes no other analyst entry than its own.
	err = asProvision(func(tx pgx.Tx) error {
		return auditWrite(ctx, tx, w.admin2.String(), w.h.String(), "analyst.granted", w.person.String(), uuid.NewString(), "cli", "")
	})
	if pgCode(err) != rlsViolation {
		t.Errorf("a provisioning entry by someone not the operator: want 42501, got %v", err)
	}
}

// Nobody reads or writes analyst_changes but the rules' owner.
func TestAnalystChangesClosed(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for _, role := range []string{"mhai_gateway", "mhai_platform", "mhai_provision", "mhai_auth", "mhai_worker", "mhai_analysis_reader"} {
		requireDenied(t, asRole(ctx, pool, role, `SELECT count(*) FROM analyst_changes`), role+" reading analyst_changes")
	}
	w := newAnalystWorld(t, pool)
	err := asPerson(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO analyst_changes (hospital_id, user_id, change, reason, via) VALUES ($1, $2, 'granted', 'granted', 'app')`,
			w.h, w.person)
		return err
	})
	requireDenied(t, err, "gateway writing analyst_changes")
	// The helpers are the owner's alone.
	for _, f := range []string{`mhai_analyst_record($1, 'granted', 'granted')`, `mhai_analyst_lock($1)`} {
		err := asPerson(ctx, pool, w.h, w.admin, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT `+f, w.person)
			return err
		})
		requireDenied(t, err, "gateway calling "+f)
	}
}

// A login that can reach the rules' owner is refused at start.
func TestCheckLoginRoleRefusesAnalystRulesReach(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	mid := "mid_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := pool.Exec(ctx, `CREATE ROLE `+mid+` NOLOGIN`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `GRANT mhai_analyst_rules TO `+mid+` WITH INHERIT FALSE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `REVOKE mhai_analyst_rules FROM `+mid) })
	_, p := loginPool(t, "IN ROLE mhai_app, "+mid)
	if err := db.CheckLoginRole(ctx, p); err == nil || !strings.Contains(err.Error(), "mhai_analyst_rules") {
		t.Errorf("a login reaching mhai_analyst_rules: %v", err)
	}
}

// 00030 goes down to the migration before it and up again, leaving
// nothing behind.
func TestMigration00030DownUp(t *testing.T) {
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
	var before string
	if err := pool.QueryRow(ctx, `SELECT string_agg(polname || ':' || pg_get_expr(polwithcheck, polrelid), ';' ORDER BY polname)
		FROM pg_policy WHERE polrelid = 'audit_log'::regclass AND polname IN ('provision_append', 'platform_append')`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 29); err != nil {
		t.Fatalf("down to 29: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_class WHERE relname IN ('analyst_changes', 'audit_log_analyst_change_idx'))
		+ (SELECT count(*) FROM pg_proc WHERE proname LIKE 'mhai_analyst%' OR proname IN ('mhai_grant_analyst', 'mhai_remove_analyst'))
		+ (SELECT count(*) FROM pg_trigger WHERE tgname LIKE 'user_roles_analyst%')
		+ (SELECT count(*) FROM pg_policy WHERE polname LIKE 'analyst_rules%' OR polname = 'provision_analyst_append')
		+ (SELECT count(*) FROM unnest(ARRAY['users', 'user_mfa', 'user_roles', 'audit_log']) t
		   WHERE has_any_column_privilege('mhai_analyst_rules', t, 'SELECT'))
		+ (SELECT count(*) WHERE has_function_privilege('mhai_analyst_rules', 'mhai_analysis_position(uuid)', 'EXECUTE'))`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("after down, %d objects or grants of 00030 remain", left)
	}
	var after string
	if err := pool.QueryRow(ctx, `SELECT string_agg(polname || ':' || pg_get_expr(polwithcheck, polrelid), ';' ORDER BY polname)
		FROM pg_policy WHERE polrelid = 'audit_log'::regclass AND polname IN ('provision_append', 'platform_append')`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("the audit appends changed:\n%s\nwant\n%s", after, before)
	}
	// An analyst row without clinician, from before the rules: Up refuses
	// until it is fixed.
	h := uuid.New()
	var u uuid.UUID
	if _, err := pool.Exec(ctx, `INSERT INTO hospitals (id, name, short_code, status, time_zone) VALUES ($1, 'Synthetic', $2, 'active', 'UTC')`,
		h, uniqueCode("M")); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users (hospital_id, email, display_name) VALUES ($1, $2, 'Synthetic Staff') RETURNING id`,
		h, uniqueCode("m")+"@example.test").Scan(&u); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'analyst')`, u); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err == nil || !strings.Contains(err.Error(), "without the clinician role") {
		t.Fatalf("up with an analyst without clinician: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'clinician')`, u); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// Security review: a provisioning login that inherits mhai_provision
// without SET ROLE can't pose as the gateway (it has no grantor), and the
// provisioning role writes no analyst entry without a change of its own
// transaction behind it.
func TestProvisionCantPoseOrForge(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newAnalystWorld(t, pool)
	_, prov := loginPool(t, "IN ROLE mhai_provision")
	tx, err := prov.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"mhai.user_id", "mhai.operator_id"} {
		if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, k, w.admin.String()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, w.h.String()); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `SELECT mhai_grant_analyst($1)`, w.person)
	_ = tx.Rollback(ctx)
	if refusal(err) != "forbidden" {
		t.Errorf("an inheriting provisioning login posing as the app: %v", err)
	}
	// As mhai_provision with the operator set: no entry without its change.
	ptx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ptx.Rollback(ctx) }()
	if _, err := ptx.Exec(ctx, `SET LOCAL ROLE mhai_provision`); err != nil {
		t.Fatal(err)
	}
	if _, err := ptx.Exec(ctx, `SELECT set_config('mhai.operator_id', $1, true)`, w.admin.String()); err != nil {
		t.Fatal(err)
	}
	err = auditWrite(ctx, ptx, w.admin.String(), w.h.String(), "analyst.granted", w.person.String(), uuid.NewString(), "cli", "")
	if pgCode(err) != rlsViolation {
		t.Errorf("a provisioning entry with no change behind it: want 42501, got %v", err)
	}
	if isAnalyst(t, pool, w.person) {
		t.Fatal("granted")
	}
}

// Security review: a grant refuses any isolation but READ COMMITTED (the
// lock then gives fresh reads), and a caller who isn't an admin learns
// nothing of whether the person already holds the role.
func TestGrantNeedsReadCommittedAndAdminFirst(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newAnalystWorld(t, pool)
	for _, iso := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{`SET LOCAL ROLE mhai_gateway`} {
			if _, err := tx.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true), set_config('mhai.user_id', $2, true)`,
			w.h.String(), w.admin.String()); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, `SELECT mhai_grant_analyst($1)`, w.person)
		_ = tx.Rollback(ctx)
		if refusal(err) != "not_eligible" {
			t.Errorf("%s: want not_eligible, got %v", iso, err)
		}
	}
	held := staffer(t, pool, w.h, staffOpts{roles: []string{"clinician", "analyst"}})
	clin := staffer(t, pool, w.h, staffOpts{roles: []string{"clinician"}})
	err := asPerson(ctx, pool, w.h, clin, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT mhai_grant_analyst($1)`, held)
		return err
	})
	if refusal(err) != "forbidden" {
		t.Errorf("a clinician asking about an analyst: want forbidden, got %v", err)
	}
}
