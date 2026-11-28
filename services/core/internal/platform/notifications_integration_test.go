//go:build integration

package platform_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The platform notifications (mhai-web Plan 7d; notifications-contract.ts):
// a broken audit check first, then What changed's open changes; seen per
// person; counts, codes and ids only. Everything here is synthetic.

var (
	changeNotificationKeys    = []string{"kind", "id", "change_id", "hospital", "summary", "serious", "direction", "measure", "previous", "current", "at", "seen"}
	integrityNotificationKeys = []string{"kind", "id", "run_id", "first_broken_seq", "first_broken_id", "at", "seen"}
)

func notifications(t *testing.T, tok string) ([]map[string]any, int) {
	t.Helper()
	r := get(t, "/platform/notifications", tok)
	ok(t, "notifications", r)
	sameKeys(t, "notifications", r.body, "items", "unseen")
	list, _ := r.body["items"].([]any)
	var out []map[string]any
	for _, x := range list {
		m, _ := x.(map[string]any)
		out = append(out, m)
	}
	return out, num(r.body["unseen"])
}

func markSeen(t *testing.T, tok string, v any) resp {
	return send(t, handler, http.MethodPost, "/platform/notifications/seen", tok, jsonBody(t, v))
}

func TestNotifications(t *testing.T) {
	clearRuns(t)
	f := seedChanges(t, uuid.Nil, "")
	run := seedRun(t, "broken", time.Minute)
	a, b := platformAdmin(t), platformAdmin(t)
	ta, tb := mint(t, a), mint(t, b)

	items, unseen := notifications(t, ta)
	if len(items) != 3 || unseen != 3 {
		t.Fatalf("%d items, %d unseen: %v", len(items), unseen, items)
	}
	in := items[0]
	sameKeys(t, "integrity", in, integrityNotificationKeys...)
	if in["kind"] != "audit_integrity" || in["id"] != "ntf_audit_"+run.String() || in["run_id"] != run.String() ||
		num(in["first_broken_seq"]) != 5 || in["first_broken_id"] != "5" || in["seen"] != false {
		t.Errorf("integrity %v", in)
	}
	byChange := map[string]map[string]any{}
	for _, it := range items[1:] {
		sameKeys(t, "change", it, changeNotificationKeys...)
		if it["kind"] != "analytics_change" || it["id"] != "ntf_"+it["change_id"].(string) || it["serious"] != false || it["seen"] != false {
			t.Errorf("change %v", it)
		}
		byChange[it["change_id"].(string)] = it
	}
	h := byChange[f.hospChange.String()]
	if h == nil || h["hospital"].(map[string]any)["id"] != f.hospital.String() || h["measure"] != "questions" ||
		h["direction"] != "rising" || h["summary"] != "Questions up: 14.0 to 40 a week" ||
		jsonOf(h["current"]) != `{"suppressed":false,"value":40}` {
		t.Errorf("hospital change %v", h)
	}
	if all := byChange[f.allChng.String()]; all == nil || all["hospital"] != nil {
		t.Errorf("all-hospitals change %v", all)
	}
	if byChange[f.closed.String()] != nil {
		t.Error("a closed change is a notification")
	}
	// No person, patient or free text: the raw reply names nothing else.
	raw := get(t, "/platform/notifications", ta).raw
	if strings.Contains(raw, a.email) || strings.Contains(raw, a.name) {
		t.Error("the reply names a person")
	}

	// Seen is per person: some, then all.
	r := markSeen(t, ta, map[string]any{"ids": []string{items[1]["id"].(string), "ntf_unknown"}})
	ok(t, "seen one", r)
	sameKeys(t, "seen", r.body, "unseen")
	if num(r.body["unseen"]) != 2 {
		t.Errorf("unseen %v", r.body)
	}
	if got, n := notifications(t, ta); n != 2 || got[1]["seen"] != true || got[0]["seen"] != false {
		t.Errorf("after one seen: %d %v", n, got)
	}
	if _, n := notifications(t, tb); n != 3 {
		t.Errorf("another admin's unseen %d", n)
	}
	r = markSeen(t, ta, map[string]any{})
	ok(t, "seen all", r)
	if num(r.body["unseen"]) != 0 {
		t.Errorf("unseen %v", r.body)
	}
	problem(t, "ids not strings", send(t, handler, http.MethodPost, "/platform/notifications/seen", ta, `{"ids":[1]}`), 422, "validation_failed")
	problem(t, "unknown key", send(t, handler, http.MethodPost, "/platform/notifications/seen", ta, `{"all":true}`), 422, "validation_failed")

	// Auditors read; marking seen is a platform admin's write.
	aud := mint(t, auditor(t))
	if _, n := notifications(t, aud); n != 3 {
		t.Errorf("auditor unseen %d", n)
	}
	problem(t, "auditor seen", markSeen(t, aud, map[string]any{}), 403, "forbidden")

	// A newer intact check ends the integrity notification.
	seedRun(t, "intact", time.Second)
	if got, _ := notifications(t, ta); len(got) != 2 || got[0]["kind"] != "analytics_change" {
		t.Errorf("after an intact check %v", got)
	}
}

// Nothing to tell: an empty list, never an error.
func TestNotificationsEmpty(t *testing.T) {
	clearRuns(t)
	seedChanges(t, uuid.Nil, "")
	if _, err := super.Exec(t.Context(), `DELETE FROM analytics_changes WHERE opened_week >= '2080-01-01'`); err != nil {
		t.Fatal(err)
	}
	r := get(t, "/platform/notifications", mint(t, platformAdmin(t)))
	ok(t, "empty", r)
	if jsonOf(r.body) != `{"items":[],"unseen":0}` {
		t.Errorf("empty %s", r.raw)
	}
	problem(t, "query", get(t, "/platform/notifications?x=1", mint(t, platformAdmin(t))), 422, "validation_failed")
}
