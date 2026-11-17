package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
)

const testDemoKey = "demo-test-mfa-key-0123456789abcdef0123456789"

func demoEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDemoCmdArgs(t *testing.T) {
	env := demoEnv(map[string]string{demoSeedEnv: "on", "APP_ENV": "demo", "MFA_ENCRYPTION_KEY": testDemoKey})
	for _, args := range [][]string{nil, {}, {"seeds"}, {"seed", "extra"}, {"--help"}} {
		if err := demoCmd(context.Background(), args, "", env, &bytes.Buffer{}); !errors.Is(err, errDemoArgs) {
			t.Errorf("%q: %v, want usage", args, err)
		}
	}
}

// The guards before any connection: the env switch exactly "on", and a key.
func TestDemoCmdRefusesBeforeConnecting(t *testing.T) {
	for _, v := range []string{"", "On", "on ", "true", "1", "yes"} {
		err := demoCmd(context.Background(), []string{"seed"}, "postgres://unused", demoEnv(map[string]string{
			demoSeedEnv: v, "APP_ENV": "demo", "MFA_ENCRYPTION_KEY": testDemoKey}), &bytes.Buffer{})
		if !errors.Is(err, errDemoOff) {
			t.Errorf("MHAI_DEMO_SEED=%q: %v, want refused", v, err)
		}
	}
	// The flag alone isn't enough: APP_ENV must be the demo (unset is production).
	for _, v := range []string{"", "production", "development", "Demo", "demo "} {
		err := demoCmd(context.Background(), []string{"seed"}, "postgres://unused", demoEnv(map[string]string{
			demoSeedEnv: "on", "APP_ENV": v, "MFA_ENCRYPTION_KEY": testDemoKey}), &bytes.Buffer{})
		if !errors.Is(err, errDemoOff) {
			t.Errorf("APP_ENV=%q: %v, want refused", v, err)
		}
	}
	for _, k := range []string{"", "short-key"} {
		err := demoCmd(context.Background(), []string{"seed"}, "postgres://unused", demoEnv(map[string]string{
			demoSeedEnv: "on", "APP_ENV": "demo", "MFA_ENCRYPTION_KEY": k}), &bytes.Buffer{})
		if !errors.Is(err, errDemoKey) {
			t.Errorf("key %q: %v, want errDemoKey", k, err)
		}
	}
	// No DATABASE_URL: the owner connection is required.
	err := demoCmd(context.Background(), []string{"seed"}, "", demoEnv(map[string]string{
		demoSeedEnv: "on", "APP_ENV": "demo", "MFA_ENCRYPTION_KEY": testDemoKey}), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("no url: %v", err)
	}
}

// The published values are what the web helper and the docs use, and pass
// core's own checks.
func TestDemoContract(t *testing.T) {
	if _, ok := account.NormaliseShortCode(DemoHospitalCode); !ok {
		t.Fatal("bad short code")
	}
	b, err := auth.DecodeTOTPSecret(DemoTOTPSecret)
	if err != nil || len(b) != 20 {
		t.Fatalf("demo secret: %d bytes, %v", len(b), err)
	}
	if err := password.Preload(); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, a := range DemoAccounts {
		if seen[a.Email] || seen[a.Key] {
			t.Fatalf("duplicate %s", a.Key)
		}
		seen[a.Email], seen[a.Key] = true, true
		if err := password.Check(DemoPassword, a.Email); err != nil {
			t.Errorf("%s: demo password refused: %v", a.Key, err)
		}
		if slices.Contains(a.Roles, "analyst") {
			t.Errorf("%s: analyst goes through the grant", a.Key)
		}
		if err := account.ValidateNewUser(account.NewUser{Email: a.Email, Name: a.Display, Roles: a.Roles,
			HospitalShortCode: map[bool]string{false: DemoHospitalCode, true: ""}[a.Platform]}); err != nil {
			t.Errorf("%s: %v", a.Key, err)
		}
		if !a.Platform && !strings.HasSuffix(a.Email, demoDomain) {
			t.Errorf("%s: not on the demo domain", a.Key)
		}
		// Every two-step role enrols, or signs in for the first time (and
		// enrols then): a sample admin can never be left without a way in.
		if a.MFA == mfaNone && !a.MustChange && slices.ContainsFunc(a.Roles, func(r string) bool {
			return r == "hospital_admin" || r == "platform_admin"
		}) {
			t.Errorf("%s: admin with no two-step", a.Key)
		}
	}
	if len(DemoAccounts) != 9 {
		t.Fatalf("%d accounts", len(DemoAccounts))
	}
	if demoAccountByKey("platformAdmin").MFA != mfaRandom || demoAccountByKey("locked").Password != pwRandom {
		t.Fatal("the platform account and the locked one must be unusable")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("unknown key: no panic")
		}
	}()
	demoAccountByKey("nobody")
}

func TestDemoContentShapes(t *testing.T) {
	ids := map[string]bool{}
	for _, g := range demoGuidelines {
		ids[g.id] = true
		if !strings.Contains(guidelineMeta(g.title), `"source":"`+demoSourceName+`"`) {
			t.Fatalf("meta %s", guidelineMeta(g.title))
		}
	}
	decisions := map[string]int{}
	for _, c := range demoConsultations {
		// The citation is recorded (citations_json), never written in the
		// text: the app never shows chunk ids.
		if !ids[c.chunk] || strings.Contains(c.answer, "KB#") {
			t.Errorf("%s cites %s", c.key, c.chunk)
		}
		if !urgencyOK(c.urgency) {
			t.Errorf("%s urgency %s", c.key, c.urgency)
		}
		decisions[c.decision]++
	}
	if decisions["PASS"] == 0 || decisions["REGENERATE"] == 0 || decisions["ESCALATE"] == 0 {
		t.Fatalf("decisions %v", decisions)
	}
	if deref(nil) != "" || nullStr("") != nil || nullStr("x") != "x" {
		t.Fatal("helpers")
	}
}

func TestDemoErrNamesCodeOnly(t *testing.T) {
	err := demoErr(&pgconn.PgError{Code: "23514", ConstraintName: "c", Message: "value secret-ish", Detail: "Key (email)=(x)"})
	if !errors.Is(err, errDemoWrite) || !strings.Contains(err.Error(), "23514 c") || strings.Contains(err.Error(), "secret") ||
		strings.Contains(err.Error(), "email") {
		t.Fatalf("%v", err)
	}
	if err := demoErr(errors.New("dial tcp 10.0.0.1")); err != errDemoWrite {
		t.Fatalf("%v", err)
	}
	for _, bad := range []string{"", "not base32!", "MHAIDEMO"} {
		if _, err := auth.DecodeTOTPSecret(bad); err == nil {
			t.Errorf("%q decoded", bad)
		}
	}
	a, err1 := auth.RandomTOTPSecret()
	b, err2 := auth.RandomTOTPSecret()
	if err1 != nil || err2 != nil || len(a) != 20 || bytes.Equal(a, b) {
		t.Fatal("random secrets")
	}
}

func urgencyOK(u string) bool { return u == "routine" || u == "urgent" || u == "emergency" }
