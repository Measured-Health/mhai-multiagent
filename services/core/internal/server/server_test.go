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
