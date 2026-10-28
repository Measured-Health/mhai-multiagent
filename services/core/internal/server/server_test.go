package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

const testKey = "test-only-bff-service-key-0123456789abcdef"

// TestEveryRouteInTable walks the mux the service serves: every pattern on
// it is in the permission table and the other way round, so no route is
// served without authz.Route.
func TestEveryRouteInTable(t *testing.T) {
	m := authz.NewMux(nil)
	Register(m, Deps{})
	var table []string
	for _, rr := range m.Table() {
		table = append(table, rr.Method+" "+rr.Pattern)
	}
	slices.Sort(table)
	got := m.Registered()
	if !slices.Equal(got, table) {
		t.Fatalf("mux and table differ:\nmux   %v\ntable %v", got, table)
	}
	for _, want := range []string{"GET /api/v1/healthz", "POST /api/v1/auth/login", "GET /api/v1/auth/sessions"} {
		if !slices.Contains(table, want) {
			t.Errorf("table lacks %s", want)
		}
	}
}

// No code outside authz and httpx builds a mux or router of its own, so the
// mux walked above is the only way to serve a route.
func TestNoOtherMux(t *testing.T) {
	root := filepath.Join("..", "..")
	allowed := map[string]bool{"internal/authz": true, "internal/httpx": true}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		if allowed[filepath.ToSlash(rel)] {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok &&
					((x.Name == "http" && sel.Sel.Name == "NewServeMux") || (x.Name == "httpx" && sel.Sel.Name == "NewRouter")) {
					t.Errorf("%s: %s.%s outside authz; register routes with authz.Route", fset.Position(sel.Pos()), x.Name, sel.Sel.Name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The health probe answers without the BFF credential; every other route
// needs it.
func TestHealthzOutsideBFF(t *testing.T) {
	m := authz.NewMux(nil)
	Register(m, Deps{})
	h := m.Handler([]string{testKey})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil))
	if rec.Code != 200 || rec.Header().Get("X-Request-ID") == "" {
		t.Fatalf("healthz %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/sessions", nil))
	if rec.Code != 401 {
		t.Fatalf("sessions without BFF key: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1//healthz", nil))
	if rec.Code == 200 || rec.Code/100 == 3 {
		t.Fatalf("unclean healthz path: %d", rec.Code)
	}
}

// fixtureRequest asks for a fixture route with a valid BFF key.
func fixtureStatus(t *testing.T, path string) int {
	t.Helper()
	m := authz.NewMux(nil)
	Register(m, Deps{})
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("X-MHAI-Service", testKey)
	rec := httptest.NewRecorder()
	m.Handler([]string{testKey}).ServeHTTP(rec, r)
	return rec.Code
}

func hasFixtures() bool {
	for _, rr := range Table() {
		if strings.HasPrefix(rr.Pattern, authz.FixturePrefix) {
			return true
		}
	}
	return false
}

// Over NATS (Mux.NATSHandler) the probes don't exist: they are HTTP only,
// for the container's health check. Every other route is there, with no
// BFF key, once the request is marked by httpx.ViaNATS.
func TestNATSHandlerHasNoProbes(t *testing.T) {
	m := authz.NewMux(nil)
	Register(m, Deps{})
	h := m.NATSHandler()
	for _, p := range []string{"/api/v1/healthz", "/api/v1/readyz"} {
		r, _ := httpx.ViaNATS(httptest.NewRequest(http.MethodGet, p, nil), httpx.AudienceClinical)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"not_found"`) {
			t.Fatalf("%s over NATS: %d %s", p, rec.Code, rec.Body.String())
		}
	}
	// A route behind the guard is reached (the table-only mux answers 503
	// not_ready for it) without X-MHAI-Service.
	r, _ := httpx.ViaNATS(httptest.NewRequest(http.MethodGet, "/api/v1/auth/sessions", nil), httpx.AudienceClinical)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 503 {
		t.Fatalf("sessions over NATS: %d %s", rec.Code, rec.Body.String())
	}
	// Unmarked (not from natsapi): refused.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/sessions", nil))
	if rec.Code != 401 {
		t.Fatalf("unmarked: %d", rec.Code)
	}
}

// CORE_HTTP_API=off: the HTTP listener's handler serves the probes and
// nothing else, whatever key is sent.
func TestProbesHandlerServesOnlyProbes(t *testing.T) {
	m := authz.NewMux(nil)
	Register(m, Deps{})
	h := m.ProbesHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil))
	if rec.Code != 200 || rec.Header().Get("X-Request-ID") == "" {
		t.Fatalf("healthz %d", rec.Code)
	}
	for _, p := range []string{"/api/v1/auth/login", "/api/v1/auth/sessions", "/api/v1//healthz", "/api/docs"} {
		r := httptest.NewRequest(http.MethodPost, p, nil)
		if strings.HasSuffix(p, "healthz") || p == "/api/docs" {
			r = httptest.NewRequest(http.MethodGet, p, nil)
		}
		r.Header.Set("X-MHAI-Service", testKey)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"not_found"`) {
			t.Fatalf("%s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}
