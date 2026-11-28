//go:build integration

package platform_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/platform"
)

// Platform settings (mhai-web Plan 8c; migration 00062) through core's
// whole chain. The settings are one platform-wide set, so every test
// starts from the defaults (resetSettings). Every name and reason is
// synthetic.

var (
	settingsKeys      = []string{"retention", "triage", "feedback_buffer", "pending"}
	retentionKeys     = []string{"confirmed", "paused", "paused_by", "paused_at", "version", "items"}
	retentionItemKeys = []string{"type", "days", "minimum_days", "last_run", "next_run_at", "changed_by", "changed_at", "version"}
	triageKeys        = []string{"enabled", "ready", "not_ready_reason", "changed_by", "changed_at", "version"}
	bufferKeys        = []string{"waiting", "last_reset", "running", "failed_job_id"}
	requestKeys       = []string{"id", "kind", "requested_by", "requested_at", "reason", "requested_by_you", "expires_at",
		"version", "type", "from", "to"}
)

// resetSettings puts the settings back to the migration's defaults, now
// and when the test ends (the shorten trigger is bypassed for the reset
// only).
func resetSettings(t *testing.T) {
	t.Helper()
	reset := func() {
		ctx := context.Background()
		tx, err := super.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		for _, q := range []string{
			`SET LOCAL session_replication_role = replica`,
			`DELETE FROM platform_setting_requests`,
			`UPDATE platform_retention SET version = 1, changed_by = NULL, changed_at = NULL,
				days = CASE type WHEN 'pending_corrections' THEN 365 ELSE 2190 END`,
			`UPDATE platform_retention_pause SET paused = false, paused_by = NULL, paused_at = NULL, version = 1`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	reset()
	t.Cleanup(reset)
}

func settingsPut(t *testing.T, tok, path string, v any) resp {
	return send(t, handler, http.MethodPut, "/platform/settings"+path, tok, jsonBody(t, v))
}

func settingsPost(t *testing.T, tok, path string, v any) resp {
	return send(t, handler, http.MethodPost, "/platform/settings"+path, tok, jsonBody(t, v))
}

// item is the settings reply's retention item of type typ.
func item(t *testing.T, body map[string]any, typ string) map[string]any {
	t.Helper()
	ret, _ := body["retention"].(map[string]any)
	list, _ := ret["items"].([]any)
	for _, x := range list {
		if m, _ := x.(map[string]any); m["type"] == typ {
			return m
		}
	}
	t.Fatalf("no item %s in %v", typ, body)
	return nil
}

func num(v any) int { f, _ := v.(float64); return int(f) }

func pending(body map[string]any) []map[string]any {
	list, _ := body["pending"].([]any)
	var out []map[string]any
	for _, x := range list {
		m, _ := x.(map[string]any)
		out = append(out, m)
	}
	return out
}

func auditCount(t *testing.T, action string, actor uuid.UUID) int {
	return count(t, `SELECT count(*) FROM audit_log WHERE action = $1 AND actor_id = $2 AND hospital_id IS NULL`, action, actor)
}

// The read: the contract's keys, today's placeholder periods, not
// confirmed and never run, triage off and not ready, and the roles.
func TestSettingsRead(t *testing.T) {
	resetSettings(t)
	r := get(t, "/platform/settings", mint(t, platformAdmin(t)))
	ok(t, "settings", r)
	sameKeys(t, "settings", r.body, settingsKeys...)
	ret := r.body["retention"].(map[string]any)
	sameKeys(t, "retention", ret, retentionKeys...)
	if ret["confirmed"] != false || ret["paused"] != false || ret["paused_by"] != nil || num(ret["version"]) != 1 {
		t.Errorf("retention %v", ret)
	}
	want := map[string][2]int{"records": {2190, 365}, "documents": {2190, 365}, "pending_corrections": {365, 30},
		"audit_log": {2190, 2190}}
	list := ret["items"].([]any)
	if len(list) != 4 || list[0].(map[string]any)["type"] != "records" || list[3].(map[string]any)["type"] != "audit_log" {
		t.Fatalf("items %v", list)
	}
	for typ, w := range want {
		it := item(t, r.body, typ)
		sameKeys(t, typ, it, retentionItemKeys...)
		if num(it["days"]) != w[0] || num(it["minimum_days"]) != w[1] || it["last_run"] != nil || it["next_run_at"] != nil ||
			it["changed_by"] != nil || num(it["version"]) != 1 {
			t.Errorf("%s %v", typ, it)
		}
	}
	tr := r.body["triage"].(map[string]any)
	sameKeys(t, "triage", tr, triageKeys...)
	if tr["enabled"] != false || tr["ready"] != false || tr["not_ready_reason"] != "missing_files" || num(tr["version"]) != 1 {
		t.Errorf("triage %v", tr)
	}
	b := r.body["feedback_buffer"].(map[string]any)
	sameKeys(t, "buffer", b, bufferKeys...)
	if b["running"] != nil || b["failed_job_id"] != nil {
		t.Errorf("buffer %v", b)
	}
	if p := r.body["pending"].([]any); len(p) != 0 {
		t.Errorf("pending %v", p)
	}
	ok(t, "auditor", get(t, "/platform/settings", mint(t, auditor(t))))
	clin := seedUser(t, seedHospital(t, "active"), "", authz.RoleHospitalAdmin)
	problem(t, "hospital admin", get(t, "/platform/settings", mint(t, clin)), 403, "not_platform")
	problem(t, "query", get(t, "/platform/settings?x=1", mint(t, platformAdmin(t))), 422, "validation_failed")
	problem(t, "no two-step", get(t, "/platform/settings", signIn(t, platformAdmin(t), false).tok), 403, "mfa_required")
}

// Auditors read and change nothing; every write is refused them before a
// body is read.
func TestSettingsAuditorWrites(t *testing.T) {
	resetSettings(t)
	aud := mint(t, auditor(t))
	id := uuid.NewString()
	for _, c := range []struct{ method, path string }{
		{"PUT", "/retention/records"}, {"PUT", "/retention-pause"}, {"PUT", "/triage"},
		{"POST", "/requests/" + id + "/approve"}, {"POST", "/requests/" + id + "/cancel"}, {"POST", "/feedback-buffer/reset"},
	} {
		problem(t, c.method+" "+c.path, send(t, handler, c.method, "/platform/settings"+c.path, aud, `{}`), 403, "forbidden")
	}
}

// Longer: one platform admin, step-up, at once; audited and in Activity.
// The refusals come before a code is spent.
func TestRetentionLonger(t *testing.T) {
	resetSettings(t)
	a := platformAdmin(t)
	tok := mint(t, a)
	body := func(days, version int, code string) map[string]any {
		m := map[string]any{"days": days, "reason": "Synthetic: keep records longer", "version": version}
		if code != "" {
			m["backup_code"] = code
		}
		return m
	}
	problem(t, "unknown type", settingsPut(t, tok, "/retention/questions", body(2555, 1, "")), 404, "not_found")
	problem(t, "stale", settingsPut(t, tok, "/retention/records", body(2555, 7, "")), 409, "conflict")
	problem(t, "below the minimum", settingsPut(t, tok, "/retention/records", body(300, 1, "")), 422, "below_minimum")
	problem(t, "audit log below six years", settingsPut(t, tok, "/retention/audit_log", body(2000, 1, "")), 422, "below_minimum")
	problem(t, "the same", settingsPut(t, tok, "/retention/records", body(2190, 1, "")), 422, "validation_failed")
	problem(t, "no reason", settingsPut(t, tok, "/retention/records", map[string]any{"days": 2555, "version": 1}), 422, "validation_failed")
	problem(t, "control character", settingsPut(t, tok, "/retention/records",
		map[string]any{"days": 2555, "version": 1, "reason": "a\u0007b"}), 422, "validation_failed")
	problem(t, "not whole", send(t, handler, http.MethodPut, "/platform/settings/retention/records", tok,
		`{"days": 2555.5, "reason": "x", "version": 1}`), 422, "validation_failed")
	problem(t, "no code", settingsPut(t, tok, "/retention/records", body(2555, 1, "")), 403, "mfa_required")
	r := settingsPut(t, tok, "/retention/records", body(2555, 1, backupCode(t, a)))
	ok(t, "longer", r)
	sameKeys(t, "reply", r.body, settingsKeys...)
	it := item(t, r.body, "records")
	if num(it["days"]) != 2555 || num(it["version"]) != 2 || it["changed_by"].(map[string]any)["id"] != a.id.String() || it["changed_at"] == nil {
		t.Errorf("records %v", it)
	}
	if n := auditCount(t, "platform.retention_changed", a.id); n != 1 {
		t.Errorf("audit rows %d", n)
	}
	if n := count(t, `SELECT count(*) FROM platform_setting_log WHERE by = $1 AND action = 'retention_changed'
		AND reason = 'Synthetic: keep records longer'`, a.id); n != 1 {
		t.Errorf("reason rows %d", n)
	}
	act := get(t, "/platform/activity", tok)
	ok(t, "activity", act)
	e := act.body["events"].([]any)[0].(map[string]any)
	if e["action"] != "retention_changed" || e["target"] != "Records: 6 years to 7 years" {
		t.Errorf("activity %v", e)
	}
}

// Shorter: a request (202), a preview first, a different platform admin's
// approval with step-up. The requester can't approve their own; a stale
// version, a second request and a lapsed one are refused.
func TestRetentionShorterTwoPeople(t *testing.T) {
	resetSettings(t)
	a, b := platformAdmin(t), platformAdmin(t)
	ta, tb := mint(t, a), mint(t, b)
	r := settingsPut(t, ta, "/retention/records", map[string]any{"days": 1095, "reason": "Synthetic: shorten",
		"version": 1, "backup_code": backupCode(t, a)})
	if r.code != http.StatusAccepted {
		t.Fatalf("shorter: %d %s", r.code, r.raw)
	}
	if it := item(t, r.body, "records"); num(it["days"]) != 2190 || num(it["version"]) != 2 {
		t.Errorf("the period changed before approval: %v", it)
	}
	p := pending(r.body)
	if len(p) != 1 {
		t.Fatalf("pending %v", r.body["pending"])
	}
	sameKeys(t, "request", p[0], requestKeys...)
	req := p[0]
	if req["kind"] != "retention_shorten" || req["type"] != "records" || num(req["from"]) != 2190 || num(req["to"]) != 1095 ||
		req["requested_by_you"] != true || req["reason"] != "Synthetic: shorten" || num(req["version"]) != 2 ||
		req["requested_by"].(map[string]any)["id"] != a.id.String() {
		t.Errorf("request %v", req)
	}
	at, _ := time.Parse(time.RFC3339, req["requested_at"].(string))
	exp, _ := time.Parse(time.RFC3339, req["expires_at"].(string))
	if exp.Sub(at) != 7*24*time.Hour {
		t.Errorf("lapses after %v", exp.Sub(at))
	}
	id := req["id"].(string)
	g := get(t, "/platform/settings", tb)
	if q := pending(g.body); len(q) != 1 || q[0]["requested_by_you"] != false {
		t.Errorf("as the other admin %v", g.body["pending"])
	}
	problem(t, "a second shorter", settingsPut(t, tb, "/retention/records", map[string]any{"days": 730, "reason": "x",
		"version": 2}), 409, "conflict")

	approve := func(tok string, v int, code string) resp {
		m := map[string]any{"version": v}
		if code != "" {
			m["backup_code"] = code
		}
		return settingsPost(t, tok, "/requests/"+id+"/approve", m)
	}
	problem(t, "the requester", approve(ta, 2, ""), 409, "same_person")
	problem(t, "stale", approve(tb, 1, ""), 409, "conflict")
	problem(t, "unknown", settingsPost(t, tb, "/requests/"+uuid.NewString()+"/approve", map[string]any{"version": 2}), 404, "not_found")
	problem(t, "not an id", settingsPost(t, tb, "/requests/nope/approve", map[string]any{"version": 2}), 404, "not_found")
	problem(t, "no code", approve(tb, 2, ""), 403, "mfa_required")
	r = approve(tb, 2, backupCode(t, b))
	ok(t, "approve", r)
	it := item(t, r.body, "records")
	if num(it["days"]) != 1095 || num(it["version"]) != 3 || it["changed_by"].(map[string]any)["id"] != b.id.String() {
		t.Errorf("after approval %v", it)
	}
	if len(pending(r.body)) != 0 {
		t.Errorf("still pending %v", r.body["pending"])
	}
	if auditCount(t, "platform.setting_change_requested", a.id) != 1 || auditCount(t, "platform.setting_change_approved", b.id) != 1 {
		t.Error("audit rows")
	}
	problem(t, "approved twice", approve(tb, 3, ""), 409, "conflict")
	act := get(t, "/platform/activity", tb)
	if e := act.body["events"].([]any)[0].(map[string]any); e["action"] != "setting_change_approved" ||
		e["target"] != "Shorten Records to 3 years" {
		t.Errorf("activity %v", e)
	}
}

// The database holds the two-person rule whatever a handler does: a
// shorter period with no approved request, and an approval by the
// requester, are refused as mhai_platform.
func TestRetentionShorterDatabaseRule(t *testing.T) {
	resetSettings(t)
	a := platformAdmin(t)
	ctx := context.Background()
	checkViolation := func(what string, err error) {
		t.Helper()
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "23514" {
			t.Errorf("%s: want a check violation, got %v", what, err)
		}
	}
	checkViolation("shorter alone", db.WithPlatform(ctx, app, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE platform_retention SET days = 400 WHERE type = 'records'`)
		return err
	}))
	checkViolation("own approval", db.WithPlatform(ctx, app, func(tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO platform_setting_requests (kind, type, from_days, to_days, reason, requested_by, expires_at)
			VALUES ('retention_shorten', 'records', 2190, 400, 'x', $1, now() + interval '1 day') RETURNING id`, a.id).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE platform_setting_requests SET approved_by = $1, approved_at = now() WHERE id = $2`, a.id, id)
		return err
	}))
	// The minimum can't be changed, nor a period set below it.
	if err := db.WithPlatform(ctx, app, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE platform_retention SET minimum_days = 1 WHERE type = 'records'`)
		return err
	}); err == nil {
		t.Error("the minimum was changed")
	}
	if n := count(t, `SELECT days FROM platform_retention WHERE type = 'records'`); n != 2190 {
		t.Errorf("records kept %d days", n)
	}
}

// Cancel: either admin, no step-up, with the version; a cancelled request
// can't be approved. A longer period replaces a shorter one waiting.
func TestRetentionCancelAndSupersede(t *testing.T) {
	resetSettings(t)
	a, b := platformAdmin(t), platformAdmin(t)
	ta, tb := mint(t, a), mint(t, b)
	shorten := func(typ string, days, version int) string {
		t.Helper()
		r := settingsPut(t, ta, "/retention/"+typ, map[string]any{"days": days, "reason": "Synthetic", "version": version,
			"backup_code": backupCode(t, a)})
		if r.code != http.StatusAccepted {
			t.Fatalf("shorten: %d %s", r.code, r.raw)
		}
		return pending(r.body)[0]["id"].(string)
	}
	id := shorten("documents", 1825, 1)
	problem(t, "stale cancel", settingsPost(t, tb, "/requests/"+id+"/cancel", map[string]any{"version": 1}), 409, "conflict")
	r := settingsPost(t, tb, "/requests/"+id+"/cancel", map[string]any{"version": 2})
	ok(t, "cancel", r)
	if len(pending(r.body)) != 0 || num(item(t, r.body, "documents")["version"]) != 3 {
		t.Errorf("after cancel %v", r.body)
	}
	if auditCount(t, "platform.setting_change_cancelled", b.id) != 1 {
		t.Error("cancel audit")
	}
	problem(t, "approve a cancelled one", settingsPost(t, tb, "/requests/"+id+"/approve", map[string]any{"version": 3,
		"backup_code": "synthunspent"}), 409, "conflict")
	problem(t, "cancel twice", settingsPost(t, ta, "/requests/"+id+"/cancel", map[string]any{"version": 3}), 409, "conflict")

	id = shorten("documents", 1825, 3)
	r = settingsPut(t, tb, "/retention/documents", map[string]any{"days": 2555, "reason": "Synthetic longer", "version": 4,
		"backup_code": backupCode(t, b)})
	ok(t, "longer replaces", r)
	if len(pending(r.body)) != 0 || num(item(t, r.body, "documents")["days"]) != 2555 {
		t.Errorf("after longer %v", r.body)
	}
	if n := count(t, `SELECT count(*) FROM platform_setting_requests WHERE id = $1 AND end_reason = 'superseded'`, id); n != 1 {
		t.Errorf("superseded rows %d", n)
	}
}

// A request lapses after 7 days: it isn't waiting, approving it is 409
// expired, and the next write ends it, audited as lapsed.
func TestRetentionRequestLapses(t *testing.T) {
	resetSettings(t)
	a, b := platformAdmin(t), platformAdmin(t)
	ta, tb := mint(t, a), mint(t, b)
	r := settingsPut(t, ta, "/retention/pending_corrections", map[string]any{"days": 90, "reason": "Synthetic", "version": 1,
		"backup_code": backupCode(t, a)})
	if r.code != http.StatusAccepted {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	id := pending(r.body)[0]["id"].(string)
	if _, err := super.Exec(context.Background(), `UPDATE platform_setting_requests SET requested_at = now() - interval '8 days',
		expires_at = now() - interval '1 day' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if g := get(t, "/platform/settings", tb); len(pending(g.body)) != 0 {
		t.Errorf("a lapsed request is waiting: %v", g.body["pending"])
	}
	problem(t, "approve lapsed", settingsPost(t, tb, "/requests/"+id+"/approve", map[string]any{"version": 2}), 409, "expired")
	problem(t, "cancel lapsed", settingsPost(t, tb, "/requests/"+id+"/cancel", map[string]any{"version": 2}), 409, "conflict")
	// The next write ends it, and a new request can be made.
	r = settingsPut(t, tb, "/retention/pending_corrections", map[string]any{"days": 60, "reason": "Synthetic again", "version": 2,
		"backup_code": backupCode(t, b)})
	if r.code != http.StatusAccepted {
		t.Fatalf("new request: %d %s", r.code, r.raw)
	}
	if n := count(t, `SELECT count(*) FROM audit_log WHERE action = 'platform.setting_change_lapsed' AND payload->>'request_id' = $1
		AND actor_id = $2`, id, a.id); n != 1 {
		t.Errorf("lapse audit rows %d", n)
	}
	problem(t, "approve after the lapse was recorded", settingsPost(t, tb, "/requests/"+id+"/approve", map[string]any{"version": 3}),
		409, "expired")
}

// A requester removed or demoted is no longer one of the two people: the
// team write ends their requests; one changed outside the routes is
// refused at approval.
func TestRetentionRequesterGone(t *testing.T) {
	resetSettings(t)
	b := platformAdmin(t)
	tb := mint(t, b)
	request := func() (user, string) {
		a := platformAdmin(t)
		r := settingsPut(t, mint(t, a), "/retention/records", map[string]any{"days": 730, "reason": "Synthetic", "version": 1,
			"backup_code": backupCode(t, a)})
		if r.code != http.StatusAccepted {
			t.Fatalf("%d %s", r.code, r.raw)
		}
		return a, pending(r.body)[0]["id"].(string)
	}
	a, id := request()
	if _, err := super.Exec(context.Background(), `UPDATE users SET is_active = false WHERE id = $1`, a.id); err != nil {
		t.Fatal(err)
	}
	problem(t, "approve a deactivated requester's", settingsPost(t, tb, "/requests/"+id+"/approve",
		map[string]any{"version": 2, "backup_code": "synthunspent"}), 409, "conflict")
	resetSettings(t)

	a, id = request()
	r := send(t, handler, http.MethodPatch, "/platform/team/"+a.id.String(), tb,
		jsonBody(t, map[string]any{"role": "auditor", "version": 1, "backup_code": backupCode(t, b)}))
	ok(t, "demote", r)
	if n := count(t, `SELECT count(*) FROM platform_setting_requests WHERE id = $1 AND end_reason = 'requester_left'
		AND ended_by = $2`, id, b.id); n != 1 {
		t.Errorf("the demoted requester's request is open (%d)", n)
	}
	if auditCount(t, "platform.setting_change_cancelled", b.id) != 1 {
		t.Error("cancel audit")
	}
}

// The preview: counts only, 0 when not shorter, the hospitals with an open
// legal hold left out.
func TestRetentionPreview(t *testing.T) {
	resetSettings(t)
	tok := mint(t, auditor(t))
	preview := func(typ, days string) int {
		t.Helper()
		r := get(t, "/platform/settings/retention/"+typ+"/preview?days="+days, tok)
		ok(t, "preview "+typ, r)
		sameKeys(t, "preview", r.body, "would_delete")
		return num(r.body["would_delete"])
	}
	before := preview("records", "365")
	free, held := seedHospital(t, "active"), seedHospital(t, "active")
	ctx := context.Background()
	for _, h := range []uuid.UUID{free, free, free, held, held} {
		if _, err := super.Exec(ctx, `INSERT INTO clinical_decisions (id, hospital_id, created_at) VALUES ($1, $2, now() - interval '400 days')`,
			uuid.New(), h); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := super.Exec(ctx, `INSERT INTO clinical_decisions (id, hospital_id) VALUES ($1, $2)`, uuid.New(), free); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `INSERT INTO legal_holds (hospital_id, reference) VALUES ($1, $2)`, held,
		"LH-2026-"+strconv.Itoa(int(time.Now().UnixNano()%900000)+100000)); err != nil {
		t.Fatal(err)
	}
	// Exactly what the definer counts (the held hospital's rows kept),
	// rounded up to a multiple of 10.
	raw := count(t, `SELECT count(*) FROM clinical_decisions c WHERE c.created_at < now() - interval '365 days'
		AND NOT EXISTS (SELECT 1 FROM legal_holds l WHERE l.hospital_id = c.hospital_id AND l.lifted_at IS NULL)`)
	if got, want := preview("records", "365"), int(platform.PreviewRound(int64(raw))); got != want || got < before {
		t.Errorf("would delete %d, want %d (raw %d, before %d)", got, want, raw, before)
	}
	var mine int64
	if err := db.WithPlatform(ctx, app, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mhai_retention_preview('records', 365)`).Scan(&mine)
	}); err != nil || int(mine) != raw {
		t.Errorf("the definer counted %d, want %d (%v)", mine, raw, err)
	}
	if got := preview("records", "2190"); got != 0 {
		t.Errorf("the current period would delete %d", got)
	}
	if got := preview("records", "3000"); got != 0 {
		t.Errorf("a longer period would delete %d", got)
	}
	preview("documents", "365")
	preview("pending_corrections", "30")
	problem(t, "unknown type", get(t, "/platform/settings/retention/questions/preview?days=365", tok), 404, "not_found")
	for _, q := range []string{"", "?days=0", "?days=x", "?days=01", "?days=365&days=366", "?days=365&x=1", "?days=99999"} {
		problem(t, "query "+q, get(t, "/platform/settings/retention/records/preview"+q, tok), 422, "validation_failed")
	}
	// The caller's hospital setting is untouched after the definer.
	if err := db.WithPlatform(ctx, app, func(tx pgx.Tx) error {
		var n int64
		if err := tx.QueryRow(ctx, `SELECT mhai_retention_preview('records', 365)`).Scan(&n); err != nil {
			return err
		}
		var h *string
		if err := tx.QueryRow(ctx, `SELECT NULLIF(current_setting('mhai.hospital_id', true), '')`).Scan(&h); err != nil {
			return err
		}
		if h != nil {
			t.Errorf("mhai.hospital_id left set to %s", *h)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Pause: one admin, step-up, at once; its own version; a stale one or the
// same state is 409.
func TestRetentionPause(t *testing.T) {
	resetSettings(t)
	a := platformAdmin(t)
	tok := mint(t, a)
	body := func(paused bool, v int, code string) map[string]any {
		m := map[string]any{"paused": paused, "reason": "Synthetic: a hold is being arranged", "version": v}
		if code != "" {
			m["backup_code"] = code
		}
		return m
	}
	problem(t, "resume when running", settingsPut(t, tok, "/retention-pause", body(false, 1, "")), 409, "conflict")
	problem(t, "stale", settingsPut(t, tok, "/retention-pause", body(true, 9, "")), 409, "conflict")
	problem(t, "no code", settingsPut(t, tok, "/retention-pause", body(true, 1, "")), 403, "mfa_required")
	r := settingsPut(t, tok, "/retention-pause", body(true, 1, backupCode(t, a)))
	ok(t, "pause", r)
	ret := r.body["retention"].(map[string]any)
	if ret["paused"] != true || ret["paused_by"].(map[string]any)["id"] != a.id.String() || ret["paused_at"] == nil || num(ret["version"]) != 2 {
		t.Errorf("paused %v", ret)
	}
	problem(t, "pause twice", settingsPut(t, tok, "/retention-pause", body(true, 2, "")), 409, "conflict")
	r = settingsPut(t, tok, "/retention-pause", body(false, 2, backupCode(t, a)))
	ok(t, "resume", r)
	if ret := r.body["retention"].(map[string]any); ret["paused"] != false || ret["paused_by"] != nil || num(ret["version"]) != 3 {
		t.Errorf("resumed %v", ret)
	}
	if auditCount(t, "platform.clean_up_paused", a.id) != 1 || auditCount(t, "platform.clean_up_resumed", a.id) != 1 {
		t.Error("audit rows")
	}
}

// Triage: core has none. Off is already off (409), on is never ready
// (503 not_ready), both before a code is spent; on needs a sign-off.
func TestTriageGate(t *testing.T) {
	resetSettings(t)
	tok := mint(t, platformAdmin(t))
	problem(t, "off", settingsPut(t, tok, "/triage", map[string]any{"enabled": false, "reason": "Synthetic", "version": 1}), 409, "conflict")
	problem(t, "on", settingsPut(t, tok, "/triage", map[string]any{"enabled": true, "reason": "Synthetic", "version": 1,
		"clinical_signoff": "CS-2026-0001"}), 503, "not_ready")
	problem(t, "on without a sign-off", settingsPut(t, tok, "/triage", map[string]any{"enabled": true, "reason": "Synthetic",
		"version": 1}), 422, "validation_failed")
	problem(t, "stale", settingsPut(t, tok, "/triage", map[string]any{"enabled": true, "reason": "Synthetic", "version": 2,
		"clinical_signoff": "CS-2026-0001"}), 409, "conflict")
}

// The learning-data reset: one admin, step-up; marks every waiting row
// cleared (no row is deleted), records who, when and why, and is done in
// the reply.
func TestFeedbackBufferReset(t *testing.T) {
	resetSettings(t)
	ctx := context.Background()
	a := platformAdmin(t)
	tok := mint(t, a)
	h := seedHospital(t, "active")
	// Waiting rows, written past the buffer's own checks (its approval rule
	// is 00013's test).
	tx, err := super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := tx.Exec(ctx, `INSERT INTO feedback_buffer (hospital_id, correction_id, system_decision) VALUES ($1, $2, 'PASS')`,
			h, uuid.New()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	total := count(t, `SELECT count(*) FROM feedback_buffer`)
	waiting := count(t, `SELECT count(*) FROM feedback_buffer WHERE cleared_at IS NULL`)
	g := get(t, "/platform/settings", tok)
	if got := num(g.body["feedback_buffer"].(map[string]any)["waiting"]); got != waiting {
		t.Errorf("waiting %d, want %d", got, waiting)
	}
	problem(t, "no reason", settingsPost(t, tok, "/feedback-buffer/reset", map[string]any{}), 422, "validation_failed")
	problem(t, "no code", settingsPost(t, tok, "/feedback-buffer/reset", map[string]any{"reason": "Synthetic test run"}), 403, "mfa_required")
	r := settingsPost(t, tok, "/feedback-buffer/reset", map[string]any{"reason": "Synthetic test run", "backup_code": backupCode(t, a)})
	if r.code != http.StatusAccepted {
		t.Fatalf("reset: %d %s", r.code, r.raw)
	}
	sameKeys(t, "reset", r.body, "job_id")
	if id, _ := r.body["job_id"].(string); len(id) != 38 || id[:2] != "j_" {
		t.Errorf("job id %v", r.body["job_id"])
	}
	if n := count(t, `SELECT count(*) FROM feedback_buffer`); n != total {
		t.Errorf("rows deleted: %d → %d", total, n)
	}
	if n := count(t, `SELECT count(*) FROM feedback_buffer WHERE cleared_at IS NULL`); n != 0 {
		t.Errorf("%d still waiting", n)
	}
	g = get(t, "/platform/settings", tok)
	b := g.body["feedback_buffer"].(map[string]any)
	l, _ := b["last_reset"].(map[string]any)
	if num(b["waiting"]) != 0 || b["running"] != nil || l == nil || num(l["cleared"]) != waiting || l["reason"] != "Synthetic test run" ||
		l["by"].(map[string]any)["id"] != a.id.String() {
		t.Errorf("after reset %v", b)
	}
	if auditCount(t, "platform.feedback_buffer_cleared", a.id) != 1 {
		t.Error("audit rows")
	}
	problem(t, "auditor", settingsPost(t, mint(t, auditor(t)), "/feedback-buffer/reset", map[string]any{"reason": "x"}), 403, "forbidden")
}
