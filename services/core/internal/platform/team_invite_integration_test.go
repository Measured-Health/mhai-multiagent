//go:build integration

package platform_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// Platform team invites (mhai-web Plan 7b; migration 00062): the hospital
// admin's invite mechanics for a Measured Health staff member. Every name
// and email is synthetic.

func inviteBody(name, email, role, code string) map[string]any {
	m := map[string]any{"name": name, "email": email, "role": role}
	if code != "" {
		m["backup_code"] = code
	}
	return m
}

func invite(t *testing.T, h http.Handler, tok string, v any) resp {
	return send(t, h, http.MethodPost, "/platform/team/invites", tok, jsonBody(t, v))
}

func staffEmail() string { return "synthetic.staff." + uniq() + "@measured.example" }

// redeem sets the invited person's first password with the link's token,
// and returns the status and the reply's body.
func redeem(t *testing.T, link string) (int, string) {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	body := jsonBody(t, map[string]any{"token": u.Query().Get("token"), "new_password": "Synthetic-Harbour-Willow-8213!"})
	req := httptest.NewRequest(http.MethodPost, httpx.BasePath+"/auth/password/reset", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MHAI-Service", serviceKey)
	req.Header.Set("X-MHAI-Client-IP", "198.51.100.78")
	req.Header.Set("Idempotency-Key", "k-"+uniq())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func TestTeamInvite(t *testing.T) {
	a := platformAdmin(t)
	tok := mint(t, a)
	email := staffEmail()
	problem(t, "auditor", invite(t, handler, mint(t, auditor(t)), inviteBody("Synthetic Staff", email, "auditor", "")), 403, "forbidden")
	clin := seedUser(t, seedHospital(t, "active"), "", authz.RoleHospitalAdmin)
	problem(t, "hospital admin", invite(t, handler, mint(t, clin), inviteBody("Synthetic Staff", email, "auditor", "")), 403, "not_platform")
	problem(t, "clinical role", invite(t, handler, tok, inviteBody("Synthetic Staff", email, "clinician", "")), 422, "validation_failed")
	problem(t, "bad email", invite(t, handler, tok, inviteBody("Synthetic Staff", "not-an-email", "auditor", "")), 422, "validation_failed")
	problem(t, "no name", invite(t, handler, tok, map[string]any{"email": email, "role": "auditor"}), 422, "validation_failed")
	problem(t, "control character", invite(t, handler, tok, inviteBody("Synthetic\u0007", email, "auditor", "")), 422, "validation_failed")
	problem(t, "email in use", invite(t, handler, tok, inviteBody("Synthetic Staff", a.email, "auditor", "")), 409, "duplicate")
	problem(t, "no code", invite(t, handler, tok, inviteBody("Synthetic Staff", email, "auditor", "")), 403, "mfa_required")

	r := invite(t, handler, tok, inviteBody("  Synthetic Staff Member  ", strings.ToUpper(email), "platform_admin", backupCode(t, a)))
	created(t, "invite", r)
	sameKeys(t, "member", r.body, memberKeys...)
	id := r.body["id"].(string)
	if r.body["status"] != "invite_pending" || r.body["role"] != "platform_admin" || r.body["name"] != "Synthetic Staff Member" ||
		r.body["email"] != email || r.body["mfa_enabled"] != false || r.body["is_you"] != false || r.body["last_sign_in_at"] != nil {
		t.Errorf("member %v", r.body)
	}
	if m := teamMember(t, tok, uuid.MustParse(id)); m == nil || m["status"] != "invite_pending" {
		t.Errorf("listed %v", m)
	}
	var hid *string
	if err := super.QueryRow(context.Background(), `SELECT hospital_id::text FROM users WHERE id = $1`, id).Scan(&hid); err != nil || hid != nil {
		t.Errorf("the account has a hospital: %v %v", hid, err)
	}
	if n := count(t, `SELECT count(*) FROM account_tokens WHERE user_id = $1 AND purpose = 'invite' AND hospital_id IS NULL
		AND expires_at BETWEEN now() + interval '71 hours' AND now() + interval '73 hours'`, id); n != 1 {
		t.Errorf("invite links %d", n)
	}
	rows := platformAuditRows(t, "platform.team_invited", "user_id", id)
	if len(rows) != 1 || rows[0].actor != a.id.String() || rows[0].hospital != nil || rows[0].payload["role"] != "platform_admin" ||
		len(rows[0].payload) != 2 {
		t.Errorf("audit %+v", rows)
	}
	act := get(t, "/platform/activity", tok)
	if e := act.body["events"].([]any)[0].(map[string]any); e["action"] != "team_invited" || e["target"] != "Synthetic Staff Member" {
		t.Errorf("activity %v", e)
	}
	msgs := invites.to(id)
	if len(msgs) != 1 || msgs[0].HospitalID != uuid.Nil || msgs[0].Kind != "invite" || !strings.HasPrefix(msgs[0].Link, testAppURL+"/reset-password?token=") {
		t.Fatalf("messages %+v", msgs)
	}
	problem(t, "invited twice", invite(t, handler, tok, inviteBody("Synthetic Staff", email, "auditor", backupCode(t, a))), 409, "duplicate")

	// A pending admin is never the other admin the last-admin rule counts.
	if n := count(t, `SELECT count(*) FROM users u WHERE u.hospital_id IS NULL AND u.is_active AND u.id = $1
		AND u.invited_at IS NOT NULL AND u.last_login_at IS NULL`, id); n != 1 {
		t.Fatal("not pending")
	}

	// The link sets the first password, once, and the reply sends a
	// platform account to the platform app's sign-in (the link opens the
	// clinical app's page, whose own sign-in refuses platform accounts).
	if code, body := redeem(t, msgs[0].Link); code != http.StatusOK || body != `{"sign_in":"platform"}` {
		t.Fatalf("redeem: %d %q", code, body)
	}
	if code, _ := redeem(t, msgs[0].Link); code != http.StatusGone {
		t.Errorf("redeemed twice: %d", code)
	}
}

// With no sender nothing is invited (503 no_delivery); a sender that
// refuses leaves the member invited (424 not_delivered).
func TestTeamInviteDelivery(t *testing.T) {
	a := platformAdmin(t)
	email := staffEmail()
	problem(t, "no sender", invite(t, emrOff, mint(t, a), inviteBody("Synthetic Staff", email, "auditor", backupCode(t, a))), 503, "no_delivery")
	if n := count(t, `SELECT count(*) FROM users WHERE email = $1`, email); n != 0 {
		t.Errorf("%d accounts", n)
	}
	invites.setFailing(true)
	defer invites.setFailing(false)
	problem(t, "not delivered", invite(t, handler, mint(t, a), inviteBody("Synthetic Staff", email, "auditor", backupCode(t, a))), 424, "not_delivered")
	if n := count(t, `SELECT count(*) FROM users WHERE email = $1 AND hospital_id IS NULL`, email); n != 1 {
		t.Errorf("%d accounts after a refused send", n)
	}
	// Asking again is a duplicate: the member is already invited.
	invites.setFailing(false)
	problem(t, "again", invite(t, handler, mint(t, a), inviteBody("Synthetic Staff", email, "auditor", backupCode(t, a))), 409, "duplicate")
}

// A platform invite can't be made for a hospital account, nor a hospital
// reset link without a hospital, whatever the handler does.
func TestTeamInviteDatabaseRules(t *testing.T) {
	ctx := context.Background()
	h := seedHospital(t, "active")
	clin := seedUser(t, h, "", authz.RoleClinician)
	a := platformAdmin(t)
	if _, err := super.Exec(ctx, `INSERT INTO account_tokens (hospital_id, user_id, purpose, token_hash, created_by, expires_at)
		VALUES (NULL, $1, 'password_reset', repeat('cd', 32), $2, now() + interval '1 day')`, clin.id, a.id); err == nil {
		t.Error("a reset link with no hospital was stored")
	}
	// An invited platform account commits only with its own team_invited
	// entry of the same transaction: another account's entry won't do.
	commit := func(withEntryFor func(id uuid.UUID) uuid.UUID) error {
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_platform`); err != nil {
			t.Fatal(err)
		}
		id := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO users (id, hospital_id, email, display_name, invited_at) VALUES ($1, NULL, $2, 'S', now())`,
			id, staffEmail()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_log (ts, actor_id, action, entity, entity_id, payload, prev_hash, hash)
			VALUES (now(), $1, 'platform.team_invited', 'user', $2, '{}', $3, $4)`, a.id, withEntryFor(id).String(), randHex(), randHex()); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if err := commit(func(uuid.UUID) uuid.UUID { return uuid.New() }); err == nil {
		t.Error("an invited platform account committed with another account's entry")
	}
	if err := commit(func(id uuid.UUID) uuid.UUID { return id }); err != nil {
		t.Errorf("with its own entry: %v", err)
	}
	// A hospital account given a hospital-less invite can't redeem it.
	if _, err := super.Exec(ctx, `INSERT INTO account_tokens (hospital_id, user_id, purpose, token_hash, created_by, expires_at)
		VALUES (NULL, $1, 'invite', encode(sha256('synthetic-mismatch'::bytea), 'hex'), $2, now() + interval '1 day')`, clin.id, a.id); err != nil {
		t.Fatal(err)
	}
	if code, _ := redeem(t, "https://x.example/r?token=synthetic-mismatch"); code != http.StatusGone {
		t.Errorf("a hospital account redeemed a platform invite: %d", code)
	}
}
