//go:build integration

package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00031 (EMR plan Tasks 35-37): what core's finding consumer
// needs. Every name and value is synthetic.

// An alert names an active patient of its hospital: an excluded or merged
// patient is refused, whoever writes; clearing the patient is allowed.
func TestAlertPatientMustBeActive(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	f := newAlertFixture(t, pool)
	a := seedAlert(t, pool, f, "r1", "critical", true)
	for _, status := range []string{"excluded", "merged_into"} {
		p := seedPatient(t, pool, f.h, f.conn, "p-"+uuid.NewString()[:8], "MRN-AP")
		if status == "merged_into" {
			if _, err := pool.Exec(ctx, `UPDATE emr.patients SET status = 'merged_into', merged_into = $2 WHERE id = $1`,
				p, f.patient); err != nil {
				t.Fatal(err)
			}
		} else if _, err := pool.Exec(ctx, `UPDATE emr.patients SET status = 'excluded', excluded_at = now(),
			exclude_reason = 'objection' WHERE id = $1`, p); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO alerts (hospital_id, patient_id, rule_id, category, severity, routed_queue)
			VALUES ($1, $2, 'r2', 'blood_sugar', 'critical', true)`, f.h, p); pgCode(err) != checkViolation {
			t.Errorf("alert for a %s patient: %v", status, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE alerts SET patient_id = $2 WHERE id = $1`, a, p); pgCode(err) != checkViolation {
			t.Errorf("alert moved to a %s patient: %v", status, err)
		}
	}
	// Another hospital's patient is refused too.
	other, _ := seedHospital(t, pool, uniqueCode("AP"))
	oc := seedConn(t, pool, other)
	op := seedPatient(t, pool, other, oc, "p-other", "MRN-AP")
	if _, err := pool.Exec(ctx, `UPDATE alerts SET patient_id = $2 WHERE id = $1`, a, op); err == nil {
		t.Error("alert moved to another hospital's patient")
	}
	// A role bound to hospital A can't use the check on B's patient.
	if err := asRoleIn(ctx, pool, "mhai_emr_core", f.h, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alerts (hospital_id, patient_id, rule_id, category, severity, routed_queue)
			VALUES ($1, $2, 'r3', 'blood_sugar', 'critical', true)`, other, op)
		return err
	}); pgCode(err) != permissionDenied {
		t.Errorf("cross-hospital alert through the check: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE alerts SET patient_id = NULL, finding_id = NULL WHERE id = $1`, a); err != nil {
		t.Fatalf("clearing the patient: %v", err)
	}
}

// Design §11.4: a handled alert can carry the withdrawn mark and keeps its
// handler; a new alert can't be withdrawn without being dismissed.
func TestHandledAlertKeepsWithdrawnMark(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	f := newAlertFixture(t, pool)
	a := seedAlert(t, pool, f, "r1", "critical", true)
	if _, err := pool.Exec(ctx, `UPDATE alerts SET withdrawn = 'source_amended' WHERE id = $1`, a); pgCode(err) != checkViolation {
		t.Fatalf("a new alert withdrawn: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE alerts SET status = 'handled', handled_by = $2, handled_at = now() WHERE id = $1`, a, f.u); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE alerts SET withdrawn = 'source_retracted' WHERE id = $1`, a); err != nil {
		t.Fatalf("a handled alert marked: %v", err)
	}
	// Still no handled alert without a handler or a withdrawal.
	b := seedAlert(t, pool, f, "r2", "critical", true)
	if _, err := pool.Exec(ctx, `UPDATE alerts SET status = 'handled', handled_at = now() WHERE id = $1`, b); pgCode(err) != checkViolation {
		t.Fatalf("handled by nobody: %v", err)
	}
}

// emr_consumed: its hospital's rows only, for core's consumer role; no
// other role holds it; the consumer appends only its own actions to the
// chain, for its own hospital, with no actor.
func TestAlertConsumerGrants(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	f := newAlertFixture(t, pool)
	other, _ := seedHospital(t, pool, uniqueCode("AC"))
	if err := asRoleIn(ctx, pool, "mhai_emr_core", f.h, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO emr_consumed (hospital_id, kind, ref_id, status, outcome) VALUES ($1, 'finding', $2, 'open', 'raised')`,
			f.h, f.finding)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := asRoleIn(ctx, pool, "mhai_emr_core", other, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM emr_consumed`).Scan(&n); err != nil || n != 0 {
			t.Errorf("another hospital sees %d consumed rows: %v", n, err)
		}
		_, err := tx.Exec(ctx, `INSERT INTO emr_consumed (hospital_id, kind, ref_id, status, outcome) VALUES ($1, 'finding', $2, 'open', 'raised')`,
			f.h, uuid.New())
		return err
	}); pgCode(err) != rlsViolation {
		t.Fatalf("cross-hospital consumed row: %v", err)
	}
	for _, q := range []string{`UPDATE emr_consumed SET outcome = 'no_change'`, `DELETE FROM emr_consumed`} {
		err := asRoleIn(ctx, pool, "mhai_emr_core", f.h, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
		requireDenied(t, err, "emr_core: "+q)
	}
	for _, role := range []string{"mhai_gateway", "mhai_emr", "mhai_platform", "mhai_worker"} {
		err := asRoleIn(ctx, pool, role, f.h, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT count(*) FROM emr_consumed`)
			return err
		})
		requireDenied(t, err, role+" reading emr_consumed")
	}
	// Each append is rolled back, so the shared chain never holds a row
	// with a made-up hash.
	errRollback := errors.New("rolled back")
	appendAs := func(h uuid.UUID, hospital any, actor any, action string) error {
		err := asRoleIn(ctx, pool, "mhai_emr_core", h, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO audit_log (ts, actor_id, hospital_id, action, payload, prev_hash, hash)
				VALUES (now(), $1, $2, $3, '{}', 'x', $4)`, actor, hospital, action, uuid.NewString()); err != nil {
				return err
			}
			return errRollback
		})
		if errors.Is(err, errRollback) {
			return nil
		}
		return err
	}
	if err := appendAs(f.h, f.h, nil, "alert.raised"); err != nil {
		t.Fatalf("own action: %v", err)
	}
	for name, err := range map[string]error{
		"another action":   appendAs(f.h, f.h, nil, "alert.acknowledged"),
		"another hospital": appendAs(f.h, other, nil, "alert.raised"),
		"an actor":         appendAs(f.h, f.h, f.u, "alert.raised"),
		"no hospital":      appendAs(f.h, nil, nil, "emr.source_paused"),
	} {
		if pgCode(err) != rlsViolation {
			t.Errorf("%s: %v", name, err)
		}
	}
	var del bool
	if err := pool.QueryRow(ctx, `SELECT has_table_privilege('mhai_emr_core', 'audit_log', 'UPDATE,DELETE')`).Scan(&del); err != nil || del {
		t.Fatalf("emr_core may change the chain: %v %v", del, err)
	}
}

// alert_route_facts and alert_source_known answer for the caller's
// hospital only, and only to core's consumer role.
func TestAlertRouteFactsOwnHospital(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	f := newAlertFixture(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO alert_recipients (hospital_id, user_id) VALUES ($1, $2), ($1, $3)`, f.h, f.u, f.v); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1`, f.v); err != nil {
		t.Fatal(err)
	}
	other, _ := seedHospital(t, pool, uniqueCode("RF"))
	facts := func(h uuid.UUID) (string, []uuid.UUID, bool) {
		var hospital string
		var users []uuid.UUID
		var known bool
		if err := asRoleIn(ctx, pool, "mhai_emr_core", h, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT r.hospital, r.recipients, alert_source_known($2) FROM alert_route_facts($1) r`,
				f.patient, f.conn).Scan(&hospital, &users, &known)
		}); err != nil {
			t.Fatal(err)
		}
		return hospital, users, known
	}
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET enabled = true WHERE id = $1`, f.conn); err != nil {
		t.Fatal(err)
	}
	if h, users, known := facts(f.h); h != "ok" || len(users) != 1 || users[0] != f.u || !known {
		t.Fatalf("own hospital: %s %v %v", h, users, known)
	}
	if _, err := pool.Exec(ctx, `UPDATE hospitals SET status = 'active' WHERE id = $1`, other); err != nil {
		t.Fatal(err)
	}
	if h, users, known := facts(other); h != "no_connection" || len(users) != 0 || known {
		t.Fatalf("another hospital: %s %v %v", h, users, known)
	}
	for _, role := range []string{"mhai_gateway", "mhai_emr", "mhai_platform"} {
		err := asRoleIn(ctx, pool, role, f.h, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT * FROM alert_route_facts($1)`, f.patient)
			return err
		})
		requireDenied(t, err, role+" calling alert_route_facts")
	}
}

func TestMigration00031DownUp(t *testing.T) {
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
	// A handled alert with the withdrawn mark: the down takes the mark off.
	f := newAlertFixture(t, pool)
	a := seedAlert(t, pool, f, "r1", "critical", true)
	if _, err := pool.Exec(ctx, `UPDATE alerts SET status = 'handled', handled_by = $2, handled_at = now(),
		withdrawn = 'source_amended' WHERE id = $1`, a, f.u); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 29); err != nil {
		t.Fatalf("down to 29: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_class WHERE relname = 'emr_consumed')
		+ (SELECT count(*) FROM pg_proc WHERE proname IN ('alert_route_facts', 'alert_source_known', 'mhai_alert_patient_check'))
		+ (SELECT count(*) FROM pg_policy WHERE polname IN ('emr_core_append', 'owner_alert_read'))`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("after down: %d objects left", left)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
