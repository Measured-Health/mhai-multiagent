//go:build integration

package hospital_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/delivery"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/hospital"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// Invites and password resets by a hospital admin (migration 00050):
// accounts created invited, links hashed at rest, single live link per
// person and purpose, the link shown once only on development with the
// log sender, never logged, and the abuse cases.

type inviteReply struct {
	Invited []map[string]any `json:"invited"`
	Skipped []struct {
		Email  string `json:"email"`
		Reason string `json:"reason"`
	} `json:"skipped"`
}

func (w *world) invite(t *testing.T, body string) (int, inviteReply, string) {
	t.Helper()
	st, raw := w.as(w.h, w.admin, http.MethodPost, "/hospital/staff/invites", body)
	var out inviteReply
	_ = json.Unmarshal(raw, &out)
	return st, out, string(raw)
}

// tokenOf reads the plain token out of a link.
func tokenOf(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil || u.Path != "/reset-password" {
		t.Fatalf("link %q", link)
	}
	return u.Query().Get("token")
}

func (w *world) lastLink(t *testing.T, kind delivery.Kind) string {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := len(w.sent) - 1; i >= 0; i-- {
		if w.sent[i].Kind == kind {
			return w.sent[i].Link
		}
	}
	t.Fatalf("no %s sent", kind)
	return ""
}

func TestInvite(t *testing.T) {
	w := newWorld(t)
	tag := w.h.String()[:6]
	newA, newB := "Synth.New-"+tag+"@h1.example.test", "synth.second-"+tag+"@h1.example.test"
	body := `{"emails": ["` + newA + `", "` + strings.ToLower(newA) + `", "not an email", "ada-` + tag + `@h1.example.test", "` + newB + `"],
		"roles": ["clinician"], "specialty": "Synthetic cardiology", "mfa_code": "` + goodCode + `"}`

	// The body's own refusals, before the code.
	for _, bad := range []struct{ body, code string }{
		{`{"emails": [], "roles": ["clinician"], "mfa_code": "` + goodCode + `"}`, "validation_failed"},
		{`{"emails": ["a@h1.example.test"], "roles": [], "mfa_code": "` + goodCode + `"}`, "validation_failed"},
		{`{"emails": ["a@h1.example.test"], "roles": ["analyst", "clinician"], "mfa_code": "` + goodCode + `"}`, "not_eligible"},
		{`{"emails": ["a@h1.example.test"], "roles": ["platform_admin"], "mfa_code": "` + goodCode + `"}`, "validation_failed"},
		{`{"emails": ["a@h1.example.test"], "roles": ["clinician", "clinician"], "mfa_code": "` + goodCode + `"}`, "validation_failed"},
		{`{"emails": ["a@h1.example.test"], "roles": ["clinician"], "specialty": "<b>", "mfa_code": "` + goodCode + `"}`, "validation_failed"},
		{`{"emails": ["a@h1.example.test"], "roles": ["clinician"], "hospital_id": "` + w.h2.String() + `"}`, "validation_failed"},
	} {
		if st, raw := w.as(w.h, w.admin, http.MethodPost, "/hospital/staff/invites", bad.body); !strings.Contains(string(raw), `"`+bad.code+`"`) {
			t.Errorf("%s: %d %s", bad.body, st, raw)
		}
	}
	many := `{"emails": [` + strings.Repeat(`"x@h1.example.test",`, 200) + `"y@h1.example.test"], "roles": ["clinician"], "mfa_code": "` + goodCode + `"}`
	if st, _, _ := w.invite(t, many); st != 422 {
		t.Errorf("201 addresses: %d", st)
	}
	if w.codesChecked != 0 {
		t.Fatalf("a refused invite checked %d codes", w.codesChecked)
	}
	if st, _, raw := w.invite(t, strings.Replace(body, `, "mfa_code": "`+goodCode+`"`, "", 1)); st != 403 || !strings.Contains(raw, "mfa_required") {
		t.Fatalf("no code %d %s", st, raw)
	}

	st, out, raw := w.invite(t, body)
	if st != 200 || len(out.Invited) != 2 || len(out.Skipped) != 3 {
		t.Fatalf("invite %d %s", st, raw)
	}
	reasons := map[string]string{}
	for _, s := range out.Skipped {
		reasons[s.Reason] = s.Email
	}
	if reasons["duplicate"] == "" || reasons["invalid_email"] != "not an email" || reasons["already_staff"] == "" {
		t.Errorf("skipped %v", out.Skipped)
	}
	a := out.Invited[0]
	if a["status"] != "active" || a["email"] != strings.ToLower(newA) || a["invited_at"] == nil || a["invite_expires_at"] == nil ||
		a["joined_at"] != nil || a["sign_in"] != nil || a["can_review"] != false || a["link"] != nil || a["specialty"] != "Synthetic cardiology" {
		t.Errorf("invited %v", a)
	}
	if issues, _ := a["issues"].([]any); len(issues) != 0 {
		t.Errorf("issues %v", issues)
	}
	// Only the hash is stored; the link goes to the sender alone.
	id := uuid.MustParse(a["id"].(string))
	link := w.lastLink(t, delivery.Invite)
	plain := tokenOf(t, link)
	if n := scalar(t, `SELECT count(*) FROM account_tokens WHERE hospital_id = $1 AND purpose = 'invite' AND token_hash = $2
		AND expires_at BETWEEN now() + interval '2 days 23 hours' AND now() + interval '3 days'`, w.h, token.HashRefresh(plain)); n != 1 {
		t.Fatalf("stored token %d", n)
	}
	if strings.Contains(raw, plain) || strings.Contains(w.logs.String(), plain) ||
		scalar(t, `SELECT count(*) FROM audit_log WHERE payload::text LIKE '%' || $1 || '%'`, plain) != 0 {
		t.Fatal("the token leaked into the reply, the logs or the audit log")
	}
	if strings.Contains(w.logs.String(), "synth.second") || strings.Contains(w.logs.String(), "Synth.New") {
		t.Error("an address in the logs")
	}
	if audits(t, w.h, "staff.invited") != 2 || messages(t, w.h, delivery.Invite) != 2 || w.roles(t, id)[0] != "clinician" {
		t.Errorf("audit %d outbox %d roles %v", audits(t, w.h, "staff.invited"), messages(t, w.h, delivery.Invite), w.roles(t, id))
	}
	// An invitee can't be reminded about two-step, reset or reviewed yet.
	if st, _ := w.adm(http.MethodPost, "/hospital/staff/"+id.String()+"/remind", ""); st != 409 {
		t.Errorf("remind invitee %d", st)
	}
	if st, _ := w.adm(http.MethodPost, "/hospital/staff/"+id.String()+"/password-reset", code); st != 409 {
		t.Errorf("reset invitee %d", st)
	}
	q := query(t, w.h, w.ada, "completed", "PASS", "")
	c := correction(t, w.h, w.ada, q)
	if st, out := w.adm(http.MethodPost, "/hospital/corrections/"+c.String()+"/assign", `{"staff_id": "`+id.String()+`"}`); st != 422 || out["code"] != "not_eligible" {
		t.Errorf("assign invitee %d %v", st, out)
	}
	noLeaks(t, w, nil)
}

// An admin learns nothing from an invite about accounts outside their
// hospital: an address in use at another hospital gets the same reply as
// one held by a platform account (no hospital), never already_staff, and
// the other account is untouched. Their own staff still read already_staff.
// The attempts are audited (staff.invite_unavailable, a count), so probing
// is visible to Measured Health.
func TestInviteNoCrossHospitalSignal(t *testing.T) {
	w := newWorld(t)
	tag := w.h.String()[:6]
	other := strings.Replace(otherEmail, "x9", tag, 1)
	platformAddr := "synth.platform-" + tag + "@mh.example.test"
	exec(t, `INSERT INTO users (email, password_hash, display_name) VALUES ($1, 'synthetic-hash', 'Synth Platform')`, platformAddr)
	var before string
	if err := sharedSuper.QueryRow(context.Background(), `SELECT row_to_json(u)::text FROM users u WHERE id = $1`, w.other).Scan(&before); err != nil {
		t.Fatal(err)
	}
	st, out, raw := w.invite(t, `{"emails": ["`+other+`", "`+platformAddr+`", "ada-`+tag+`@h1.example.test"], "roles": ["clinician"], "mfa_code": "`+goodCode+`"}`)
	if st != 200 || len(out.Invited) != 0 || len(out.Skipped) != 3 {
		t.Fatalf("%d %s", st, raw)
	}
	reason := map[string]string{}
	for _, s := range out.Skipped {
		reason[s.Email] = s.Reason
	}
	if reason[other] == "already_staff" || reason[other] != reason[platformAddr] {
		t.Errorf("another hospital's address reads %q, a platform account's %q: they must match and not be already_staff",
			reason[other], reason[platformAddr])
	}
	if reason["ada-"+tag+"@h1.example.test"] != "already_staff" {
		t.Errorf("own staff %v", out.Skipped)
	}
	var after string
	if err := sharedSuper.QueryRow(context.Background(), `SELECT row_to_json(u)::text FROM users u WHERE id = $1`, w.other).Scan(&after); err != nil || after != before {
		t.Errorf("the other hospital's account changed: %v", err)
	}
	if n := scalar(t, `SELECT count(*) FROM account_tokens WHERE user_id = $1`, w.other); n != 0 {
		t.Errorf("%d tokens for the other hospital's account", n)
	}
	if n := scalar(t, `SELECT count(*) FROM audit_log WHERE hospital_id = $1 AND action = 'staff.invite_unavailable'
		AND payload = '{"count": 2}'::jsonb`, w.h); n != 1 {
		t.Errorf("probe audit %d", n)
	}
	noLeaks(t, w, nil)
}

// On development with the log sender the reply carries each link once; a
// resend replaces it; the hospital's daily limit holds.
func TestInviteLinkShownAndResent(t *testing.T) {
	w := newWorld(t)
	d := w.deps
	d.ShowLinks, d.AppURL = true, "https://clinical.example.test"
	w.serve(d)
	tag := w.h.String()[:6]
	st, out, raw := w.invite(t, `{"emails": ["synth.shown-`+tag+`@h1.example.test"], "roles": ["clinician", "hospital_admin"], "mfa_code": "`+goodCode+`"}`)
	if st != 200 || len(out.Invited) != 1 {
		t.Fatalf("%d %s", st, raw)
	}
	link, _ := out.Invited[0]["link"].(map[string]any)
	if link == nil || !strings.HasPrefix(link["url"].(string), "https://clinical.example.test/reset-password?token=") || link["expires_at"] == nil {
		t.Fatalf("link %v", out.Invited[0]["link"])
	}
	first := tokenOf(t, strings.TrimPrefix(link["url"].(string), "https://clinical.example.test"))
	id := out.Invited[0]["id"].(string)
	// A resend: a new link, the old one revoked.
	path := "/hospital/staff/" + id + "/resend-invite"
	if st, out := w.adm(http.MethodPost, "/hospital/staff/"+w.ada.String()+"/resend-invite", code); st != 409 || out["code"] != "conflict" {
		t.Errorf("resend to a member %d %v", st, out)
	}
	if st, _ := w.adm(http.MethodPost, "/hospital/staff/"+w.other.String()+"/resend-invite", code); st != 404 {
		t.Errorf("other hospital %d", st)
	}
	st, re := w.adm(http.MethodPost, path, code)
	rl, _ := re["link"].(map[string]any)
	if st != 200 || re["status"] != "active" || re["invited_at"] == nil || rl == nil {
		t.Fatalf("resend %d %v", st, re)
	}
	second := tokenOf(t, strings.TrimPrefix(rl["url"].(string), "https://clinical.example.test"))
	if second == first || scalar(t, `SELECT count(*) FROM account_tokens WHERE token_hash = $1 AND revoked_at IS NOT NULL`, token.HashRefresh(first)) != 1 ||
		scalar(t, `SELECT count(*) FROM account_tokens WHERE user_id = $1 AND used_at IS NULL AND revoked_at IS NULL`, uuid.MustParse(id)) != 1 {
		t.Error("the old link still works, or two are live")
	}
	if strings.Contains(w.logs.String(), first) || strings.Contains(w.logs.String(), second) {
		t.Error("a token in the logs")
	}
	// Five resends a day per person.
	for range 3 {
		if st, _ := w.adm(http.MethodPost, path, code); st != 200 {
			t.Fatalf("resend %d", st)
		}
	}
	if st, out := w.adm(http.MethodPost, path, code); st != 429 || out["code"] != "rate_limited" {
		t.Errorf("sixth invite of a person %d %v", st, out)
	}
	// An expired invite shows as such.
	exec(t, `UPDATE account_tokens SET created_at = now() - interval '8 days', expires_at = now() - interval '1 day' WHERE user_id = $1`, uuid.MustParse(id))
	_, raw2 := w.as(w.h, w.admin, http.MethodGet, "/hospital/staff", "")
	var list []map[string]any
	_ = json.Unmarshal(raw2, &list)
	for _, p := range list {
		if p["id"] == id {
			exp, _ := p["invite_expires_at"].(string)
			if p["invited_at"] == nil || exp == "" || exp >= time.Now().UTC().Format(time.RFC3339) {
				t.Errorf("expired invite: %v", p)
			}
		}
	}
	// The hospital's daily limit.
	exec(t, `INSERT INTO account_messages (hospital_id, recipient_id, kind, channel, created_by)
		SELECT $1, $2, 'invite', 'log', $2 FROM generate_series(1, 500)`, w.h, w.admin)
	if st, _, raw := w.invite(t, `{"emails": ["synth.over-`+tag+`@h1.example.test"], "roles": ["clinician"], "mfa_code": "`+goodCode+`"}`); st != 429 {
		t.Errorf("over the daily limit %d %s", st, raw)
	}
	// Removing an invitee cancels the invite (its link dies).
	if st, _ := w.adm(http.MethodDelete, "/hospital/staff/"+id, code); st != 204 ||
		scalar(t, `SELECT count(*) FROM account_tokens WHERE user_id = $1 AND used_at IS NULL AND revoked_at IS NULL`, uuid.MustParse(id)) != 0 {
		t.Errorf("remove invitee %d", st)
	}
}

func TestPasswordResetByAdmin(t *testing.T) {
	w := newWorld(t)
	exec(t, `INSERT INTO sessions (user_id) VALUES ($1)`, w.ada)
	exec(t, `INSERT INTO trusted_devices (user_id, token_hash) VALUES ($1, $2)`, w.ada, "synth-device-hash-pr-"+w.ada.String())
	path := "/hospital/staff/" + w.ada.String() + "/password-reset"
	if st, out := w.adm(http.MethodPost, "/hospital/staff/"+w.admin.String()+"/password-reset", code); st != 403 || out["code"] != "self_change" {
		t.Fatalf("self %d %v", st, out)
	}
	if st, _ := w.adm(http.MethodPost, "/hospital/staff/"+w.other.String()+"/password-reset", code); st != 404 {
		t.Fatalf("other hospital %d", st)
	}
	if w.codesChecked != 0 {
		t.Fatal("a refused reset checked a code")
	}
	st, out := w.adm(http.MethodPost, path, code)
	if st != 200 || out["link"] != nil || out["status"] != "active" {
		t.Fatalf("reset %d %v", st, out)
	}
	plain := tokenOf(t, w.lastLink(t, delivery.PasswordReset))
	if n := scalar(t, `SELECT count(*) FROM account_tokens WHERE user_id = $1 AND purpose = 'password_reset' AND token_hash = $2
		AND expires_at <= now() + interval '30 minutes'`, w.ada, token.HashRefresh(plain)); n != 1 {
		t.Fatalf("token %d", n)
	}
	if scalar(t, `SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, w.ada) != 0 ||
		scalar(t, `SELECT count(*) FROM trusted_devices WHERE user_id = $1`, w.ada) != 0 ||
		scalar(t, `SELECT count(*) FROM users WHERE id = $1 AND password_hash IS NOT NULL`, w.ada) != 1 {
		t.Fatal("sessions or devices kept, or the password dropped")
	}
	if audits(t, w.h, "staff.password_reset_sent") != 1 || strings.Contains(w.logs.String(), plain) {
		t.Fatal("audit missing or token logged")
	}
	for range 2 {
		if st, _ := w.adm(http.MethodPost, path, code); st != 200 {
			t.Fatalf("again %d", st)
		}
	}
	if st, out := w.adm(http.MethodPost, path, code); st != 429 || out["code"] != "rate_limited" {
		t.Errorf("fourth in an hour %d %v", st, out)
	}
	// Deactivation kills the live link.
	if st, _ := w.adm(http.MethodPost, "/hospital/staff/"+w.ada.String()+"/deactivate", code); st != 200 ||
		scalar(t, `SELECT count(*) FROM account_tokens WHERE user_id = $1 AND used_at IS NULL AND revoked_at IS NULL`, w.ada) != 0 {
		t.Errorf("deactivate %d", st)
	}
	if st, _ := w.adm(http.MethodPost, path, code); st != 409 {
		t.Errorf("reset a deactivated person %d", st)
	}
	// No sender: fails closed, before the code and before anything changes.
	d := w.deps
	d.Sender = nil
	w.serve(d)
	before := w.codesChecked
	for _, p := range []string{"/hospital/staff/" + w.ben.String() + "/password-reset"} {
		if st, out := w.adm(http.MethodPost, p, code); st != 503 || out["code"] != "no_delivery" {
			t.Errorf("%s %d %v", p, st, out)
		}
	}
	if st, _, raw := w.invite(t, `{"emails": ["synth.nosend@h1.example.test"], "roles": ["clinician"], "mfa_code": "`+goodCode+`"}`); st != 503 {
		t.Errorf("invite without a sender %d %s", st, raw)
	}
	if w.codesChecked != before || scalar(t, `SELECT count(*) FROM users WHERE email = 'synth.nosend@h1.example.test'`) != 0 {
		t.Error("an action without a sender checked a code or changed something")
	}
}

// Resend without a sender fails closed; an invite whose message can't be
// handed over is reported as 424 not_delivered (the invite stands).
func TestInviteDeliveryFailures(t *testing.T) {
	w := newWorld(t)
	tag := w.h.String()[:6]
	st, out, raw := w.invite(t, `{"emails": ["synth.fail-`+tag+`@h1.example.test"], "roles": ["clinician"], "mfa_code": "`+goodCode+`"}`)
	if st != 200 || len(out.Invited) != 1 {
		t.Fatalf("%d %s", st, raw)
	}
	id := out.Invited[0]["id"].(string)
	w.sendErr = errSynthSend
	if st, out, raw := w.invite(t, `{"emails": ["synth.fail2-`+tag+`@h1.example.test"], "roles": ["clinician"], "mfa_code": "`+goodCode+`"}`); st != 424 ||
		len(out.Invited) != 0 || !strings.Contains(raw, `"not_delivered"`) {
		t.Errorf("send failure %d %s", st, raw)
	}
	if scalar(t, `SELECT count(*) FROM users WHERE email = $1`, "synth.fail2-"+tag+"@h1.example.test") != 1 {
		t.Error("the invite whose message failed wasn't kept")
	}
	w.sendErr = nil
	d := w.deps
	d.Sender = nil
	w.serve(d)
	if st, out := w.adm(http.MethodPost, "/hospital/staff/"+id+"/resend-invite", code); st != 503 || out["code"] != "no_delivery" {
		t.Errorf("resend without a sender %d %v", st, out)
	}
}

var errSynthSend = errors.New("synthetic send failure")

// Probing addresses held elsewhere is capped per hospital per day: past
// the cap the whole request is refused (429) and changes nothing.
func TestInviteProbingCapped(t *testing.T) {
	w := newWorld(t)
	tag := w.h.String()[:6]
	var addrs []string
	for i := range hospital.MaxUnavailablePerDay + 1 {
		a := "synth.held" + strconv.Itoa(i) + "-" + tag + "@mh.example.test"
		exec(t, `INSERT INTO users (email, password_hash, display_name) VALUES ($1, 'synthetic-hash', 'Synth Held')`, a)
		addrs = append(addrs, `"`+a+`"`)
	}
	fresh := `"synth.fresh-` + tag + `@h1.example.test"`
	first := strings.Join(addrs[:hospital.MaxUnavailablePerDay], ", ")
	if st, out, raw := w.invite(t, `{"emails": [`+first+`], "roles": ["clinician"], "mfa_code": "`+goodCode+`"}`); st != 200 ||
		len(out.Skipped) != hospital.MaxUnavailablePerDay {
		t.Fatalf("up to the cap: %d %s", st, raw)
	}
	st, _, raw := w.invite(t, `{"emails": [`+fresh+`, `+addrs[hospital.MaxUnavailablePerDay]+`], "roles": ["clinician"], "mfa_code": "`+goodCode+`"}`)
	if st != 429 || !strings.Contains(raw, "rate_limited") {
		t.Fatalf("past the cap: %d %s", st, raw)
	}
	if n := scalar(t, `SELECT count(*) FROM users WHERE email = $1`, strings.Trim(fresh, `"`)); n != 0 {
		t.Error("a refused request still invited someone")
	}
}
