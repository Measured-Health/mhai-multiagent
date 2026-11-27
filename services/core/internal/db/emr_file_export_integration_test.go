//go:build integration

package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00043 (EMR PR 11, design §15.4): a switched-on file export is
// stale when no new file has been loaded for longer than the hospital's
// declared interval plus 50%, or ever, or when the interval can't be read;
// other types are unchanged.

// TestStaleByExpectedInterval: the sources' status (clinicians, hospital
// admins) and the platform's health, for a file export polled a minute
// ago, as the newest file ages.
func TestStaleByExpectedInterval(t *testing.T) {
	// Migration 00061: hl7v2_file (HL7 v2 message files) has the same rule.
	for _, typ := range []string{"file_export", "hl7v2_file"} {
		t.Run(typ, func(t *testing.T) { staleByExpectedInterval(t, typ) })
	}
}

func staleByExpectedInterval(t *testing.T, typ string) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("FE"))
	c := uuid.New()
	fhir := seedConn(t, pool, h)
	if _, err := pool.Exec(ctx, `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url, settings,
		credentials_origin, enabled, poll_seconds) VALUES ($1, $2, $3, 'Lab export', 'sftp://sftp.synthetic-hospital.example',
		'{"folder": "/outbound", "expected_every_minutes": "60"}', '{sftp://sftp.synthetic-hospital.example:22}', true, 300)`, c, h, typ); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET enabled = true, poll_seconds = 300 WHERE id = $1`, fhir); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO emr.sync_runs (hospital_id, connection_id, started_at, finished_at, ok, records, outcome)
		VALUES ($1, $2, now() - interval '1 minute', now() - interval '1 minute', true, 0, 'ok'),
		       ($1, $3, now() - interval '1 minute', now() - interval '1 minute', true, 0, 'ok')`, h, c, fhir); err != nil {
		t.Fatal(err)
	}
	status := func(id uuid.UUID) string {
		var s string
		if err := asRoleIn(ctx, pool, "mhai_gateway", h, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT status FROM emr_sources_status() WHERE connection_id = $1`, id).Scan(&s)
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	health := func(id uuid.UUID) string {
		var s string
		if err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT health FROM emr_platform_health() WHERE connection_id = $1`, id).Scan(&s)
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	check := func(name, wantStatus, wantHealth string) {
		t.Helper()
		if s, hl := status(c), health(c); s != wantStatus || hl != wantHealth {
			t.Errorf("%s: status %s health %s, want %s %s", name, s, hl, wantStatus, wantHealth)
		}
	}
	// Polled, but no file ever loaded (no state row yet, then a row
	// without a file).
	check("no state", "behind", "errors")
	if _, err := pool.Exec(ctx, `INSERT INTO emr.sync_state (connection_id, hospital_id) VALUES ($1, $2)`, c, h); err != nil {
		t.Fatal(err)
	}
	check("no file yet", "behind", "errors")
	setFile := func(ago string) {
		if _, err := pool.Exec(ctx, `UPDATE emr.sync_state SET last_file_at = now() - $2::interval WHERE connection_id = $1`, c, ago); err != nil {
			t.Fatal(err)
		}
	}
	setFile("30 minutes")
	check("a file 30 minutes ago, every 60", "current", "ok")
	setFile("89 minutes")
	check("89 minutes, within 90", "current", "ok")
	setFile("91 minutes")
	check("91 minutes, past 90", "behind", "errors")
	setFile("5 minutes")
	for _, bad := range []string{`{"expected_every_minutes": "sixty"}`, `{"expected_every_minutes": "0"}`, `{}`,
		`{"expected_every_minutes": "4"}`, `{"expected_every_minutes": "10081"}`} {
		if _, err := pool.Exec(ctx, `UPDATE emr_connections SET settings = $2::jsonb WHERE id = $1`, c, bad); err != nil {
			t.Fatal(err)
		}
		check("interval "+bad, "behind", "errors")
	}
	// Security review (14 Nov): rows kept as dead letters or files set
	// aside (an unreadable_data error row in the last 24 hours) turn the
	// platform's health to errors, while the data stays current for
	// clinicians.
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET settings = '{"expected_every_minutes": "60"}' WHERE id = $1`, c); err != nil {
		t.Fatal(err)
	}
	check("fresh", "current", "ok")
	if _, err := pool.Exec(ctx, `INSERT INTO emr.connection_errors (hospital_id, connection_id, at, code)
		VALUES ($1, $2, now() - interval '2 hours', 'unreadable_data')`, h, c); err != nil {
		t.Fatal(err)
	}
	check("a dead letter 2 hours ago", "current", "errors")
	if _, err := pool.Exec(ctx, `UPDATE emr.connection_errors SET at = now() - interval '25 hours' WHERE connection_id = $1`, c); err != nil {
		t.Fatal(err)
	}
	check("a dead letter 25 hours ago", "current", "ok")
	// The poll rule still holds for a file export with fresh files.
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET settings = '{"expected_every_minutes": "60"}' WHERE id = $1`, c); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE emr.sync_runs SET finished_at = now() - interval '16 minutes' WHERE connection_id = $1`, c); err != nil {
		t.Fatal(err)
	}
	check("no poll for 16 minutes at 300 s", "behind", "errors")
	// FHIR has no file rule.
	if s, hl := status(fhir), health(fhir); s != "current" || hl != "ok" {
		t.Errorf("fhir: %s %s", s, hl)
	}
}

// The emr role writes last_file_at for its own hospital only; no other
// role gains anything on sync_state.
func TestLastFileAtGrants(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("FA"))
	b, _ := seedHospital(t, pool, uniqueCode("FB"))
	ca, cb := seedConn(t, pool, a), seedConn(t, pool, b)
	if _, err := pool.Exec(ctx, `INSERT INTO emr.sync_state (connection_id, hospital_id) VALUES ($1, $2), ($3, $4)`, ca, a, cb, b); err != nil {
		t.Fatal(err)
	}
	if n, err := asEMR(ctx, pool, &a, `UPDATE emr.sync_state SET last_file_at = now() WHERE connection_id IN ($1, $2)`, ca, cb); err != nil || n != 1 {
		t.Fatalf("emr update: %d %v, want its own row only", n, err)
	}
	for _, role := range []string{"mhai_gateway", "mhai_platform", "mhai_emr_sched", "mhai_emr_core", "mhai_worker"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT has_column_privilege($1, 'emr.sync_state', 'last_file_at', 'SELECT,UPDATE')`, role).Scan(&ok); err != nil || ok {
			t.Errorf("%s holds last_file_at: %v %v", role, ok, err)
		}
	}
}

// 00043 goes down and up again, leaving the column gone and the functions
// as 00027 made them.
func TestMigration00043DownUp(t *testing.T) {
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
	if err := db.MigrateDownTo(ctx, pg.URL, 42); err != nil {
		t.Fatalf("down to 42: %v", err)
	}
	var cols int
	var body string
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM information_schema.columns WHERE table_schema = 'emr'
		AND table_name = 'sync_state' AND column_name = 'last_file_at'), (SELECT prosrc FROM pg_proc WHERE proname = 'emr_sources_status')`).
		Scan(&cols, &body); err != nil {
		t.Fatal(err)
	}
	if cols != 0 || len(body) == 0 || strings.Contains(body, "file_export") {
		t.Fatalf("after down: column %d, function names file_export %v", cols, strings.Contains(body, "file_export"))
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// 00061 (HL7 v2 message files) accepts hl7v2_file, and goes down to 00060
// and up again: the type refused again, the functions as 00043 left them.
func TestMigration00061HL7v2File(t *testing.T) {
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
	h, _ := seedHospital(t, pool, uniqueCode("HL"))
	insert := func() error {
		_, err := pool.Exec(ctx, `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url, settings)
			VALUES ($1, $2, 'hl7v2_file', 'Lab messages', 'sftp://sftp.synthetic-hospital.example', '{}')`, uuid.New(), h)
		return err
	}
	if err := insert(); err != nil {
		t.Fatalf("hl7v2_file refused: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM emr_connections WHERE type = 'hl7v2_file'`); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 60); err != nil {
		t.Fatalf("down to 60: %v", err)
	}
	var body string
	if err := pool.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE proname = 'emr_sources_status'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "hl7v2_file") || !strings.Contains(body, "file_export") {
		t.Fatal("after down: the function isn't 00043's")
	}
	if err := insert(); err == nil {
		t.Fatal("after down: hl7v2_file accepted")
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
