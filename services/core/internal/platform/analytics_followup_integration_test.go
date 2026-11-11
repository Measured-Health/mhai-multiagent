//go:build integration

package platform_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/platform"
)

// What changed and follow-ups through core's whole chain (Plan 7c Tasks 8
// and 9, spec §7.3, §7.4). The run and the changes are written by hand as
// the superuser for the fixed week of 22–31 March 2090 (compared with
// February 2090), so the run is the newest whatever today's date is. Every
// name, id and note is synthetic.

const followMarker = "SYNTHETIC-FOLLOW-UP-MARKER-9c3e"

type changesFixture struct {
	hospital            uuid.UUID
	hospitalName        string
	hospChange, allChng uuid.UUID // open
	closed              uuid.UUID
}

// seedChanges stores the run and three changes (two open, one closed),
// and removes them, their follow-ups and the run when the test ends.
// hospital is the hospital of the hospital's changes (uuid.Nil: a new
// one).
func seedChanges(t *testing.T, hospital uuid.UUID, name string) *changesFixture {
	t.Helper()
	ctx := context.Background()
	if hospital == uuid.Nil {
		hospital = seedHospital(t, "active")
		if err := super.QueryRow(ctx, `SELECT name FROM hospitals WHERE id = $1`, hospital).Scan(&name); err != nil {
			t.Fatal(err)
		}
	}
	clean := func() {
		for _, q := range []string{
			`DELETE FROM analytics_follow_ups WHERE change_id IN (SELECT id FROM analytics_changes WHERE opened_week >= '2080-01-01')`,
			`DELETE FROM analytics_changes WHERE opened_week >= '2080-01-01'`,
			`DELETE FROM analytics_change_runs WHERE week_from >= '2080-01-01'`,
		} {
			if _, err := super.Exec(ctx, q); err != nil {
				t.Error(err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	if _, err := super.Exec(ctx, `INSERT INTO analytics_change_runs (week_from, week_to, month_from, month_to, evaluated_at)
		VALUES ('2090-03-22', '2090-04-01', '2090-02-01', '2090-03-01', '2090-04-01T00:20:00Z')`); err != nil {
		t.Fatal(err)
	}
	f := &changesFixture{hospital: hospital, hospitalName: name}
	insert := func(h any, area, measure, dir, cur, prev, summary, opened, last string, closed any) uuid.UUID {
		var id uuid.UUID
		if err := super.QueryRow(ctx, `INSERT INTO analytics_changes (hospital_id, area, measure, direction, current, previous, summary,
			opened_week, last_week, closed_week) VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8::date, $9::date, $10::date) RETURNING id`,
			h, area, measure, dir, cur, prev, summary, opened, last, closed).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	f.hospChange = insert(hospital, "use", "questions", "rising", `{"value": 40, "suppressed": false}`, `{"value": 14.0, "suppressed": false}`,
		"Questions up: 14.0 to 40 a week", "2090-03-15", "2090-03-22", nil)
	f.allChng = insert(nil, "quality", "needs_review_share", "worse", `{"value": 0.7500, "suppressed": false}`, `{"value": 0.2500, "suppressed": false}`,
		"Needs-review share worse: 25.00% to 75.00%", "2090-03-22", "2090-03-22", nil)
	f.closed = insert(hospital, "quality", "agreement_rate", "worse", `{"value": 0.2500, "suppressed": false}`, `{"value": 0.7500, "suppressed": false}`,
		"Agreement rate worse: 75.00% to 25.00%", "2090-03-08", "2090-03-08", "2090-03-15")
	return f
}

func followPath(id uuid.UUID) string {
	return "/platform/analytics/changes/" + id.String() + "/follow-up"
}

func putFollow(t *testing.T, tok string, id uuid.UUID, body string) resp {
	t.Helper()
	return send(t, handler, http.MethodPut, followPath(id), tok, body)
}

func delFollow(t *testing.T, tok string, id uuid.UUID, body string) resp {
	t.Helper()
	return send(t, handler, http.MethodDelete, followPath(id), tok, body)
}

// changeVersion is the change's current version.
func changeVersion(t *testing.T, id uuid.UUID) int {
	t.Helper()
	var v int
	if err := super.QueryRow(context.Background(), `SELECT version FROM analytics_changes WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func vbody(v int, extra string) string { return fmt.Sprintf(`{"version":%d%s}`, v, extra) }

// changeItems is the changes area's items of GET /platform/analytics.
func changeItems(t *testing.T, tok, period string) (map[string]any, map[string]map[string]any) {
	t.Helper()
	r := analyticsGet(t, tok, "?period="+period)
	ok(t, "analytics", r)
	area := requireKeys(t, "changes", r.body["what_changed"], "updated_at", "window", "items")
	out := map[string]map[string]any{}
	for _, x := range list(t, "items", area["items"]) {
		it := requireKeys(t, "item", x, "id", "hospital", "area", "measure", "direction", "serious", "previous", "current", "summary",
			"follow_up", "version")
		out[it["id"].(string)] = it
	}
	return area, out
}

// followAudits are the follow-up audit rows of a change.
func followAudits(t *testing.T, id uuid.UUID) []auditRow {
	t.Helper()
	rows, err := super.Query(context.Background(), `SELECT action, actor_id::text, entity, entity_id, hospital_id::text, payload
		FROM audit_log WHERE entity_id = $1 AND action LIKE 'analytics.%' ORDER BY id`, id.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(&a.action, &a.actor, &a.entity, &a.entityID, &a.hospital, &a.payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// The changes area (spec §7.3): the newest run's open changes, the same for
// every period, the closed one left out, the window, the stored values,
// larger change first, follow-ups below with by_you per caller.
func TestChangesArea(t *testing.T) {
	f := seedChanges(t, uuid.Nil, "")
	admin, other := platformAdmin(t), platformAdmin(t)
	area, items := changeItems(t, mint(t, admin), "week")
	if area["updated_at"] != "2090-04-01T00:20:00.000Z" {
		t.Errorf("updated_at %v", area["updated_at"])
	}
	if w := requireKeys(t, "window", area["window"], "from", "to"); w["from"] != "2090-02-01T00:00:00.000Z" || w["to"] != "2090-04-01T00:00:00.000Z" {
		t.Errorf("window %v", w)
	}
	if len(items) != 2 || items[f.closed.String()] != nil {
		t.Fatalf("items %v", items)
	}
	h := items[f.hospChange.String()]
	if jsonOf(h["current"]) != `{"suppressed":false,"value":40}` || jsonOf(h["previous"]) != `{"suppressed":false,"value":14}` ||
		h["serious"] != false || h["follow_up"] != nil || h["version"] != float64(1) || h["direction"] != "rising" || h["area"] != "use" {
		t.Errorf("hospital change %v", h)
	}
	if ref := h["hospital"].(map[string]any); ref["id"] != f.hospital.String() || ref["name"] != f.hospitalName {
		t.Errorf("hospital %v", ref)
	}
	if a := items[f.allChng.String()]; a["hospital"] != nil || jsonOf(a["current"]) != `{"suppressed":false,"value":0.75}` {
		t.Errorf("all change %v", a)
	}
	// The same for every period, and for an auditor.
	for _, k := range []string{"month", "quarter"} {
		if _, got := changeItems(t, mint(t, auditor(t)), k); len(got) != 2 {
			t.Errorf("%s: %v", k, got)
		}
	}
	// Order: the questions change (up 186%, 3.71 times its 50%) before the
	// share (50 points, 3.33 times its 15); a followed-up one last.
	r := analyticsGet(t, mint(t, admin), "?period=month")
	first := list(t, "items", r.body["what_changed"].(map[string]any)["items"])[0].(map[string]any)["id"]
	if first != f.hospChange.String() {
		t.Errorf("first %v, want the larger change", first)
	}
	ok(t, "follow up", putFollow(t, mint(t, admin), f.hospChange, vbody(1, `,"note":"Synthetic note"`)))
	r = analyticsGet(t, mint(t, admin), "?period=month")
	got := list(t, "items", r.body["what_changed"].(map[string]any)["items"])
	if got[0].(map[string]any)["id"] != f.allChng.String() {
		t.Errorf("a followed-up change isn't last: %v", got)
	}
	// by_you per caller; auditors see the note.
	for who, want := range map[string]bool{"author": true, "other admin": false, "auditor": false} {
		u := admin
		switch who {
		case "other admin":
			u = other
		case "auditor":
			u = auditor(t)
		}
		_, items := changeItems(t, mint(t, u), "month")
		fu := requireKeys(t, "follow_up", items[f.hospChange.String()]["follow_up"], "by", "at", "note", "by_you", "version")
		if fu["by_you"] != want || fu["note"] != "Synthetic note" || fu["version"] != float64(1) {
			t.Errorf("%s: %v", who, fu)
		}
		if by := fu["by"].(map[string]any); by["id"] != admin.id.String() || by["name"] != admin.name {
			t.Errorf("%s: by %v", who, by)
		}
	}
}

// No run yet: changes is {unavailable: true}, the other areas unchanged.
func TestChangesAreaBeforeAnyRun(t *testing.T) {
	fixedAnalytics(t)
	for _, q := range []string{`DELETE FROM analytics_follow_ups`, `DELETE FROM analytics_changes`, `DELETE FROM analytics_change_runs`} {
		if _, err := super.Exec(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	r := analyticsGet(t, mint(t, auditor(t)), "?period=month")
	ok(t, "analytics", r)
	if c := requireKeys(t, "changes", r.body["what_changed"], "unavailable"); c["unavailable"] != true {
		t.Errorf("changes %v", c)
	}
	requireKeys(t, "use", r.body["use"], "updated_at", "rows", "median", "trend")
}

// A18: two PUTs with one version, in parallel: one 200, one 409.
func TestFollowUpParallelSameVersion(t *testing.T) {
	for round := 0; round < 5; round++ {
		f := seedChanges(t, uuid.Nil, "")
		toks := []string{mint(t, platformAdmin(t)), mint(t, platformAdmin(t))}
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for i := range toks {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				codes[i] = send(t, handler, http.MethodPut, followPath(f.hospChange), toks[i], vbody(1, `,"note":"Synthetic race"`)).code
			}(i)
		}
		wg.Wait()
		if !(codes[0] == 200 && codes[1] == 409) && !(codes[0] == 409 && codes[1] == 200) {
			t.Fatalf("round %d: %v, want one 200 and one 409", round, codes)
		}
		if n := len(followAudits(t, f.hospChange)); n != 1 || changeVersion(t, f.hospChange) != 2 {
			t.Fatalf("round %d: %d audit rows, version %d", round, n, changeVersion(t, f.hospChange))
		}
	}
}

// A19: another admin may not edit the note (403), but may undo (200).
func TestFollowUpNonAuthorEdit(t *testing.T) {
	f := seedChanges(t, uuid.Nil, "")
	a, b := mint(t, platformAdmin(t)), mint(t, platformAdmin(t))
	ok(t, "follow up", putFollow(t, a, f.hospChange, vbody(1, `,"note":"Synthetic first"`)))
	problem(t, "another's edit", putFollow(t, b, f.hospChange, vbody(2, `,"note":"Synthetic other"`)), 403, "forbidden")
	if changeVersion(t, f.hospChange) != 2 {
		t.Errorf("a refused edit moved the version")
	}
	// The author edits: follow_up.version moves on.
	r := putFollow(t, a, f.hospChange, vbody(2, `,"note":"Synthetic edited"`))
	ok(t, "edit", r)
	if fu := r.body["follow_up"].(map[string]any); fu["note"] != "Synthetic edited" || fu["version"] != float64(2) || fu["by_you"] != true ||
		r.body["version"] != float64(3) {
		t.Errorf("edit: %s", r.raw)
	}
	r = delFollow(t, b, f.hospChange, vbody(3, ""))
	ok(t, "another's undo", r)
	if r.body["follow_up"] != nil || r.body["version"] != float64(4) {
		t.Errorf("undo: %s", r.raw)
	}
	// Now b may follow it up.
	ok(t, "b follows up", putFollow(t, b, f.hospChange, vbody(4, "")))
}

// A20: auditors 403 on both writes; hospital audiences not_platform;
// two-step always.
func TestFollowUpRoles(t *testing.T) {
	f := seedChanges(t, uuid.Nil, "")
	h := seedHospital(t, "active")
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		path := followPath(f.hospChange)
		problem(t, m+" auditor", send(t, handler, m, path, mint(t, auditor(t)), vbody(1, "")), 403, "forbidden")
		problem(t, m+" clinician", send(t, handler, m, path, mint(t, seedUser(t, h, "", authz.RoleClinician)), vbody(1, "")), 403, "not_platform")
		problem(t, m+" hospital admin", send(t, handler, m, path, mint(t, seedUser(t, h, "", authz.RoleHospitalAdmin)), vbody(1, "")), 403, "not_platform")
		problem(t, m+" no two-step", send(t, handler, m, path, signIn(t, platformAdmin(t), false).tok, vbody(1, "")), 403, "mfa_required")
		problem(t, m+" anonymous", send(t, handler, m, path, "", vbody(1, "")), 401, "unauthenticated")
	}
	if changeVersion(t, f.hospChange) != 1 || len(followAudits(t, f.hospChange)) != 0 {
		t.Error("a refused write changed something")
	}
}

// A21: each write appends exactly one audit row with its closed payload, in
// the write's transaction: a failure after the row rolls both back.
func TestFollowUpAuditedOnce(t *testing.T) {
	f := seedChanges(t, uuid.Nil, "")
	u := platformAdmin(t)
	tok := mint(t, u)
	ok(t, "follow up", putFollow(t, tok, f.hospChange, vbody(1, `,"note":"Synthetic note"`)))
	ok(t, "edit", putFollow(t, tok, f.hospChange, vbody(2, `,"note":null`)))
	ok(t, "undo", delFollow(t, tok, f.hospChange, vbody(3, "")))
	ok(t, "again, no note", putFollow(t, tok, f.hospChange, vbody(4, "")))
	rows := followAudits(t, f.hospChange)
	want := []struct {
		action  string
		payload string
	}{
		{"analytics.followed_up", `{"change_id":"` + f.hospChange.String() + `","has_note":true}`},
		{"analytics.follow_up_edited", `{"change_id":"` + f.hospChange.String() + `"}`},
		{"analytics.follow_up_undone", `{"change_id":"` + f.hospChange.String() + `"}`},
		{"analytics.followed_up", `{"change_id":"` + f.hospChange.String() + `","has_note":false}`},
	}
	if len(rows) != len(want) {
		t.Fatalf("%d audit rows: %+v", len(rows), rows)
	}
	for i, w := range want {
		r := rows[i]
		if r.action != w.action || jsonOf(r.payload) != w.payload || r.actor != u.id.String() || r.entity != "analytics_change" || r.hospital != nil {
			t.Errorf("row %d: %+v, want %s %s", i, r, w.action, w.payload)
		}
	}
	// A failure after the audit row: the write and the row roll back.
	mux := authz.NewMux(guard)
	platform.Register(mux, platform.Deps{Pool: app, AfterFollowUpAudit: func() error { return errors.New("synthetic failure") }})
	h := mux.Handler([]string{serviceKey})
	problem(t, "forced failure", send(t, h, http.MethodPut, followPath(f.allChng), tok, vbody(1, `,"note":"Synthetic note"`)), 500, "internal_error")
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM analytics_follow_ups WHERE change_id = $1`, f.allChng).Scan(&n); err != nil || n != 0 {
		t.Errorf("the follow-up survived: %d %v", n, err)
	}
	if len(followAudits(t, f.allChng)) != 0 || changeVersion(t, f.allChng) != 1 {
		t.Error("the audit row or the version survived the rollback")
	}
}

// A22: a random id is 404 on every route that takes one; a closed change is
// 409 on the writes and 404 on its series; a stale version 409; nothing to
// undo 409.
func TestFollowUpUnknownOrClosed(t *testing.T) {
	f := seedChanges(t, uuid.Nil, "")
	tok := mint(t, platformAdmin(t))
	random := uuid.New()
	problem(t, "PUT random", putFollow(t, tok, random, vbody(1, "")), 404, "not_found")
	problem(t, "DELETE random", delFollow(t, tok, random, vbody(1, "")), 404, "not_found")
	problem(t, "series random", get(t, "/platform/analytics/changes/"+random.String()+"/series?period=month", tok), 404, "not_found")
	problem(t, "not a uuid", send(t, handler, http.MethodPut, "/platform/analytics/changes/"+strings.ToUpper(random.String())+"/follow-up", tok, vbody(1, "")),
		404, "not_found")
	problem(t, "PUT closed", putFollow(t, tok, f.closed, vbody(1, "")), 409, "conflict")
	problem(t, "DELETE closed", delFollow(t, tok, f.closed, vbody(1, "")), 409, "conflict")
	problem(t, "series closed", get(t, "/platform/analytics/changes/"+f.closed.String()+"/series?period=month", tok), 404, "not_found")
	problem(t, "stale version", putFollow(t, tok, f.hospChange, vbody(7, "")), 409, "conflict")
	problem(t, "nothing to undo", delFollow(t, tok, f.hospChange, vbody(1, "")), 409, "conflict")
	if changeVersion(t, f.hospChange) != 1 || changeVersion(t, f.closed) != 1 {
		t.Error("a refused write moved a version")
	}
}

// The note (R11): 1 to 500 characters, newlines allowed; Unicode Cf, Zl,
// Zp refused; the body strict.
func TestFollowUpNoteRules(t *testing.T) {
	f := seedChanges(t, uuid.Nil, "")
	tok := mint(t, platformAdmin(t))
	for name, body := range map[string]string{
		"501 characters":      vbody(1, `,"note":"`+strings.Repeat("n", 501)+`"`),
		"a bidi control":      vbody(1, `,"note":"Synthetic ‮ note"`),
		"U+2028":              vbody(1, `,"note":"Synthetic   note"`),
		"a soft hyphen":       vbody(1, `,"note":"Synthetic­note"`),
		"blank":               vbody(1, `,"note":"   "`),
		"empty":               vbody(1, `,"note":""`),
		"a pseudonym":         vbody(1, `,"note":"see P-AB12CD"`),
		"no version":          `{"note":"Synthetic"}`,
		"version 0":           vbody(0, ""),
		"version a string":    `{"version":"1"}`,
		"an unknown field":    vbody(1, `,"by_user":"`+uuid.NewString()+`"`),
		"not JSON":            `version=1`,
		"a note not a string": vbody(1, `,"note":5`),
	} {
		problem(t, name, putFollow(t, tok, f.hospChange, body), 422, "validation_failed")
	}
	problem(t, "DELETE with a note", delFollow(t, tok, f.hospChange, vbody(1, `,"note":"x"`)), 422, "validation_failed")
	if changeVersion(t, f.hospChange) != 1 {
		t.Fatal("a refused note moved the version")
	}
	r := putFollow(t, tok, f.hospChange, vbody(1, `,"note":"Synthetic line one\nline twoé `+strings.Repeat("n", 470)+`"`))
	ok(t, "a newline and 500 characters", r)
	if note := r.body["follow_up"].(map[string]any)["note"].(string); !strings.Contains(note, "\n") {
		t.Errorf("note %q", note)
	}
}

// A15: the note is in no log line and no audit payload, on success or
// refusal; it is in the changes list.
func TestFollowUpNoteNeverLeaves(t *testing.T) {
	f := seedChanges(t, uuid.Nil, "")
	a, b := mint(t, platformAdmin(t)), mint(t, platformAdmin(t))
	note := `,"note":"Synthetic ` + followMarker + `"`
	ok(t, "follow up", putFollow(t, a, f.hospChange, vbody(1, note)))
	problem(t, "another's edit", putFollow(t, b, f.hospChange, vbody(2, note)), 403, "forbidden")
	problem(t, "stale", putFollow(t, a, f.hospChange, vbody(1, note)), 409, "conflict")
	problem(t, "bad note", putFollow(t, a, f.allChng, vbody(1, `,"note":"Synthetic `+followMarker+` "`)), 422, "validation_failed")
	// A failed write's log line.
	mux := authz.NewMux(guard)
	platform.Register(mux, platform.Deps{Pool: app, Log: guard.Log, AfterFollowUpAudit: func() error { return errors.New("synthetic failure") }})
	problem(t, "failure", send(t, mux.Handler([]string{serviceKey}), http.MethodPut, followPath(f.allChng), a, vbody(1, note)), 500, "internal_error")
	if strings.Contains(logs.String(), followMarker) {
		t.Error("the note reached a log line")
	}
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE payload::text LIKE '%' || $1 || '%'`, followMarker).Scan(&n); err != nil || n != 0 {
		t.Errorf("the note is in %d audit payloads (%v)", n, err)
	}
	r := analyticsGet(t, a, "?period=month")
	if !strings.Contains(r.raw, followMarker) {
		t.Error("the note isn't in the changes list")
	}
}

// The same Idempotency-Key replays the result: one write, one audit row.
func TestFollowUpIdempotent(t *testing.T) {
	f := seedChanges(t, uuid.Nil, "")
	tok := mint(t, platformAdmin(t))
	key := "k-" + uniq()
	path := httpx.BasePath + followPath(f.hospChange)
	first := sendKeyed(t, http.MethodPut, path, tok, vbody(1, `,"note":"Synthetic once"`), key)
	ok(t, "first", first)
	again := sendKeyed(t, http.MethodPut, path, tok, vbody(1, `,"note":"Synthetic once"`), key)
	ok(t, "replay", again)
	if first.raw != again.raw {
		t.Errorf("replay differs:\n%s\n%s", first.raw, again.raw)
	}
	if n := len(followAudits(t, f.hospChange)); n != 1 || changeVersion(t, f.hospChange) != 2 {
		t.Errorf("%d audit rows, version %d", n, changeVersion(t, f.hospChange))
	}
	problem(t, "same key, other body", sendKeyed(t, http.MethodPut, path, tok, vbody(2, ""), key), 409, "conflict")
}

// by_you is true for the author only, in the write's reply too.
func TestByYouPerCaller(t *testing.T) {
	f := seedChanges(t, uuid.Nil, "")
	author, other := platformAdmin(t), platformAdmin(t)
	r := putFollow(t, mint(t, author), f.allChng, vbody(1, ""))
	ok(t, "follow up", r)
	if r.body["follow_up"].(map[string]any)["by_you"] != true {
		t.Errorf("reply: %s", r.raw)
	}
	r = delFollow(t, mint(t, other), f.allChng, vbody(2, ""))
	ok(t, "undo", r)
	r = putFollow(t, mint(t, other), f.allChng, vbody(3, ""))
	ok(t, "follow up", r)
	_, items := changeItems(t, mint(t, author), "week")
	if items[f.allChng.String()]["follow_up"].(map[string]any)["by_you"] != false {
		t.Error("by_you for the former author")
	}
	_, items = changeItems(t, mint(t, other), "week")
	if items[f.allChng.String()]["follow_up"].(map[string]any)["by_you"] != true {
		t.Error("by_you not set for the author")
	}
}

// Ten follow-up writes a minute per session, PUT and DELETE together, then
// 429 rate_limited, as the job retry and model policy writes; a refused
// body isn't counted, and another session has its own minute.
func TestFollowUpRateLimited(t *testing.T) {
	f := seedChanges(t, uuid.Nil, "")
	u := platformAdmin(t)
	tok := mint(t, u)
	problem(t, "a bad body first", putFollow(t, tok, f.hospChange, `{"note":"x"}`), 422, "validation_failed")
	for v := 1; v <= platform.FollowUpWritesPerMinute; v++ {
		var r resp
		if v%2 == 1 {
			r = putFollow(t, tok, f.hospChange, vbody(v, ""))
		} else {
			r = delFollow(t, tok, f.hospChange, vbody(v, ""))
		}
		ok(t, fmt.Sprintf("write %d", v), r)
	}
	v := platform.FollowUpWritesPerMinute + 1
	problem(t, "one more PUT", putFollow(t, tok, f.hospChange, vbody(v, "")), 429, "rate_limited")
	problem(t, "one more DELETE", delFollow(t, tok, f.hospChange, vbody(v, "")), 429, "rate_limited")
	if changeVersion(t, f.hospChange) != v {
		t.Errorf("a refused write moved the version to %d", changeVersion(t, f.hospChange))
	}
	ok(t, "another session", putFollow(t, mint(t, u), f.hospChange, vbody(v, "")))
}
