//go:build integration

package platform_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// POST /platform/hospitals/{id}/deactivate and /reactivate (mhai-web's
// Deactivate and Reactivate buttons; migration 00060). Every name, note and
// reason here is synthetic.

var deactivationKeys = []string{"by", "at", "reason", "note"}

// statusKeyed is POST /platform/hospitals/{id}/{verb} with Idempotency-Key
// key ("" for a fresh one).
func statusKeyed(t *testing.T, tok, hid, verb, body, key string) resp {
	t.Helper()
	if key == "" {
		key = "k-" + uniq()
	}
	r := httptest.NewRequest(http.MethodPost, httpx.BasePath+"/platform/hospitals/"+hid+"/"+verb, strings.NewReader(body))
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

func deactivate(t *testing.T, tok, hid, body string) resp {
	return statusKeyed(t, tok, hid, "deactivate", body, "")
}

func reactivate(t *testing.T, tok, hid, body string) resp {
	return statusKeyed(t, tok, hid, "reactivate", body, "")
}

// statusBody is a body with a fresh backup code of u's.
func statusBody(t *testing.T, u user, m map[string]any) string {
	t.Helper()
	return coded(t, u, jsonBody(t, m))
}

func hospitalStatus(t *testing.T, hid string) (string, int) {
	t.Helper()
	var s string
	var v int
	if err := super.QueryRow(context.Background(), `SELECT status, version FROM hospitals WHERE id = $1`, hid).Scan(&s, &v); err != nil {
		t.Fatal(err)
	}
	return s, v
}

func statusAudits(t *testing.T, action, hid string) []auditRow {
	t.Helper()
	return platformAuditRows(t, action, "hospital_id", hid)
}

// A platform admin deactivates an active hospital: 200 with the page, the
// status deactivated with who, when and why, the version bumped, its
// staff's sessions ended at once, one audit entry by ids and codes, and a
// line in the activity feed. Reactivating it puts it back as it was.
func TestHospitalDeactivateAndReactivate(t *testing.T) {
	adm := platformAdmin(t)
	tok := mint(t, adm)
	hid := seedHospital(t, "active")
	h := hid.String()
	staff := seedUser(t, hid, "", authz.RoleClinician)
	staffTok := mint(t, staff)
	other := seedHospital(t, "active")
	otherTok := mint(t, seedUser(t, other, "", authz.RoleClinician))
	if r := do(t, http.MethodGet, "/auth/sessions", staffTok, false); r.code != 200 {
		t.Fatalf("staff before: %d %s", r.code, r.raw)
	}
	_, v0 := hospitalStatus(t, h)

	before := time.Now().UTC()
	r := deactivate(t, tok, h, withTOTP(t, adm, jsonBody(t, map[string]any{"reason": "contract_ended", "note": "  Synthetic note  "})))
	ok(t, "deactivate", r)
	sameKeys(t, "deactivate", r.body, hospitalKeys...)
	if r.body["status"] != "deactivated" || r.body["version"] != float64(v0+1) {
		t.Errorf("after deactivate: %s", r.raw)
	}
	d, _ := r.body["deactivation"].(map[string]any)
	sameKeys(t, "deactivation", d, deactivationKeys...)
	if jsonOf(d["by"]) != jsonOf(map[string]any{"id": adm.id.String(), "name": adm.name}) || d["reason"] != "contract_ended" ||
		d["note"] != "Synthetic note" {
		t.Errorf("deactivation %s", jsonOf(d))
	}
	if at := stampTime(t, "at", d["at"]); at.Before(before.Add(-time.Minute)) || at.After(time.Now().Add(time.Minute)) {
		t.Errorf("at %v", at)
	}
	g := get(t, "/platform/hospitals/"+h, mint(t, auditor(t)))
	ok(t, "get", g)
	if jsonOf(g.body) != jsonOf(r.body) {
		t.Errorf("GET %s\nPOST %s", g.raw, r.raw)
	}

	// The staff's session is revoked and refused now, not at the next
	// refresh; another hospital's is untouched.
	if n := count(t, `SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, staff.id); n != 0 {
		t.Errorf("%d open staff sessions", n)
	}
	problem(t, "staff after", do(t, http.MethodGet, "/auth/sessions", staffTok, false), 401, "unauthenticated")
	if r := do(t, http.MethodGet, "/auth/sessions", otherTok, false); r.code != 200 {
		t.Errorf("another hospital's staff: %d %s", r.code, r.raw)
	}
	// The platform admin's own session is not a hospital's.
	ok(t, "admin after", get(t, "/platform/hospitals/"+h, tok))

	rows := statusAudits(t, "hospital.deactivated", h)
	if len(rows) != 1 {
		t.Fatalf("%d hospital.deactivated rows", len(rows))
	}
	a := rows[0]
	if a.actor != adm.id.String() || a.entity != "hospital" || a.entityID != h || a.hospital != nil ||
		jsonOf(a.payload) != `{"from":"active","hospital_id":"`+h+`","reason":"contract_ended","sessions_revoked":1}` {
		t.Errorf("audit %+v %s", a, jsonOf(a.payload))
	}

	// Reactivated with a backup code: back to active, no deactivation, the
	// version bumped again; the record is kept, closed.
	r = reactivate(t, tok, h, statusBody(t, adm, map[string]any{"reason": " Synthetic contract renewed "}))
	ok(t, "reactivate", r)
	sameKeys(t, "reactivate", r.body, hospitalKeys...)
	if r.body["status"] != "active" || r.body["deactivation"] != nil || r.body["version"] != float64(v0+2) {
		t.Errorf("after reactivate: %s", r.raw)
	}
	if n := count(t, `SELECT count(*) FROM hospital_deactivations WHERE hospital_id = $1 AND reactivated_at IS NOT NULL
		AND reactivated_by = $2 AND reactivation_reason = 'Synthetic contract renewed' AND note = 'Synthetic note'`, h, adm.id); n != 1 {
		t.Errorf("%d closed records", n)
	}
	rows = statusAudits(t, "hospital.reactivated", h)
	if len(rows) != 1 || rows[0].actor != adm.id.String() || rows[0].entityID != h ||
		jsonOf(rows[0].payload) != `{"hospital_id":"`+h+`","to":"active"}` {
		t.Errorf("reactivate audit %+v", rows)
	}
	for _, a := range append(statusAudits(t, "hospital.deactivated", h), rows...) {
		if strings.Contains(strings.ToLower(jsonOf(a.payload)), "synthetic") {
			t.Errorf("audit carries free text: %s", jsonOf(a.payload))
		}
	}

	// The feed: deactivated (the reason in words), then reactivated.
	f := get(t, "/platform/activity?hospital="+h, tok)
	ok(t, "activity", f)
	evs, _ := f.body["events"].([]any)
	var actions []string
	for _, e := range evs {
		m := e.(map[string]any)
		actions = append(actions, m["action"].(string))
		if m["action"] == "hospital_deactivated" && m["detail"] != "Contract ended" {
			t.Errorf("deactivate detail %v", m["detail"])
		}
		if m["action"] == "hospital_reactivated" && m["detail"] != nil {
			t.Errorf("reactivate detail %v", m["detail"])
		}
	}
	if strings.Join(actions, ",") != "hospital_reactivated,hospital_deactivated" {
		t.Errorf("activity %v", actions)
	}
}

// A hospital still in set-up is deactivated and comes back in set-up;
// "other" needs a note, and a blank note is no note.
func TestHospitalDeactivateSetUpAndNotes(t *testing.T) {
	adm := platformAdmin(t)
	tok := mint(t, adm)
	h := addedHospital(t, adm)
	problem(t, "other, no note", deactivate(t, tok, h, statusBody(t, adm, map[string]any{"reason": "other"})), 422, "validation_failed")
	problem(t, "other, blank note", deactivate(t, tok, h, statusBody(t, adm, map[string]any{"reason": "other", "note": "  "})),
		422, "validation_failed")
	r := deactivate(t, tok, h, statusBody(t, adm, map[string]any{"reason": "security_concern", "note": " "}))
	ok(t, "blank note", r)
	if d := r.body["deactivation"].(map[string]any); d["note"] != nil || d["reason"] != "security_concern" {
		t.Errorf("deactivation %v", d)
	}
	if rows := statusAudits(t, "hospital.deactivated", h); len(rows) != 1 || rows[0].payload["from"] != "setup_incomplete" ||
		rows[0].payload["sessions_revoked"] != float64(0) {
		t.Errorf("audit %+v", rows)
	}
	r = reactivate(t, tok, h, statusBody(t, adm, map[string]any{"reason": "Synthetic mistake"}))
	ok(t, "reactivate", r)
	if r.body["status"] != "setup_incomplete" {
		t.Errorf("back as %v, want setup_incomplete", r.body["status"])
	}
	r = deactivate(t, tok, h, statusBody(t, adm, map[string]any{"reason": "other", "note": "Synthetic\nreason"}))
	ok(t, "other with a note", r)
	if d := r.body["deactivation"].(map[string]any); d["note"] != "Synthetic\nreason" || d["reason"] != "other" {
		t.Errorf("deactivation %v", d)
	}
	if n := count(t, `SELECT count(*) FROM hospital_deactivations WHERE hospital_id = $1`, h); n != 2 {
		t.Errorf("%d records, want both kept", n)
	}
}

// Wrong state is 409 conflict, an unknown id 404, both before a code is
// spent (the same backup code then works); nothing changes.
func TestHospitalStatusRefusals(t *testing.T) {
	adm := platformAdmin(t)
	tok := mint(t, adm)
	active := seedHospital(t, "active").String()
	code := backupCode(t, adm)
	body := func(m map[string]any) string {
		m["backup_code"] = code
		return jsonBody(t, m)
	}
	dBody := body(map[string]any{"reason": "hospital_request"})
	rBody := body(map[string]any{"reason": "Synthetic"})
	problem(t, "reactivate active", reactivate(t, tok, active, rBody), 409, "conflict")
	for _, st := range []string{"deactivated", "removal_pending", "removal_scheduled", "removed"} {
		h := seedHospital(t, st).String()
		problem(t, "deactivate "+st, deactivate(t, tok, h, dBody), 409, "conflict")
		if s, v := hospitalStatus(t, h); s != st || v != 1 {
			t.Errorf("%s: now %s v%d", st, s, v)
		}
	}
	// Deactivated outside the platform (no record): nothing to restore.
	bare := seedHospital(t, "deactivated").String()
	problem(t, "reactivate without a record", reactivate(t, tok, bare, rBody), 409, "conflict")
	g := get(t, "/platform/hospitals/"+bare, tok)
	ok(t, "bare page", g)
	if g.body["deactivation"] != nil {
		t.Errorf("deactivation %v, want null", g.body["deactivation"])
	}
	for _, verb := range []string{"deactivate", "reactivate"} {
		problem(t, verb+" unknown", statusKeyed(t, tok, uuid.NewString(), verb, dBody, ""), 404, "not_found")
		problem(t, verb+" not a uuid", statusKeyed(t, tok, "not-a-uuid", verb, dBody, ""), 404, "not_found")
	}
	// The code was never spent.
	ok(t, "the same code works", deactivate(t, tok, active, dBody))
	problem(t, "deactivate twice", deactivate(t, tok, active, statusBody(t, adm, map[string]any{"reason": "other", "note": "x"})),
		409, "conflict")
	if rows := statusAudits(t, "hospital.deactivated", active); len(rows) != 1 {
		t.Errorf("%d audit rows", len(rows))
	}
}

// A bad body is 422 and spends nothing; no code is 403 mfa_required, a
// wrong one 403 mfa_invalid (audited); nothing changes.
func TestHospitalStatusBadBodyAndCode(t *testing.T) {
	adm := platformAdmin(t)
	tok := mint(t, adm)
	h := seedHospital(t, "active").String()
	long := strings.Repeat("x", 501)
	for name, b := range map[string]map[string]any{
		"no reason":         {},
		"unknown reason":    {"reason": "bored"},
		"reason null":       {"reason": nil},
		"reason as number":  {"reason": 1},
		"long note":         {"reason": "contract_ended", "note": long},
		"control character": {"reason": "contract_ended", "note": "Synthetic\x07"},
		"pseudonym":         {"reason": "contract_ended", "note": "about P-AB12CD"},
		"note as number":    {"reason": "contract_ended", "note": 5},
		"unknown field":     {"reason": "contract_ended", "status": "active"},
		"a version":         {"reason": "contract_ended", "version": 1},
	} {
		problem(t, "deactivate "+name, deactivate(t, tok, h, statusBody(t, adm, b)), 422, "validation_failed")
	}
	problem(t, "query", statusKeyed(t, tok, h, "deactivate?x=1", statusBody(t, adm, map[string]any{"reason": "other", "note": "x"}), ""),
		422, "validation_failed")
	problem(t, "no code", deactivate(t, tok, h, jsonBody(t, map[string]any{"reason": "contract_ended"})), 403, "mfa_required")
	problem(t, "wrong code", deactivate(t, tok, h, jsonBody(t, map[string]any{"reason": "contract_ended", "backup_code": "synthwrong0"})),
		403, "mfa_invalid")
	if s, v := hospitalStatus(t, h); s != "active" || v != 1 {
		t.Errorf("after refusals: %s v%d", s, v)
	}
	if n := count(t, `SELECT count(*) FROM audit_log WHERE action = 'auth.mfa.failed' AND actor_id = $1`, adm.id); n != 1 {
		t.Errorf("%d auth.mfa.failed rows", n)
	}

	ok(t, "deactivate", deactivate(t, tok, h, statusBody(t, adm, map[string]any{"reason": "contract_ended"})))
	for name, b := range map[string]map[string]any{
		"no reason":     {},
		"blank reason":  {"reason": "  "},
		"long reason":   {"reason": long},
		"reason null":   {"reason": nil},
		"unknown field": {"reason": "Synthetic", "note": "x"},
	} {
		problem(t, "reactivate "+name, reactivate(t, tok, h, statusBody(t, adm, b)), 422, "validation_failed")
	}
	problem(t, "reactivate no code", reactivate(t, tok, h, jsonBody(t, map[string]any{"reason": "Synthetic"})), 403, "mfa_required")
	if s, _ := hospitalStatus(t, h); s != "deactivated" {
		t.Errorf("after refusals: %s", s)
	}
}

// Platform admins only: auditors 403 forbidden, hospital staff 403
// not_platform, a session without two-step 403 mfa_required, no token 401.
func TestHospitalStatusRoles(t *testing.T) {
	hid := seedHospital(t, "active")
	h := hid.String()
	body := jsonBody(t, map[string]any{"reason": "contract_ended", "backup_code": "synthunused"})
	for _, verb := range []string{"deactivate", "reactivate"} {
		problem(t, verb+" auditor", statusKeyed(t, mint(t, auditor(t)), h, verb, body, ""), 403, "forbidden")
		for _, roles := range [][]string{{authz.RoleHospitalAdmin}, {authz.RoleClinician}} {
			problem(t, verb+" "+roles[0], statusKeyed(t, mint(t, seedUser(t, hid, "", roles...)), h, verb, body, ""), 403, "not_platform")
		}
		problem(t, verb+" no two-step", statusKeyed(t, signIn(t, platformAdmin(t), false).tok, h, verb, body, ""), 403, "mfa_required")
		problem(t, verb+" no token", statusKeyed(t, "", h, verb, body, ""), 401, "unauthenticated")
	}
	if s, v := hospitalStatus(t, h); s != "active" || v != 1 {
		t.Errorf("after refusals: %s v%d", s, v)
	}
}

// A retry with the same Idempotency-Key replays the reply: one change,
// one audit entry, the code not checked again.
func TestHospitalDeactivateIdempotent(t *testing.T) {
	adm := platformAdmin(t)
	tok := mint(t, adm)
	h := seedHospital(t, "active").String()
	body, key := statusBody(t, adm, map[string]any{"reason": "contract_ended"}), "k-"+uniq()
	first := statusKeyed(t, tok, h, "deactivate", body, key)
	ok(t, "first", first)
	again := statusKeyed(t, tok, h, "deactivate", body, key)
	if again.code != 200 || again.raw != first.raw {
		t.Errorf("retry %d %s", again.code, again.raw)
	}
	if n := len(statusAudits(t, "hospital.deactivated", h)); n != 1 {
		t.Errorf("%d audit rows", n)
	}
	if _, v := hospitalStatus(t, h); v != 2 {
		t.Errorf("version %d", v)
	}
}
