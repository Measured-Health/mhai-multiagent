//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// The EMR operator CLI (EMR PR 9): the exclusion fallback as the
// operator's mhai_emr_admin login, legal holds and the removal hook as the
// provisioning login. Every name, MRN and value is synthetic.

type emrCLIWorld struct {
	super                *pgxpool.Pool
	provURL, adminURL    string
	h, conn, patient, al uuid.UUID
	code                 string
}

func newEMRCLIWorld(t *testing.T) *emrCLIWorld {
	t.Helper()
	ctx := context.Background()
	pg, super := testutil.MigratedPostgres(t)
	_, provURL, err := testutil.ProvisionLogin(ctx, pg, super)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureLogin(ctx, super, "emr_operator", "test-only-emr-operator-pw", "mhai_emr_admin"); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("emr_operator", "test-only-emr-operator-pw")
	w := &emrCLIWorld{super: super, provURL: provURL, adminURL: u.String(), h: uuid.New(), conn: uuid.New(),
		code: "EC" + strings.ToUpper(uuid.NewString()[:6])}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO hospitals (id, name, short_code, status, time_zone) VALUES ($1, 'Synthetic Hospital', $2, 'active', 'Africa/Lagos')`, []any{w.h, w.code}},
		{`INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url) VALUES ($1, $2, 'fhir_r4', 'Synthetic record', 'https://fhir.example.org/r4')`, []any{w.conn, w.h}},
	} {
		if _, err := super.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := super.QueryRow(ctx, `INSERT INTO emr.patients (hospital_id, display_name, mrn, birth_date, sex)
		VALUES ($1, 'Synthetic Patient', 'SYN-6837', '1970-01-01', 'female') RETURNING id`, w.h).Scan(&w.patient); err != nil {
		t.Fatal(err)
	}
	var raw uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO emr.raw_records (hospital_id, source_kind, connection_id, resource_kind, source_id,
		payload, payload_sha256, connector_type, connector_version)
		VALUES ($1, 'connector', $2, 'observation', 'obs-cli', '{"patient": "p-cli", "value_quantity": "6.8371"}', $3, 'fhir_r4', 1)
		RETURNING id`, w.h, w.conn, bytes.Repeat([]byte{7}, 32)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO emr.patient_links (hospital_id, connection_id, source_patient_id, patient_id) VALUES ($1, $2, 'p-cli', $3)`,
		`INSERT INTO emr.observations (hospital_id, patient_id, category, loinc, value_num, unit_ucum, value_si, status, effective_at,
			standardised, raw_id, source_kind, connection_id, connector_type, connector_version, source_id, source_version, received_at,
			mapping_version, normaliser_version)
		 VALUES ($1, $3, 'laboratory', '99901-1', 6.8371, 'mmol/L', 6.8371, 'final', now(), true, $4, 'connector', $2, 'fhir_r4', 1,
			'obs-cli', '1', now(), 'synthetic-map', 'n1')`,
	} {
		args := []any{w.h, w.conn, w.patient}
		if strings.Contains(q, "$4") {
			args = append(args, raw)
		}
		if _, err := super.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := super.QueryRow(ctx, `INSERT INTO alerts (hospital_id, patient_id, connection_id, rule_id, category, severity, routed_queue)
		VALUES ($1, $2, $3, 'r-cli', 'blood_chemistry', 'critical', true) RETURNING id`, w.h, w.patient, w.conn).Scan(&w.al); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *emrCLIWorld) audit(t *testing.T, action string) (n int, payloads string, actors int) {
	t.Helper()
	if err := w.super.QueryRow(context.Background(), `SELECT count(*), coalesce(string_agg(payload::text, ' '), ''),
		count(actor_id) FROM audit_log WHERE hospital_id = $1 AND action = $2`, w.h, action).Scan(&n, &payloads, &actors); err != nil {
		t.Fatal(err)
	}
	return n, payloads, actors
}

// Task 34's CLI fallback, moved to PR 9: the operator excludes by MRN read
// from stdin, through the same function as the route, audited via cli
// with no actor; the output and the audit carry ids only. Another login
// can't run it.
func TestCLIExcludeAudited(t *testing.T) {
	w := newEMRCLIWorld(t)
	ctx := context.Background()
	args := []string{"exclude", "--hospital", w.code, "--mrn-stdin", "--reason", "objection"}
	if err := emrCmd(ctx, args, w.provURL, strings.NewReader("SYN-6837\n"), &bytes.Buffer{}); !errors.Is(err, errNotEMRAdmin) {
		t.Fatalf("as the provisioning login: %v", err)
	}
	if err := emrCmd(ctx, []string{"exclude", "--hospital", w.code, "--mrn-stdin", "--reason", "other"}, w.adminURL,
		strings.NewReader("SYN-0000\n"), &bytes.Buffer{}); !errors.Is(err, errNoPatient) {
		t.Fatalf("unknown MRN: %v", err)
	}
	var out bytes.Buffer
	if err := emrCmd(ctx, args, w.adminURL, strings.NewReader("SYN-6837\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), w.patient.String()) || strings.Contains(out.String(), "SYN-6837") {
		t.Fatalf("output: %s", out.String())
	}
	var status string
	var obs int
	if err := w.super.QueryRow(ctx, `SELECT p.status, (SELECT count(*) FROM emr.observations o WHERE o.patient_id = p.id)
		FROM emr.patients p WHERE p.id = $1`, w.patient).Scan(&status, &obs); err != nil || status != "excluded" || obs != 0 {
		t.Fatalf("patient %s with %d observations (%v)", status, obs, err)
	}
	for _, action := range []string{"emr.patient_excluded", "alert.withdrawn", "emr.exclusion_purged"} {
		n, payloads, actors := w.audit(t, action)
		if n != 1 || actors != 0 || strings.Contains(payloads, "SYN-6837") || strings.Contains(payloads, "Synthetic Patient") {
			t.Errorf("%s: %d rows, %d with an actor: %s", action, n, actors, payloads)
		}
		if action != "alert.withdrawn" && !strings.Contains(payloads, `"via": "cli"`) {
			t.Errorf("%s not via cli: %s", action, payloads)
		}
	}
	// The emr admin login can do nothing else: not a legal hold.
	if err := hospitalsLegalHold(ctx, []string{"place", w.code, "--reference", "LH-2026-0042"}, w.adminURL, &out); !errors.Is(err, account.ErrNotProvisioner) {
		t.Fatalf("legal hold as the emr admin login: %v", err)
	}
}

// K8: only the provisioning login places and lifts a hold; each is audited
// with the hold's id, never the reference; a hold defers the CLI's purge.
func TestLegalHoldCLIProvisionOnly(t *testing.T) {
	w := newEMRCLIWorld(t)
	ctx := context.Background()
	pg := w.adminURL // any non-provisioning login
	if err := hospitalsLegalHold(ctx, []string{"place", w.code, "--reference", "LH-2026-0042"}, pg, &bytes.Buffer{}); !errors.Is(err, account.ErrNotProvisioner) {
		t.Fatalf("place as another login: %v", err)
	}
	var out bytes.Buffer
	if err := hospitalsLegalHold(ctx, []string{"place", w.code, "--reference", "LH-2026-0042"}, w.provURL, &out); err != nil {
		t.Fatal(err)
	}
	if err := hospitalsLegalHold(ctx, []string{"place", w.code, "--reference", "LH-2026-0042"}, w.provURL, &out); !errors.Is(err, errLegalHoldExists) {
		t.Fatalf("the same hold twice: %v", err)
	}
	if strings.Contains(out.String(), "LH-2026-0042") {
		t.Fatalf("output names the reference: %s", out.String())
	}
	if err := emrCmd(ctx, []string{"exclude", "--hospital", w.code, "--patient", w.patient.String(), "--reason", "objection"},
		w.adminURL, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	var obs int
	if err := w.super.QueryRow(ctx, `SELECT count(*) FROM emr.observations WHERE patient_id = $1`, w.patient).Scan(&obs); err != nil || obs != 1 {
		t.Fatalf("purged under the hold: %d %v", obs, err)
	}
	if n, _, _ := w.audit(t, "emr.exclusion_purge_held"); n != 1 {
		t.Fatalf("held purge audited %d times", n)
	}
	if err := hospitalsLegalHold(ctx, []string{"lift", w.code, "--reference", "LH-2026-0042"}, w.provURL, &out); err != nil {
		t.Fatal(err)
	}
	if err := hospitalsLegalHold(ctx, []string{"lift", w.code, "--reference", "LH-2026-0042"}, w.provURL, &out); !errors.Is(err, errLegalHoldNone) {
		t.Fatalf("lift twice: %v", err)
	}
	for _, action := range []string{"legal_hold.placed", "legal_hold.lifted"} {
		n, payloads, actors := w.audit(t, action)
		if n != 1 || actors != 0 || strings.Contains(payloads, "LH-2026-0042") || !strings.Contains(payloads, "hold_id") {
			t.Errorf("%s: %d rows, %d with an actor: %s", action, n, actors, payloads)
		}
	}
}

// Plan departure 11: the removal hook refuses an active hospital and one
// whose credentials aren't wiped; then it removes and audits with counts.
func TestRemoveHospitalDataCLI(t *testing.T) {
	w := newEMRCLIWorld(t)
	ctx := context.Background()
	var out bytes.Buffer
	if err := emrCmd(ctx, []string{"remove-hospital-data", w.code}, w.provURL, nil, &out); !errors.Is(err, errRemovalRefused) {
		t.Fatalf("active hospital: %v", err)
	}
	if _, err := w.super.Exec(ctx, `UPDATE hospitals SET status = 'removal_scheduled' WHERE id = $1`, w.h); err != nil {
		t.Fatal(err)
	}
	if _, err := w.super.Exec(ctx, `INSERT INTO emr.credentials (connection_id, hospital_id, field, set_at, secret_version, origin)
		VALUES ($1, $2, 'private_key', now(), 'v1', '{https://fhir.example.org:443}')`, w.conn, w.h); err != nil {
		t.Fatal(err)
	}
	if err := emrCmd(ctx, []string{"remove-hospital-data", w.code}, w.provURL, nil, &out); !errors.Is(err, errRemovalCreds) {
		t.Fatalf("credentials not wiped: %v", err)
	}
	if _, err := w.super.Exec(ctx, `DELETE FROM emr.credentials WHERE hospital_id = $1`, w.h); err != nil {
		t.Fatal(err)
	}
	if err := emrCmd(ctx, []string{"remove-hospital-data", w.code}, w.adminURL, nil, &out); !errors.Is(err, account.ErrNotProvisioner) {
		t.Fatalf("as the emr admin login: %v", err)
	}
	if err := emrCmd(ctx, []string{"remove-hospital-data", w.code}, w.provURL, nil, &out); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := w.super.QueryRow(ctx, `SELECT (SELECT count(*) FROM emr.patients WHERE hospital_id = $1)
		+ (SELECT count(*) FROM alerts WHERE hospital_id = $1) + (SELECT count(*) FROM emr_connections WHERE hospital_id = $1)`,
		w.h).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d rows left (%v)", left, err)
	}
	if n, payloads, actors := w.audit(t, "emr.hospital_data_removed"); n != 1 || actors != 0 || !strings.Contains(payloads, `"observations": 1`) {
		t.Fatalf("removal audit: %d %d %s", n, actors, payloads)
	}
}
