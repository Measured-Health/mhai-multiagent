package auth

import (
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

func TestLockoutKey(t *testing.T) {
	a := LockoutKey(" Grace.Nwosu@Havana.example ", "198.51.100.1")
	b := LockoutKey("grace.nwosu@havana.example", "198.51.100.1")
	c := LockoutKey("grace.nwosu@havana.example", "198.51.100.2")
	if a != b || a == c || len(a) != 64 || strings.Contains(a, "havana") {
		t.Fatalf("keys %s %s %s", a, b, c)
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
