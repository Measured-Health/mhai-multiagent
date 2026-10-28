package authz

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

const testKey = "test-only-bff-service-key-0123456789abcdef"

// requestOn returns a request that passed the BFF check, on the platform
// audience or not.
func requestOn(t *testing.T, platform bool) *http.Request {
	t.Helper()
	return requestWith(t, http.MethodGet, platform)
}

func requestWith(t *testing.T, method string, platform bool) *http.Request {
	t.Helper()
	var got *http.Request
	r := httptest.NewRequest(method, "/", nil)
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
	selfService   = Rule{SelfService: true}
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
		{"platform account, self-service, clinical audience", platform(RoleAuditor), false, selfService, "forbidden"},
		{"clinical account, self-service, platform audience", clinical(RoleClinician), true, selfService, "not_platform"},
		{"platform account, self-service, platform audience", platform(RoleAuditor), true, selfService, ""},
		{"clinical account, self-service, clinical audience", clinical(RoleClinician), false, selfService, ""},
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
		"public with roles":        {"GET", "/x", Rule{Public: true, AnyRole: []string{RoleClinician}}},
		"public write":             {"POST", "/x", Rule{Public: true, Write: true}},
		"probe not public":         {"GET", "/x", Rule{Probe: true}},
		"probe post":               {"POST", "/x", Rule{Public: true, Probe: true}},
		"get write":                {"GET", "/x", Rule{Write: true}},
		"unknown role":             {"GET", "/x", Rule{AnyRole: []string{"superuser"}}},
		"platform role off":        {"GET", "/x", Rule{AnyRole: []string{RoleAuditor}}},
		"clinical role on plat":    {"GET", "/x", Rule{Platform: true, AnyRole: []string{RoleClinician}}},
		"enrol with roles":         {"POST", "/x", Rule{MFAEnrol: true, AnyRole: []string{RoleClinician}}},
		"no method":                {"", "/x", Rule{}},
		"platform post unmarked":   {"POST", "/x", Rule{Platform: true, AnyRole: []string{RoleAuditor}}},
		"platform delete unmarked": {"DELETE", "/x", Rule{Platform: true}},
		"self-service off /auth":   {"GET", "/x", Rule{SelfService: true}},
		"self-service with roles":  {"GET", "/auth/x", Rule{SelfService: true, AnyRole: []string{RoleClinician}}},
		"self-service platform":    {"GET", "/auth/x", Rule{SelfService: true, Platform: true}},
		"self-service write":       {"POST", "/auth/x", Rule{SelfService: true, Write: true}},
		"enrol not self-service":   {"POST", "/auth/x", Rule{MFAEnrol: true}},
		"relative pattern":         {"GET", "x", Rule{}},
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
	m := NewMux(&Guard{Idem: passIdem, IdemAnon: passIdem})
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

// I1: a route that is neither Platform nor SelfService is clinical. A
// platform account is refused there on either audience, even with no
// AnyRole, so a clinical "any signed-in" route never runs as mhai_platform.
func TestPlatformRefusedOnClinicalAnyRoute(t *testing.T) {
	for _, aud := range []bool{true, false} {
		for _, role := range []string{RolePlatformAdmin, RoleAuditor} {
			if got := check(requestOn(t, aud), Rule{}, platform(role)); got != "forbidden" {
				t.Errorf("%s on Rule{} (platform audience %v): %q", role, aud, got)
			}
		}
	}
	if got := check(requestOn(t, false), Rule{}, clinical(RoleAnalyst)); got != "" {
		t.Errorf("clinical on Rule{}: %q", got)
	}
}

// I2: auditors are refused every unsafe method unless the route is self-
// service, whether or not the route remembered Write.
func TestAuditorUnsafeMethodDenied(t *testing.T) {
	unmarked := Rule{Platform: true, AnyRole: []string{RolePlatformAdmin, RoleAuditor}}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if got := check(requestWith(t, m, true), unmarked, platform(RoleAuditor)); got != "forbidden" {
			t.Errorf("auditor %s unmarked: %q", m, got)
		}
		if got := check(requestWith(t, m, true), unmarked, platform(RolePlatformAdmin)); got != "" {
			t.Errorf("platform admin %s unmarked: %q", m, got)
		}
		if got := check(requestWith(t, m, true), selfService, platform(RoleAuditor)); got != "" {
			t.Errorf("auditor %s self-service: %q", m, got)
		}
	}
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if got := check(requestWith(t, m, true), unmarked, platform(RoleAuditor)); got != "" {
			t.Errorf("auditor %s: %q", m, got)
		}
	}
}

// M2: with a valid token, a denylist that can't be read fails closed with
// 503 and the handler never runs.
func TestGuardFailsClosedWithoutDenylist(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	iss := token.Issuer{Key: priv, KeyID: "k"}
	at, _, err := iss.Access(token.Claims{Sub: uuid.NewString(), Hid: uuid.NewString(), Roles: []string{RoleClinician},
		Sid: uuid.NewString(), Jti: uuid.NewString(), AMR: []string{"pwd"}})
	if err != nil {
		t.Fatal(err)
	}
	m := NewMux(&Guard{Keys: map[string]ed25519.PublicKey{"k": pub}, Idem: passIdem, IdemAnon: passIdem})
	ran := false
	Route(m, http.MethodGet, "/x", Rule{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true }))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
	r.Header.Set("X-MHAI-Service", testKey)
	r.Header.Set("Authorization", "Bearer "+at)
	rec := httptest.NewRecorder()
	m.Handler([]string{testKey}).ServeHTTP(rec, r)
	if ran || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ran=%v status %d", ran, rec.Code)
	}
}

// passIdem stands in for the idempotency middleware in unit tests.
func passIdem(h http.Handler) http.Handler { return h }

// M9: a guard without its idempotency middleware fails at start-up, rather
// than serving with idempotency silently off.
func TestNewMuxRefusesGuardWithoutIdempotency(t *testing.T) {
	for name, g := range map[string]*Guard{
		"both nil":      {},
		"idem nil":      {IdemAnon: passIdem},
		"idem anon nil": {Idem: passIdem},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: NewMux did not panic", name)
				}
			}()
			NewMux(g)
		}()
	}
	NewMux(&Guard{Idem: passIdem, IdemAnon: passIdem})
	NewMux(nil) // table only
}
