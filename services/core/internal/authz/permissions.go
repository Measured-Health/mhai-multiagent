package authz

import (
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// Rule is what a route needs (contract §3, §14.1). The zero Rule is "any
// signed-in clinical account whose session meets its two-step policy".
type Rule struct {
	// Public routes need no token (sign-in, refresh, the two-step
	// challenge, health). No other field may be set with it, except Probe.
	Public bool
	// AnyRole: the principal must hold at least one of these roles. Roles
	// combine on clinical accounts. A clinical-only route lists only
	// clinician, so a hospital_admin who isn't a clinician gets 403.
	AnyRole []string
	// Platform routes need X-MHAI-Audience: platform and a platform
	// account, or 403 not_platform. Every route that is neither Platform
	// nor SelfService is clinical: it refuses a platform account (403
	// forbidden, on either audience) and a clinical account on the platform
	// audience (403 not_platform). A Platform route with an unsafe method
	// must set Write.
	Platform bool
	// NeedsMFA: the session itself must have been proven by a second
	// factor (mfa = true), whatever the account's policy.
	NeedsMFA bool
	// StepUp: the handler asks for a fresh code (auth.StepUp). Recorded in
	// the table for the generated docs and the Python checks.
	StepUp bool
	// Write: the route changes data; auditors get 403 forbidden. Auditors
	// are refused every unsafe method outside SelfService routes anyway;
	// Write makes it explicit (and Platform routes must set it).
	Write bool
	// MFAEnrol: the route also accepts a session limited to two-step
	// enrolment, of an account that hasn't turned two-step on (enrol,
	// verify, password change and logout only; SelfService required).
	// Every other authenticated route refuses it with 403 mfa_required.
	MFAEnrol bool
	// SelfService marks the account's own routes under /auth/ (logout,
	// password, two-step, sessions, terms), open to clinical and platform
	// accounts alike, auditors included.
	SelfService bool
	// Probe: a public GET served before the BFF credential check (health
	// probes only).
	Probe bool
}

// RouteRule is one row of the permission table. Pattern is the full path,
// including httpx.BasePath.
type RouteRule struct {
	Method  string
	Pattern string
	Rule    Rule
}

// Mux is the only way to serve an /api/v1 route: Route registers the
// handler behind the guard and records it in the permission table.
type Mux struct {
	guard  *Guard
	rt     *httpx.Router
	probes *http.ServeMux
	table  []RouteRule
}

// NewMux returns an empty mux whose routes are guarded by g. A nil guard
// builds a table only (cmd/permgen): its non-public routes answer 503.
func NewMux(g *Guard) *Mux {
	return &Mux{guard: g, rt: httpx.NewRouter(), probes: http.NewServeMux()}
}

// Route registers h for method and pattern (a path under /api/v1, such as
// "/auth/login") with rule, and records it in the table. It panics on an
// inconsistent rule, so a bad route fails at start-up and in every test.
func Route(m *Mux, method, pattern string, rule Rule, h http.Handler) {
	if err := validate(method, pattern, rule); err != nil {
		panic(err)
	}
	rule.AnyRole = SortRoles(rule.AnyRole)
	m.table = append(m.table, RouteRule{Method: method, Pattern: httpx.BasePath + pattern, Rule: rule})
	if rule.Probe {
		m.probes.Handle(method+" "+httpx.BasePath+pattern, h)
		return
	}
	var guarded http.Handler
	if m.guard == nil {
		guarded = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { httpx.Error(w, r, "not_ready") })
	} else {
		guarded = m.guard.wrap(rule, h)
	}
	m.rt.Handle(method, pattern, guarded)
}

func validate(method, pattern string, rule Rule) error {
	if method == "" || !strings.HasPrefix(pattern, "/") {
		return fmt.Errorf("authz: route %q %q needs a method and a pattern starting with /", method, pattern)
	}
	if rule.Public && (len(rule.AnyRole) > 0 || rule.Platform || rule.NeedsMFA || rule.StepUp || rule.Write || rule.MFAEnrol || rule.SelfService) {
		return fmt.Errorf("authz: public route %s %s cannot carry roles or checks", method, pattern)
	}
	if rule.Probe && (!rule.Public || method != http.MethodGet) {
		return fmt.Errorf("authz: probe route %s %s must be a public GET", method, pattern)
	}
	if rule.Write && (method == http.MethodGet || method == http.MethodHead) {
		return fmt.Errorf("authz: %s %s cannot be a write", method, pattern)
	}
	if rule.SelfService && (rule.Public || len(rule.AnyRole) > 0 || rule.Platform || rule.Write ||
		!strings.HasPrefix(pattern, "/auth/")) {
		return fmt.Errorf("authz: self-service route %s %s must be an /auth/ route with no roles, platform or write", method, pattern)
	}
	if rule.MFAEnrol && (!rule.SelfService || rule.NeedsMFA) {
		return fmt.Errorf("authz: enrolment route %s %s must be a self-service route", method, pattern)
	}
	if rule.Platform && !safeMethod(method) && !rule.Write {
		return fmt.Errorf("authz: platform route %s %s changes data and must set Write", method, pattern)
	}
	for _, r := range rule.AnyRole {
		if !KnownRole(r) {
			return fmt.Errorf("authz: route %s %s names unknown role %q", method, pattern, r)
		}
		if IsPlatformRole(r) != rule.Platform {
			return fmt.Errorf("authz: route %s %s mixes platform and clinical rules (%q)", method, pattern, r)
		}
	}
	return nil
}

// Table returns the permission table, sorted by pattern then method.
func (m *Mux) Table() []RouteRule {
	out := slices.Clone(m.table)
	slices.SortFunc(out, func(a, b RouteRule) int {
		if c := strings.Compare(a.Pattern, b.Pattern); c != 0 {
			return c
		}
		return strings.Compare(a.Method, b.Method)
	})
	return out
}

// Registered returns every "METHOD /path" pattern actually on the mux,
// whoever registered it. TestEveryRouteInTable compares it with Table.
func (m *Mux) Registered() []string {
	out := m.rt.Patterns()
	for _, rr := range m.table {
		if rr.Rule.Probe {
			out = append(out, rr.Method+" "+rr.Pattern)
		}
	}
	slices.Sort(out)
	return out
}

// Handler serves the routes: probes with only a request id; everything
// else behind RequestID → BFF(serviceKeys) → body limit → guard.
func (m *Mux) Handler(serviceKeys []string) http.Handler {
	api := m.rt.Handler(serviceKeys)
	probes := httpx.RequestID(m.probes)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, p := m.probes.Handler(r); p != "" && path.Clean(r.URL.Path) == r.URL.Path {
			probes.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	})
}
