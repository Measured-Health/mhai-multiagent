//go:build integration

package platform_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
)

// The platform app's hospitals list and a hospital's page (mhai-web Plan
// 7b, design §7.1, §7.3, §13): GET /platform/hospitals and GET
// /platform/hospitals/{id}, through core's whole chain. Every name, code,
// email and reference here is synthetic.

var summaryKeys = []string{"id", "name", "short_code", "status", "staff_count", "sso_enabled", "created_at",
	"last_activity_at", "legal_holds_active"}

var hospitalKeys = append(append([]string{}, summaryKeys...), "country", "time_zone", "email_domains", "version",
	"deactivation", "first_admin", "removal", "admins", "legal_holds")

var holdShortKeys = []string{"id", "scope", "reference", "status", "review_on"}

// seedNamedHospital adds a hospital called name with short code code.
func seedNamedHospital(t *testing.T, name, code, status string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO hospitals (name, short_code, status, time_zone) VALUES ($1, $2, $3, 'Africa/Lagos')
		RETURNING id`, name, code, status).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `INSERT INTO hospital_settings (hospital_id) VALUES ($1)`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedHold places a hold on hid at placed; lifted, it is lifted a minute later.
func seedHold(t *testing.T, hid uuid.UUID, reference string, placed time.Time, lifted bool) uuid.UUID {
	t.Helper()
	var l any
	if lifted {
		l = placed.Add(time.Minute)
	}
	var id uuid.UUID
	if err := super.QueryRow(context.Background(), `INSERT INTO legal_holds (hospital_id, reference, placed_at, lifted_at)
		VALUES ($1, $2, $3, $4) RETURNING id`, hid, reference, placed, l).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func hospitalsList(t *testing.T, tok string, q url.Values) resp {
	t.Helper()
	p := "/platform/hospitals"
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	return get(t, p, tok)
}

func listed(t *testing.T, r resp) []map[string]any {
	t.Helper()
	raw, ok := r.body["hospitals"].([]any)
	if !ok {
		t.Fatalf("no hospitals list: %s", r.raw)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, x := range raw {
		m, _ := x.(map[string]any)
		out = append(out, m)
	}
	return out
}

// Only platform accounts, on the platform audience, with two-step; auditors
// read like platform admins.
func TestHospitalsRoles(t *testing.T) {
	h := seedHospital(t, "active")
	paths := []string{"/platform/hospitals", "/platform/hospitals/" + h.String(), "/platform/hospitals/" + h.String() + "/sso"}
	for _, p := range paths {
		for _, roles := range [][]string{{authz.RoleClinician}, {authz.RoleHospitalAdmin}} {
			problem(t, fmt.Sprint(roles, " ", p), get(t, p, mint(t, seedUser(t, h, "", roles...))), 403, "not_platform")
		}
		problem(t, "no two-step "+p, get(t, p, signIn(t, platformAdmin(t), false).tok), 403, "mfa_required")
		problem(t, "clinical audience "+p, do(t, "GET", p, mint(t, platformAdmin(t)), false), 403, "not_platform")
		ok(t, "auditor "+p, get(t, p, mint(t, auditor(t))))
		ok(t, "platform admin "+p, get(t, p, mint(t, platformAdmin(t))))
	}
}

// A hospital as the list shows it: exactly the summary's keys, staff counted
// from active accounts, last activity from their sessions, and the holds
// that keep records (open ones; core has no lift grace).
func TestHospitalsListSummary(t *testing.T) {
	tag := "Zebrafinch" + uniq()
	h := seedNamedHospital(t, "Synthetic "+tag+" Teaching", "ZF"+strings.ToUpper(uniq()[:6]), "active")
	a := seedUser(t, h, "", authz.RoleClinician)
	seedUser(t, h, "", authz.RoleHospitalAdmin)
	gone := seedUser(t, h, "", authz.RoleClinician)
	if _, err := super.Exec(context.Background(), `UPDATE users SET is_active = false WHERE id = $1`, gone.id); err != nil {
		t.Fatal(err)
	}
	seen := time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC)
	if _, err := super.Exec(context.Background(), `INSERT INTO sessions (user_id, last_seen_at, mfa, amr)
		VALUES ($1, $2, false, '{pwd}'), ($1, $3, false, '{pwd}')`,
		a.id, seen, seen.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedHold(t, h, "LH-2026-4401", now.Add(-48*time.Hour), false)
	seedHold(t, h, "LH-2026-4402", now.Add(-72*time.Hour), true)

	r := hospitalsList(t, mint(t, platformAdmin(t)), url.Values{"q": {tag}})
	ok(t, "list", r)
	sameKeys(t, "list", r.body, "hospitals", "next_cursor")
	if r.body["next_cursor"] != nil {
		t.Errorf("one hospital: next_cursor %v", r.body["next_cursor"])
	}
	rows := listed(t, r)
	if len(rows) != 1 {
		t.Fatalf("q %s: %d rows %s", tag, len(rows), r.raw)
	}
	s := rows[0]
	sameKeys(t, "summary", s, summaryKeys...)
	if s["id"] != h.String() || s["status"] != "active" || s["sso_enabled"] != false {
		t.Errorf("summary %v", s)
	}
	if s["staff_count"] != float64(2) {
		t.Errorf("staff_count %v, want 2 (the inactive account left out)", s["staff_count"])
	}
	if s["legal_holds_active"] != float64(1) {
		t.Errorf("legal_holds_active %v, want 1 (the lifted hold left out)", s["legal_holds_active"])
	}
	if s["last_activity_at"] != "2026-03-14T09:26:53.000Z" {
		t.Errorf("last_activity_at %v", s["last_activity_at"])
	}
	if c, _ := s["created_at"].(string); !strings.HasSuffix(c, "Z") {
		t.Errorf("created_at %v", s["created_at"])
	}

	// A hospital nobody has used yet: no activity, no staff.
	tag2 := "Kingfisher" + uniq()
	seedNamedHospital(t, "Synthetic "+tag2+" Cottage", "KF"+strings.ToUpper(uniq()[:6]), "setup_incomplete")
	rows = listed(t, hospitalsList(t, mint(t, auditor(t)), url.Values{"q": {tag2}}))
	if len(rows) != 1 || rows[0]["last_activity_at"] != nil || rows[0]["staff_count"] != float64(0) ||
		rows[0]["status"] != "setup_incomplete" {
		t.Errorf("unused hospital %v", rows)
	}
}

// Search by name or short code, any case; filter by status; refuse anything
// else in the query.
func TestHospitalsListFilters(t *testing.T) {
	tag := "Heron" + uniq()
	code := "HR" + strings.ToUpper(uniq()[:6])
	act := seedNamedHospital(t, "Synthetic "+tag+" North", code, "active")
	deact := seedNamedHospital(t, "Synthetic "+tag+" South", "HS"+strings.ToUpper(uniq()[:6]), "deactivated")
	tok := mint(t, platformAdmin(t))

	ids := func(r resp) []string {
		ok(t, "filtered list", r)
		var out []string
		for _, h := range listed(t, r) {
			out = append(out, h["id"].(string))
		}
		return out
	}
	if got := ids(hospitalsList(t, tok, url.Values{"q": {strings.ToUpper(tag)}})); len(got) != 2 {
		t.Errorf("q upper case: %v", got)
	}
	if got := ids(hospitalsList(t, tok, url.Values{"q": {strings.ToLower(code)}})); len(got) != 1 || got[0] != act.String() {
		t.Errorf("q short code: %v", got)
	}
	if got := ids(hospitalsList(t, tok, url.Values{"q": {tag}, "status": {"deactivated"}})); len(got) != 1 || got[0] != deact.String() {
		t.Errorf("status deactivated: %v", got)
	}
	// LIKE's wildcards are plain characters here.
	if got := ids(hospitalsList(t, tok, url.Values{"q": {"%"}})); len(got) != 0 {
		t.Errorf("q %%: %v", got)
	}
	for what, q := range map[string]url.Values{
		"unknown status":  {"status": {"open"}},
		"unknown key":     {"hospital": {act.String()}},
		"empty q":         {"q": {""}},
		"long q":          {"q": {strings.Repeat("a", 101)}},
		"control in q":    {"q": {"a\x00b"}},
		"repeated status": {"status": {"active", "deactivated"}},
		"bad cursor":      {"cursor": {"not-a-cursor!"}},
		"forged cursor":   {"cursor": {"eyJuIjoiQSIsImkiOiJ4In0"}},
	} {
		problem(t, what, hospitalsList(t, tok, q), 422, "validation_failed")
	}
}

// Pages of 50 by name: every hospital once, in order, and the cursor
// carries on from the last one.
func TestHospitalsListPages(t *testing.T) {
	tag := "Pelican" + uniq()
	want := map[string]bool{}
	for i := range 57 {
		id := seedNamedHospital(t, fmt.Sprintf("Synthetic %s %03d", tag, 56-i), fmt.Sprintf("PL%s%02d", strings.ToUpper(uniq()[:4]), i), "active")
		want[id.String()] = true
	}
	tok := mint(t, auditor(t))
	q := url.Values{"q": {tag}}
	var names []string
	got := map[string]bool{}
	for page := 0; ; page++ {
		r := hospitalsList(t, tok, q)
		ok(t, "page", r)
		rows := listed(t, r)
		if page == 0 && len(rows) != 50 {
			t.Fatalf("first page %d rows", len(rows))
		}
		for _, h := range rows {
			if got[h["id"].(string)] {
				t.Fatalf("%v listed twice", h["id"])
			}
			got[h["id"].(string)] = true
			names = append(names, h["name"].(string))
		}
		c, _ := r.body["next_cursor"].(string)
		if c == "" {
			break
		}
		if page > 3 {
			t.Fatal("no end")
		}
		q.Set("cursor", c)
	}
	if len(got) != len(want) {
		t.Fatalf("listed %d, want %d", len(got), len(want))
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("not by name: %s before %s", names[i-1], names[i])
		}
	}
}

// A hospital's page: the summary and the page's parts, the active hospital
// admins to contact, and the open holds in short form.
func TestHospitalPage(t *testing.T) {
	tag := "Ibis" + uniq()
	h := seedNamedHospital(t, "Synthetic "+tag+" General", "IB"+strings.ToUpper(uniq()[:6]), "active")
	admin := seedUser(t, h, "Dr Synthetic Okonkwo-Ibis", authz.RoleHospitalAdmin, authz.RoleClinician)
	seedUser(t, h, "Dr Synthetic Clinician-Only", authz.RoleClinician)
	off := seedUser(t, h, "Dr Synthetic Former-Admin", authz.RoleHospitalAdmin)
	if _, err := super.Exec(context.Background(), `UPDATE users SET is_active = false WHERE id = $1`, off.id); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	recent := seedHold(t, h, "LH-2026-5501", now.Add(-24*time.Hour), false)
	old := seedHold(t, h, "LH-2025-5502", now.AddDate(0, -7, 0), false)
	seedHold(t, h, "LH-2025-5503", now.AddDate(0, -8, 0), true)

	r := get(t, "/platform/hospitals/"+h.String(), mint(t, auditor(t)))
	ok(t, "page", r)
	sameKeys(t, "page", r.body, hospitalKeys...)
	b := r.body
	if b["id"] != h.String() || b["time_zone"] != "Africa/Lagos" || b["country"] != "" || b["version"] != float64(1) {
		t.Errorf("page %v", b)
	}
	if d, _ := b["email_domains"].([]any); d == nil || len(d) != 0 {
		t.Errorf("email_domains %v, want []", b["email_domains"])
	}
	for _, k := range []string{"deactivation", "first_admin", "removal"} {
		if b[k] != nil {
			t.Errorf("%s %v, want null (core keeps none)", k, b[k])
		}
	}
	admins, _ := b["admins"].([]any)
	if len(admins) != 1 {
		t.Fatalf("admins %v", b["admins"])
	}
	a, _ := admins[0].(map[string]any)
	sameKeys(t, "admin", a, "name", "email")
	if a["name"] != admin.name || a["email"] != admin.email {
		t.Errorf("admin %v", a)
	}
	holds, _ := b["legal_holds"].([]any)
	if len(holds) != 2 || b["legal_holds_active"] != float64(2) {
		t.Fatalf("legal_holds %v", b["legal_holds"])
	}
	first, _ := holds[0].(map[string]any)
	second, _ := holds[1].(map[string]any)
	sameKeys(t, "hold", first, holdShortKeys...)
	if first["id"] != recent.String() || first["reference"] != "LH-2026-5501" || first["status"] != "active" {
		t.Errorf("newest hold %v", first)
	}
	if sc, _ := first["scope"].(map[string]any); sc == nil || sc["from"] != nil || sc["to"] != nil || len(sc) != 2 {
		t.Errorf("scope %v, want every record", first["scope"])
	}
	// Six calendar months after the day it was placed in Lagos (Postgres's
	// month arithmetic: the 31st of a short month is its last day).
	var review time.Time
	if err := super.QueryRow(context.Background(), `SELECT ((placed_at AT TIME ZONE 'Africa/Lagos') + interval '6 months')::date
		FROM legal_holds WHERE id = $1`, recent).Scan(&review); err != nil {
		t.Fatal(err)
	}
	if first["review_on"] != review.Format(time.DateOnly) {
		t.Errorf("review_on %v", first["review_on"])
	}
	if second["id"] != old.String() || second["status"] != "review_due" {
		t.Errorf("older hold %v, want review_due", second)
	}
	if strings.Contains(logs.String(), admin.email) {
		t.Error("an admin's email reached the log")
	}
}

// An id that isn't a hospital's is 404; so is one that isn't a canonical
// uuid; and the page takes no query.
func TestHospitalPageNotFound(t *testing.T) {
	tok := mint(t, platformAdmin(t))
	for _, id := range []string{uuid.NewString(), "not-a-uuid", strings.ToUpper(uuid.NewString())} {
		problem(t, "page "+id, get(t, "/platform/hospitals/"+id, tok), 404, "not_found")
		problem(t, "sso "+id, get(t, "/platform/hospitals/"+id+"/sso", tok), 404, "not_found")
	}
	h := seedHospital(t, "active")
	problem(t, "query", get(t, "/platform/hospitals/"+h.String()+"?x=1", tok), 422, "validation_failed")
}

// Core keeps no single sign-on for a hospital: the read says off, nothing
// saved, never tested.
func TestHospitalSSOOff(t *testing.T) {
	h := seedHospital(t, "active")
	r := get(t, "/platform/hospitals/"+h.String()+"/sso", mint(t, auditor(t)))
	ok(t, "sso", r)
	sameKeys(t, "sso", r.body, "enabled", "sign_in_url", "client_id", "issuer", "allowed_domains", "secret_saved",
		"secret_ends", "last_test", "version")
	if r.body["enabled"] != false || r.body["secret_saved"] != false || r.body["sign_in_url"] != nil ||
		r.body["last_test"] != nil || r.body["version"] != float64(1) {
		t.Errorf("sso %v", r.body)
	}
	if d, _ := r.body["allowed_domains"].([]any); d == nil || len(d) != 0 {
		t.Errorf("allowed_domains %v", r.body["allowed_domains"])
	}
}

// Reading the list and a page writes nothing to the audit log (platform
// configuration, as the model policy read).
func TestHospitalReadsNotRecorded(t *testing.T) {
	u := platformAdmin(t)
	h := seedHospital(t, "active")
	tok := mint(t, u)
	ok(t, "list", hospitalsList(t, tok, nil))
	ok(t, "page", get(t, "/platform/hospitals/"+h.String(), tok))
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE actor_id = $1`, u.id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d audit rows for two reads", n)
	}
}

// The platform sidebar's Analysis status (mhai-web Plan 12): platform
// admins only. Core serves no platform Analysis views yet, so the
// aggregates and patient views are off; the hospitals are split by their
// effective switch (the global row and the hospital's both on), and none
// is listed with a clearance.
func TestPlatformAnalysisStatus(t *testing.T) {
	ctx := context.Background()
	on := seedNamedHospital(t, "Synthetic Analysis On "+uniq(), "AO"+strings.ToUpper(uniq()[:6]), "active")
	off := seedNamedHospital(t, "Synthetic Analysis Off "+uniq(), "AF"+strings.ToUpper(uniq()[:6]), "active")
	for _, q := range []string{
		`INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
			VALUES (NULL, 'on', 'SO-2026-7701', 'SO-2026-7702', 'SO-2026-7703')
			ON CONFLICT (hospital_id) DO UPDATE SET position = 'on', signoff_clinical = 'SO-2026-7701',
			signoff_privacy = 'SO-2026-7702', signoff_legal = 'SO-2026-7703'`,
	} {
		if _, err := super.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := super.Exec(ctx, `INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		VALUES ($1, 'on', 'SO-2026-7704', 'SO-2026-7705', 'SO-2026-7706')`, on); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = super.Exec(ctx, `UPDATE analysis_switch SET position = 'off' WHERE hospital_id IS NULL`)
	})

	problem(t, "auditor", get(t, "/platform/analysis/status", mint(t, auditor(t))), 403, "forbidden")
	problem(t, "hospital admin", get(t, "/platform/analysis/status", mint(t, seedUser(t, on, "", authz.RoleHospitalAdmin))), 403, "not_platform")
	problem(t, "query", get(t, "/platform/analysis/status?x=1", mint(t, platformAdmin(t))), 422, "validation_failed")
	r := get(t, "/platform/analysis/status", mint(t, platformAdmin(t)))
	ok(t, "status", r)
	sameKeys(t, "status", r.body, "aggregates_enabled", "patient_views_enabled", "sample", "hospitals", "my_clearances")
	if r.body["aggregates_enabled"] != false || r.body["patient_views_enabled"] != false || r.body["sample"] != false {
		t.Errorf("status %v", r.body)
	}
	if c, _ := r.body["my_clearances"].([]any); c == nil || len(c) != 0 {
		t.Errorf("my_clearances %v", r.body["my_clearances"])
	}
	h, _ := r.body["hospitals"].(map[string]any)
	sameKeys(t, "hospitals", h, "included", "total", "list", "excluded")
	list, _ := h["list"].([]any)
	excluded, _ := h["excluded"].([]any)
	if h["included"] != float64(len(list)) || h["total"] != float64(len(list)+len(excluded)) {
		t.Errorf("counts %v", h)
	}
	in := func(xs []any, id uuid.UUID) map[string]any {
		for _, x := range xs {
			if m, _ := x.(map[string]any); m["id"] == id.String() {
				return m
			}
		}
		return nil
	}
	if m := in(list, on); m == nil || len(m) != 2 {
		t.Errorf("the switched-on hospital isn't included: %v", m)
	}
	if m := in(excluded, off); m == nil || m["reason"] != "not_covered" || len(m) != 3 {
		t.Errorf("the switched-off hospital: %v", m)
	}
	if in(list, off) != nil || in(excluded, on) != nil {
		t.Error("a hospital on the wrong side")
	}
}
