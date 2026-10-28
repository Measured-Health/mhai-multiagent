package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

func TestPrimaryRole(t *testing.T) {
	cases := map[string]string{
		"analyst,hospital_admin,clinician": "clinician",
		"analyst,hospital_admin":           "hospital_admin",
		"analyst":                          "analyst",
		"auditor":                          "auditor",
		"platform_admin":                   "platform_admin",
		"auditor,platform_admin":           "platform_admin",
	}
	for in, want := range cases {
		roles := strings.Split(in, ",")
		if got := primaryRole(roles); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
		sorted := sortRoles(roles)
		if sorted[0] != want {
			t.Errorf("%s: sorted %v", in, sorted)
		}
	}
}

func TestNeedsMFA(t *testing.T) {
	for _, c := range []struct {
		roles  string
		policy bool
		want   bool
	}{
		{"clinician", false, false},
		{"clinician", true, true},
		{"clinician,analyst", false, true},
		{"hospital_admin", false, true},
		{"platform_admin", false, true},
		{"auditor", false, true},
	} {
		if got := needsMFA(strings.Split(c.roles, ","), c.policy); got != c.want {
			t.Errorf("%s/%v: %v", c.roles, c.policy, got)
		}
	}
}

func TestIPPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.45":       "203.0.113.0/24",
		"2001:db8:1:2::1":    "2001:db8:1::/48",
		"::ffff:203.0.113.9": "203.0.113.0/24",
		"":                   "",
	} {
		if got := ipPrefix(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

// m3: counter keys are HMAC-SHA256 under a key derived from the MFA key
// with its own label: stable for one server key, different under another,
// never the bare SHA-256 of the input, and never holding the email.
func TestLockoutKey(t *testing.T) {
	s := &Service{MFAKey: []byte("test-only-mfa-key-0123456789abcdef0123456789")}
	other := &Service{MFAKey: []byte("another-test-mfa-key-0123456789abcdef0123")}
	key := func(s *Service, email, ip string) string {
		t.Helper()
		k, err := s.LockoutKey(email, ip)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	a := key(s, " Grace.Nwosu@Havana.example ", "198.51.100.1")
	b := key(s, "grace.nwosu@havana.example", "198.51.100.1")
	c := key(s, "grace.nwosu@havana.example", "198.51.100.2")
	d := key(other, "grace.nwosu@havana.example", "198.51.100.1")
	if a != b || a == c || a == d || len(a) != 64 || strings.Contains(a, "havana") {
		t.Fatalf("keys %s %s %s %s", a, b, c, d)
	}
	bare := sha256.Sum256([]byte("grace.nwosu@havana.example|198.51.100.1"))
	if a == hex.EncodeToString(bare[:]) {
		t.Fatal("lockout key is a bare sha256")
	}
	ceil, err := s.CeilingKey("Grace.Nwosu@Havana.example")
	if err != nil || ceil == a || len(ceil) != 64 {
		t.Fatalf("ceiling key %s %v", ceil, err)
	}
	mfa, err := s.mfaFailKey("6f1c2c1e-0000-4000-8000-000000000001")
	if err != nil || strings.Contains(mfa, "6f1c2c1e") || len(mfa) != len("mfafail.")+64 {
		t.Fatalf("mfa key %s %v", mfa, err)
	}
	// Without a usable server key, there are no counter keys (fail closed).
	if _, err := (&Service{MFAKey: []byte("short")}).LockoutKey("a@b.example", "198.51.100.1"); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestSessionSatisfiesMFA(t *testing.T) {
	open := mfaPolicy{}
	strictClin := mfaPolicy{RequireForClinicians: true}
	trust := mfaPolicy{RequireForClinicians: true, AllowTrustDevice: true}
	for _, c := range []struct {
		name  string
		roles string
		p     mfaPolicy
		mfa   bool
		amr   string
		want  bool
	}{
		{"clinician, no policy, pwd", "clinician", open, false, "pwd", true},
		{"clinician, policy, pwd", "clinician", strictClin, false, "pwd", false},
		{"clinician, policy, otp", "clinician", strictClin, true, "pwd,otp", true},
		{"clinician, device, trust on", "clinician", trust, true, "pwd,device", true},
		{"clinician, device, trust off", "clinician", strictClin, true, "pwd,device", false},
		{"admin, device", "clinician,hospital_admin", trust, true, "pwd,device", false},
		{"analyst, device", "analyst", trust, true, "pwd,device", false},
		{"admin, otp", "hospital_admin", open, true, "pwd,otp", true},
		{"admin, pwd only", "hospital_admin", open, false, "pwd", false},
		{"mfa flag without proof", "hospital_admin", open, true, "pwd", false},
		{"auditor, backup", "auditor", open, true, "pwd,backup", true},
	} {
		if got := sessionSatisfiesMFA(strings.Split(c.roles, ","), c.p, false, c.mfa, strings.Split(c.amr, ",")); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestDenylistOutlivesAccessToken(t *testing.T) {
	if bus.DenylistTTL <= token.AccessTTL {
		t.Fatalf("denylist TTL %v must exceed the access token life %v", bus.DenylistTTL, token.AccessTTL)
	}
}

// I1: the ceiling lets a right password through only with a trusted
// device that may stand in for the code, on the account's own audience.
func TestCeilingEscape(t *testing.T) {
	trust := func(roles ...string) *acctRec {
		return &acctRec{Roles: roles, AllowTrustDevice: true, RequireMFAClinicians: true, MFAEnabled: true}
	}
	strict := &acctRec{Roles: []string{"clinician"}, RequireMFAClinicians: true, MFAEnabled: true}
	for _, c := range []struct {
		name              string
		a                 *acctRec
		trusted, platform bool
		want              bool
	}{
		{"clinician, device", trust("clinician"), true, false, true},
		{"clinician, no device", trust("clinician"), false, false, false},
		{"clinician, platform audience", trust("clinician"), true, true, false},
		{"hospital admin", trust("clinician", "hospital_admin"), true, false, false},
		{"analyst", trust("analyst"), true, false, false},
		{"platform admin", trust("platform_admin"), true, true, false},
		{"auditor", trust("auditor"), true, true, false},
		{"trusted devices off", strict, true, false, false},
	} {
		if got := ceilingEscape(c.a, c.trusted, c.platform); got != c.want {
			t.Errorf("%s: ceilingEscape = %v, want %v", c.name, got, c.want)
		}
	}
}
