//go:build integration

package db_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00020 (EMR plan Task 6; design §3.3, §6.1, §18 item 1): the emr
// schema, its roles, the connections (configuration only), credential
// metadata (never a value), sync runs and connection errors.

const (
	exclusionViolation = "23P01"
	raiseException     = "P0001"
)

// emrOrigin is the synthetic connection's credential origin.
var emrOrigin = []string{"https://fhir.example.org:443"}

// seedConn inserts a connection as the superuser.
func seedConn(t *testing.T, pool *pgxpool.Pool, hospital uuid.UUID, cidrs ...string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if cidrs == nil {
		cidrs = []string{}
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO emr_connections
		(id, hospital_id, type, source_label, base_url, settings, credentials_origin, private_network, private_cidrs)
		VALUES ($1, $2, 'fhir_r4', $6, 'https://fhir.example.org/r4', '{"client_id": "synthetic-client"}',
		        $3, $4, $5::cidr[])`, id, hospital, emrOrigin, len(cidrs) > 0, cidrs, "Record "+id.String()[:8]); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	return id
}

func asEMR(ctx context.Context, pool *pgxpool.Pool, hospital *uuid.UUID, q string, args ...any) (int64, error) {
	return inTx(ctx, pool, []string{`SET LOCAL ROLE mhai_emr`}, hospital, q, args...)
}

func TestEMRConnectionsRefuseSecretKeysInSettings(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("ES"))
	insert := func(settings string) error {
		_, err := pool.Exec(ctx, `INSERT INTO emr_connections (hospital_id, type, source_label, base_url, settings)
			VALUES ($1, 'fhir_r4', 'Main record', 'https://fhir.example.org', $2::jsonb)`, h, settings)
		return err
	}
	if err := insert(`{"client_id": "synthetic-client", "token_url": "https://auth.example.org/token"}`); err != nil {
		t.Fatalf("plain settings: %v", err)
	}
	for _, k := range []string{"api_key", "private_key", "shared_key", "access_key", "password", "secret", "token", "client_secret"} {
		if err := insert(`{"` + k + `": "synthetic"}`); pgCode(err) != checkViolation {
			t.Errorf("settings with %s: %v, want a check violation", k, err)
		}
	}
	for name, bad := range map[string][]string{
		"settings not an object": {"settings", `'[]'::jsonb`},
		"poll under 60 s":        {"poll_seconds", "59"},
		"ranges without private": {"private_cidrs", `'{10.20.0.0/24}'::cidr[]`},
		"label too long":         {"source_label", "'" + strings.Repeat("x", 41) + "'"},
		"empty label":            {"source_label", "''"},
		"address too long":       {"base_url", "'https://fhir.example.org/" + strings.Repeat("p", 480) + "'"},
		"unknown type":           {"type", "'hl7v2_feed'"},
		"unknown test code":      {"last_test_code", "'weird'"},
		"version 0":              {"version", "0"},
	} {
		var err error
		if bad[0] == "settings" {
			_, err = pool.Exec(ctx, `INSERT INTO emr_connections (hospital_id, type, source_label, base_url, settings)
				VALUES ($1, 'fhir_r4', 'Main record', 'https://fhir.example.org', `+bad[1]+`)`, h)
		} else {
			// Duplicate columns are refused by Postgres, so these go through
			// an UPDATE of a valid row.
			id := seedConn(t, pool, h)
			_, err = pool.Exec(ctx, `UPDATE emr_connections SET `+bad[0]+` = `+bad[1]+` WHERE id = $1`, id)
		}
		if pgCode(err) != checkViolation {
			t.Errorf("%s: %v, want a check violation", name, err)
		}
	}
}

// A37, database part (design §6.4): two hospitals' declared ranges can't
// overlap; one hospital's may (§6.4: the proxy keys by connection anyway).
// Each range is a private IPv4 network of at most a /24.
func TestEMRRangesCannotOverlapAcrossHospitals(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("RA"))
	b, _ := seedHospital(t, pool, uniqueCode("RB"))
	ca := seedConn(t, pool, a, "10.77.30.0/24")
	seedConn(t, pool, a, "10.77.30.128/25") // the same hospital: allowed

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM emr.connection_ranges WHERE connection_id = $1`, ca).Scan(&n); err != nil || n != 1 {
		t.Fatalf("ranges of A's connection: %d %v", n, err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO emr_connections (hospital_id, type, source_label, base_url, private_network, private_cidrs)
		VALUES ($1, 'fhir_r4', 'Main record', 'https://fhir.example.org', true, '{10.77.30.64/26}')`, b)
	if pgCode(err) != exclusionViolation {
		t.Fatalf("B overlapping A: %v, want an exclusion violation", err)
	}
	cb := seedConn(t, pool, b, "10.77.31.0/24")
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET private_cidrs = '{10.77.30.0/28}' WHERE id = $1`, cb); pgCode(err) != exclusionViolation {
		t.Fatalf("B moved onto A: %v, want an exclusion violation", err)
	}
	// The ranges follow the column: a cleared list frees them.
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET private_cidrs = '{}', private_network = false WHERE id = $1`, ca); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM emr.connection_ranges WHERE connection_id = $1`, ca).Scan(&n); err != nil || n != 0 {
		t.Fatalf("ranges after clearing: %d %v", n, err)
	}
	for _, bad := range []string{"10.0.0.0/16", "8.8.8.0/24", "169.254.169.0/24", "127.0.0.0/24", "fd00::/120", "100.64.0.0/24"} {
		_, err := pool.Exec(ctx, `UPDATE emr_connections SET private_cidrs = ARRAY[$2::cidr], private_network = true WHERE id = $1`, cb, bad)
		if c := pgCode(err); c != checkViolation {
			t.Errorf("range %s: %v, want a check violation", bad, err)
		}
	}
}

// A2: no role reads a credential value, because none is stored; core's
// clinical role holds nothing on the EMR tables; the platform holds the
// connections (configuration) and nothing in schema emr.
func TestEMRRoleGrants(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	var cols []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(attname::text ORDER BY attnum) FROM pg_attribute
		WHERE attrelid = 'emr.credentials'::regclass AND attnum > 0 AND NOT attisdropped`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if want := []string{"connection_id", "hospital_id", "field", "set_at", "secret_version", "set_by", "origin"}; !slices.Equal(cols, want) {
		t.Fatalf("emr.credentials columns %v, want metadata only %v", cols, want)
	}
	tables := []string{"emr_connections", "emr.credentials", "emr.sync_runs", "emr.connection_errors", "emr.connection_ranges"}
	holds := func(role, table string) bool {
		var any bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege($1, $2, 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
			OR has_any_column_privilege($1, $2, 'SELECT,INSERT,UPDATE,REFERENCES')`, role, table).Scan(&any); err != nil {
			t.Fatal(err)
		}
		return any
	}
	for _, role := range []string{"mhai_gateway", "mhai_auth", "mhai_worker", "mhai_jobs", "mhai_audit_drain", "mhai_app", "mhai_emr_app", "mhai_emr_core", "mhai_emr_admin"} {
		for _, table := range tables {
			if holds(role, table) {
				t.Errorf("%s holds a privilege on %s", role, table)
			}
		}
	}
	for _, table := range tables[1:] {
		if holds("mhai_platform", table) {
			t.Errorf("mhai_platform holds a privilege on %s", table)
		}
		if holds("mhai_emr_sched", table) {
			t.Errorf("mhai_emr_sched holds a privilege on %s", table)
		}
	}
	if holds("mhai_emr", "emr.connection_ranges") {
		t.Error("mhai_emr holds a privilege on emr.connection_ranges (only the trigger writes it)")
	}
	// The emr role: its columns of emr_connections, read only; nothing of
	// core's accounts, sessions, audit or clinical tables.
	var emrCols []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(a.attname::text ORDER BY a.attname) FROM pg_attribute a
		WHERE a.attrelid = 'emr_connections'::regclass AND a.attnum > 0 AND NOT a.attisdropped
		  AND has_column_privilege('mhai_emr', 'emr_connections', a.attname, 'SELECT')`).Scan(&emrCols); err != nil {
		t.Fatal(err)
	}
	if want := []string{"base_url", "credentials_origin", "enabled", "hospital_id", "id", "paused", "poll_seconds",
		"private_cidrs", "settings", "type", "version"}; !slices.Equal(emrCols, want) {
		t.Errorf("mhai_emr reads %v of emr_connections, want %v", emrCols, want)
	}
	for _, priv := range []string{"INSERT", "UPDATE", "DELETE"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege('mhai_emr', 'emr_connections', $1)
			OR has_any_column_privilege('mhai_emr', 'emr_connections', CASE WHEN $1 = 'DELETE' THEN 'INSERT' ELSE $1 END)`, priv).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Errorf("mhai_emr may %s emr_connections", priv)
		}
	}
	for _, table := range []string{"users", "sessions", "refresh_tokens", "audit_log", "clinical_queries", "jobs", "idempotency_keys", "user_roles"} {
		if holds("mhai_emr", table) {
			t.Errorf("mhai_emr holds a privilege on %s", table)
		}
	}
	var hosp []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(a.attname::text ORDER BY a.attname) FROM pg_attribute a
		WHERE a.attrelid = 'hospitals'::regclass AND a.attnum > 0 AND NOT a.attisdropped
		  AND has_column_privilege('mhai_emr', 'hospitals', a.attname, 'SELECT')`).Scan(&hosp); err != nil {
		t.Fatal(err)
	}
	// 00025 adds the time zone (the reconciliation hour, design §5.1).
	if !slices.Equal(hosp, []string{"id", "status", "time_zone"}) {
		t.Errorf("mhai_emr reads %v of hospitals, want id, status and time_zone", hosp)
	}
	// The scheduler: ids, switches and the interval only.
	var sched []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(a.attname::text ORDER BY a.attname) FROM pg_attribute a
		WHERE a.attrelid = 'emr_connections'::regclass AND a.attnum > 0 AND NOT a.attisdropped
		  AND has_column_privilege('mhai_emr_sched', 'emr_connections', a.attname, 'SELECT')`).Scan(&sched); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sched, []string{"enabled", "hospital_id", "id", "paused", "poll_seconds"}) {
		t.Errorf("mhai_emr_sched reads %v", sched)
	}
	// The platform never updates the key columns.
	for _, col := range []string{"id", "hospital_id", "created_at"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT has_column_privilege('mhai_platform', 'emr_connections', $1, 'UPDATE')`, col).Scan(&ok); err != nil || ok {
			t.Errorf("mhai_platform may update emr_connections.%s (%v)", col, err)
		}
	}
}

// A13 for these tables: as mhai_emr with hospital A set, no row of B; an
// insert naming B fails WITH CHECK. Without a hospital, nothing.
func TestEMRRLSWithHospital(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("EA"))
	b, _ := seedHospital(t, pool, uniqueCode("EB"))
	ca, cb := seedConn(t, pool, a), seedConn(t, pool, b)
	for _, c := range []struct {
		h, conn uuid.UUID
	}{{a, ca}, {b, cb}} {
		if _, err := pool.Exec(ctx, `INSERT INTO emr.credentials (connection_id, hospital_id, field, set_at, secret_version, origin)
			VALUES ($1, $2, 'private_key', now(), 'v1', $3)`, c.conn, c.h, emrOrigin); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO emr.sync_runs (hospital_id, connection_id, ok, records, outcome) VALUES ($1, $2, true, 3, 'ok')`, c.h, c.conn); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO emr.connection_errors (hospital_id, connection_id, code) VALUES ($1, $2, 'unreachable')`, c.h, c.conn); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"emr_connections", "emr.credentials", "emr.sync_runs"} {
		var seen []uuid.UUID
		err := func() error {
			tx, err := pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_emr`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, a.String()); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT DISTINCT hospital_id FROM `+table+` WHERE hospital_id IN ($1, $2)`, a, b)
			if err != nil {
				return err
			}
			seen, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			return err
		}()
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if len(seen) != 1 || seen[0] != a {
			t.Errorf("%s as A: hospitals %v, want A only", table, seen)
		}
		n, err := countAs(t, pool, "mhai_emr", `SELECT count(*) FROM `+table)
		if err != nil || n != 0 {
			t.Errorf("%s with no hospital: %d rows (%v)", table, n, err)
		}
	}
	for what, q := range map[string]string{
		"credential": `INSERT INTO emr.credentials (connection_id, hospital_id, field, set_at, secret_version, origin) VALUES ($1, $2, 'api_key', now(), 'v1', '{https://fhir.example.org:443}')`,
		"sync run":   `INSERT INTO emr.sync_runs (connection_id, hospital_id, ok, records, outcome) VALUES ($1, $2, false, 0, 'unreachable')`,
		"error":      `INSERT INTO emr.connection_errors (connection_id, hospital_id, code) VALUES ($1, $2, 'timed_out')`,
	} {
		if _, err := asEMR(ctx, pool, &a, q, cb, b); pgCode(err) != rlsViolation {
			t.Errorf("%s for B as A: %v, want an RLS refusal", what, err)
		}
		// A connection of B named with A's hospital: the key (id, hospital)
		// doesn't exist.
		if _, err := asEMR(ctx, pool, &a, q, cb, a); err == nil {
			t.Errorf("%s for B's connection under A's hospital accepted", what)
		}
		if _, err := asEMR(ctx, pool, &a, q, ca, a); err != nil {
			t.Errorf("%s for A as A: %v", what, err)
		}
	}
	// The emr role reads A's connection, and only A's hospital row.
	var status string
	err := func() error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_emr`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, a.String()); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM hospitals WHERE id IN ($1, $2)`, a, b).Scan(&n); err != nil || n != 1 {
			t.Errorf("hospitals as A: %d %v", n, err)
		}
		return tx.QueryRow(ctx, `SELECT status FROM hospitals WHERE id = $1`, a).Scan(&status)
	}()
	if err != nil || status != "active" {
		t.Fatalf("hospital status as A: %q %v", status, err)
	}
}

// The emr service's login holds nothing until SET ROLE mhai_emr (or the
// scheduler's role), and can reach no other role (as intelligence_app).
func TestEMRAppLoginIsNoInherit(t *testing.T) {
	pg, super := shared(t)
	ctx := context.Background()
	name := "emr_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	pw := "synthetic-emr-password-" + uuid.NewString()
	if err := db.EnsureLogin(ctx, super, name, pw, "mhai_emr_app"); err != nil {
		t.Fatalf("ensure emr login: %v", err)
	}
	p := connectAs(t, pg, name, pw)
	defer p.Close()
	if _, err := p.Exec(ctx, `SELECT count(*) FROM emr_connections`); pgCode(err) != permissionDenied {
		t.Fatalf("emr login without SET ROLE read emr_connections: %v", err)
	}
	for _, role := range []string{"mhai_emr", "mhai_emr_sched"} {
		if _, err := inTx(ctx, p, []string{`SET LOCAL ROLE ` + role}, nil, `SELECT count(*) FROM emr_connections`); err != nil {
			t.Errorf("emr login as %s: %v", role, err)
		}
	}
	for _, role := range []string{"mhai_gateway", "mhai_platform", "mhai_worker", "mhai_emr_core", "mhai_emr_admin", "mhai_app", "mhai_migrate"} {
		if _, err := inTx(ctx, p, []string{`SET LOCAL ROLE ` + role}, nil, `SELECT 1`); err == nil {
			t.Errorf("emr login could SET ROLE %s", role)
		}
	}
	// And core's login reaches mhai_emr_core, never mhai_emr.
	var core, emr bool
	if err := super.QueryRow(ctx, `SELECT pg_has_role('mhai_app', 'mhai_emr_core', 'SET'), pg_has_role('mhai_app', 'mhai_emr', 'SET')`).Scan(&core, &emr); err != nil {
		t.Fatal(err)
	}
	if !core || emr {
		t.Errorf("mhai_app: SET mhai_emr_core %v (want true), SET mhai_emr %v (want false)", core, emr)
	}
}

// K3: mhai_platform appends the EMR connection actions with no hospital, as
// its other actions; nothing else is admitted.
func TestPlatformAppendAllowsEMRConnectionActionsOnly(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("PA"))
	setup := []string{`SET LOCAL ROLE mhai_platform`}
	for _, action := range []string{"emr.connection_added", "emr.connection_changed", "emr.connection_switched_on",
		"emr.connection_switched_off", "emr.connection_tested", "emr.source_resumed", "hospital.model_policy_changed", "job.retried",
		"emr.rule_set_switched"} {
		if _, err := inTx(ctx, pool, setup, nil, auditInsert, nil, nil, action, uuid.NewString()); err != nil {
			t.Errorf("platform appending %s: %v", action, err)
		}
		if _, err := inTx(ctx, pool, setup, nil, auditInsert, h, nil, action, uuid.NewString()); pgCode(err) != rlsViolation {
			t.Errorf("platform appending %s with a hospital: %v", action, err)
		}
	}
	for _, action := range []string{"emr.source_paused", "emr.credentials_deleted", "emr.patient_created", "alert.raised", "emr.connection_removed"} {
		if _, err := inTx(ctx, pool, setup, nil, auditInsert, nil, nil, action, uuid.NewString()); pgCode(err) != rlsViolation {
			t.Errorf("platform appending %s: %v, want an RLS refusal", action, err)
		}
	}
}

// Credential metadata follows the origin (design §6.3): rows set for
// another origin read as not set, and an enabled connection's origin can't
// change (core wipes and switches off first).
func TestEMRCredentialsMetaFollowsOrigin(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("CM"))
	c := seedConn(t, pool, h)
	if _, err := pool.Exec(ctx, `INSERT INTO emr.credentials (connection_id, hospital_id, field, set_at, secret_version, origin)
		VALUES ($1, $2, 'private_key', now(), 'v1', $3)`, c, h, emrOrigin); err != nil {
		t.Fatal(err)
	}
	meta := func() []string {
		t.Helper()
		var fields []string
		err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT field FROM emr_credentials_meta($1)`, c)
			if err != nil {
				return err
			}
			fields, err = pgx.CollectRows(rows, pgx.RowTo[string])
			return err
		})
		if err != nil {
			t.Fatalf("emr_credentials_meta: %v", err)
		}
		return fields
	}
	if got := meta(); !slices.Equal(got, []string{"private_key"}) {
		t.Fatalf("meta %v", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET credentials_origin = '{https://other.example.org:443}' WHERE id = $1`, c); err != nil {
		t.Fatal(err)
	}
	if got := meta(); len(got) != 0 {
		t.Fatalf("meta after an origin change %v, want none", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET enabled = true, last_test_at = now(), last_test_ok = true, last_test_code = 'ok' WHERE id = $1`, c); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET credentials_origin = '{https://fhir.example.org:443}' WHERE id = $1`, c); pgCode(err) != raiseException {
		t.Fatalf("origin change while on: %v, want refused", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET credentials_origin = '{https://fhir.example.org:443}', enabled = false WHERE id = $1`, c); err != nil {
		t.Fatalf("origin change switching off: %v", err)
	}
	// Health: codes and counts per connection; a connection with no run is
	// never.
	var health string
	var errs int64
	if err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT health, errors_24h FROM emr_platform_health() WHERE connection_id = $1`, c).Scan(&health, &errs)
	}); err != nil || health != "never" || errs != 0 {
		t.Fatalf("health %q %d %v", health, errs, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO emr.sync_runs (hospital_id, connection_id, started_at, finished_at, ok, records, outcome)
		VALUES ($1, $2, now() - interval '3 minutes', now() - interval '2 minutes', true, 7, 'ok'),
		       ($1, $2, now() - interval '1 minute', now(), false, 0, 'unreachable')`, h, c); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO emr.connection_errors (hospital_id, connection_id, at, code)
		VALUES ($1, $2, now(), 'unreachable'), ($1, $2, now() - interval '1 hour', 'unreachable'),
		       ($1, $2, now() - interval '2 days', 'timed_out')`, h, c); err != nil {
		t.Fatal(err)
	}
	var ok bool
	var records int
	if err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT health, errors_24h, last_sync_ok, last_sync_records FROM emr_platform_health() WHERE connection_id = $1`, c).
			Scan(&health, &errs, &ok, &records)
	}); err != nil || health != "errors" || errs != 2 || ok || records != 0 {
		t.Fatalf("health %q errors %d last ok %v records %d: %v", health, errs, ok, records, err)
	}
	var codes []string
	var counts []int64
	if err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT code, count FROM emr_platform_errors($1)`, c)
		if err != nil {
			return err
		}
		for rows.Next() {
			var code string
			var n int64
			if err := rows.Scan(&code, &n); err != nil {
				return err
			}
			codes, counts = append(codes, code), append(counts, n)
		}
		return rows.Err()
	}); err != nil || !slices.Equal(codes, []string{"unreachable"}) || !slices.Equal(counts, []int64{2}) {
		t.Fatalf("errors %v %v %v", codes, counts, err)
	}
}

// K2: a temporary table named like a relation a definer reads doesn't
// change what the definer answers.
func TestDefinersIgnoreTempTables(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("TT"))
	c := seedConn(t, pool, h)
	if _, err := pool.Exec(ctx, `INSERT INTO emr.credentials (connection_id, hospital_id, field, set_at, secret_version, origin)
		VALUES ($1, $2, 'private_key', now(), 'v1', $3)`, c, h, emrOrigin); err != nil {
		t.Fatal(err)
	}
	read := func(tx pgx.Tx) (string, int) {
		var health string
		var n int
		if err := tx.QueryRow(ctx, `SELECT health FROM emr_platform_health() WHERE connection_id = $1`, c).Scan(&health); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM emr_credentials_meta($1)`, c).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return health, n
	}
	err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		h0, n0 := read(tx)
		for _, q := range []string{
			`CREATE TEMP TABLE emr_connections (id uuid, hospital_id uuid, credentials_origin text[]) ON COMMIT DROP`,
			`CREATE TEMP TABLE credentials (connection_id uuid, field text, set_at timestamptz, origin text[]) ON COMMIT DROP`,
			`CREATE TEMP TABLE sync_runs (connection_id uuid, ok boolean, finished_at timestamptz) ON COMMIT DROP`,
			`INSERT INTO pg_temp.credentials VALUES ('` + c.String() + `', 'api_key', now(), '{https://fhir.example.org:443}')`,
			`INSERT INTO pg_temp.sync_runs VALUES ('` + c.String() + `', false, now())`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		h1, n1 := read(tx)
		if h1 != h0 || n1 != n0 {
			t.Errorf("a temp table changed the definers: %s/%d -> %s/%d", h0, n0, h1, n1)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Every definer of 00020: owner mhai_migrate, the pinned search_path,
	// EXECUTE for its roles only.
	for name, want := range map[string]string{
		"emr_platform_health":  "mhai_platform",
		"emr_platform_errors":  "mhai_platform",
		"emr_credentials_meta": "mhai_platform",
		"emr_sync_ranges":      "",
		"emr_origin_guard":     "",
	} {
		var owner string
		var definer bool
		var config, grantees []string
		if err := pool.QueryRow(ctx, `
			SELECT pg_get_userbyid(p.proowner), p.prosecdef, COALESCE(p.proconfig, '{}'),
			       COALESCE((SELECT array_agg(CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END ORDER BY 1)
			                 FROM aclexplode(p.proacl) a
			                 WHERE a.privilege_type = 'EXECUTE' AND a.grantee <> p.proowner), '{}')
			FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'public' AND p.proname = $1`, name).Scan(&owner, &definer, &config, &grantees); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if owner != "mhai_migrate" || !definer || len(config) != 1 || config[0] != "search_path=pg_catalog, pg_temp" {
			t.Errorf("%s: owner %s definer %v config %v", name, owner, definer, config)
		}
		if strings.Join(grantees, ",") != want {
			t.Errorf("%s: EXECUTE held by %v, want [%s]", name, grantees, want)
		}
	}
	// Every new table: RLS enabled and forced, owner mhai_migrate.
	for _, table := range []string{"emr_connections", "emr.credentials", "emr.sync_runs", "emr.connection_errors", "emr.connection_ranges"} {
		var rls, force bool
		var owner string
		if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity, pg_get_userbyid(relowner) FROM pg_class WHERE oid = $1::regclass`, table).
			Scan(&rls, &force, &owner); err != nil {
			t.Fatal(err)
		}
		if !rls || !force || owner != "mhai_migrate" {
			t.Errorf("%s: rls %v force %v owner %s", table, rls, force, owner)
		}
	}
}

// 00020 goes down to 00019 and up again: the audit policy is restored
// exactly, the schema and its objects are gone, the roles stay (they are
// cluster-wide) without their memberships.
func TestMigration00020DownUp(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	pool, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	check := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT with_check FROM pg_policies WHERE tablename = 'audit_log' AND policyname = 'platform_append'`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if err := db.MigrateUpTo(ctx, pg.URL, 19); err != nil {
		t.Fatalf("up to 19: %v", err)
	}
	pre := check()
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
	if up := check(); !strings.Contains(up, "'emr.connection_added'::text") || up == pre {
		t.Fatalf("after up, platform_append: %s", up)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 19); err != nil {
		t.Fatalf("down to 19: %v", err)
	}
	if down := check(); down != pre {
		t.Errorf("after down, platform_append is not as before:\n down:   %s\n before: %s", down, pre)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_namespace WHERE nspname = 'emr')
		+ (SELECT count(*) FROM pg_class WHERE relname = 'emr_connections')
		+ (SELECT count(*) FROM pg_proc WHERE proname LIKE 'emr\_%')
		+ (SELECT count(*) FROM pg_policies WHERE tablename = 'hospitals' AND policyname = 'emr_read')
		+ (SELECT count(*) FROM pg_auth_members m WHERE m.roleid IN (SELECT oid FROM pg_roles WHERE rolname LIKE 'mhai\_emr%'))
		+ (SELECT count(*) FROM information_schema.column_privileges WHERE grantee LIKE 'mhai\_emr%')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("after down, %d objects, policies, memberships or grants of 00020 remain", left)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
}

// Ruling E1: one hospital has each source label once, in any case; another
// hospital may use it.
func TestEMRLabelUniquePerHospital(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("LA"))
	b, _ := seedHospital(t, pool, uniqueCode("LB"))
	insert := func(h uuid.UUID, label string) error {
		_, err := pool.Exec(ctx, `INSERT INTO emr_connections (hospital_id, type, source_label, base_url)
			VALUES ($1, 'fhir_r4', $2, 'https://fhir.example.org')`, h, label)
		return err
	}
	if err := insert(a, "Main record"); err != nil {
		t.Fatal(err)
	}
	if err := insert(a, "MAIN RECORD"); pgCode(err) != "23505" || pgConstraint(err) != "emr_connections_label" {
		t.Fatalf("same label, same hospital: %v", err)
	}
	if err := insert(b, "Main record"); err != nil {
		t.Fatalf("same label, other hospital: %v", err)
	}
}
