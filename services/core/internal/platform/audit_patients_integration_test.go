//go:build integration

package platform_test

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
)

// GET /platform/audit/{id}/patients (plan Task 14): a drill-down entry's
// pids, revealed to platform admins and auditors, recorded each time.
// The list is opened as a synthetic hospital admin would (migration
// 00028's function, its audit entry appended in the same transaction).

var pidShape = regexp.MustCompile(`^P-[A-Z2-7]{8}$`)

// drillList opens a de-identified list of n synthetic female subjects in
// a demo hospital's frozen block and returns its audit entry and pids.
func drillList(t *testing.T, n int) (int64, []string, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := super.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	h := uuid.New()
	exec(`INSERT INTO hospitals (id, name, short_code, status, time_zone, is_demo) VALUES ($1, $2, $3, 'active', 'Africa/Lagos', true)`,
		h, "Synthetic Demo "+uniq(), "D"+uniq())
	exec(`INSERT INTO hospital_settings (hospital_id) VALUES ($1)`, h)
	exec(`INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		VALUES (NULL, 'on', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC') ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		VALUES ($1, 'on', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC')`, h)
	period, cond := uuid.New(), uuid.New()
	exec(`INSERT INTO analysis_periods (id, hospital_id, level, start, "end", engine_version) VALUES ($1, $2, 'week', '2026-04-01', '2026-04-07', '1')`, period, h)
	exec(`INSERT INTO analysis_conditions (hospital_id, id, icd10, name) VALUES ($1, $2, 'I10', 'Synthetic hypertension')`, h, cond)
	for i := 0; i < n; i++ {
		s := uuid.New()
		exec(`INSERT INTO analysis_subjects (hospital_id, subject_id, ref_hmac, created_day) VALUES ($1, $2, decode(md5($3) || md5($3), 'hex'), '2026-03-01')`, h, s, s.String())
		exec(`INSERT INTO analysis_facts (hospital_id, subject_id, day, kind, condition_id, age_band, sex, setting, period_late)
			VALUES ($1, $2, '2026-04-02', 'diagnosis', $3, '40-64', 'female', 'outpatient', false)`, h, s, cond)
	}
	admin := seedUser(t, h, "", authz.RoleHospitalAdmin)
	view := uuid.New()
	var pids []string
	if err := db.WithHospitalUser(ctx, app, h, admin.id, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT pid FROM mhai_analysis_open($1, $2, '{}', '{"kind": "part", "dim": "sex", "key": "female"}',
			'affected', $3, $4, 300, NULL)`, view, period, make([]byte, 32), n)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return err
			}
			pids = append(pids, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Event{ActorID: admin.id.String(), HospitalID: h.String(), Action: "analysis.patients_viewed",
			Entity: "analysis_view", EntityID: view.String(), Payload: map[string]any{"view": "affected", "period": "week:2026-04-01",
				"filters": map[string]any{}, "patient_count": n, "rows": n}})
	}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := super.QueryRow(ctx, `SELECT id FROM audit_log WHERE entity = 'analysis_view' AND entity_id = $1`, view.String()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id, pids, h
}

func TestAuditPatientsReveal(t *testing.T) {
	id, pids, h := drillList(t, 7)
	if len(pids) != 7 {
		t.Fatalf("%d pids", len(pids))
	}
	a := auditor(t)
	tok := mint(t, a)
	r := get(t, "/platform/audit/"+strconv.FormatInt(id, 10)+"/patients", tok)
	ok(t, "reveal", r)
	sameKeys(t, "reveal", r.body, "pseudonyms")
	got, _ := r.body["pseudonyms"].([]any)
	if len(got) != len(pids) {
		t.Fatalf("revealed %d, want %d", len(got), len(pids))
	}
	for i, p := range got {
		if p != pids[i] || !pidShape.MatchString(pids[i]) {
			t.Errorf("pid %d not the list's, in order", i)
		}
	}
	if r.hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q", r.hdr.Get("Cache-Control"))
	}
	// A platform admin may too; another entry, or none, is 404.
	ok(t, "a platform admin's reveal", get(t, "/platform/audit/"+strconv.FormatInt(id, 10)+"/patients",
		mint(t, seedUser(t, uuid.Nil, "", authz.RolePlatformAdmin))))
	other := insertRow(t, row{hospital: h, action: "auth.logout"})
	problem(t, "not a drill-down", get(t, "/platform/audit/"+strconv.FormatInt(other, 10)+"/patients", tok), 404, "not_found")
	problem(t, "no entry", get(t, "/platform/audit/987654321099/patients", tok), 404, "not_found")
	problem(t, "with a query", get(t, "/platform/audit/"+strconv.FormatInt(id, 10)+"/patients?x=1", tok), 404, "not_found")
	// A hospital session is refused by the guard.
	clin := seedUser(t, h, "", authz.RoleHospitalAdmin)
	r = do(t, "GET", "/platform/audit/"+strconv.FormatInt(id, 10)+"/patients", mint(t, clin), true)
	if r.code != 403 || strings.Contains(r.raw, "P-") {
		t.Errorf("a hospital session: %d %s", r.code, r.raw)
	}
}

// A reader whose role is taken away is refused at once, whatever the
// guard's cache still holds (review finding 6).
func TestRevealRoleRemovedRefusedAtOnce(t *testing.T) {
	id, _, _ := drillList(t, 5)
	a := auditor(t)
	tok := mint(t, a)
	ok(t, "reveal", get(t, "/platform/audit/"+strconv.FormatInt(id, 10)+"/patients", tok))
	if _, err := super.Exec(context.Background(), `DELETE FROM user_roles WHERE user_id = $1`, a.id); err != nil {
		t.Fatal(err)
	}
	r := get(t, "/platform/audit/"+strconv.FormatInt(id, 10)+"/patients", tok)
	if (r.code != 403 && r.code != 401) || strings.Contains(r.raw, "P-") {
		t.Errorf("after the role was removed: %d %s", r.code, r.raw)
	}
}

// Every reveal is recorded, one entry each, with the entry revealed and a
// count; never a pid.
func TestRevealAudited(t *testing.T) {
	id, _, _ := drillList(t, 5)
	a := auditor(t)
	tok := mint(t, a)
	for i := 0; i < 2; i++ {
		ok(t, "reveal", get(t, "/platform/audit/"+strconv.FormatInt(id, 10)+"/patients", tok))
	}
	rows := revealsBy(t, a)
	if len(rows) != 2 {
		t.Fatalf("%d reveal entries, want 2", len(rows))
	}
	for _, r := range rows {
		if r.entity != "audit_log" || r.entityID != strconv.FormatInt(id, 10) || r.hospital != nil || mustJSON(r.payload) != `{"rows":5}` {
			t.Errorf("reveal entry %+v", r)
		}
	}
	// Unrecordable: 500 and no pid.
	ctx := context.Background()
	var check string
	if err := super.QueryRow(ctx, `SELECT with_check FROM pg_policies WHERE tablename = 'audit_log' AND policyname = 'platform_append'`).
		Scan(&check); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `ALTER POLICY platform_append ON audit_log WITH CHECK (false)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := super.Exec(ctx, `ALTER POLICY platform_append ON audit_log WITH CHECK (`+check+`)`); err != nil {
			t.Errorf("restore platform_append: %v", err)
		}
	})
	r := get(t, "/platform/audit/"+strconv.FormatInt(id, 10)+"/patients", tok)
	problem(t, "unrecordable reveal", r, 500, "internal_error")
	if strings.Contains(r.raw, "P-") {
		t.Errorf("an unrecorded reveal sent pids")
	}
}

// The detail of a drill-down entry says how many rows its list sent.
func TestAuditEntryPatientsCount(t *testing.T) {
	id, _, _ := drillList(t, 6)
	tok := mint(t, auditor(t))
	r := get(t, "/platform/audit/"+strconv.FormatInt(id, 10), tok)
	ok(t, "detail", r)
	p, _ := r.body["patients"].(map[string]any)
	if len(p) != 1 || p["count"] != float64(6) {
		t.Errorf("patients %v", r.body["patients"])
	}
	other := insertRow(t, row{action: "auth.logout"})
	r = get(t, "/platform/audit/"+strconv.FormatInt(other, 10), tok)
	ok(t, "other detail", r)
	if r.body["patients"] != nil {
		t.Errorf("a non drill-down entry has patients %v", r.body["patients"])
	}
}

// T23: no audit or platform read other than the reveal sends a pid.
func TestT23NoPidElsewhere(t *testing.T) {
	id, _, h := drillList(t, 5)
	a := auditor(t)
	tok := mint(t, a)
	ok(t, "reveal", get(t, "/platform/audit/"+strconv.FormatInt(id, 10)+"/patients", tok))
	pidIn := regexp.MustCompile(`(^|[^A-Za-z0-9])P-[A-Z0-9]{4,12}([^A-Za-z0-9]|$)`)
	for _, p := range []string{"/platform/audit", "/platform/audit?hospital=" + h.String(), "/platform/audit?hospital=platform",
		"/platform/audit/" + strconv.FormatInt(id, 10), "/platform/audit/hospitals", "/platform/audit?q=" + strconv.FormatInt(id, 10)} {
		r := get(t, p, tok)
		ok(t, p, r)
		if pidIn.MatchString(r.raw) {
			t.Errorf("%s sends a pid", p)
		}
	}
}

func revealsBy(t *testing.T, u user) []auditRow {
	t.Helper()
	rows, err := super.Query(context.Background(), `SELECT action, actor_id::text, COALESCE(entity, ''), COALESCE(entity_id, ''),
		hospital_id::text, payload FROM audit_log WHERE actor_id = $1 AND action = 'audit.patients_viewed' ORDER BY id`, u.id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.action, &r.actor, &r.entity, &r.entityID, &r.hospital, &r.payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}
