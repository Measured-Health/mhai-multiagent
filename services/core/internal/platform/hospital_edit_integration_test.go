//go:build integration

package platform_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// PATCH /platform/hospitals/{id} (mhai-web's Edit hospital form,
// HospitalPatch; migration 00059). Every value here is synthetic.

func editPath(hid string) string { return "/platform/hospitals/" + hid }

// patchKeyed is the PATCH with Idempotency-Key key ("" for a fresh one).
func patchKeyed(t *testing.T, tok, hid, body, key string) resp {
	t.Helper()
	if key == "" {
		key = "k-" + uniq()
	}
	r := httptest.NewRequest(http.MethodPatch, httpx.BasePath+editPath(hid), strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-MHAI-Service", serviceKey)
	r.Header.Set("X-MHAI-Audience", "platform")
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	out := resp{code: w.Code, raw: w.Body.String(), hdr: w.Header()}
	_ = json.Unmarshal(w.Body.Bytes(), &out.body)
	return out
}

func patch(t *testing.T, tok, hid, body string) resp { return patchKeyed(t, tok, hid, body, "") }

// editAudits are hid's hospital.updated rows.
func editAudits(t *testing.T, hid string) []auditRow {
	t.Helper()
	return platformAuditRows(t, "hospital.updated", "hospital_id", hid)
}

// addedHospital adds a hospital through the route and returns its id.
func addedHospital(t *testing.T, adm user) string {
	t.Helper()
	r := addHospital(t, mint(t, adm), coded(t, adm, addBody(t, newCode(), newEmail(), nil)))
	created(t, "add", r)
	return r.body["id"].(string)
}

// An edit changes what it sends, bumps the version once, audits the
// changed field names only, and GET and the activity feed show it.
func TestHospitalEditChangesAndAudits(t *testing.T) {
	adm := platformAdmin(t)
	tok := mint(t, adm)
	hid := addedHospital(t, adm)
	before := get(t, "/platform/hospitals/"+hid, tok)
	ok(t, "before", before)
	body := jsonBody(t, map[string]any{"version": 1, "name": before.body["name"], "short_code": before.body["short_code"],
		"country": " gh ", "time_zone": "Africa/Lagos", "email_domains": []string{"New.Synthetic.Example", "new.synthetic.example"}})
	r := patch(t, tok, hid, body)
	ok(t, "edit", r)
	sameKeys(t, "edit", r.body, hospitalKeys...)
	if r.body["version"] != float64(2) || r.body["country"] != "GH" || jsonOf(r.body["email_domains"]) != `["new.synthetic.example"]` ||
		r.body["name"] != before.body["name"] || r.body["short_code"] != before.body["short_code"] {
		t.Errorf("after edit: %s", r.raw)
	}
	g := get(t, "/platform/hospitals/"+hid, tok)
	if jsonOf(g.body) != jsonOf(r.body) {
		t.Errorf("GET %s\nPATCH %s", g.raw, r.raw)
	}
	rows := editAudits(t, hid)
	if len(rows) != 1 {
		t.Fatalf("%d hospital.updated rows", len(rows))
	}
	a := rows[0]
	if a.actor != adm.id.String() || a.entity != "hospital" || a.entityID != hid || a.hospital != nil ||
		jsonOf(a.payload) != `{"fields":["country","email_domains"],"hospital_id":"`+hid+`","via":"platform"}` {
		t.Errorf("audit %+v %s", a, jsonOf(a.payload))
	}
	if strings.Contains(jsonOf(a.payload), "synthetic") || strings.Contains(jsonOf(a.payload), "GH") {
		t.Errorf("audit carries values: %s", jsonOf(a.payload))
	}

	// The feed: the add and the edit, the edit's fields in words.
	f := get(t, "/platform/activity?hospital="+hid, tok)
	ok(t, "activity", f)
	evs, _ := f.body["events"].([]any)
	var actions []string
	for _, e := range evs {
		m := e.(map[string]any)
		actions = append(actions, m["action"].(string))
		if m["action"] == "hospital_updated" && m["detail"] != "Country, Email domains" {
			t.Errorf("edit detail %v", m["detail"])
		}
	}
	if strings.Join(actions, ",") != "hospital_updated,hospital_added" {
		t.Errorf("activity %v", actions)
	}

	// The same edit again changes nothing: no version bump, no entry.
	same := jsonBody(t, map[string]any{"version": 2, "country": "GH"})
	r = patch(t, tok, hid, same)
	ok(t, "no change", r)
	if r.body["version"] != float64(2) || len(editAudits(t, hid)) != 1 {
		t.Errorf("a no-change edit wrote: %s", r.raw)
	}
	// A partial edit keeps what it doesn't send.
	r = patch(t, tok, hid, jsonBody(t, map[string]any{"version": 2, "time_zone": "Africa/Accra"}))
	ok(t, "partial", r)
	if r.body["time_zone"] != "Africa/Accra" || r.body["country"] != "GH" || r.body["version"] != float64(3) {
		t.Errorf("partial: %s", r.raw)
	}
}

// A stale version is 409 conflict and changes nothing; a retry with the
// same key replays the reply and bumps nothing.
func TestHospitalEditVersionAndIdempotency(t *testing.T) {
	adm := platformAdmin(t)
	tok := mint(t, adm)
	hid := addedHospital(t, adm)
	body, key := jsonBody(t, map[string]any{"version": 1, "name": "Synthetic Renamed " + uniq()}), "k-"+uniq()
	first := patchKeyed(t, tok, hid, body, key)
	ok(t, "first", first)
	again := patchKeyed(t, tok, hid, body, key)
	if again.code != 200 || again.raw != first.raw {
		t.Errorf("retry %d %s", again.code, again.raw)
	}
	problem(t, "stale", patch(t, tok, hid, jsonBody(t, map[string]any{"version": 1, "name": "Synthetic Other"})), 409, "conflict")
	g := get(t, "/platform/hospitals/"+hid, tok)
	if g.body["version"] != float64(2) || g.body["name"] != first.body["name"] || len(editAudits(t, hid)) != 1 {
		t.Errorf("after retry and stale edit: %s", g.raw)
	}
}

// A hospital the CLI made (no country) takes a country; another
// hospital's short code is 409 duplicate; a removed one 409
// hospital_removed; an unknown id 404.
func TestHospitalEditRefusals(t *testing.T) {
	adm := platformAdmin(t)
	tok := mint(t, adm)
	cli := seedHospital(t, "active").String()
	r := patch(t, tok, cli, jsonBody(t, map[string]any{"version": 1, "country": "NG"}))
	ok(t, "CLI hospital", r)
	if r.body["country"] != "NG" || r.body["version"] != float64(2) {
		t.Errorf("CLI hospital: %s", r.raw)
	}
	other := get(t, "/platform/hospitals/"+addedHospital(t, adm), tok).body["short_code"].(string)
	problem(t, "duplicate", patch(t, tok, cli, jsonBody(t, map[string]any{"version": 2, "short_code": strings.ToLower(other)})),
		409, "duplicate")
	removed := seedHospital(t, "removed").String()
	problem(t, "removed", patch(t, tok, removed, jsonBody(t, map[string]any{"version": 1, "name": "Synthetic"})), 409, "hospital_removed")
	problem(t, "unknown", patch(t, tok, uuid.NewString(), jsonBody(t, map[string]any{"version": 1})), 404, "not_found")
	problem(t, "not a uuid", patch(t, tok, "not-a-uuid", jsonBody(t, map[string]any{"version": 1})), 404, "not_found")
	if n := count(t, `SELECT version FROM hospitals WHERE id = $1`, cli); n != 2 {
		t.Errorf("version %d after refusals", n)
	}
}

// Every field is checked as the add checks it: 422, nothing changed.
func TestHospitalEditBadBody(t *testing.T) {
	adm := platformAdmin(t)
	tok := mint(t, adm)
	hid := addedHospital(t, adm)
	cases := map[string]map[string]any{
		"no version":     {"name": "Synthetic"},
		"version 0":      {"version": 0},
		"version string": {"version": "1"},
		"null name":      {"version": 1, "name": nil},
		"empty name":     {"version": 1, "name": " "},
		"long name":      {"version": 1, "name": strings.Repeat("x", 121)},
		"bad short code": {"version": 1, "short_code": "A"},
		"null country":   {"version": 1, "country": nil},
		"country name":   {"version": 1, "country": "Ghana"},
		"reserved code":  {"version": 1, "country": "UK"},
		"bad time zone":  {"version": 1, "time_zone": "Mars/Olympus"},
		"local zone":     {"version": 1, "time_zone": "Local"},
		"null domains":   {"version": 1, "email_domains": nil},
		"bad domain":     {"version": 1, "email_domains": []string{"localhost"}},
		"unknown field":  {"version": 1, "status": "active"},
		"first admin":    {"version": 1, "first_admin": map[string]any{"name": "x", "email": "x@synthetic.example"}},
		"a step-up code": {"version": 1, "mfa_code": "123456"},
	}
	for name, b := range cases {
		problem(t, name, patch(t, tok, hid, jsonBody(t, b)), 422, "validation_failed")
	}
	if n := count(t, `SELECT version FROM hospitals WHERE id = $1`, hid); n != 1 || len(editAudits(t, hid)) != 0 {
		t.Errorf("bad bodies changed the hospital: version %d", n)
	}
}

// Platform admins only, two-step, platform audience.
func TestHospitalEditRoles(t *testing.T) {
	adm := platformAdmin(t)
	hid := addedHospital(t, adm)
	body := jsonBody(t, map[string]any{"version": 1, "name": "Synthetic Intruder"})
	problem(t, "auditor", patch(t, mint(t, auditor(t)), hid, body), 403, "forbidden")
	h := seedHospital(t, "active")
	for _, roles := range [][]string{{authz.RoleHospitalAdmin}, {authz.RoleClinician}} {
		problem(t, roles[0], patch(t, mint(t, seedUser(t, h, "", roles...)), hid, body), 403, "not_platform")
	}
	problem(t, "no two-step", patch(t, signIn(t, platformAdmin(t), false).tok, hid, body), 403, "mfa_required")
	problem(t, "no token", patch(t, "", hid, body), 401, "unauthenticated")
	var name string
	if err := super.QueryRow(context.Background(), `SELECT name FROM hospitals WHERE id = $1`, hid).Scan(&name); err != nil || name == "Synthetic Intruder" {
		t.Errorf("name %q %v", name, err)
	}
}
