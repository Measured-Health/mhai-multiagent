//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00033 (EMR plan Task 48, PR 13): a question may name an MHAI
// patient. Every name and value is synthetic.

// seedLonePatient inserts an active patient with no link, as the superuser.
func seedLonePatient(t *testing.T, pool *pgxpool.Pool, hospital uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `INSERT INTO emr.patients (hospital_id, display_name, mrn, birth_date, sex)
		VALUES ($1, 'Synthetic Quillon Varga', $2, '1961-04-17', 'female') RETURNING id`,
		hospital, "SYN-QP-"+uuid.NewString()[:13]).Scan(&id); err != nil {
		t.Fatalf("seed patient: %v", err)
	}
	return id
}

// insertQuestion inserts a question naming patient as core's clinical role
// bound to hospital.
func insertQuestion(ctx context.Context, pool *pgxpool.Pool, hospital, patient uuid.UUID) (uuid.UUID, error) {
	id := uuid.New()
	err := asRoleIn(ctx, pool, "mhai_gateway", hospital, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, patient_id)
			VALUES ($1, $2, 'synthetic question', 'routine', $3)`, id, hospital, patient)
		return err
	})
	return id, err
}

func questionPatient(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) *uuid.UUID {
	t.Helper()
	var p *uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT patient_id FROM clinical_queries WHERE id = $1`, id).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A question names an active patient of its own hospital only: another
// hospital's patient, an excluded one or a merged one is refused, whoever
// writes, and a role bound to one hospital can't name another's.
func TestQueryPatientMustBeActiveOwnHospital(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("QP"))
	b, _ := seedHospital(t, pool, uniqueCode("QP"))
	own := seedLonePatient(t, pool, a)
	if _, err := insertQuestion(ctx, pool, a, own); err != nil {
		t.Fatalf("own active patient: %v", err)
	}
	theirs := seedLonePatient(t, pool, b)
	if _, err := insertQuestion(ctx, pool, a, theirs); err == nil {
		t.Error("a question named another hospital's patient")
	}
	// The superuser too: the key and the check hold for every writer.
	if _, err := pool.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, patient_id)
		VALUES ($1, $2, 'synthetic question', 'routine', $3)`, uuid.New(), a, theirs); err == nil {
		t.Error("the superuser named another hospital's patient")
	}
	// A role bound to hospital A writing a row of B is refused before the
	// patient is looked at.
	if err := asRoleIn(ctx, pool, "mhai_gateway", a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, patient_id)
			VALUES ($1, $2, 'synthetic question', 'routine', $3)`, uuid.New(), b, theirs)
		return err
	}); pgCode(err) != permissionDenied {
		t.Errorf("cross-hospital question through the check: %v", err)
	}
	gone := seedLonePatient(t, pool, a)
	if _, err := pool.Exec(ctx, `UPDATE emr.patients SET status = 'excluded', excluded_at = now(), exclude_reason = 'objection'
		WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := insertQuestion(ctx, pool, a, gone); pgCode(err) != checkViolation {
		t.Errorf("excluded patient: %v", err)
	}
	merged := seedLonePatient(t, pool, a)
	if _, err := pool.Exec(ctx, `UPDATE emr.patients SET status = 'merged_into', merged_into = $2 WHERE id = $1`, merged, own); err != nil {
		t.Fatal(err)
	}
	if _, err := insertQuestion(ctx, pool, a, merged); pgCode(err) != checkViolation {
		t.Errorf("merged patient: %v", err)
	}
	if _, err := insertQuestion(ctx, pool, a, uuid.New()); err == nil {
		t.Error("a question named no patient at all")
	}
}

// A patient that stops being active is unlinked from its questions at
// once; a deleted patient is too; other questions keep theirs.
func TestQueryPatientUnlinkedWhenNotActive(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("QU"))
	keep := seedLonePatient(t, pool, a)
	excl := seedLonePatient(t, pool, a)
	merged := seedLonePatient(t, pool, a)
	deleted := seedLonePatient(t, pool, a)
	ids := map[string]uuid.UUID{}
	for name, p := range map[string]uuid.UUID{"keep": keep, "excl": excl, "merged": merged, "deleted": deleted} {
		id, err := insertQuestion(ctx, pool, a, p)
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	// The hospital admin's definers run as the owner, bound to the hospital.
	if err := asRoleIn(ctx, pool, "mhai_gateway", a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT * FROM emr_exclude_patient($1, 'objection')`, excl)
		return err
	}); err != nil {
		t.Fatalf("exclude: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE emr.patients SET status = 'merged_into', merged_into = $2 WHERE id = $1`, merged, keep); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM emr.patients WHERE id = $1`, deleted); err != nil {
		t.Fatalf("delete patient: %v", err)
	}
	for _, name := range []string{"excl", "merged", "deleted"} {
		if p := questionPatient(t, pool, ids[name]); p != nil {
			t.Errorf("%s: question still names the patient", name)
		}
	}
	if p := questionPatient(t, pool, ids["keep"]); p == nil || *p != keep {
		t.Errorf("an active patient's question lost it: %v", p)
	}
}

// Who holds the column: core's jobs role reads it (to re-mint) and can't
// change it; the clinical role can't change it either.
func TestQueryPatientGrants(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("QG"))
	p := seedLonePatient(t, pool, a)
	other := seedLonePatient(t, pool, a)
	id, err := insertQuestion(ctx, pool, a, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := asRoleIn(ctx, pool, "mhai_jobs", a, func(tx pgx.Tx) error {
		var got uuid.UUID
		return tx.QueryRow(ctx, `SELECT patient_id FROM clinical_queries WHERE id = $1`, id).Scan(&got)
	}); err != nil {
		t.Fatalf("jobs reading patient_id: %v", err)
	}
	for _, role := range []string{"mhai_jobs", "mhai_gateway"} {
		err := asRoleIn(ctx, pool, role, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE clinical_queries SET patient_id = $2 WHERE id = $1`, id, other)
			return err
		})
		requireDenied(t, err, role+" changing patient_id")
	}
	for _, fn := range []string{"mhai_query_patient_check()", "mhai_query_patient_unlink()"} {
		for _, role := range []string{"mhai_gateway", "mhai_jobs", "mhai_worker", "mhai_emr"} {
			var ok bool
			if err := pool.QueryRow(ctx, `SELECT has_function_privilege($1, $2, 'EXECUTE')`, role, fn).Scan(&ok); err != nil || ok {
				t.Errorf("%s may execute %s (%v)", role, fn, err)
			}
		}
	}
	// Both definers: owner mhai_migrate, search_path pinned.
	for _, name := range []string{"mhai_query_patient_check", "mhai_query_patient_unlink"} {
		var owner string
		var definer bool
		var config []string
		if err := pool.QueryRow(ctx, `SELECT pg_get_userbyid(proowner), prosecdef, coalesce(proconfig, '{}') FROM pg_proc WHERE proname = $1`,
			name).Scan(&owner, &definer, &config); err != nil {
			t.Fatal(err)
		}
		if owner != "mhai_migrate" || !definer || len(config) != 1 || config[0] != "search_path=pg_catalog, pg_temp" {
			t.Errorf("%s: owner %s definer %v config %v", name, owner, definer, config)
		}
	}
}

func TestMigration00033DownUp(t *testing.T) {
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
	a, _ := seedHospital(t, pool, uniqueCode("QD"))
	if _, err := insertQuestion(ctx, pool, a, seedLonePatient(t, pool, a)); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 32); err != nil {
		t.Fatalf("down to 32: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM information_schema.columns
		                                WHERE table_name = 'clinical_queries' AND column_name = 'patient_id')
		+ (SELECT count(*) FROM pg_proc WHERE proname IN ('mhai_query_patient_check', 'mhai_query_patient_unlink'))
		+ (SELECT count(*) FROM pg_policy WHERE polname IN ('patient_unlink', 'patient_unlink_read'))
		+ (SELECT count(*) FROM pg_trigger WHERE tgname IN ('clinical_queries_patient_check', 'patients_unlink_queries'))`).
		Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("after down: %d objects left", left)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
