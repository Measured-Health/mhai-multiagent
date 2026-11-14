//go:build integration

package emrmaint_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/emrmaint"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Plan Task 40: core's maintenance loop. Every name and value is synthetic,
// and the observation value (6.8371) is distinctive, so a search for it
// can't hit a timestamp or an id.

// shared is a fresh migrated database for one test, so one test's
// hospitals never reach another's run.
func shared(t *testing.T) *pgxpool.Pool {
	t.Helper()
	_, pool := testutil.MigratedPostgres(t)
	return pool
}

// world is a hospital with a connection and one patient with an
// observation (value 6.8371) and an open alert.
type world struct {
	h, conn, patient, obs, alert uuid.UUID
}

func exec(t *testing.T, pool *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func scan(t *testing.T, pool *pgxpool.Pool, dst any, q string, args ...any) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), q, args...).Scan(dst); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func newWorld(t *testing.T, pool *pgxpool.Pool) world {
	t.Helper()
	var w world
	w.h, w.conn = uuid.New(), uuid.New()
	code := "MT" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	exec(t, pool, `INSERT INTO hospitals (id, name, short_code, status, time_zone) VALUES ($1, 'Synthetic Hospital', $2, 'active', 'Africa/Lagos')`, w.h, code)
	exec(t, pool, `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url) VALUES ($1, $2, 'fhir_r4', 'Synthetic record', 'https://fhir.example.org/r4')`, w.conn, w.h)
	scan(t, pool, &w.patient, `INSERT INTO emr.patients (hospital_id, display_name, mrn, birth_date, sex)
		VALUES ($1, 'Synthetic Patient', 'SYN-6837', '1970-01-01', 'female') RETURNING id`, w.h)
	exec(t, pool, `INSERT INTO emr.patient_links (hospital_id, connection_id, source_patient_id, patient_id) VALUES ($1, $2, 'p-mt', $3)`, w.h, w.conn, w.patient)
	var raw uuid.UUID
	scan(t, pool, &raw, `INSERT INTO emr.raw_records (hospital_id, source_kind, connection_id, resource_kind, source_id,
		payload, payload_sha256, connector_type, connector_version)
		VALUES ($1, 'connector', $2, 'observation', $3, '{"patient": "p-mt", "value_quantity": "6.8371"}', $4, 'fhir_r4', 1) RETURNING id`,
		w.h, w.conn, "obs-"+uuid.NewString()[:8], bytes.Repeat([]byte{7}, 32))
	scan(t, pool, &w.obs, `INSERT INTO emr.observations (hospital_id, patient_id, category, loinc, value_num, unit_ucum, value_si,
		status, effective_at, standardised, raw_id, source_kind, connection_id, connector_type, connector_version, source_id,
		source_version, received_at, mapping_version, normaliser_version)
		VALUES ($1, $2, 'laboratory', '99901-1', 6.8371, 'mmol/L', 6.8371, 'final', now(), true, $3, 'connector', $4, 'fhir_r4', 1,
		        'obs-mt', '1', now(), 'synthetic-map', 'n1') RETURNING id`, w.h, w.patient, raw, w.conn)
	scan(t, pool, &w.alert, `INSERT INTO alerts (hospital_id, patient_id, connection_id, rule_id, category, severity, routed_queue)
		VALUES ($1, $2, $3, 'r-mt', 'blood_chemistry', 'critical', true) RETURNING id`, w.h, w.patient, w.conn)
	return w
}

// holdAndExclude places a hold and excludes the patient as the gateway:
// the request is held. It returns the request and the hold.
func (w world) holdAndExclude(t *testing.T, pool *pgxpool.Pool) (req, hold uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	scan(t, pool, &hold, `INSERT INTO legal_holds (hospital_id, reference) VALUES ($1, 'LH-2026-0042') RETURNING id`, w.h)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{`SET LOCAL ROLE mhai_gateway`, `SELECT set_config('mhai.hospital_id', '` + w.h.String() + `', true)`} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var held bool
	if err := tx.QueryRow(ctx, `SELECT request_id, held FROM emr_exclude_patient($1, 'objection')`, w.patient).Scan(&req, &held); err != nil || !held {
		t.Fatalf("exclude: held %v %v", held, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return req, hold
}

func count(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	scan(t, pool, &n, q, args...)
	return n
}

// auditRows are the hospital's rows of action: actor, payload.
func auditRows(t *testing.T, pool *pgxpool.Pool, h uuid.UUID, action string) []map[string]any {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT actor_id IS NULL, payload FROM audit_log WHERE hospital_id = $1 AND action = $2 ORDER BY id`, h, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var noActor bool
		var raw []byte
		if err := rows.Scan(&noActor, &raw); err != nil {
			t.Fatal(err)
		}
		if !noActor {
			t.Errorf("%s has an actor", action)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// A41, P7: no job message exists; the loop alone releases the request once
// the hold is lifted and purges it on the same run, audited with counts.
func TestPurgeWithin24hWithoutJobMessage(t *testing.T) {
	pool := shared(t)
	ctx := context.Background()
	w := newWorld(t, pool)
	req, hold := w.holdAndExclude(t, pool)
	var logs bytes.Buffer
	l := &emrmaint.Loop{Pool: pool, Log: slog.New(slog.NewJSONHandler(&logs, nil)), Metrics: &emrmaint.Metrics{}}

	// Held: a run deletes nothing.
	if err := l.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, pool, `SELECT count(*) FROM emr.observations WHERE id = $1`, w.obs) != 1 {
		t.Fatal("purged under the hold")
	}
	exec(t, pool, `UPDATE legal_holds SET lifted_at = now() WHERE id = $1`, hold)
	if err := l.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, pool, `SELECT count(*) FROM emr.purge_requests WHERE id = $1 AND state = 'done'`, req) != 1 {
		t.Fatal("the request isn't done after the lift")
	}
	if count(t, pool, `SELECT count(*) FROM emr.observations WHERE patient_id = $1`, w.patient)+
		count(t, pool, `SELECT count(*) FROM emr.raw_records WHERE hospital_id = $1 AND payload::text LIKE '%6.8371%'`, w.h)+
		count(t, pool, `SELECT count(*) FROM emr.purge_held_alerts WHERE request_id = $1`, req) != 0 {
		t.Fatal("the patient's data remains")
	}
	if count(t, pool, `SELECT count(*) FROM alerts WHERE id = $1 AND patient_id IS NULL`, w.alert) != 1 {
		t.Fatal("the alert names the patient, or is gone")
	}
	released := auditRows(t, pool, w.h, "emr.exclusion_purge_released")
	purged := auditRows(t, pool, w.h, "emr.exclusion_purged")
	if len(released) != 1 || len(purged) != 1 {
		t.Fatalf("audit: released %v purged %v", released, purged)
	}
	if purged[0]["request_id"] != req.String() || purged[0]["patient_id"] != w.patient.String() || purged[0]["via"] != "loop" ||
		purged[0]["observations"] != float64(1) || purged[0]["alerts"] != float64(1) {
		t.Errorf("purged payload: %v", purged[0])
	}
	if l.Metrics.Purged.Load() != 1 || l.Metrics.Released.Load() != 1 || l.Metrics.Failures.Load() != 0 {
		t.Errorf("metrics: purged %d released %d failures %d", l.Metrics.Purged.Load(), l.Metrics.Released.Load(), l.Metrics.Failures.Load())
	}
	// A second run changes nothing and audits nothing more.
	if err := l.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(auditRows(t, pool, w.h, "emr.exclusion_purged")); n != 1 {
		t.Errorf("purged audited %d times", n)
	}
	requireNoPHI(t, logs.String())
}

// A request held for over 90 days is counted and logged with its id only.
func TestHeldOver90DaysListed(t *testing.T) {
	pool := shared(t)
	ctx := context.Background()
	w := newWorld(t, pool)
	req, _ := w.holdAndExclude(t, pool)
	exec(t, pool, `UPDATE emr.purge_requests SET held_since = now() - interval '91 days' WHERE id = $1`, req)
	var logs bytes.Buffer
	l := &emrmaint.Loop{Pool: pool, Log: slog.New(slog.NewJSONHandler(&logs, nil)), Metrics: &emrmaint.Metrics{}}
	if err := l.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if l.Metrics.HeldOverdue.Load() < 1 {
		t.Fatalf("held overdue %d", l.Metrics.HeldOverdue.Load())
	}
	if !strings.Contains(logs.String(), req.String()) || !strings.Contains(logs.String(), "held over 90 days") {
		t.Fatalf("the overdue request isn't logged: %s", logs.String())
	}
	if count(t, pool, `SELECT count(*) FROM emr.observations WHERE id = $1`, w.obs) != 1 {
		t.Fatal("purged under the hold")
	}
	requireNoPHI(t, logs.String())
}

// The nightly sweep runs from 03:00 UTC, once a day, only when switched
// on; each pass that removed something is audited with counts.
func TestRetentionSweepNightlyWhenOn(t *testing.T) {
	pool := shared(t)
	ctx := context.Background()
	w := newWorld(t, pool)
	exec(t, pool, `UPDATE emr.observations SET effective_at = now() - interval '14 months' WHERE id = $1`, w.obs)
	at := func(hour int) func() time.Time {
		return func() time.Time { return time.Date(2026, 11, 14, hour, 30, 0, 0, time.UTC) }
	}
	off := &emrmaint.Loop{Pool: pool, Now: at(4)}
	if err := off.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	early := &emrmaint.Loop{Pool: pool, Retention: true, Now: at(2)}
	if err := early.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, pool, `SELECT count(*) FROM emr.observations WHERE id = $1`, w.obs) != 1 {
		t.Fatal("swept while off or before 03:00")
	}
	on := &emrmaint.Loop{Pool: pool, Retention: true, Now: at(3), Metrics: &emrmaint.Metrics{}}
	if err := on.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, pool, `SELECT count(*) FROM emr.observations WHERE id = $1`, w.obs) != 0 {
		t.Fatal("the 14-month observation stays")
	}
	swept := auditRows(t, pool, w.h, "emr.retention_swept")
	if len(swept) != 1 || swept[0]["observations"] != float64(1) {
		t.Fatalf("swept audit: %v", swept)
	}
	passes := on.Metrics.SweepPasses.Load()
	if err := on.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if on.Metrics.SweepPasses.Load() != passes {
		t.Error("swept twice in one day")
	}
}

// One copy leads: with the lock held elsewhere, a tick does nothing.
func TestLoopLeaderOnly(t *testing.T) {
	pool := shared(t)
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('mhai.emr.maint'))`).Scan(&locked); err != nil || !locked {
		t.Fatalf("lock: %v %v", locked, err)
	}
	defer func() { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext('mhai.emr.maint'))`) }()
	l := &emrmaint.Loop{Pool: pool}
	if ran, err := l.Tick(ctx); err != nil || ran {
		t.Fatalf("tick while another copy leads: ran %v %v", ran, err)
	}
}

func requireNoPHI(t *testing.T, logs string) {
	t.Helper()
	for _, s := range []string{"Synthetic Patient", "SYN-6837", "6.8371", "1970-01-01", "p-mt", "LH-2026-0042"} {
		if strings.Contains(logs, s) {
			t.Errorf("logs carry %q", s)
		}
	}
}
