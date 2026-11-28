//go:build integration

package link

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/testpg"
)

// NT6, NT8: notify_link_recipient names the address only for an active
// user of the token's hospital with a live token of that purpose issued in
// the last 2 minutes; an older, used or revoked token, another purpose,
// another hospital or a deactivated user names nobody.
func TestLinkRecipient(t *testing.T) {
	ctx := context.Background()
	url, super := testpg.Migrated(t)
	app, err := pgxpool.New(ctx, testpg.Login(t, url, super, "notify_app", "mhai_notify_app"))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	st := NewPG(app)
	h, h2 := testpg.Hospital(t, super), testpg.Hospital(t, super)
	user := func(h uuid.UUID) uuid.UUID {
		var u uuid.UUID
		if err := super.QueryRow(ctx, `INSERT INTO users (hospital_id, email, display_name) VALUES ($1, $2, 'Synth L')
			RETURNING id`, h, "synth-link-"+uuid.NewString()[:8]+"@example.test").Scan(&u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	token := func(h, u uuid.UUID, purpose, extra string) {
		hash := uuid.NewString()[:8] + uuid.NewString()[:8] + uuid.NewString()[:8] + uuid.NewString()[:8]
		hash = hash + hash
		if _, err := super.Exec(ctx, `INSERT INTO account_tokens (hospital_id, user_id, purpose, token_hash, created_by, expires_at`+
			extra+`) VALUES ($1, $2, $3, $4, $2, now() + interval '30 minutes'`+map[bool]string{true: ", now() - interval '3 minutes'", false: ""}[extra != ""]+`)`,
			h, u, purpose, hash); err != nil {
			t.Fatal(err)
		}
	}
	fresh := user(h)
	token(h, fresh, "password_reset", "")
	addr, name, ok, err := st.Recipient(ctx, h, fresh, "password_reset")
	if err != nil || !ok || addr == "" || name == "" {
		t.Fatalf("fresh: %v %v", ok, err)
	}
	if _, _, ok, _ := st.Recipient(ctx, h, fresh, "invite"); ok {
		t.Error("another purpose named the user")
	}
	if _, _, ok, _ := st.Recipient(ctx, h2, fresh, "password_reset"); ok {
		t.Error("another hospital named the user")
	}
	old := user(h)
	token(h, old, "password_reset", ", created_at")
	if _, _, ok, _ := st.Recipient(ctx, h, old, "password_reset"); ok {
		t.Error("a token older than 2 minutes named the user")
	}
	for _, edit := range []string{`UPDATE account_tokens SET used_at = now() WHERE user_id = $1`,
		`UPDATE account_tokens SET revoked_at = now() WHERE user_id = $1`, `UPDATE users SET is_active = false WHERE id = $1`} {
		u := user(h)
		token(h, u, "invite", "")
		if _, err := super.Exec(ctx, edit, u); err != nil {
			t.Fatal(err)
		}
		if _, _, ok, _ := st.Recipient(ctx, h, u, "invite"); ok {
			t.Errorf("%s: named the user", edit)
		}
	}
	if _, _, ok, _ := st.Recipient(ctx, h, uuid.New(), "invite"); ok {
		t.Error("an unknown user was named")
	}
}

// notify_platform_link_recipient names the address only of an active
// platform account (no hospital, a platform role) with a live invite of no
// hospital issued in the last 2 minutes; a hospital account, a reset, an
// old, used or revoked invite, a deactivated account or one with no
// platform role names nobody.
func TestPlatformLinkRecipient(t *testing.T) {
	ctx := context.Background()
	url, super := testpg.Migrated(t)
	app, err := pgxpool.New(ctx, testpg.Login(t, url, super, "notify_app", "mhai_notify_app"))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	st := NewPG(app)
	staff := func(role string) uuid.UUID {
		var u uuid.UUID
		if err := super.QueryRow(ctx, `INSERT INTO users (hospital_id, email, display_name) VALUES (NULL, $1, 'Synth P')
			RETURNING id`, "synth-staff-"+uuid.NewString()[:8]+"@example.test").Scan(&u); err != nil {
			t.Fatal(err)
		}
		if role != "" {
			if _, err := super.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, u, role); err != nil {
				t.Fatal(err)
			}
		}
		return u
	}
	invite := func(u uuid.UUID, old bool) {
		hash := uuid.NewString()[:8] + uuid.NewString()[:8] + uuid.NewString()[:8] + uuid.NewString()[:8]
		hash = hash + hash
		created := "now()"
		if old {
			created = "now() - interval '3 minutes'"
		}
		if _, err := super.Exec(ctx, `INSERT INTO account_tokens (hospital_id, user_id, purpose, token_hash, created_by, expires_at, created_at)
			VALUES (NULL, $1, 'invite', $2, $1, now() + interval '1 day', `+created+`)`, u, hash); err != nil {
			t.Fatal(err)
		}
	}
	fresh := staff("auditor")
	invite(fresh, false)
	if addr, ok, err := st.PlatformRecipient(ctx, fresh, "invite"); err != nil || !ok || addr == "" {
		t.Fatalf("fresh: %v %v", ok, err)
	}
	if _, ok, _ := st.PlatformRecipient(ctx, fresh, "password_reset"); ok {
		t.Error("a reset named the account")
	}
	old := staff("platform_admin")
	invite(old, true)
	if _, ok, _ := st.PlatformRecipient(ctx, old, "invite"); ok {
		t.Error("an invite older than 2 minutes named the account")
	}
	noRole := staff("")
	invite(noRole, false)
	if _, ok, _ := st.PlatformRecipient(ctx, noRole, "invite"); ok {
		t.Error("an account with no platform role was named")
	}
	for _, edit := range []string{`UPDATE account_tokens SET used_at = now() WHERE user_id = $1`,
		`UPDATE account_tokens SET revoked_at = now() WHERE user_id = $1`, `UPDATE users SET is_active = false WHERE id = $1`} {
		u := staff("auditor")
		invite(u, false)
		if _, err := super.Exec(ctx, edit, u); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := st.PlatformRecipient(ctx, u, "invite"); ok {
			t.Errorf("%s: named the account", edit)
		}
	}
	// A hospital account with a hospital invite is never a platform
	// recipient.
	h := testpg.Hospital(t, super)
	var hu uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO users (hospital_id, email, display_name) VALUES ($1, $2, 'Synth H') RETURNING id`,
		h, "synth-h-"+uuid.NewString()[:8]+"@example.test").Scan(&hu); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `INSERT INTO account_tokens (hospital_id, user_id, purpose, token_hash, created_by, expires_at)
		VALUES ($1, $2, 'invite', repeat('ab', 32), $2, now() + interval '1 day')`, h, hu); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.PlatformRecipient(ctx, hu, "invite"); ok {
		t.Error("a hospital account was named")
	}
	// The hospital-scoped definer still names nobody without a hospital.
	if _, _, ok, _ := st.Recipient(ctx, h, fresh, "invite"); ok {
		t.Error("the hospital definer named a platform account")
	}
}
