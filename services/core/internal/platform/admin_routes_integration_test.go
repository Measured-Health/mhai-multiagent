//go:build integration

package platform_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
)

// The platform admin routes (mhai-web Plan 7b §9, Plan 8a, Plan 7d): the
// team, legal holds, activity, the analytics e-mail setting and the routes
// core has no feature for (501). Every name, email and reference here is
// synthetic.

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// platformAuditRows are the rows of action whose payload key equals value.
func platformAuditRows(t *testing.T, action, key, value string) []auditRow {
	t.Helper()
	rows, err := super.Query(context.Background(), `SELECT action, COALESCE(actor_id::text, ''), COALESCE(entity, ''),
		COALESCE(entity_id, ''), hospital_id::text, payload FROM audit_log WHERE action = $1 AND payload->>$2 = $3 ORDER BY id`,
		action, key, value)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var a auditRow
		var raw []byte
		if err := rows.Scan(&a.action, &a.actor, &a.entity, &a.entityID, &a.hospital, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &a.payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// --- the team --------------------------------------------------------------

var memberKeys = []string{"id", "name", "email", "role", "status", "mfa_enabled", "last_sign_in_at", "is_you", "version"}

func teamMember(t *testing.T, tok string, id uuid.UUID) map[string]any {
	t.Helper()
	r := get(t, "/platform/team", tok)
	ok(t, "team", r)
	sameKeys(t, "team", r.body, "members")
	list, _ := r.body["members"].([]any)
	for _, x := range list {
		m, _ := x.(map[string]any)
		if m["id"] == id.String() {
			return m
		}
	}
	return nil
}

func TestTeamList(t *testing.T) {
	me := platformAdmin(t)
	aud := auditor(t)
	gone := platformAdmin(t)
	if _, err := super.Exec(context.Background(), `UPDATE users SET is_active = false WHERE id = $1`, gone.id); err != nil {
		t.Fatal(err)
	}
	clin := seedUser(t, seedHospital(t, "active"), "", authz.RoleHospitalAdmin)
	tok := mint(t, me)
	m := teamMember(t, tok, me.id)
	if m == nil {
		t.Fatal("the signed-in admin isn't listed")
	}
	sameKeys(t, "member", m, memberKeys...)
	if m["is_you"] != true || m["role"] != "platform_admin" || m["status"] != "active" || m["mfa_enabled"] != true ||
		m["email"] != me.email || m["name"] != me.name || m["version"] != float64(1) {
		t.Errorf("me %v", m)
	}
	if a := teamMember(t, tok, aud.id); a == nil || a["is_you"] != false || a["role"] != "auditor" {
		t.Errorf("auditor %v", a)
	}
	if teamMember(t, tok, gone.id) != nil || teamMember(t, tok, clin.id) != nil {
		t.Error("an inactive account or a hospital account is listed")
	}
	ok(t, "auditor reads", get(t, "/platform/team", mint(t, aud)))
	problem(t, "hospital admin", get(t, "/platform/team", mint(t, clin)), 403, "not_platform")
	problem(t, "query", get(t, "/platform/team?x=1", tok), 422, "validation_failed")
}

func TestTeamRoleChange(t *testing.T) {
	me := platformAdmin(t)
	other := platformAdmin(t)
	tok := mint(t, me)
	otherTok := mint(t, other)
	path := "/platform/team/" + other.id.String()
	patch := func(tok, path string, v any) resp {
		return send(t, handler, http.MethodPatch, path, tok, jsonBody(t, v))
	}

	problem(t, "auditor", patch(mint(t, auditor(t)), path, map[string]any{"role": "auditor", "version": 1}), 403, "forbidden")
	problem(t, "own role", patch(tok, "/platform/team/"+me.id.String(), map[string]any{"role": "auditor", "version": 1}), 409, "own_role")
	problem(t, "clinical role", patch(tok, path, map[string]any{"role": "clinician", "version": 1}), 422, "validation_failed")
	problem(t, "no version", patch(tok, path, map[string]any{"role": "auditor"}), 422, "validation_failed")
	problem(t, "stale", patch(tok, path, map[string]any{"role": "auditor", "version": 7}), 409, "conflict")
	problem(t, "not a member", patch(tok, "/platform/team/"+uuid.NewString(), map[string]any{"role": "auditor", "version": 1}), 404, "not_found")
	clin := seedUser(t, seedHospital(t, "active"), "", authz.RoleClinician)
	problem(t, "a hospital account", patch(tok, "/platform/team/"+clin.id.String(), map[string]any{"role": "auditor", "version": 1}), 404, "not_found")

	problem(t, "no code", patch(tok, path, map[string]any{"role": "auditor", "version": 1}), 403, "mfa_required")
	r := patch(tok, path, map[string]any{"role": "auditor", "version": 1, "backup_code": backupCode(t, me)})
	ok(t, "demote", r)
	sameKeys(t, "changed", r.body, memberKeys...)
	if r.body["role"] != "auditor" || r.body["version"] != float64(2) || r.body["is_you"] != false {
		t.Errorf("changed %v", r.body)
	}
	rows := platformAuditRows(t, "platform.team_role_changed", "user_id", other.id.String())
	if len(rows) != 1 || rows[0].actor != me.id.String() || rows[0].hospital != nil ||
		rows[0].payload["role"] != "auditor" || rows[0].payload["previous"] != "platform_admin" {
		t.Errorf("audit %+v", rows)
	}
	// The demoted admin's sessions are revoked and denylisted: the token
	// stops at once, and nothing else changes.
	if r := get(t, "/platform/team", otherTok); r.code == 200 {
		t.Errorf("a demoted admin's token still reads: %s", r.raw)
	}
	var open int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`,
		other.id).Scan(&open); err != nil || open != 0 {
		t.Errorf("demoted: %d open sessions %v", open, err)
	}
	if !strings.Contains(r.raw, other.email) || strings.Contains(logs.String(), other.email) {
		t.Error("the member's email is missing from the reply or reached the log")
	}
}

// The last active platform admin can't be removed: with every other
// platform admin switched off (and back after the test), the sole admin
// removing their own account is 409 last_admin, and nothing changes. (A
// demotion of the last admin can't reach the rule through the API: the
// actor is an active admin and can't change their own role; the rule
// still guards it under the team lock.)
func TestTeamLastAdmin(t *testing.T) {
	ctx := context.Background()
	last := platformAdmin(t)
	var others []uuid.UUID
	rows, err := super.Query(ctx, `UPDATE users u SET is_active = false WHERE u.hospital_id IS NULL AND u.is_active AND u.id <> $1
		AND EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role = 'platform_admin') RETURNING u.id`, last.id)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		others = append(others, id)
	}
	rows.Close()
	t.Cleanup(func() {
		if _, err := super.Exec(ctx, `UPDATE users SET is_active = true WHERE id = ANY($1)`, others); err != nil {
			t.Error(err)
		}
	})
	tok := mint(t, last)
	r := send(t, handler, http.MethodDelete, "/platform/team/"+last.id.String(), tok,
		jsonBody(t, map[string]any{"backup_code": backupCode(t, last)}))
	problem(t, "remove the last admin", r, 409, "last_admin")
	if n := len(platformAuditRows(t, "platform.team_member_removed", "user_id", last.id.String())); n != 0 {
		t.Errorf("a refused removal was audited: %d", n)
	}
	ok(t, "still an admin", get(t, "/platform/team", tok))

	// With a second admin, removing one's own account is allowed.
	peer := platformAdmin(t)
	r = send(t, handler, http.MethodDelete, "/platform/team/"+peer.id.String(), mint(t, peer),
		jsonBody(t, map[string]any{"backup_code": backupCode(t, peer)}))
	if r.code != http.StatusNoContent {
		t.Errorf("self-removal with another admin left: %d %s", r.code, r.raw)
	}
}

func TestTeamRemove(t *testing.T) {
	me := platformAdmin(t)
	gone := auditor(t)
	tok := mint(t, me)
	goneSession := signIn(t, gone, true)
	path := "/platform/team/" + gone.id.String()
	del := func(tok, path, body string) resp { return send(t, handler, http.MethodDelete, path, tok, body) }
	problem(t, "auditor", del(mint(t, auditor(t)), path, `{}`), 403, "forbidden")
	problem(t, "no code", del(tok, path, `{}`), 403, "mfa_required")
	problem(t, "wrong code", del(tok, path, `{"backup_code":"synthwrong1"}`), 403, "mfa_invalid")
	problem(t, "unknown", del(tok, "/platform/team/"+uuid.NewString(), `{"backup_code":"x"}`), 404, "not_found")
	r := del(tok, path, jsonBody(t, map[string]any{"backup_code": backupCode(t, me)}))
	if r.code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", r.code, r.raw)
	}
	var active bool
	var open int
	if err := super.QueryRow(context.Background(), `SELECT is_active, (SELECT count(*) FROM sessions WHERE user_id = $1
		AND revoked_at IS NULL) FROM users WHERE id = $1`, gone.id).Scan(&active, &open); err != nil {
		t.Fatal(err)
	}
	if active || open != 0 {
		t.Errorf("removed: active %v, %d open sessions", active, open)
	}
	if r := get(t, "/platform/team", goneSession.tok); r.code == 200 {
		t.Error("a removed member's token still reads")
	}
	if teamMember(t, tok, gone.id) != nil {
		t.Error("a removed member is listed")
	}
	rows := platformAuditRows(t, "platform.team_member_removed", "user_id", gone.id.String())
	if len(rows) != 1 || rows[0].actor != me.id.String() || rows[0].payload["role"] != "auditor" {
		t.Errorf("audit %+v", rows)
	}
	problem(t, "again", del(tok, path, jsonBody(t, map[string]any{"backup_code": backupCode(t, me)})), 404, "not_found")
}

// --- legal holds ------------------------------------------------------------

var legalHoldKeys = []string{"id", "hospital", "scope", "reason", "reference", "asked_by", "note", "placed_by", "placed_at",
	"review_on", "status", "lift", "stopped_removal", "version"}

func reviewDay(days int) string { return time.Now().UTC().AddDate(0, 0, days).Format(time.DateOnly) }

func placeBody(t *testing.T, h uuid.UUID, ref, backup string, extra map[string]any) string {
	m := map[string]any{"hospital_id": h.String(), "reason": "regulator", "reference": ref, "review_on": reviewDay(90)}
	if backup != "" {
		m["backup_code"] = backup
	}
	for k, v := range extra {
		m[k] = v
	}
	return jsonBody(t, m)
}

func holdPath(id any, rest string) string { return "/platform/legal-holds/" + id.(string) + rest }

func TestLegalHoldPlace(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	h := seedHospital(t, "active")
	ref := "LH-2026-6107"
	post := func(body string) resp { return send(t, handler, http.MethodPost, "/platform/legal-holds", tok, body) }

	problem(t, "auditor", send(t, handler, http.MethodPost, "/platform/legal-holds", mint(t, auditor(t)),
		placeBody(t, h, ref, "", nil)), 403, "forbidden")
	problem(t, "no code", post(placeBody(t, h, ref, "", nil)), 403, "mfa_required")
	for what, extra := range map[string]map[string]any{
		"a note":              {"note": "Synthetic counsel note"},
		"asked by a name":     {"asked_by": "Synthetic Barrister"},
		"a free reference":    {"reference": "Synthetic Case"},
		"a free reason":       {"reason": "lawsuit"},
		"a past review date":  {"review_on": reviewDay(-1)},
		"a far review date":   {"review_on": reviewDay(6 * 366)},
		"not a date":          {"review_on": "2026-02-30"},
		"not a hospital uuid": {"hospital_id": "not-a-uuid"},
	} {
		problem(t, what, post(placeBody(t, h, ref, "x", extra)), 422, "validation_failed")
	}
	problem(t, "a date range", post(placeBody(t, h, ref, "x", map[string]any{"from": "2026-01-01"})), 422, "invalid_range")
	problem(t, "unknown hospital", post(placeBody(t, uuid.New(), ref, "x", nil)), 404, "not_found")
	problem(t, "removed hospital", post(placeBody(t, seedHospital(t, "removed"), ref, "x", nil)), 409, "hospital_removed")

	r := post(placeBody(t, h, ref, backupCode(t, admin), map[string]any{"asked_by": "", "note": nil}))
	ok(t, "place", r)
	sameKeys(t, "hold", r.body, append(slices.Clone(legalHoldKeys), "history")...)
	if r.body["status"] != "active" || r.body["reason"] != "regulator" || r.body["reference"] != ref ||
		r.body["asked_by"] != nil || r.body["note"] != nil || r.body["lift"] != nil || r.body["version"] != float64(1) ||
		r.body["review_on"] != reviewDay(90) {
		t.Errorf("placed %v", r.body)
	}
	if pb, _ := r.body["placed_by"].(map[string]any); pb["id"] != admin.id.String() || pb["name"] != admin.name {
		t.Errorf("placed_by %v", r.body["placed_by"])
	}
	if hs, _ := r.body["history"].([]any); len(hs) != 1 || hs[0].(map[string]any)["action"] != "placed" {
		t.Errorf("history %v", r.body["history"])
	}
	rows := platformAuditRows(t, "legal_hold.placed", "hold_id", r.body["id"].(string))
	if len(rows) != 1 || rows[0].hospital != nil || rows[0].payload["hospital_id"] != h.String() ||
		rows[0].payload["reason"] != "regulator" || strings.Contains(jsonBody(t, rows[0].payload), ref) {
		t.Errorf("audit %+v", rows)
	}
	problem(t, "the same reference again, before a code is spent", post(placeBody(t, h, ref, "synthunspent", nil)), 409, "conflict")
	problem(t, "patch", send(t, handler, http.MethodPatch, holdPath(r.body["id"], ""), tok, `{"version":1}`), 501, "not_implemented")
	problem(t, "undo", send(t, handler, http.MethodPost, holdPath(r.body["id"], "/lift/undo"), tok, `{"version":1}`), 501, "not_implemented")
}

// Two different platform admins lift a hold: the requester can't approve,
// a stale version is refused, and the lift lands at once.
func TestLegalHoldTwoPersonLift(t *testing.T) {
	a, b := platformAdmin(t), platformAdmin(t)
	ta, tb := mint(t, a), mint(t, b)
	h := seedHospital(t, "active")
	placed := send(t, handler, http.MethodPost, "/platform/legal-holds", ta, placeBody(t, h, "LH-2026-6202", backupCode(t, a), nil))
	ok(t, "place", placed)
	id := placed.body["id"]
	lift := func(tok, body string) resp {
		return send(t, handler, http.MethodPost, holdPath(id, "/lift"), tok, body)
	}

	problem(t, "a note", lift(ta, jsonBody(t, map[string]any{"reason": "case_closed", "authority": "CT-2026-0091",
		"note": "Synthetic note", "backup_code": "x"})), 422, "validation_failed")
	problem(t, "a free authority", lift(ta, jsonBody(t, map[string]any{"reason": "case_closed",
		"authority": "Counsel's letter", "backup_code": "x"})), 422, "validation_failed")
	r := lift(ta, jsonBody(t, map[string]any{"reason": "case_closed", "authority": "CT-2026-0091", "backup_code": backupCode(t, a)}))
	ok(t, "request", r)
	l, _ := r.body["lift"].(map[string]any)
	if r.body["status"] != "lift_requested" || l == nil || l["requested_by_you"] != true || l["kind"] != "hold_lift" ||
		l["authority"] != "CT-2026-0091" || l["approved_by"] != nil || r.body["version"] != float64(2) {
		t.Fatalf("requested %v", r.body)
	}
	problem(t, "a second request, before a code is spent", lift(tb, jsonBody(t, map[string]any{"reason": "withdrawn",
		"authority": "CT-2026-0092", "backup_code": "synthunspent"})), 409, "conflict")
	seen := get(t, holdPath(id, ""), tb)
	if sl, _ := seen.body["lift"].(map[string]any); sl["requested_by_you"] != false {
		t.Errorf("the other admin sees requested_by_you %v", sl["requested_by_you"])
	}
	approve := func(tok string, v int, code string) resp {
		return send(t, handler, http.MethodPost, holdPath(id, "/lift/approve"), tok, jsonBody(t, map[string]any{"version": v, "backup_code": code}))
	}
	problem(t, "the requester approves", approve(ta, 2, "synthunspent"), 409, "same_person")
	problem(t, "stale", approve(tb, 1, "x"), 409, "conflict")
	r = approve(tb, 2, backupCode(t, b))
	ok(t, "approve", r)
	l, _ = r.body["lift"].(map[string]any)
	if r.body["status"] != "lifted" || l["approved_at"] == nil || l["released_on"] != l["approved_at"] {
		t.Errorf("approved %v", r.body)
	}
	if ab, _ := l["approved_by"].(map[string]any); ab["id"] != b.id.String() {
		t.Errorf("approved_by %v", l["approved_by"])
	}
	var lifted *time.Time
	if err := super.QueryRow(context.Background(), `SELECT lifted_at FROM legal_holds WHERE id = $1`, id).Scan(&lifted); err != nil || lifted == nil {
		t.Errorf("not lifted in the table: %v", err)
	}
	if hs, _ := r.body["history"].([]any); len(hs) != 3 {
		t.Errorf("history %v", r.body["history"])
	}
	if rows := platformAuditRows(t, "legal_hold.lifted", "hold_id", id.(string)); len(rows) != 1 || rows[0].actor != b.id.String() {
		t.Errorf("lift audit %+v", rows)
	}
	// A lifted hold is 409, before a code is spent.
	problem(t, "lift a lifted hold", lift(ta, jsonBody(t, map[string]any{"reason": "other", "authority": "CT-2026-0094",
		"backup_code": "synthunspent"})), 409, "conflict")
	problem(t, "approve on a lifted hold", approve(tb, 3, "synthunspent"), 409, "conflict")
	// The database refuses an approval by the requester whatever the
	// handler does.
	if _, err := super.Exec(context.Background(), `UPDATE legal_hold_lifts SET approved_by = requested_by WHERE hold_id = $1`, id); err == nil {
		t.Error("a self-approval was stored")
	}
}

func TestLegalHoldCancelAndLapse(t *testing.T) {
	a, b := platformAdmin(t), platformAdmin(t)
	ta, tb := mint(t, a), mint(t, b)
	h := seedHospital(t, "active")
	placed := send(t, handler, http.MethodPost, "/platform/legal-holds", ta, placeBody(t, h, "LH-2026-6303", backupCode(t, a), nil))
	ok(t, "place", placed)
	id := placed.body["id"]
	req := func() resp {
		return send(t, handler, http.MethodPost, holdPath(id, "/lift"), ta, jsonBody(t, map[string]any{"reason": "mistake",
			"authority": "CT-2026-0093", "backup_code": backupCode(t, a)}))
	}
	r := req()
	ok(t, "request", r)
	v := int(r.body["version"].(float64))
	r = send(t, handler, http.MethodPost, holdPath(id, "/lift/cancel"), tb, jsonBody(t, map[string]any{"version": v}))
	ok(t, "cancel", r)
	if r.body["status"] != "active" || r.body["lift"] != nil {
		t.Errorf("cancelled %v", r.body)
	}
	r = req()
	ok(t, "request again", r)
	v = int(r.body["version"].(float64))
	if _, err := super.Exec(context.Background(), `UPDATE legal_hold_lifts SET requested_at = now() - interval '9 days',
		expires_at = now() - interval '2 days' WHERE hold_id = $1 AND cancelled_at IS NULL`, id); err != nil {
		t.Fatal(err)
	}
	if g := get(t, holdPath(id, ""), tb); g.body["status"] != "active" {
		t.Errorf("a lapsed request still shows: %v", g.body["status"])
	}
	problem(t, "approve a lapsed request", send(t, handler, http.MethodPost, holdPath(id, "/lift/approve"), tb,
		jsonBody(t, map[string]any{"version": v, "backup_code": "x"})), 409, "expired")
	ok(t, "a new request after the lapse", req())
}

func TestLegalHoldList(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	tag := uniq()
	h := seedNamedHospital(t, "Synthetic Holds "+tag, "HL"+strings.ToUpper(tag[:6]), "active")
	ref := "LH-2026-6404"
	placed := send(t, handler, http.MethodPost, "/platform/legal-holds", tok, placeBody(t, h, ref, backupCode(t, admin), nil))
	ok(t, "place", placed)
	cli := seedHold(t, h, "LH-2025-6405", time.Now().UTC().Add(-time.Hour), false)
	lifted := seedHold(t, h, "LH-2025-6406", time.Now().UTC().Add(-2*time.Hour), true)
	list := func(q url.Values) []map[string]any {
		t.Helper()
		r := get(t, "/platform/legal-holds?"+q.Encode(), mint(t, auditor(t)))
		ok(t, "holds", r)
		sameKeys(t, "holds", r.body, "holds", "next_cursor")
		raw, _ := r.body["holds"].([]any)
		var out []map[string]any
		for _, x := range raw {
			out = append(out, x.(map[string]any))
		}
		return out
	}
	open := list(url.Values{"hospital": {h.String()}})
	if len(open) != 2 {
		t.Fatalf("open holds %v", open)
	}
	for _, x := range open {
		sameKeys(t, "listed", x, legalHoldKeys...)
		if x["id"] == cli.String() {
			if x["reason"] != nil || x["placed_by"].(map[string]any)["id"] != "cli" {
				t.Errorf("a CLI hold %v", x)
			}
		}
	}
	if all := list(url.Values{"hospital": {h.String()}, "status": {"all"}}); len(all) != 3 {
		t.Errorf("all %d", len(all))
	}
	if l := list(url.Values{"hospital": {h.String()}, "status": {"lifted"}}); len(l) != 1 || l[0]["id"] != lifted.String() {
		t.Errorf("lifted %v", l)
	}
	if q := list(url.Values{"hospital": {h.String()}, "q": {"lh-2025-6405"}}); len(q) != 1 || q[0]["id"] != cli.String() {
		t.Errorf("q %v", q)
	}
	for what, q := range map[string]string{"status": "status=closed", "hospital": "hospital=x", "cursor": "cursor=!!"} {
		problem(t, what, get(t, "/platform/legal-holds?"+q, tok), 422, "validation_failed")
	}
	problem(t, "unknown hold", get(t, "/platform/legal-holds/"+uuid.NewString(), tok), 404, "not_found")
}

// --- activity ---------------------------------------------------------------

func TestActivity(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	h := seedHospital(t, "active")
	placed := send(t, handler, http.MethodPost, "/platform/legal-holds", tok, placeBody(t, h, "LH-2026-6507", backupCode(t, admin), nil))
	ok(t, "place", placed)
	member := auditor(t)
	ok(t, "role change", send(t, handler, http.MethodPatch, "/platform/team/"+member.id.String(), tok,
		jsonBody(t, map[string]any{"role": "platform_admin", "version": 1, "backup_code": backupCode(t, admin)})))

	r := get(t, "/platform/activity?hospital="+h.String(), mint(t, auditor(t)))
	ok(t, "activity", r)
	sameKeys(t, "activity", r.body, "events", "next_cursor")
	evs, _ := r.body["events"].([]any)
	if len(evs) != 1 {
		t.Fatalf("hospital's events %v", evs)
	}
	e := evs[0].(map[string]any)
	sameKeys(t, "event", e, "id", "at", "actor", "action", "hospital", "target", "detail")
	if e["action"] != "hold_placed" || e["target"] != "LH-2026-6507" || e["actor"].(map[string]any)["id"] != admin.id.String() ||
		e["hospital"].(map[string]any)["id"] != h.String() {
		t.Errorf("event %v", e)
	}
	all := get(t, "/platform/activity", tok)
	ok(t, "all", all)
	found := false
	for _, x := range all.body["events"].([]any) {
		m := x.(map[string]any)
		if m["action"] == "team_role_changed" && m["target"] == member.name && m["detail"] == "platform_admin" && m["hospital"] == nil {
			found = true
		}
	}
	if !found {
		t.Error("the role change isn't in the feed")
	}
	for what, q := range map[string]string{"hospital": "hospital=x", "cursor": "cursor=abc", "key": "x=1"} {
		problem(t, what, get(t, "/platform/activity?"+q, tok), 422, "validation_failed")
	}
	problem(t, "hospital account", get(t, "/platform/activity", mint(t, seedUser(t, h, "", authz.RoleHospitalAdmin))), 403, "not_platform")
}

// --- the analytics e-mail setting -------------------------------------------

func TestAnalyticsEmail(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	problem(t, "auditor reads", get(t, "/platform/me/analytics-email", mint(t, auditor(t))), 403, "forbidden")
	r := get(t, "/platform/me/analytics-email", tok)
	ok(t, "default", r)
	sameKeys(t, "pref", r.body, "mode", "version")
	if r.body["mode"] != "off" || r.body["version"] != float64(1) {
		t.Errorf("default %v", r.body)
	}
	put := func(body string) resp {
		return send(t, handler, http.MethodPut, "/platform/me/analytics-email", tok, body)
	}
	problem(t, "free mode", put(`{"mode":"weekly","version":1}`), 422, "validation_failed")
	problem(t, "someone else's", put(`{"mode":"critical","version":1,"user_id":"`+uuid.NewString()+`"}`), 422, "validation_failed")
	r = put(`{"mode":"critical","version":1}`)
	ok(t, "set", r)
	if r.body["mode"] != "critical" || r.body["version"] != float64(2) {
		t.Errorf("set %v", r.body)
	}
	problem(t, "stale", put(`{"mode":"off","version":1}`), 409, "conflict")
	if g := get(t, "/platform/me/analytics-email", mint(t, platformAdmin(t))); g.body["mode"] != "off" {
		t.Errorf("another admin's setting changed: %v", g.body)
	}
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'platform.analytics_email_changed'
		AND actor_id = $1 AND payload->>'mode' = 'critical' AND hospital_id IS NULL`, admin.id).Scan(&n); err != nil || n != 1 {
		t.Errorf("audit rows %d %v", n, err)
	}
}

// --- what core has no feature for ------------------------------------------

func TestNotImplementedRoutes(t *testing.T) {
	tok := mint(t, platformAdmin(t))
	aud := mint(t, auditor(t))
	id := uuid.NewString()
	for _, c := range []struct{ method, path string }{
		{"GET", "/platform/notifications"}, {"POST", "/platform/notifications/seen"},
		{"GET", "/platform/settings"}, {"GET", "/platform/settings/retention/questions/preview"},
		{"PUT", "/platform/settings/retention/questions"}, {"PUT", "/platform/settings/retention-pause"},
		{"PUT", "/platform/settings/triage"}, {"POST", "/platform/settings/requests/" + id + "/approve"},
		{"POST", "/platform/settings/requests/" + id + "/cancel"}, {"POST", "/platform/settings/feedback-buffer/reset"},
		{"GET", "/platform/named-access"}, {"POST", "/platform/named-access/requests"},
		{"POST", "/platform/named-access/requests/" + id + "/approve"}, {"POST", "/platform/named-access/requests/" + id + "/cancel"},
		{"POST", "/platform/named-access/lower"}, {"POST", "/platform/team/invites"},
	} {
		body := ""
		if c.method != "GET" {
			body = `{}`
		}
		problem(t, c.method+" "+c.path, send(t, handler, c.method, c.path, tok, body), 501, "not_implemented")
		if c.method != "GET" {
			problem(t, "auditor "+c.method+" "+c.path, send(t, handler, c.method, c.path, aud, body), 403, "forbidden")
		}
	}
	problem(t, "no two-step", get(t, "/platform/settings", signIn(t, platformAdmin(t), false).tok), 403, "mfa_required")
}

// A lift request outlives no change to its requester: a requester who is
// deactivated, removed or demoted can't have their request approved (409),
// and removing or demoting a member through the team routes cancels their
// open requests, audited.
func TestLegalHoldLiftRequesterGone(t *testing.T) {
	ctx := context.Background()
	approver := platformAdmin(t)
	tokB := mint(t, approver)
	requested := func(t *testing.T) (user, any, int) {
		t.Helper()
		a := platformAdmin(t)
		ta := mint(t, a)
		placed := send(t, handler, http.MethodPost, "/platform/legal-holds", ta,
			placeBody(t, seedHospital(t, "active"), "LH-2026-7301", backupCode(t, a), nil))
		ok(t, "place", placed)
		id := placed.body["id"]
		r := send(t, handler, http.MethodPost, holdPath(id, "/lift"), ta, jsonBody(t, map[string]any{"reason": "case_closed",
			"authority": "CT-2026-7302", "backup_code": backupCode(t, a)}))
		ok(t, "request", r)
		return a, id, int(r.body["version"].(float64))
	}
	approve := func(id any, v int) resp {
		return send(t, handler, http.MethodPost, holdPath(id, "/lift/approve"), tokB,
			jsonBody(t, map[string]any{"version": v, "backup_code": "synthunspent"}))
	}
	for what, change := range map[string]string{
		"deactivated": `UPDATE users SET is_active = false WHERE id = $1`,
		"demoted":     `UPDATE user_roles SET role = 'auditor' WHERE user_id = $1 AND role = 'platform_admin'`,
	} {
		a, id, v := requested(t)
		if _, err := super.Exec(ctx, change, a.id); err != nil {
			t.Fatal(err)
		}
		problem(t, "approve after the requester was "+what, approve(id, v), 409, "conflict")
		var lifted *time.Time
		if err := super.QueryRow(ctx, `SELECT lifted_at FROM legal_holds WHERE id = $1`, id).Scan(&lifted); err != nil || lifted != nil {
			t.Errorf("%s: lifted %v %v", what, lifted, err)
		}
	}

	// Through the team routes: the request is cancelled, audited by the
	// admin who changed the team.
	for what, act := range map[string]func(a user) resp{
		"removed": func(a user) resp {
			return send(t, handler, http.MethodDelete, "/platform/team/"+a.id.String(), tokB,
				jsonBody(t, map[string]any{"backup_code": backupCode(t, approver)}))
		},
		"demoted": func(a user) resp {
			return send(t, handler, http.MethodPatch, "/platform/team/"+a.id.String(), tokB,
				jsonBody(t, map[string]any{"role": "auditor", "version": 1, "backup_code": backupCode(t, approver)}))
		},
	} {
		a, id, _ := requested(t)
		if r := act(a); r.code != http.StatusNoContent && r.code != http.StatusOK {
			t.Fatalf("%s: %d %s", what, r.code, r.raw)
		}
		g := get(t, holdPath(id, ""), tokB)
		if g.body["status"] != "active" || g.body["lift"] != nil {
			t.Errorf("%s: the request is still open: %v", what, g.body)
		}
		rows := platformAuditRows(t, "legal_hold.lift_cancelled", "hold_id", id.(string))
		if len(rows) != 1 || rows[0].actor != approver.id.String() {
			t.Errorf("%s: cancel audit %+v", what, rows)
		}
		problem(t, what+": approve", approve(id, int(g.body["version"].(float64))), 409, "conflict")
	}
}
