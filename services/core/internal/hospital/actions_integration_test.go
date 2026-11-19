//go:build integration

package hospital_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/delivery"
)

// The account and review actions that answered 501 (migration 00049):
// removal, two-step reset, two-step reminder, reviewer assignment and
// reminders, with their abuse cases: another hospital, yourself, the last
// admin, a refused request spending no code, no sender, the rate limits,
// and the database's own rules on assignment.

const code = `{"mfa_code": "` + goodCode + `"}`

func messages(t *testing.T, h uuid.UUID, kind delivery.Kind) int {
	return scalar(t, `SELECT count(*) FROM account_messages WHERE hospital_id = $1 AND kind = $2`, h, string(kind))
}

// assignAs assigns correction c to reviewer to as admin by (the trigger's
// rules apply to the superuser too: it checks the session's user).
func assignAs(t *testing.T, c, by, to uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, sharedSuper, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.user_id', $1, true)`, by.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE feedback_events SET assigned_to = $2 WHERE id = $1`, c, to)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRemoveStaff(t *testing.T) {
	w := newWorld(t)
	exec(t, `INSERT INTO sessions (user_id) VALUES ($1), ($1)`, w.ada)
	exec(t, `INSERT INTO trusted_devices (user_id, token_hash) VALUES ($1, $2)`, w.ada, "synth-device-hash-rm-"+w.ada.String())
	exec(t, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'analyst')`, w.ada) // superuser: exempt from 00030
	q := query(t, w.h, w.ben, "completed", "PASS", "")
	c := correction(t, w.h, w.ben, q)
	assignAs(t, c, w.admin, w.ada)
	path := "/hospital/staff/" + w.ada.String()

	// Refusals before the code, none spending it.
	if st, out := w.adm(http.MethodDelete, "/hospital/staff/"+w.admin.String(), code); st != 403 || out["code"] != "self_change" {
		t.Fatalf("self %d %v", st, out)
	}
	if st, _ := w.adm(http.MethodDelete, "/hospital/staff/"+w.other.String(), code); st != 404 {
		t.Fatalf("other hospital %d", st)
	}
	if st, _ := w.adm(http.MethodDelete, "/hospital/staff/"+uuid.NewString(), code); st != 404 {
		t.Fatalf("unknown %d", st)
	}
	if w.codesChecked != 0 {
		t.Fatalf("a refused request checked %d codes", w.codesChecked)
	}
	if st, out := w.adm(http.MethodDelete, path, ""); st != 403 || out["code"] != "mfa_required" {
		t.Fatalf("no code %d %v", st, out)
	}
	if st, out := w.adm(http.MethodDelete, path, `{"mfa_code": "000000"}`); st != 403 || out["code"] != "mfa_invalid" {
		t.Fatalf("wrong code %d %v", st, out)
	}
	if st, raw := w.as(w.h, w.admin, http.MethodDelete, path, code); st != 204 || len(raw) != 0 {
		t.Fatalf("remove %d %s", st, raw)
	}
	var active bool
	var email string
	var hash *string
	if err := sharedSuper.QueryRow(context.Background(), `SELECT is_active, email::text, password_hash FROM users WHERE id = $1 AND removed_at IS NOT NULL`,
		w.ada).Scan(&active, &email, &hash); err != nil || active || hash != nil || !strings.HasPrefix(email, "removed-") || !strings.HasSuffix(email, "@removed.invalid") {
		t.Fatalf("removed row: active %v email tombstone %v hash %v err %v", active, strings.HasPrefix(email, "removed-"), hash != nil, err)
	}
	for q, want := range map[string]int{
		`SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`: 0,
		`SELECT count(*) FROM trusted_devices WHERE user_id = $1`:                 0,
		`SELECT count(*) FROM user_mfa WHERE user_id = $1`:                        0,
		`SELECT count(*) FROM user_roles WHERE user_id = $1`:                      0,
		`SELECT count(*) FROM feedback_events WHERE assigned_to = $1`:             0,
		`SELECT count(*) FROM audit_log WHERE action = 'staff.removed' AND entity_id = $1::text AND payload = '{"sessions_revoked": 2}'::jsonb`: 1,
		`SELECT count(*) FROM audit_log WHERE action = 'analyst.removed' AND entity_id = $1::text`:                                             1,
	} {
		if n := scalar(t, q, w.ada); n != want {
			t.Errorf("%s: %d, want %d", q, n, want)
		}
	}
	// Gone from the list, and every later action on them is 404.
	_, raw := w.as(w.h, w.admin, http.MethodGet, "/hospital/staff", "")
	if strings.Contains(string(raw), w.ada.String()) {
		t.Error("a removed person is listed")
	}
	if st, _ := w.adm(http.MethodPost, path+"/reactivate", code); st != 404 {
		t.Errorf("reactivate removed %d", st)
	}
	if st, _ := w.adm(http.MethodDelete, path, code); st != 404 {
		t.Errorf("remove again %d", st)
	}
	noLeaks(t, w, raw)
}

// The last active admin can't be removed by anyone; concurrent removals of
// each other by the only two admins leave one.
func TestRemoveLastAdmin(t *testing.T) {
	w := newWorld(t)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, pair := range [][2]uuid.UUID{{w.admin, w.admin2}, {w.admin2, w.admin}} {
		wg.Add(1)
		go func(i int, by, target uuid.UUID) {
			defer wg.Done()
			codes[i], _ = w.as(w.h, by, http.MethodDelete, "/hospital/staff/"+target.String(), code)
		}(i, pair[0], pair[1])
	}
	wg.Wait()
	slices.Sort(codes)
	if codes[0] != 204 || codes[1] == 204 {
		t.Fatalf("codes %v, want one 204", codes)
	}
	if n := scalar(t, `SELECT count(*) FROM users u JOIN user_roles r ON r.user_id = u.id AND r.role = 'hospital_admin'
		WHERE u.hospital_id = $1 AND u.is_active`, w.h); n != 1 {
		t.Fatalf("%d active admins left", n)
	}
}

func TestResetMFA(t *testing.T) {
	w := newWorld(t)
	exec(t, `INSERT INTO sessions (user_id) VALUES ($1)`, w.ada)
	exec(t, `INSERT INTO trusted_devices (user_id, token_hash) VALUES ($1, $2)`, w.ada, "synth-device-hash-mfa-"+w.ada.String())
	path := "/hospital/staff/" + w.ada.String() + "/reset-mfa"
	if st, out := w.adm(http.MethodPost, "/hospital/staff/"+w.admin.String()+"/reset-mfa", code); st != 403 || out["code"] != "self_change" {
		t.Fatalf("self %d %v", st, out)
	}
	if st, out := w.adm(http.MethodPost, "/hospital/staff/"+w.ben.String()+"/reset-mfa", code); st != 409 || out["code"] != "conflict" {
		t.Fatalf("no two-step %d %v", st, out)
	}
	if st, _ := w.adm(http.MethodPost, "/hospital/staff/"+w.other.String()+"/reset-mfa", code); st != 404 {
		t.Fatalf("other hospital %d", st)
	}
	if w.codesChecked != 0 {
		t.Fatalf("a refused request checked %d codes", w.codesChecked)
	}
	if st, out := w.adm(http.MethodPost, path, ""); st != 403 || out["code"] != "mfa_required" {
		t.Fatalf("no code %d %v", st, out)
	}
	st, out := w.adm(http.MethodPost, path, code)
	if st != 200 || out["sign_in"] != "password" || !slices.Contains(out["issues"].([]any), any("mfa_not_set_up")) {
		t.Fatalf("reset %d %v", st, out)
	}
	if scalar(t, `SELECT count(*) FROM user_mfa WHERE user_id = $1`, w.ada) != 0 ||
		scalar(t, `SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, w.ada) != 0 ||
		scalar(t, `SELECT count(*) FROM trusted_devices WHERE user_id = $1`, w.ada) != 0 {
		t.Fatal("two-step, a session or a trusted device kept")
	}
	if got := w.sentTo(delivery.MFAReset); len(got) != 1 || got[0] != w.ada || messages(t, w.h, delivery.MFAReset) != 1 ||
		audits(t, w.h, "staff.mfa_reset") != 1 {
		t.Fatalf("notice %v, outbox %d", got, messages(t, w.h, delivery.MFAReset))
	}
	// Again: no two-step to reset.
	if st, _ := w.adm(http.MethodPost, path, code); st != 409 {
		t.Errorf("again %d", st)
	}
	// The hourly limit: three resets an hour per person.
	for range 2 {
		exec(t, `INSERT INTO account_messages (hospital_id, recipient_id, kind, channel, created_by) VALUES ($1, $2, 'mfa_reset', 'log', $3)`, w.h, w.admin2, w.admin)
	}
	exec(t, `INSERT INTO account_messages (hospital_id, recipient_id, kind, channel, created_by) VALUES ($1, $2, 'mfa_reset', 'log', $3)`, w.h, w.admin2, w.admin)
	if st, out := w.adm(http.MethodPost, "/hospital/staff/"+w.admin2.String()+"/reset-mfa", code); st != 429 || out["code"] != "rate_limited" {
		t.Errorf("limit %d %v", st, out)
	}
	noLeaks(t, w, nil)
}

func TestRemindStaff(t *testing.T) {
	w := newWorld(t)
	path := "/hospital/staff/" + w.ben.String() + "/remind"
	if st, out := w.adm(http.MethodPost, "/hospital/staff/"+w.ada.String()+"/remind", ""); st != 409 || out["code"] != "conflict" {
		t.Fatalf("two-step on %d %v", st, out)
	}
	if st, _ := w.adm(http.MethodPost, "/hospital/staff/"+w.other.String()+"/remind", ""); st != 404 {
		t.Fatalf("other hospital %d", st)
	}
	if st, out := w.adm(http.MethodPost, path, ""); st != 200 || out["id"] != w.ben.String() {
		t.Fatalf("remind %d %v", st, out)
	}
	if got := w.sentTo(delivery.MFAReminder); len(got) != 1 || got[0] != w.ben || audits(t, w.h, "staff.mfa_reminded") != 1 {
		t.Fatalf("reminder %v", got)
	}
	if st, out := w.adm(http.MethodPost, path, "{}"); st != 429 || out["code"] != "rate_limited" {
		t.Fatalf("twice a day %d %v", st, out)
	}
	if w.codesChecked != 0 {
		t.Error("a reminder asked for a code")
	}
	noLeaks(t, w, nil)
}

// With no sender, every action that must send something fails closed and
// changes nothing; removal sends nothing and still works.
func TestNoSenderFailsClosed(t *testing.T) {
	w := newWorld(t)
	d := w.deps
	d.Sender = nil
	w.serve(d)
	q := query(t, w.h, w.ada, "completed", "PASS", "")
	c := correction(t, w.h, w.ada, q)
	for _, rq := range []struct{ method, path, body string }{
		{http.MethodPost, "/hospital/staff/" + w.ada.String() + "/reset-mfa", code},
		{http.MethodPost, "/hospital/staff/" + w.ben.String() + "/remind", ""},
		{http.MethodPost, "/hospital/corrections/" + c.String() + "/remind", ""},
		{http.MethodPost, "/hospital/corrections/" + c.String() + "/assign", `{"staff_id": "` + w.ben.String() + `"}`},
	} {
		if st, out := w.adm(rq.method, rq.path, rq.body); st != 503 || out["code"] != "no_delivery" {
			t.Errorf("%s %s: %d %v", rq.method, rq.path, st, out)
		}
	}
	if w.codesChecked != 0 || scalar(t, `SELECT count(*) FROM user_mfa WHERE user_id = $1`, w.ada) != 1 ||
		scalar(t, `SELECT count(*) FROM account_messages WHERE hospital_id = $1`, w.h) != 0 ||
		scalar(t, `SELECT count(*) FROM feedback_events WHERE id = $1 AND assigned_to IS NOT NULL`, c) != 0 {
		t.Fatal("an action without a sender changed something")
	}
	// Clearing an assignment sends nothing.
	if st, _ := w.adm(http.MethodPost, "/hospital/corrections/"+c.String()+"/assign", `{"staff_id": null}`); st != 204 {
		t.Errorf("clear %d", st)
	}
	if st, _ := w.adm(http.MethodDelete, "/hospital/staff/"+w.ben.String(), code); st != 204 {
		t.Errorf("remove without a sender %d", st)
	}
}

// A sender that fails after commit: the action stands, the admin is told.
func TestSendFailureIsReported(t *testing.T) {
	w := newWorld(t)
	w.sendErr = errors.New("synthetic send failure")
	if st, out := w.adm(http.MethodPost, "/hospital/staff/"+w.ben.String()+"/remind", ""); st != 503 || out["code"] != "no_delivery" {
		t.Fatalf("%d %v", st, out)
	}
	if !strings.Contains(w.logs.String(), "not handed over") {
		t.Error("the failure isn't logged")
	}
}

func TestAssignReviewer(t *testing.T) {
	w := newWorld(t)
	q := query(t, w.h, w.ada, "completed", "PASS", "")
	c := correction(t, w.h, w.ada, q)
	qo := query(t, w.h2, w.other, "completed", "PASS", "")
	co := correction(t, w.h2, w.other, qo)
	path := "/hospital/corrections/" + c.String() + "/assign"
	for _, bad := range []struct {
		body, code string
		status     int
	}{
		{`{}`, "validation_failed", 422},
		{`{"staff_id": "nope"}`, "validation_failed", 422},
		{`{"staff_id": "` + w.ada.String() + `"}`, "not_eligible", 422},   // the submitter
		{`{"staff_id": "` + w.admin.String() + `"}`, "not_eligible", 422}, // not a clinician
		{`{"staff_id": "` + w.other.String() + `"}`, "not_eligible", 422}, // another hospital's clinician
		{`{"staff_id": "` + uuid.NewString() + `"}`, "not_eligible", 422},
	} {
		if st, out := w.adm(http.MethodPost, path, bad.body); st != bad.status || out["code"] != bad.code {
			t.Errorf("%s: %d %v", bad.body, st, out)
		}
	}
	if st, _ := w.adm(http.MethodPost, "/hospital/corrections/"+co.String()+"/assign", `{"staff_id": "`+w.other.String()+`"}`); st != 404 {
		t.Errorf("other hospital's correction %d", st)
	}
	if st, _ := w.adm(http.MethodPost, path, `{"staff_id": "`+w.ben.String()+`"}`); st != 204 {
		t.Fatalf("assign %d", st)
	}
	if got := w.sentTo(delivery.ReviewAssigned); len(got) != 1 || got[0] != w.ben {
		t.Fatalf("notice %v", got)
	}
	// The same again changes and sends nothing.
	if st, _ := w.adm(http.MethodPost, path, `{"staff_id": "`+w.ben.String()+`"}`); st != 204 || len(w.sentTo(delivery.ReviewAssigned)) != 1 ||
		audits(t, w.h, "correction.reviewer_assigned") != 1 {
		t.Fatalf("repeat %d", st)
	}
	_, raw := w.as(w.h, w.admin, http.MethodGet, "/hospital/review-oversight", "")
	if !strings.Contains(string(raw), `"assigned_to":{"display_name":"Synth Ben`) {
		t.Errorf("oversight %s", raw)
	}
	if st, _ := w.adm(http.MethodPost, path, `{"staff_id": null}`); st != 204 ||
		scalar(t, `SELECT count(*) FROM feedback_events WHERE id = $1 AND assigned_to IS NULL`, c) != 1 {
		t.Fatalf("clear %d", st)
	}
	decide(t, c, w.ben)
	if st, out := w.adm(http.MethodPost, path, `{"staff_id": "`+w.ben.String()+`"}`); st != 409 || out["code"] != "already_decided" {
		t.Errorf("decided %d %v", st, out)
	}
	noLeaks(t, w, raw)
}

// Migration 00049's trigger: the assignment rules hold in the database,
// whatever core does.
func TestAssignmentTrigger(t *testing.T) {
	w := newWorld(t)
	q := query(t, w.h, w.ada, "completed", "PASS", "")
	c := correction(t, w.h, w.ada, q)
	as := func(u uuid.UUID, to any) error {
		ctx := context.Background()
		tx, err := sharedSuper.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.user_id', $1, true)`, u.String()); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, `UPDATE feedback_events SET assigned_to = $2 WHERE id = $1`, c, to)
		return err
	}
	if err := as(w.ben, w.ben); err == nil {
		t.Error("a clinician assigned a reviewer")
	}
	if err := as(w.admin, w.ada); err == nil {
		t.Error("the submitter was assigned")
	}
	if err := as(w.admin, w.other); err == nil {
		t.Error("another hospital's clinician was assigned")
	}
	if err := as(w.other, w.ben); err == nil {
		t.Error("another hospital's admin assigned")
	}
	if err := as(w.admin, w.ben); err != nil {
		t.Errorf("an admin's assignment: %v", err)
	}
	if err := as(w.admin, nil); err == nil {
		t.Error("a stay that changes nothing (no assignee to clear) was allowed")
	}
	assignAs(t, c, w.admin, w.ben)
	if err := as(w.admin, nil); err != nil {
		t.Errorf("clearing: %v", err)
	}
	if err := as(w.admin2, w.ben); err == nil {
		t.Error("the same assignee again is not a transition")
	}
	decide(t, c, w.ben)
	if err := as(w.admin, nil); err == nil {
		t.Error("a decided correction's assignee moved")
	}
	// The gateway may write the assignee column, nothing new besides.
	if n := scalar(t, `SELECT count(*) FROM information_schema.column_privileges WHERE grantee = 'mhai_gateway'
		AND table_name = 'feedback_events' AND privilege_type = 'UPDATE' AND column_name = 'assigned_to'`); n != 1 {
		t.Errorf("assigned_to grant %d", n)
	}
}

func TestRemindReviewers(t *testing.T) {
	w := newWorld(t)
	carl := person(t, w.h, "Synth Carl Eze-"+w.h.String()[:6], "carl-"+w.h.String()[:6]+"@h1.example.test", true, "clinician")
	q := query(t, w.h, w.ada, "completed", "PASS", "")
	c := correction(t, w.h, w.ada, q)
	qo := query(t, w.h2, w.other, "completed", "PASS", "")
	co := correction(t, w.h2, w.other, qo)
	path := "/hospital/corrections/" + c.String() + "/remind"
	if st, _ := w.adm(http.MethodPost, "/hospital/corrections/"+co.String()+"/remind", ""); st != 404 {
		t.Fatalf("other hospital's correction %d", st)
	}
	if st, raw := w.as(w.h, w.admin, http.MethodPost, path, ""); st != 204 {
		t.Fatalf("remind %d %s", st, raw)
	}
	got := w.sentTo(delivery.ReviewReminder)
	slices.SortFunc(got, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	want := []uuid.UUID{w.ben, carl}
	slices.SortFunc(want, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	if !slices.Equal(got, want) {
		t.Fatalf("reminded %v, want every eligible reviewer %v", got, want)
	}
	if n := scalar(t, `SELECT count(*) FROM audit_log WHERE action = 'correction.reviewers_reminded' AND entity_id = $1::text
		AND payload = '{"recipients": 2}'::jsonb`, c); n != 1 {
		t.Errorf("audit %d", n)
	}
	if st, out := w.adm(http.MethodPost, path, ""); st != 429 || out["code"] != "rate_limited" {
		t.Errorf("twice an hour %d %v", st, out)
	}
	_, raw := w.as(w.h, w.admin, http.MethodGet, "/hospital/review-oversight", "")
	if strings.Contains(string(raw), `"reminded_at":null`) {
		t.Errorf("reminded_at unset: %s", raw)
	}
	// With an assignee, only they are reminded.
	exec(t, `UPDATE account_messages SET created_at = now() - interval '2 hours' WHERE subject_id = $1`, c)
	if st, _ := w.adm(http.MethodPost, "/hospital/corrections/"+c.String()+"/assign", `{"staff_id": "`+carl.String()+`"}`); st != 204 {
		t.Fatal("assign")
	}
	if st, _ := w.adm(http.MethodPost, path, ""); st != 204 || len(w.sentTo(delivery.ReviewReminder)) != 3 ||
		w.sentTo(delivery.ReviewReminder)[2] != carl {
		t.Fatalf("assignee reminder %d %v", st, w.sentTo(delivery.ReviewReminder))
	}
	// Nobody left to review: no_recipients.
	exec(t, `UPDATE account_messages SET created_at = now() - interval '2 hours' WHERE subject_id = $1`, c)
	exec(t, `UPDATE users SET is_active = false WHERE id = ANY($1)`, []uuid.UUID{w.ben, carl})
	if st, out := w.adm(http.MethodPost, path, ""); st != 422 || out["code"] != "no_recipients" {
		t.Errorf("nobody %d %v", st, out)
	}
	exec(t, `UPDATE users SET is_active = true WHERE id = $1`, w.ben)
	decide(t, c, w.ben)
	if st, out := w.adm(http.MethodPost, path, ""); st != 409 || out["code"] != "already_decided" {
		t.Errorf("decided %d %v", st, out)
	}
	noLeaks(t, w, raw)
}
