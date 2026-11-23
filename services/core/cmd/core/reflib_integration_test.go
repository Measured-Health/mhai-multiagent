//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

func reflibCount(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// core reference-library load, as core-cli runs it (the provisioning
// login): the shared rows land with no hospital and no patient fields, the
// health check's guidelines part turns ok, the load is audited, a second
// load changes nothing, a new version replaces the old, and other bytes
// under a loaded version are refused. Hospital rows and the seeded
// guidelines are never touched.
func TestReflibLoadAsTheProvisioningLogin(t *testing.T) {
	pg, super := testutil.MigratedPostgres(t)
	ctx := context.Background()
	_, provURL, err := testutil.ProvisionLogin(ctx, pg, super)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := seed(ctx, []string{"hospitals", "create", "--name", "Synthetic General", "--short-code", "syn", "--time-zone", "Africa/Lagos"},
		provURL, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type)
		SELECT 'KB#77', id, 'A hospital''s own synthetic row.', 'guideline' FROM hospitals WHERE short_code = 'SYN'`); err != nil {
		t.Fatal(err)
	}
	var ok bool
	if err := super.QueryRow(ctx, `SELECT mhai_health_guidelines()`).Scan(&ok); err != nil || ok {
		t.Fatalf("guidelines before the load: %v (%v)", ok, err)
	}

	v1, pins1 := testReflibArtefact(t, "1.0.0", testReflibRows())
	out.Reset()
	if err := reflibLoad(ctx, v1, provURL, pins1, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "loaded medref 1.0.0: 3 shared rows") || strings.Contains(out.String(), "Synthetamol") {
		t.Fatalf("output: %s", out.String())
	}
	if n := reflibCount(t, super, `SELECT count(*) FROM kb_chunks k JOIN reference_libraries l ON l.id = k.library_id
		WHERE l.active AND k.hospital_id IS NULL AND k.source_patient_id IS NULL AND k.source_subject_id IS NULL
		AND k.chunk_type = 'guideline' AND cardinality(k.embedding) = 4 AND k.metadata_json->>'title' <> ''`); n != 3 {
		t.Fatalf("library rows = %d", n)
	}
	if err := super.QueryRow(ctx, `SELECT mhai_health_guidelines()`).Scan(&ok); err != nil || !ok {
		t.Fatalf("guidelines after the load: %v (%v)", ok, err)
	}
	audited := `SELECT count(*) FROM audit_log WHERE action = 'reference_library.loaded' AND actor_id IS NULL AND hospital_id IS NULL
		AND payload->>'version' = $1`
	if n := reflibCount(t, super, audited, "1.0.0"); n != 1 {
		t.Fatalf("audit entries = %d", n)
	}

	// Again: nothing changes, no second audit entry.
	out.Reset()
	if err := reflibLoad(ctx, v1, provURL, pins1, &out); err != nil || !strings.Contains(out.String(), "already the active version") {
		t.Fatalf("reload: %v %s", err, out.String())
	}
	if n := reflibCount(t, super, audited, "1.0.0"); n != 1 {
		t.Fatalf("audit entries after a reload = %d", n)
	}

	// Other bytes under the loaded version are refused.
	rows := testReflibRows()
	rows[0].Content += " Changed."
	other, otherPins := testReflibArtefact(t, "1.0.0", rows)
	wantRefused(t, reflibLoad(ctx, other, provURL, otherPins, &out), "version_reloaded_with_other_bytes")

	// A new version replaces the old one's rows.
	v2, pins2 := testReflibArtefact(t, "1.1.0", testReflibRows()[:2])
	out.Reset()
	if err := reflibLoad(ctx, v2, provURL, pins2, &out); err != nil || !strings.Contains(out.String(), "replaced 1.0.0") {
		t.Fatalf("v2: %v %s", err, out.String())
	}
	if n := reflibCount(t, super, `SELECT count(*) FROM kb_chunks WHERE library_id IS NOT NULL`); n != 2 {
		t.Fatalf("rows after v2 = %d", n)
	}
	if n := reflibCount(t, super, `SELECT count(*) FROM reference_libraries WHERE active`); n != 1 {
		t.Fatalf("active libraries = %d", n)
	}
	if n := reflibCount(t, super, audited, "1.1.0"); n != 1 {
		t.Fatalf("v2 audit entries = %d", n)
	}
	// The hospital's own row is untouched.
	if n := reflibCount(t, super, `SELECT count(*) FROM kb_chunks WHERE hospital_id IS NOT NULL`); n != 1 {
		t.Fatalf("hospital rows = %d", n)
	}
	out.Reset()
	if err := reflibStatus(ctx, provURL, &out); err != nil || !strings.Contains(out.String(), "medref 1.1.0 active: 2 rows") ||
		!strings.Contains(out.String(), "medref 1.0.0 retired") {
		t.Fatalf("status: %v %s", err, out.String())
	}
	if ok, broken, err := audit.Verify(ctx, super); err != nil || !ok {
		t.Fatalf("audit chain: ok=%v broken=%d (%v)", ok, broken, err)
	}
}

// The provisioning login reaches library rows only (00056): it can't read
// a row's text, see or delete a hospital's row or a seeded guideline, or
// write a shared row outside a library or with a patient field.
func TestReflibProvisionReachesLibraryRowsOnly(t *testing.T) {
	pg, super := testutil.MigratedPostgres(t)
	ctx := context.Background()
	prov, provURL, err := testutil.ProvisionLogin(ctx, pg, super)
	if err != nil {
		t.Fatal(err)
	}
	defer prov.Close()
	if _, err := super.Exec(ctx, `INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type)
		VALUES ('KB#0001', NULL, 'A seeded synthetic guideline.', 'guideline')`); err != nil {
		t.Fatal(err)
	}
	v1, pins1 := testReflibArtefact(t, "1.0.0", testReflibRows())
	if err := reflibLoad(ctx, v1, provURL, pins1, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	asProv := func(q string, args ...any) (int64, error) {
		tx, err := prov.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_provision`); err != nil {
			t.Fatal(err)
		}
		tag, err := tx.Exec(ctx, q, args...)
		return tag.RowsAffected(), err
	}
	if _, err := asProv(`SELECT content FROM kb_chunks`); err == nil {
		t.Error("mhai_provision read row text")
	}
	if n, err := asProv(`SELECT id FROM kb_chunks`); err != nil || n != 3 {
		t.Errorf("mhai_provision sees %d rows (%v), want the 3 library rows", n, err)
	}
	if n, err := asProv(`DELETE FROM kb_chunks WHERE library_id IS NULL`); err != nil || n != 0 {
		t.Errorf("mhai_provision deleted %d seeded rows (%v)", n, err)
	}
	if _, err := asProv(`INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type) VALUES ('KB#0009', NULL, 'x', 'guideline')`); err == nil {
		t.Error("mhai_provision wrote a shared row outside a library")
	}
	if _, err := asProv(`INSERT INTO kb_chunks (chunk_id, content, chunk_type, library_id, source_subject_id)
		SELECT 'KB#1000099', 'x', 'guideline', id, 'SYN-1' FROM reference_libraries LIMIT 1`); err == nil {
		t.Error("mhai_provision wrote a library row with a patient field")
	}
	if _, err := asProv(`UPDATE kb_chunks SET content = 'tampered'`); err == nil {
		t.Error("mhai_provision updated a row")
	}
	if n := reflibCount(t, super, `SELECT count(*) FROM kb_chunks WHERE library_id IS NULL`); n != 1 {
		t.Fatalf("seeded rows = %d", n)
	}
}

// 00056 goes down and up again: the grants and the policy go, and
// provision_append is 00021's again.
func TestMigration00056DownUp(t *testing.T) {
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
	if err := db.MigrateDownTo(ctx, pg.URL, 55); err != nil {
		t.Fatalf("down to 55: %v", err)
	}
	var check string
	if err := pool.QueryRow(ctx, `SELECT with_check FROM pg_policies WHERE tablename = 'audit_log' AND policyname = 'provision_append'`).
		Scan(&check); err != nil || strings.Contains(check, "reference_library") || !strings.Contains(check, "hospital.marked_demo") {
		t.Fatalf("provision_append after down: %s (%v)", check, err)
	}
	if n := reflibCount(t, pool, `SELECT (SELECT count(*) FROM pg_policies WHERE policyname IN ('provision_library', 'provision_library_rows'))
		+ (SELECT count(*) FROM information_schema.role_table_grants WHERE grantee = 'mhai_provision'
		   AND table_name IN ('kb_chunks', 'reference_libraries'))
		+ (SELECT count(*) FROM information_schema.column_privileges WHERE grantee = 'mhai_provision'
		   AND table_name IN ('kb_chunks', 'reference_libraries'))`); n != 0 {
		t.Fatalf("after down, %d grants or policies of 00056 remain", n)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
