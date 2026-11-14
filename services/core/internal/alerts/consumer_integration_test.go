//go:build integration

package alerts_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/alerts"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
)

// Core's finding consumer (EMR plan Tasks 35-37) on a migrated database,
// as core's login (mhai_app setting mhai_emr_core). Every name, MRN and
// value is synthetic.

func (w *world) consumer() *alerts.Consumer {
	return &alerts.Consumer{Pool: sharedApp, Metrics: &alerts.ConsumerMetrics{},
		Log: slog.New(slog.NewJSONHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
}

// observation adds a synthetic potassium result of patient p.
func (w *world) observation(t *testing.T, p uuid.UUID, value float64, status string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	src := uuid.NewString()
	sum := sha256.Sum256([]byte(src))
	var raw, obs uuid.UUID
	if err := sharedSuper.QueryRow(ctx, `INSERT INTO emr.raw_records (hospital_id, source_kind, connection_id, resource_kind,
		source_id, source_version, payload, payload_sha256, connector_type, connector_version)
		VALUES ($1, 'connector', $2, 'observation', $3, '1', '{}', $4, 'fhir_r4', 1) RETURNING id`,
		w.h, w.conn, "obs-"+src, sum[:]).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := sharedSuper.QueryRow(ctx, `INSERT INTO emr.observations (hospital_id, patient_id, category, loinc, local_display,
		value_num, unit_ucum, value_si, ref_low, ref_high, source_flag, status, effective_at, standardised, raw_id, source_kind,
		connection_id, connector_type, connector_version, source_id, source_version, received_at, mapping_version,
		normaliser_version)
		VALUES ($1, $2, 'laboratory', '99901-1', 'Potassium', $3, 'mmol/L', $3, 3.5, 5.1, 'HH', $4, now(), true, $5,
		        'connector', $6, 'fhir_r4', 1, $7, '1', now(), 'synthetic-map', 'n1') RETURNING id`,
		w.h, p, value, status, raw, w.conn, "obs-"+src).Scan(&obs); err != nil {
		t.Fatal(err)
	}
	return obs
}

type fspec struct {
	severity, status string
	shadow           bool
	supersedes       *uuid.UUID
	ago              time.Duration
}

// addFinding adds a finding of patient p on observation obs.
func (w *world) addFinding(t *testing.T, p, obs uuid.UUID, s fspec) uuid.UUID {
	t.Helper()
	if s.severity == "" {
		s.severity = "critical"
	}
	if s.status == "" {
		s.status = "open"
	}
	sum := sha256.Sum256([]byte("1"))
	var id uuid.UUID
	if err := sharedSuper.QueryRow(context.Background(), `INSERT INTO emr.findings (hospital_id, patient_id, connection_id,
		rule_set_version, rule_set_sha256, rule_id, kind, category, severity, primary_observation_id, effective_at,
		received_at, status, shadow, preliminary, arrived_late, standardised, supersedes, dedup_key)
		VALUES ($1, $2, $3, '1', $4, 'potassium-critical-high', 'threshold', 'blood_chemistry', $5, $6, now() - $7::interval,
		        now(), $8, $9, false, false, true, $10, $11) RETURNING id`,
		w.h, p, w.conn, sum[:], s.severity, obs, s.ago.String(), s.status, s.shadow, s.supersedes, uuid.NewString()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// amendFinding is the emr service's supersession: a new observation and
// finding naming the old one, the old one superseded.
func (w *world) amendFinding(t *testing.T, old uuid.UUID, value float64, severity string) uuid.UUID {
	t.Helper()
	var p uuid.UUID
	if err := sharedSuper.QueryRow(context.Background(), `SELECT patient_id FROM emr.findings WHERE id = $1`, old).Scan(&p); err != nil {
		t.Fatal(err)
	}
	obs := w.observation(t, p, value, "amended")
	f := w.addFinding(t, p, obs, fspec{severity: severity, supersedes: &old})
	exec(t, `UPDATE emr.findings SET status = 'superseded', superseded_by = $2 WHERE id = $1`, old, f)
	return f
}

func withdrawFinding(t *testing.T, f uuid.UUID, reason string) {
	t.Helper()
	exec(t, `UPDATE emr.findings SET status = 'withdrawn', withdrawn_reason = $2 WHERE id = $1`, f, reason)
}

type alertRow struct {
	id, finding        uuid.UUID
	patient            *uuid.UUID
	status, severity   string
	queue              bool
	withdrawn, amended *string
	handledBy          *uuid.UUID
	notified           *time.Time
	deliveries         int
}

func alertRows(t *testing.T, h uuid.UUID) []alertRow {
	t.Helper()
	rows, err := sharedSuper.Query(context.Background(), `SELECT a.id, coalesce(a.finding_id, '00000000-0000-0000-0000-000000000000'),
		a.patient_id, a.status, a.severity, a.routed_queue, a.withdrawn, a.amended::text, a.handled_by, a.last_notified_at,
		(SELECT count(*) FROM alert_deliveries d WHERE d.alert_id = a.id)
		FROM alerts a WHERE a.hospital_id = $1 ORDER BY a.created_at, a.id`, h)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (alertRow, error) {
		var a alertRow
		return a, r.Scan(&a.id, &a.finding, &a.patient, &a.status, &a.severity, &a.queue, &a.withdrawn, &a.amended,
			&a.handledBy, &a.notified, &a.deliveries)
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustHandle(t *testing.T, c *alerts.Consumer, h, f uuid.UUID) {
	t.Helper()
	if err := c.HandleFinding(context.Background(), h, f); err != nil {
		t.Fatalf("handle %s: %v", f, err)
	}
}

func lastPayload(t *testing.T, h uuid.UUID, action string) map[string]any {
	t.Helper()
	var raw []byte
	var actor *uuid.UUID
	if err := sharedSuper.QueryRow(context.Background(), `SELECT payload, actor_id FROM audit_log WHERE hospital_id = $1 AND action = $2
		ORDER BY id DESC LIMIT 1`, h, action).Scan(&raw, &actor); err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	if actor != nil {
		t.Fatalf("%s has an actor", action)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// An open, active, fresh critical finding becomes one alert on the queue
// (no settings saved: the defaults), delivered to the named clinicians
// when there are any, audited alert.raised with ids and codes only, and
// listed to a clinician.
func TestFindingRaisesRoutedAlertAudited(t *testing.T) {
	w := newWorld(t)
	c := w.consumer()
	mustHandle(t, c, w.h, w.finding)
	got := alertRows(t, w.h)
	if len(got) != 1 || got[0].finding != w.finding || got[0].status != "new" || !got[0].queue || got[0].severity != "critical" ||
		got[0].notified == nil || got[0].deliveries != 0 {
		t.Fatalf("alerts %+v", got)
	}
	p := lastPayload(t, w.h, "alert.raised")
	if p["alert_id"] != got[0].id.String() || p["finding_id"] != w.finding.String() || p["routed_queue"] != true ||
		p["routed_named"] != float64(0) || p["rule_id"] != "potassium-critical-high" || p["grouped"] != false {
		t.Fatalf("payload %v", p)
	}
	_, page := w.clin(w.u, "GET", "/alerts", "")
	if l := ids(page); len(l) != 1 || l[0] != got[0].id.String() {
		t.Fatalf("listed %v", page)
	}

	// Named clinicians with the queue off: one delivery each, not queued.
	if code, out := w.adm("PATCH", "/hospital/alert-settings",
		`{"version": 0, "recipients": {"queue": false, "clinician_ids": ["`+w.v.String()+`"]}}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	p2, obs2, f2 := w.newPatient(t, "src-2", "SYN-0002")
	_, _ = p2, obs2
	mustHandle(t, c, w.h, f2)
	got = alertRows(t, w.h)
	if len(got) != 2 || got[1].queue || got[1].deliveries != 1 {
		t.Fatalf("named %+v", got)
	}
	if _, page := w.clin(w.u, "GET", "/alerts", ""); len(ids(page)) != 1 {
		t.Fatalf("u sees v's alert: %v", ids(page))
	}
	if strings.Contains(auditPayloads(t, w.h), "SYN-") || strings.Contains(auditPayloads(t, w.h), "6.8371") {
		t.Fatal("an audit payload carries patient data")
	}
}

// A13 (message part): a finding id of another hospital, sent on this
// hospital's subject, is not found under RLS: no alert anywhere.
func TestFindingOfOtherHospitalNoAlert(t *testing.T) {
	w, other := newWorld(t), newWorld(t)
	c := w.consumer()
	mustHandle(t, c, w.h, other.finding)
	if len(alertRows(t, w.h)) != 0 || len(alertRows(t, other.h)) != 0 || c.Metrics.Foreign.Load() != 1 {
		t.Fatalf("an alert was raised across hospitals (foreign %d)", c.Metrics.Foreign.Load())
	}
}

// A18, K11: historical and shadow findings are acked with no alert and
// nothing recorded.
func TestHistoricalAndShadowAckedNoAlert(t *testing.T) {
	w := newWorld(t)
	c := w.consumer()
	hist := w.addFinding(t, w.patient, w.observation(t, w.patient, 7.1, "final"), fspec{status: "historical"})
	shadow := w.addFinding(t, w.patient, w.observation(t, w.patient, 7.2, "final"), fspec{shadow: true})
	mustHandle(t, c, w.h, hist)
	mustHandle(t, c, w.h, shadow)
	if len(alertRows(t, w.h)) != 0 || auditCount(t, w.h, "alert.not_raised") != 0 || c.Metrics.Skipped.Load() != 2 {
		t.Fatalf("historical or shadow alerted (skipped %d)", c.Metrics.Skipped.Load())
	}
}

// Design §12.1 step 2: a deactivated hospital, or one with no connection
// switched on, raises nothing; the finding is recorded with the reason.
func TestInactiveHospitalOrNoConnectionNotRaised(t *testing.T) {
	w := newWorld(t)
	c := w.consumer()
	exec(t, `UPDATE emr_connections SET enabled = false WHERE id = $1`, w.conn)
	mustHandle(t, c, w.h, w.finding)
	if len(alertRows(t, w.h)) != 0 || lastPayload(t, w.h, "alert.not_raised")["reason"] != "no_connection" {
		t.Fatal("raised with no connection on")
	}
	w2 := newWorld(t)
	exec(t, `UPDATE hospitals SET status = 'deactivated' WHERE id = $1`, w2.h)
	mustHandle(t, c, w2.h, w2.finding)
	if len(alertRows(t, w2.h)) != 0 || lastPayload(t, w2.h, "alert.not_raised")["reason"] != "hospital_inactive" {
		t.Fatal("raised for a deactivated hospital")
	}
}

// Design §12.1 step 3: a unit not covered, or a level that doesn't admit
// the severity, is recorded (alert.not_raised with the reason).
func TestUnitNotCoveredAndLevelOffRecorded(t *testing.T) {
	w := newWorld(t)
	c := w.consumer()
	unit := uuid.New()
	exec(t, `INSERT INTO emr.units (id, hospital_id, connection_id, source_location_id, label, kind)
		VALUES ($1, $2, $3, 'ward-1', 'Ward 1', 'ward')`,
		unit, w.h, w.conn)
	sum := sha256.Sum256([]byte(unit.String()))
	var raw uuid.UUID
	if err := sharedSuper.QueryRow(context.Background(), `INSERT INTO emr.raw_records (hospital_id, source_kind, connection_id,
		resource_kind, source_id, source_version, payload, payload_sha256, connector_type, connector_version)
		VALUES ($1, 'connector', $2, 'encounter', 'enc-1', '1', '{}', $3, 'fhir_r4', 1) RETURNING id`, w.h, w.conn, sum[:]).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	exec(t, `INSERT INTO emr.encounters (hospital_id, patient_id, class, status, period_start, unit_id, raw_id, source_kind,
		connection_id, connector_type, connector_version, source_id, source_version, received_at, mapping_version, normaliser_version)
		VALUES ($1, $2, 'inpatient', 'in-progress', now(), $3, $4, 'connector', $5, 'fhir_r4', 1, 'enc-1', '1', now(), 'm', 'n1')`,
		w.h, w.patient, unit, raw, w.conn)
	exec(t, `INSERT INTO alert_units (hospital_id, unit_id, covered) VALUES ($1, $2, false)`, w.h, unit)
	mustHandle(t, c, w.h, w.finding)
	if len(alertRows(t, w.h)) != 0 || lastPayload(t, w.h, "alert.not_raised")["reason"] != "unit_not_covered" {
		t.Fatal("raised for a unit not covered")
	}
	exec(t, `UPDATE alert_units SET covered = true WHERE unit_id = $1`, unit)
	exec(t, `INSERT INTO alert_levels (hospital_id, category, level) VALUES ($1, 'blood_chemistry', 'off')`, w.h)
	f := w.addFinding(t, w.patient, w.observation(t, w.patient, 7.0, "final"), fspec{})
	mustHandle(t, c, w.h, f)
	if len(alertRows(t, w.h)) != 0 || lastPayload(t, w.h, "alert.not_raised")["reason"] != "level_off" {
		t.Fatal("raised with the level off")
	}
}

// Design §12.2: a patient with no current location is covered.
func TestNoLocationIsCovered(t *testing.T) {
	w := newWorld(t)
	mustHandle(t, w.consumer(), w.h, w.finding)
	if len(alertRows(t, w.h)) != 1 {
		t.Fatal("a patient with no location wasn't alerted")
	}
}

// Design §16.4: a patient with no MRN raises nothing unless
// CORE_EMR_ALLOW_NULL_MRN is on.
func TestNullMRNGated(t *testing.T) {
	w := newWorld(t)
	exec(t, `UPDATE emr.patients SET mrn = NULL WHERE id = $1`, w.patient)
	mustHandle(t, w.consumer(), w.h, w.finding)
	if len(alertRows(t, w.h)) != 0 || lastPayload(t, w.h, "alert.not_raised")["reason"] != "null_mrn" {
		t.Fatal("raised with a null MRN")
	}
	f := w.addFinding(t, w.patient, w.observation(t, w.patient, 7.3, "final"), fspec{})
	c := w.consumer()
	c.AllowNullMRN = true
	mustHandle(t, c, w.h, f)
	if len(alertRows(t, w.h)) != 1 {
		t.Fatal("not raised with null MRNs allowed")
	}
}

// A19: the same finding five times in a row, and twenty times at once
// (core copies in parallel), makes one alert and one audit row.
func TestRedeliveryAndParallelConsumersOneAlert(t *testing.T) {
	w := newWorld(t)
	c := w.consumer()
	for i := 0; i < 5; i++ {
		mustHandle(t, c, w.h, w.finding)
	}
	_, _, f2 := w.newPatient(t, "src-2", "SYN-0002")
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- w.consumer().HandleFinding(context.Background(), w.h, f2)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("parallel: %v", err)
		}
	}
	if got := alertRows(t, w.h); len(got) != 2 {
		t.Fatalf("%d alerts", len(got))
	}
	if n := auditCount(t, w.h, "alert.raised"); n != 2 {
		t.Fatalf("%d alert.raised rows", n)
	}
}

// Design §12.4: a second hit on the same rule and analyte while the alert
// is new updates it, without a new alert; a higher severity re-notifies; a
// lower one never downgrades it.
func TestSecondHitUpdatesOpenAlert(t *testing.T) {
	w := newWorld(t)
	c := w.consumer()
	first := w.addFinding(t, w.patient, w.observation(t, w.patient, 5.9, "final"), fspec{severity: "urgent", ago: time.Hour})
	mustHandle(t, c, w.h, first)
	before := alertRows(t, w.h)
	if len(before) != 1 || before[0].severity != "urgent" {
		t.Fatalf("%+v", before)
	}
	exec(t, `UPDATE alerts SET last_notified_at = now() - interval '1 hour' WHERE id = $1`, before[0].id)
	mustHandle(t, c, w.h, w.finding) // critical, newer
	after := alertRows(t, w.h)
	if len(after) != 1 || after[0].finding != w.finding || after[0].severity != "critical" ||
		!after[0].notified.After(time.Now().Add(-time.Minute)) {
		t.Fatalf("severity rise: %+v", after)
	}
	if p := lastPayload(t, w.h, "alert.updated"); p["change"] != "new_hit" || p["severity"] != "critical" {
		t.Fatalf("payload %v", p)
	}
	lower := w.addFinding(t, w.patient, w.observation(t, w.patient, 5.8, "final"), fspec{severity: "urgent"})
	mustHandle(t, c, w.h, lower)
	if got := alertRows(t, w.h); len(got) != 1 || got[0].severity != "critical" || got[0].finding != w.finding {
		t.Fatalf("downgraded: %+v", got)
	}
	if lastPayload(t, w.h, "alert.not_raised")["reason"] != "already_open" {
		t.Fatal("the lower hit wasn't recorded")
	}
	// Handled: a new hit at the same severity raises a new alert (no quiet
	// hours yet: fail toward alerting).
	exec(t, `UPDATE alerts SET status = 'handled', handled_by = $2, handled_at = now() WHERE id = $1`, after[0].id, w.u)
	again := w.addFinding(t, w.patient, w.observation(t, w.patient, 7.4, "final"), fspec{})
	mustHandle(t, c, w.h, again)
	if got := alertRows(t, w.h); len(got) != 2 || got[1].status != "new" || got[1].finding != again {
		t.Fatalf("after handling: %+v", got)
	}
}

// A30: an amended result still critical updates the open alert in place
// (amended, previous value), and either message order gives the same.
func TestAmendedToCriticalUpdatedInPlace(t *testing.T) {
	for _, successorFirst := range []bool{false, true} {
		w := newWorld(t)
		c := w.consumer()
		mustHandle(t, c, w.h, w.finding)
		f2 := w.amendFinding(t, w.finding, 7.9413, "critical")
		if successorFirst {
			mustHandle(t, c, w.h, f2)
			mustHandle(t, c, w.h, w.finding)
		} else {
			mustHandle(t, c, w.h, w.finding)
			mustHandle(t, c, w.h, f2)
		}
		got := alertRows(t, w.h)
		if len(got) != 1 || got[0].finding != f2 || got[0].status != "new" || got[0].amended == nil ||
			!strings.Contains(*got[0].amended, `"previous_value": "6.8371"`) {
			t.Fatalf("successor first %v: %+v", successorFirst, got)
		}
		if auditCount(t, w.h, "alert.updated") != 1 || auditCount(t, w.h, "alert.raised") != 1 {
			t.Fatalf("audit: updated %d raised %d", auditCount(t, w.h, "alert.updated"), auditCount(t, w.h, "alert.raised"))
		}
	}
}

// A30: an amended result that no rule hits withdraws the alert
// (source_amended); an entered-in-error one withdraws it
// (source_retracted). A withdrawn alert reads dismissed, by nobody.
func TestAmendedToNormalAndEnteredInErrorWithdrawn(t *testing.T) {
	for _, reason := range []string{"source_amended", "source_retracted"} {
		w := newWorld(t)
		c := w.consumer()
		mustHandle(t, c, w.h, w.finding)
		withdrawFinding(t, w.finding, reason)
		mustHandle(t, c, w.h, w.finding)
		mustHandle(t, c, w.h, w.finding)
		got := alertRows(t, w.h)
		if len(got) != 1 || got[0].status != "dismissed" || got[0].withdrawn == nil || *got[0].withdrawn != reason ||
			got[0].handledBy != nil {
			t.Fatalf("%s: %+v", reason, got)
		}
		if auditCount(t, w.h, "alert.withdrawn") != 1 || lastPayload(t, w.h, "alert.withdrawn")["reason"] != reason {
			t.Fatalf("%s: audit", reason)
		}
		if _, cnt := w.clin(w.u, "GET", "/alerts/unread-count", ""); cnt["count"] != float64(0) {
			t.Fatalf("%s: still unread", reason)
		}
	}
	// A withdrawal whose successor message arrives first: the chain's head
	// withdrawn withdraws the alert on the superseded finding.
	w := newWorld(t)
	c := w.consumer()
	mustHandle(t, c, w.h, w.finding)
	f2 := w.amendFinding(t, w.finding, 7.9413, "critical")
	withdrawFinding(t, f2, "source_retracted")
	mustHandle(t, c, w.h, w.finding)
	if got := alertRows(t, w.h); len(got) != 1 || got[0].withdrawn == nil || *got[0].withdrawn != "source_retracted" {
		t.Fatalf("chain withdrawal: %+v", got)
	}
}

// A30: a handled alert is never reopened, but gets the amended or
// withdrawn mark; a higher severity after handling alerts anew.
func TestHandledGetsMarkNotReopened(t *testing.T) {
	w := newWorld(t)
	c := w.consumer()
	// The world's own critical finding is out of the way, so no other open
	// finding of the rule holds the new alert up at the end.
	withdrawFinding(t, w.finding, "source_amended")
	urgent := w.addFinding(t, w.patient, w.observation(t, w.patient, 5.9, "final"), fspec{severity: "urgent"})
	mustHandle(t, c, w.h, urgent)
	a := alertRows(t, w.h)[0]
	if code, _ := w.clin(w.u, "POST", "/alerts/"+a.id.String()+"/acknowledge", ""); code != 200 {
		t.Fatalf("ack %d", code)
	}
	f2 := w.amendFinding(t, urgent, 6.0, "urgent")
	mustHandle(t, c, w.h, f2)
	got := alertRows(t, w.h)
	if len(got) != 1 || got[0].status != "handled" || got[0].amended == nil || got[0].handledBy == nil || *got[0].handledBy != w.u {
		t.Fatalf("amended handled: %+v", got)
	}
	f3 := w.amendFinding(t, f2, 7.9413, "critical")
	mustHandle(t, c, w.h, f3)
	got = alertRows(t, w.h)
	if len(got) != 2 || got[0].status != "handled" || got[0].severity != "urgent" || got[1].status != "new" ||
		got[1].severity != "critical" {
		t.Fatalf("rose after handling: %+v", got)
	}
	withdrawFinding(t, f3, "source_retracted")
	mustHandle(t, c, w.h, f3)
	got = alertRows(t, w.h)
	if got[0].status != "handled" || got[0].withdrawn == nil || got[0].handledBy == nil ||
		got[1].status != "dismissed" || got[1].withdrawn == nil {
		t.Fatalf("withdrawn after handling: %+v", got)
	}
}

// Review Focus 5: an amendment racing an acknowledgement (20 pairs): the
// acknowledgement is never lost, the alert never reopens, and it ends on
// the amended finding with the mark.
func TestAmendRacesAcknowledge(t *testing.T) {
	for i := 0; i < 20; i++ {
		w := newWorld(t)
		c := w.consumer()
		mustHandle(t, c, w.h, w.finding)
		a := alertRows(t, w.h)[0]
		f2 := w.amendFinding(t, w.finding, 7.9413, "critical")
		var wg sync.WaitGroup
		var code int
		var err error
		wg.Add(2)
		go func() { defer wg.Done(); err = c.HandleFinding(context.Background(), w.h, f2) }()
		go func() { defer wg.Done(); code, _ = w.clin(w.u, "POST", "/alerts/"+a.id.String()+"/acknowledge", "") }()
		wg.Wait()
		if err != nil || code != 200 {
			t.Fatalf("pair %d: %v %d", i, err, code)
		}
		got := alertRows(t, w.h)
		if len(got) != 1 || got[0].status != "handled" || got[0].handledBy == nil || got[0].finding != f2 || got[0].amended == nil {
			t.Fatalf("pair %d: %+v", i, got)
		}
	}
}

// inGateway runs q as the gateway for hospital h in an open transaction
// the caller commits.
func inGateway(t *testing.T, h uuid.UUID, q string, args ...any) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := sharedSuper.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`SET LOCAL ROLE mhai_gateway`, `SELECT set_config('mhai.hospital_id', '` + h.String() + `', true)`} {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, q, args...); err != nil {
		t.Fatal(err)
	}
	return tx
}

// An exclusion committing while the consumer raises: the consumer waits
// on the patient, sees it excluded and retries; the finding is gone, so no
// alert names the excluded patient.
func TestExclusionRaceNoAlert(t *testing.T) {
	w := newWorld(t)
	tx := inGateway(t, w.h, `SELECT * FROM emr_exclude_patient($1, 'objection')`, w.patient)
	done := make(chan error, 1)
	go func() { done <- w.consumer().HandleFinding(context.Background(), w.h, w.finding) }()
	time.Sleep(500 * time.Millisecond)
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("handle: %v", err)
	}
	for _, a := range alertRows(t, w.h) {
		if a.patient != nil {
			t.Fatalf("an alert names the excluded patient: %+v", a)
		}
	}
	if auditCount(t, w.h, "alert.raised") != 0 {
		t.Fatal("raised for an excluded patient")
	}
}

// A merge committing while the consumer raises: the alert lands on the
// kept patient, never the merged one.
func TestMergeRaceAlertOnKeptPatient(t *testing.T) {
	w := newWorld(t)
	b, _, _ := w.newPatient(t, "src-2", "SYN-0002")
	m := w.match(w.patient, b)
	tx := inGateway(t, w.h, `SELECT emr_merge_patients($1, 'b')`, m)
	done := make(chan error, 1)
	go func() { done <- w.consumer().HandleFinding(context.Background(), w.h, w.finding) }()
	time.Sleep(500 * time.Millisecond)
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("handle: %v", err)
	}
	got := alertRows(t, w.h)
	if len(got) != 1 || got[0].patient == nil || *got[0].patient != b {
		t.Fatalf("alerts after the merge: %+v", got)
	}
}

// A breaker trip is audited once (emr.source_paused), with ids only; a
// trip naming another hospital's connection records nothing.
func TestBreakerSignalAuditedOnce(t *testing.T) {
	w, other := newWorld(t), newWorld(t)
	c := w.consumer()
	trip := uuid.New()
	sig := &emrpb.Signal{Kind: "breaker_tripped", ConnectionId: w.conn.String(), RefId: trip.String()}
	for i := 0; i < 3; i++ {
		if err := c.HandleSignal(context.Background(), w.h, sig); err != nil {
			t.Fatal(err)
		}
	}
	if auditCount(t, w.h, "emr.source_paused") != 1 || c.Metrics.SourcePaused.Load() != 1 {
		t.Fatalf("paused audited %d", auditCount(t, w.h, "emr.source_paused"))
	}
	p := lastPayload(t, w.h, "emr.source_paused")
	if p["connection_id"] != w.conn.String() || p["reason"] != "breaker_tripped" {
		t.Fatalf("payload %v", p)
	}
	forged := &emrpb.Signal{Kind: "breaker_tripped", ConnectionId: other.conn.String(), RefId: uuid.NewString()}
	if err := c.HandleSignal(context.Background(), w.h, forged); err != nil {
		t.Fatal(err)
	}
	if auditCount(t, w.h, "emr.source_paused") != 1 || auditCount(t, other.h, "emr.source_paused") != 0 {
		t.Fatal("another hospital's connection was recorded")
	}
	for _, k := range []string{"possible_match", "source_merge", "rule_set_activated", "document_rejected", "unknown"} {
		if err := c.HandleSignal(context.Background(), w.h, &emrpb.Signal{Kind: k, RefId: uuid.NewString()}); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
}

// A22 (core part): the consumer's logs, audit rows and dead letters carry
// ids and codes: no name, MRN or value.
func TestConsumerNoPHIInLogsOrAudit(t *testing.T) {
	w := newWorld(t)
	c := w.consumer()
	mustHandle(t, c, w.h, w.finding)
	f2 := w.amendFinding(t, w.finding, 7.9413, "critical")
	mustHandle(t, c, w.h, f2)
	withdrawFinding(t, f2, "source_retracted")
	mustHandle(t, c, w.h, f2)
	mustHandle(t, c, w.h, uuid.New())
	// Values distinctive enough that no timestamp or id can contain them
	// (a plain "6.8" matched a log line's time, e.g. "…:16.8…").
	requireAbsent(t, w, "SYN-0001", "Synthetic Patient", "6.8371", "7.9413", "Potassium")
	_ = authz.RoleClinician
}

// A new alert that a second hit moved stands for both findings: when the
// newer one is retracted or amended down, the alert goes back to the
// other open finding (never withdrawn, never below it).
func TestGroupedAlertNeverDropsAnOpenFinding(t *testing.T) {
	w := newWorld(t)
	c := w.consumer()
	mustHandle(t, c, w.h, w.finding) // F0 critical
	f1 := w.addFinding(t, w.patient, w.observation(t, w.patient, 7.0, "final"), fspec{})
	mustHandle(t, c, w.h, f1) // new hit: the alert moves to F1
	if got := alertRows(t, w.h); len(got) != 1 || got[0].finding != f1 {
		t.Fatalf("new hit: %+v", got)
	}
	withdrawFinding(t, f1, "source_retracted")
	mustHandle(t, c, w.h, f1)
	got := alertRows(t, w.h)
	if len(got) != 1 || got[0].status != "new" || got[0].withdrawn != nil || got[0].finding != w.finding ||
		got[0].severity != "critical" {
		t.Fatalf("retracting F1 dropped F0: %+v", got)
	}
	if lastPayload(t, w.h, "alert.updated")["change"] != "regrouped" {
		t.Fatal("not audited as regrouped")
	}
	// Amended down to urgent while F0 is still critical: stays critical on F0.
	f2 := w.addFinding(t, w.patient, w.observation(t, w.patient, 7.1, "final"), fspec{})
	mustHandle(t, c, w.h, f2)
	f3 := w.amendFinding(t, f2, 5.9, "urgent")
	mustHandle(t, c, w.h, f3)
	got = alertRows(t, w.h)
	if len(got) != 1 || got[0].severity != "critical" || got[0].status != "new" {
		t.Fatalf("amended below an open critical: %+v", got)
	}
}

// K11: a shadow successor (the hospital went back to shadow) never
// changes a live alert.
func TestShadowSuccessorLeavesLiveAlert(t *testing.T) {
	w := newWorld(t)
	c := w.consumer()
	mustHandle(t, c, w.h, w.finding)
	obs := w.observation(t, w.patient, 9.9, "amended")
	f2 := w.addFinding(t, w.patient, obs, fspec{shadow: true, supersedes: &w.finding})
	exec(t, `UPDATE emr.findings SET status = 'superseded', superseded_by = $2 WHERE id = $1`, w.finding, f2)
	mustHandle(t, c, w.h, w.finding)
	got := alertRows(t, w.h)
	if len(got) != 1 || got[0].finding != w.finding || got[0].amended != nil || got[0].status != "new" {
		t.Fatalf("a shadow finding changed the alert: %+v", got)
	}
}

// requireAbsent fails, naming the line, when a log line or an audit
// payload of the world's hospital holds one of the synthetic values.
func requireAbsent(t *testing.T, w *world, values ...string) {
	t.Helper()
	rows, err := sharedSuper.Query(context.Background(), `SELECT action, payload::text FROM audit_log WHERE hospital_id = $1 ORDER BY id`, w.h)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for rows.Next() {
		var action, payload string
		if err := rows.Scan(&action, &payload); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, "audit "+action+": "+payload)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(w.logs.String(), "\n") {
		lines = append(lines, "log: "+l)
	}
	for _, v := range values {
		for _, l := range lines {
			if strings.Contains(l, v) {
				t.Errorf("synthetic value %q in %s", v, l)
			}
		}
	}
}
