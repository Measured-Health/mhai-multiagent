//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/feedback"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

var demoOn = demoEnv(map[string]string{demoSeedEnv: "on", "APP_ENV": "demo", "MFA_ENCRYPTION_KEY": testDemoKey})

func runDemo(t *testing.T, url string) string {
	t.Helper()
	var out bytes.Buffer
	if err := demoCmd(context.Background(), []string{"seed"}, url, demoOn, &out); err != nil {
		t.Fatalf("demo seed: %v", err)
	}
	for _, a := range DemoAccounts {
		if strings.Contains(out.String(), a.Email) {
			t.Fatalf("output names an account: %s", out.String())
		}
	}
	if strings.Contains(out.String(), DemoPassword) || strings.Contains(out.String(), DemoTOTPSecret) {
		t.Fatal("output holds a demo credential")
	}
	return out.String()
}

func demoUser(t *testing.T, super *pgxpool.Pool, key string) (id uuid.UUID, hash string, must bool) {
	t.Helper()
	if err := super.QueryRow(context.Background(), `SELECT id, password_hash, must_change_password FROM users WHERE email = $1`,
		demoAccountByKey(key).Email).Scan(&id, &hash, &must); err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return id, hash, must
}

func demoSecretOf(t *testing.T, super *pgxpool.Pool, id uuid.UUID) []byte {
	t.Helper()
	var sealed []byte
	var enabled bool
	if err := super.QueryRow(context.Background(), `SELECT secret_enc, enabled FROM user_mfa WHERE user_id = $1`, id).
		Scan(&sealed, &enabled); err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("two-step not enabled")
	}
	sealer, err := auth.NewTOTPSealer([]byte(testDemoKey))
	if err != nil {
		t.Fatal(err)
	}
	secret, err := sealer.Open(id, sealed)
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func TestDemoSeedIsIdempotentAndResets(t *testing.T) {
	pg, super := testutil.MigratedPostgres(t)
	ctx := context.Background()
	runDemo(t, pg.URL)

	var hid uuid.UUID
	var demo bool
	var status string
	if err := super.QueryRow(ctx, `SELECT id, is_demo, status FROM hospitals WHERE short_code = 'DEM'`).Scan(&hid, &demo, &status); err != nil {
		t.Fatal(err)
	}
	if !demo || status != "active" {
		t.Fatalf("DEM demo=%v status=%s", demo, status)
	}
	counts := func() map[string]int {
		m := map[string]int{}
		for name, q := range map[string]string{
			"users":       `SELECT count(*) FROM users WHERE email LIKE '%@demo-hospital.example' OR email = 'platform.bakare@measured-health.example'`,
			"queries":     `SELECT count(*) FROM clinical_queries WHERE hospital_id = '` + hid.String() + `' AND status = 'completed'`,
			"decisions":   `SELECT count(*) FROM clinical_decisions WHERE hospital_id = '` + hid.String() + `' AND cited_sources_json IS NOT NULL`,
			"pending":     `SELECT count(*) FROM feedback_events WHERE hospital_id = '` + hid.String() + `' AND status = 'pending_review'`,
			"approved":    `SELECT count(*) FROM feedback_events WHERE hospital_id = '` + hid.String() + `' AND status = 'approved'`,
			"guidelines":  `SELECT count(*) FROM kb_chunks WHERE hospital_id IS NULL AND chunk_id LIKE 'KB#000_' AND metadata_json ? 'title'`,
			"analysts":    `SELECT count(*) FROM user_roles WHERE role = 'analyst'`,
			"switches":    `SELECT count(*) FROM analysis_switch WHERE position = 'on' AND signoff_clinical = 'DEMO-SYNTHETIC'`,
			"seededAudit": `SELECT count(*) FROM audit_log WHERE action = 'demo.seeded'`,
			"hospitals":   `SELECT count(*) FROM hospitals`,
		} {
			m[name] = scalar(t, super, q)
		}
		return m
	}
	first := counts()
	want := map[string]int{"users": 9, "queries": 5, "decisions": 5, "pending": 1, "approved": 1, "guidelines": 6,
		"analysts": 1, "switches": 2, "seededAudit": 1, "hospitals": 1}
	for k, v := range want {
		if first[k] != v {
			t.Errorf("%s = %d, want %d", k, first[k], v)
		}
	}

	// Each account in its documented state.
	reyes, hash, must := demoUser(t, super, "clinician")
	if ok, _ := password.Verify(DemoPassword, hash); !ok || must {
		t.Fatalf("clinician: verifies=%v must=%v", ok, must)
	}
	if _, _, must := demoUser(t, super, "newAccount"); !must {
		t.Fatal("new account must change its password")
	}
	demoSecret, _ := auth.DecodeTOTPSecret(DemoTOTPSecret)
	for _, k := range []string{"clinician", "twoStep", "hospitalAdmin", "director", "analyst"} {
		id, _, _ := demoUser(t, super, k)
		if !bytes.Equal(demoSecretOf(t, super, id), demoSecret) {
			t.Fatalf("%s: not enrolled on the demo secret", k)
		}
	}
	platform, ph, _ := demoUser(t, super, "platformAdmin")
	if bytes.Equal(demoSecretOf(t, super, platform), demoSecret) {
		t.Fatal("platform account on the published secret")
	}
	// No account with a platform role ever opens with the published password.
	if ok, _ := password.Verify(DemoPassword, ph); ok {
		t.Fatal("platform account opens with the demo password")
	}
	if _, h, _ := demoUser(t, super, "locked"); func() bool { ok, _ := password.Verify(DemoPassword, h); return ok }() {
		t.Fatal("locked account opens with the demo password")
	}

	// Run again: nothing changes, nothing is audited again.
	runDemo(t, pg.URL)
	if again := counts(); again["seededAudit"] != 1 || again["queries"] != 5 || again["users"] != 9 || again["pending"] != 1 {
		t.Fatalf("second run changed things: %v", again)
	}

	// A visitor changed things: the clinician's password, a session, the
	// new account's first steps, the analyst's role, a trusted device, the
	// two-step of an admin.
	sid := uuid.New()
	if _, err := super.Exec(ctx, `INSERT INTO sessions (id, user_id, mfa, amr) VALUES ($1, $2, false, '{pwd}')`, sid, reyes); err != nil {
		t.Fatal(err)
	}
	other, _ := password.Hash("a-visitor-changed-it-77")
	if _, err := super.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, reyes, other); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `INSERT INTO trusted_devices (user_id, token_hash) VALUES ($1, 'x')`, reyes); err != nil {
		t.Fatal(err)
	}
	newID, _, _ := demoUser(t, super, "newAccount")
	if _, err := super.Exec(ctx, `UPDATE users SET must_change_password = false, terms_accepted_at = now() WHERE id = $1`, newID); err != nil {
		t.Fatal(err)
	}
	admin, _, _ := demoUser(t, super, "hospitalAdmin")
	if _, err := super.Exec(ctx, `DELETE FROM user_mfa WHERE user_id = $1`, admin); err != nil {
		t.Fatal(err)
	}
	analyst, _, _ := demoUser(t, super, "analyst")
	if _, err := super.Exec(ctx, `DELETE FROM user_roles WHERE user_id = $1 AND role = 'analyst'`, analyst); err != nil {
		t.Fatal(err)
	}
	// A visitor withdrew the waiting correction.
	var pendingID uuid.UUID
	if err := super.QueryRow(ctx, `SELECT id FROM feedback_events WHERE status = 'pending_review'`).Scan(&pendingID); err != nil {
		t.Fatal(err)
	}
	if _, err := (feedback.Store{Pool: super}).Decide(ctx, hid, reyes, pendingID, "", feedback.Decision{Kind: "withdraw"}); err != nil {
		t.Fatal(err)
	}
	// A visitor (the admin) loosened the sign-in settings and gave the
	// clinician a role.
	if _, err := super.Exec(ctx, `UPDATE hospital_settings SET require_mfa_for_clinicians = true, allow_trust_device = true
		WHERE hospital_id = $1`, hid); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'hospital_admin')`, reyes); err != nil {
		t.Fatal(err)
	}
	runDemo(t, pg.URL)
	if n := scalar(t, super, `SELECT count(*) FROM hospital_settings WHERE hospital_id = $1
		AND NOT require_mfa_for_clinicians AND NOT allow_trust_device`, hid); n != 1 {
		t.Fatal("settings not reset")
	}
	if n := scalar(t, super, `SELECT count(*) FROM user_roles WHERE user_id = $1`, reyes); n != 1 {
		t.Fatalf("clinician has %d roles", n)
	}
	var revoked bool
	if err := super.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM sessions WHERE id = $1`, sid).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("session not revoked: %v %v", revoked, err)
	}
	if _, h, _ := demoUser(t, super, "clinician"); func() bool { ok, _ := password.Verify(DemoPassword, h); return !ok }() {
		t.Fatal("password not reset")
	}
	if n := scalar(t, super, `SELECT count(*) FROM trusted_devices WHERE user_id = $1`, reyes); n != 0 {
		t.Fatal("trusted device kept")
	}
	if _, _, must := demoUser(t, super, "newAccount"); !must {
		t.Fatal("new account not reset")
	}
	if !bytes.Equal(demoSecretOf(t, super, admin), demoSecret) {
		t.Fatal("admin two-step not reset")
	}
	third := counts()
	if third["analysts"] != 1 || third["seededAudit"] != 2 || third["pending"] != 1 || third["queries"] != 5 {
		t.Fatalf("after reset: %v", third)
	}
	if ok, broken, err := audit.Verify(ctx, super); err != nil || !ok {
		t.Fatalf("audit chain: ok=%v broken=%d err=%v", ok, broken, err)
	}
	// An operator's kill switch stays off: the seed raises a switch only when
	// it has no row yet.
	if err := analysisSwitchOff(ctx, "--global", pg.URL, io.Discard); err != nil {
		t.Fatalf("switch off --global: %v", err)
	}
	runDemo(t, pg.URL)
	if n := scalar(t, super, `SELECT count(*) FROM analysis_switch WHERE hospital_id IS NULL AND position = 'on'`); n != 0 {
		t.Fatal("the seed raised the global analysis switch an operator lowered")
	}
}

// Concurrent runs (two deploys at once) serialise on the advisory lock:
// one hospital, one set of accounts and content.
func TestDemoSeedConcurrent(t *testing.T) {
	pg, super := testutil.MigratedPostgres(t)
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = demoCmd(context.Background(), []string{"seed"}, pg.URL, demoOn, &bytes.Buffer{})
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := scalar(t, super, `SELECT count(*) FROM hospitals WHERE short_code = 'DEM'`); n != 1 {
		t.Fatalf("%d DEM", n)
	}
	if n := scalar(t, super, `SELECT count(*) FROM clinical_queries`); n != 5 {
		t.Fatalf("%d consultations", n)
	}
	if n := scalar(t, super, `SELECT count(*) FROM feedback_events`); n != 2 {
		t.Fatalf("%d corrections", n)
	}
}

func TestDemoSeedRefusals(t *testing.T) {
	ctx := context.Background()
	nothing := func(t *testing.T, super *pgxpool.Pool) {
		t.Helper()
		if n := scalar(t, super, `SELECT count(*) FROM users`); n != 0 {
			t.Fatalf("%d users written", n)
		}
		if n := scalar(t, super, `SELECT count(*) FROM kb_chunks`); n != 0 {
			t.Fatalf("%d chunks written", n)
		}
	}

	t.Run("env off", func(t *testing.T) {
		pg, super := testutil.MigratedPostgres(t)
		err := demoCmd(ctx, []string{"seed"}, pg.URL, demoEnv(map[string]string{"MFA_ENCRYPTION_KEY": testDemoKey}), &bytes.Buffer{})
		if !errors.Is(err, errDemoOff) {
			t.Fatalf("%v", err)
		}
		nothing(t, super)
	})
	t.Run("a real cloud agreement", func(t *testing.T) {
		pg, super := testutil.MigratedPostgres(t)
		hid := analysisHospital(t, super, "REA")
		if _, err := super.Exec(ctx, `UPDATE hospitals SET is_demo = false WHERE id = $1`, hid); err == nil {
			t.Log("is_demo changed") // the trigger forbids it; either way a real agreement refuses
		}
		if _, err := super.Exec(ctx, `INSERT INTO hospital_model_policy (hospital_id, cloud_provider, agreement_ref)
			VALUES ($1, 'anthropic', 'AGR-2026-01')`, hid); err != nil {
			t.Fatal(err)
		}
		err := demoCmd(ctx, []string{"seed"}, pg.URL, demoOn, &bytes.Buffer{})
		if !errors.Is(err, errDemoReal) {
			t.Fatalf("%v", err)
		}
		nothing(t, super)
		if n := scalar(t, super, `SELECT count(*) FROM hospitals WHERE short_code = 'DEM'`); n != 0 {
			t.Fatal("DEM created")
		}
	})
	t.Run("a non-demo hospital with no agreement", func(t *testing.T) {
		pg, super := testutil.MigratedPostgres(t)
		if _, err := super.Exec(ctx, `INSERT INTO hospitals (name, short_code, status, time_zone) VALUES ('Synthetic Real', 'RLH', 'active', 'UTC')`); err != nil {
			t.Fatal(err)
		}
		err := demoCmd(ctx, []string{"seed"}, pg.URL, demoOn, &bytes.Buffer{})
		if !errors.Is(err, errDemoReal) {
			t.Fatalf("%v, want errDemoReal", err)
		}
		nothing(t, super)
		if n := scalar(t, super, `SELECT count(*) FROM hospitals WHERE short_code = 'DEM'`); n != 0 {
			t.Fatal("DEM created")
		}
	})
	t.Run("DEM not the demo", func(t *testing.T) {
		pg, super := testutil.MigratedPostgres(t)
		if _, err := super.Exec(ctx, `INSERT INTO hospitals (name, short_code, status, time_zone) VALUES ('Real', 'DEM', 'active', 'UTC')`); err != nil {
			t.Fatal(err)
		}
		err := demoCmd(ctx, []string{"seed"}, pg.URL, demoOn, &bytes.Buffer{})
		if !errors.Is(err, errDemoNotDemo) {
			t.Fatalf("%v", err)
		}
		nothing(t, super)
	})
	t.Run("DEM deactivated", func(t *testing.T) {
		pg, super := testutil.MigratedPostgres(t)
		runDemo(t, pg.URL)
		if _, err := super.Exec(ctx, `UPDATE hospitals SET status = 'deactivated' WHERE short_code = 'DEM'`); err != nil {
			t.Fatal(err)
		}
		if err := demoCmd(ctx, []string{"seed"}, pg.URL, demoOn, &bytes.Buffer{}); !errors.Is(err, errDemoInactive) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("not the owner", func(t *testing.T) {
		pg, super := testutil.MigratedPostgres(t)
		_, provURL, err := testutil.ProvisionLogin(ctx, pg, super)
		if err != nil {
			t.Fatal(err)
		}
		err = demoCmd(ctx, []string{"seed"}, provURL, demoOn, &bytes.Buffer{})
		if !errors.Is(err, errDemoOwner) {
			t.Fatalf("%v", err)
		}
		nothing(t, super)
	})
	t.Run("an email outside the demo hospital", func(t *testing.T) {
		pg, super := testutil.MigratedPostgres(t)
		hid := analysisHospital(t, super, "OTH")
		if _, err := super.Exec(ctx, `INSERT INTO users (hospital_id, email, display_name) VALUES ($1, $2, 'Someone')`,
			hid, demoAccountByKey("clinician").Email); err != nil {
			t.Fatal(err)
		}
		err := demoCmd(ctx, []string{"seed"}, pg.URL, demoOn, &bytes.Buffer{})
		if !errors.Is(err, errDemoConflict) {
			t.Fatalf("%v", err)
		}
		if n := scalar(t, super, `SELECT count(*) FROM clinical_queries`); n != 0 {
			t.Fatal("content written after a conflict")
		}
	})
}
