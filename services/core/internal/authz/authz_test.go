package authz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

const testKey = "test-only-bff-service-key-0123456789abcdef"

// requestOn returns a request that passed the BFF check, on the platform
// audience or not.
func requestOn(t *testing.T, platform bool) *http.Request {
	t.Helper()
	var got *http.Request
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-MHAI-Service", testKey)
	if platform {
		r.Header.Set("X-MHAI-Audience", "platform")
	}
	httpx.BFF([]string{testKey})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r })).
		ServeHTTP(httptest.NewRecorder(), r)
	return got
}

func clinical(roles ...string) Principal {
	return Principal{UserID: uuid.New(), HospitalID: uuid.New(), Roles: SortRoles(roles), MFA: true}
}

func platform(role string) Principal {
	return Principal{UserID: uuid.New(), Roles: []string{role}, MFA: true, Platform: true}
}

var (
	clinicalOnly  = Rule{AnyRole: []string{RoleClinician}}
	adminOnly     = Rule{AnyRole: []string{RoleHospitalAdmin}}
	platformRead  = Rule{Platform: true, AnyRole: []string{RolePlatformAdmin, RoleAuditor}}
	platformWrite = Rule{Platform: true, AnyRole: []string{RolePlatformAdmin, RoleAuditor}, Write: true}
)

func TestRoleRequired(t *testing.T) {
	for _, c := range []struct {
		p    Principal
		rule Rule
		want string
	}{
		{clinical(RoleClinician), clinicalOnly, ""},
		{clinical(RoleAnalyst), clinicalOnly, "forbidden"},
		{clinical(RoleClinician), adminOnly, "forbidden"},
		{clinical(RoleHospitalAdmin), adminOnly, ""},
		{clinical(RoleClinician, RoleHospitalAdmin), clinicalOnly, ""}, // roles combine
		{clinical(RoleClinician, RoleHospitalAdmin), adminOnly, ""},
		{clinical(RoleAnalyst), Rule{}, ""}, // any signed-in account
		{clinical(), clinicalOnly, "forbidden"},
	} {
		if got := check(requestOn(t, false), c.rule, c.p); got != c.want {
			t.Errorf("roles %v rule %v: got %q want %q", c.p.Roles, c.rule.AnyRole, got, c.want)
		}
	}
}

func TestHospitalAdminNotClinician403(t *testing.T) {
	for _, roles := range [][]string{{RoleHospitalAdmin}, {RoleHospitalAdmin, RoleAnalyst}} {
		if got := check(requestOn(t, false), clinicalOnly, clinical(roles...)); got != "forbidden" {
			t.Errorf("%v on a clinical-only route: %q", roles, got)
		}
	}
}

func TestPlatformAudienceRequired(t *testing.T) {
	for _, c := range []struct {
		name     string
		p        Principal
		platform bool
		rule     Rule
		want     string
	}{
		{"platform account, platform audience", platform(RolePlatformAdmin), true, platformRead, ""},
		{"platform account, clinical audience", platform(RolePlatformAdmin), false, platformRead, "not_platform"},
		{"clinical account, platform audience", clinical(RoleClinician), true, platformRead, "not_platform"},
		{"clinical account, clinical audience", clinical(RoleClinician), false, platformRead, "not_platform"},
		{"platform account on a clinical route", platform(RolePlatformAdmin), true, clinicalOnly, "forbidden"},
		{"platform account, self-service, clinical audience", platform(RoleAuditor), false, Rule{}, "forbidden"},
		{"clinical account, self-service, platform audience", clinical(RoleClinician), true, Rule{}, "not_platform"},
		{"platform account, self-service, platform audience", platform(RoleAuditor), true, Rule{}, ""},
	} {
		if got := check(requestOn(t, c.platform), c.rule, c.p); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestAuditorWriteForbidden(t *testing.T) {
	r := requestOn(t, true)
	if got := check(r, platformWrite, platform(RoleAuditor)); got != "forbidden" {
		t.Errorf("auditor write: %q", got)
	}
	if got := check(r, platformRead, platform(RoleAuditor)); got != "" {
		t.Errorf("auditor read: %q", got)
	}
	if got := check(r, platformWrite, platform(RolePlatformAdmin)); got != "" {
		t.Errorf("platform admin write: %q", got)
	}
	// admin is read as platform_admin.
	if got := check(r, platformWrite, Principal{UserID: uuid.New(), Roles: SortRoles([]string{"admin"}), Platform: true}); got != "" {
		t.Errorf("admin alias write: %q", got)
	}
}

func TestNeedsMFARule(t *testing.T) {
	p := clinical(RoleClinician)
	p.MFA = false
	if got := check(requestOn(t, false), Rule{NeedsMFA: true}, p); got != "mfa_required" {
		t.Errorf("no mfa: %q", got)
	}
}

func TestRuleValidation(t *testing.T) {
	bad := map[string]struct {
		method, pattern string
		rule            Rule
	}{
		"public with roles":     {"GET", "/x", Rule{Public: true, AnyRole: []string{RoleClinician}}},
		"public write":          {"POST", "/x", Rule{Public: true, Write: true}},
		"probe not public":      {"GET", "/x", Rule{Probe: true}},
		"probe post":            {"POST", "/x", Rule{Public: true, Probe: true}},
		"get write":             {"GET", "/x", Rule{Write: true}},
		"unknown role":          {"GET", "/x", Rule{AnyRole: []string{"superuser"}}},
		"platform role off":     {"GET", "/x", Rule{AnyRole: []string{RoleAuditor}}},
		"clinical role on plat": {"GET", "/x", Rule{Platform: true, AnyRole: []string{RoleClinician}}},
		"enrol with roles":      {"POST", "/x", Rule{MFAEnrol: true, AnyRole: []string{RoleClinician}}},
		"no method":             {"", "/x", Rule{}},
		"relative pattern":      {"GET", "x", Rule{}},
	}
	for name, c := range bad {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			Route(NewMux(nil), c.method, c.pattern, c.rule, http.NotFoundHandler())
		}()
	}
}

func TestTxWithoutPrincipal(t *testing.T) {
	if err := Tx(context.Background(), nil, nil); !errors.Is(err, ErrNoPrincipal) {
		t.Fatalf("err %v", err)
	}
}

// Every guarded route without a token is 401, and the principal is never
// taken from a header.
func TestGuardNeedsToken(t *testing.T) {
	m := NewMux(&Guard{})
	ran := false
	Route(m, http.MethodGet, "/x", Rule{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true }))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
	r.Header.Set("X-MHAI-Service", testKey)
	r.Header.Set("X-MHAI-User", uuid.NewString())
	rec := httptest.NewRecorder()
	m.Handler([]string{testKey}).ServeHTTP(rec, r)
	if ran || rec.Code != http.StatusUnauthorized {
		t.Fatalf("ran=%v status %d", ran, rec.Code)
	}
}

func TestCacheTTLCapped(t *testing.T) {
	if (&Guard{CacheTTL: time.Hour}).ttl() != MaxCacheTTL || (&Guard{}).ttl() != MaxCacheTTL {
		t.Fatal("cache TTL not capped at 30 s")
	}
	if (&Guard{CacheTTL: 5 * time.Second}).ttl() != 5*time.Second {
		t.Fatal("shorter TTL ignored")
	}
}
