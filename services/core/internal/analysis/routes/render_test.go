package routes

import (
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
)

// I2: nothing that writes a response imports the raw reader.
func TestRenderImportsNoCellsReader(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no files (%v)", err)
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		ast, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range ast.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if strings.HasSuffix(p, "/internal/analysis/cells") {
				t.Errorf("%s imports the raw reader", f)
			}
		}
	}
}

// The routes: hospital admins and analysts, two-step on, clinical audience
// (a platform account is refused by authz, as on every clinical route).
func TestAnalysisRolesAndMFA(t *testing.T) {
	if !Rule.NeedsMFA || Rule.Platform || Rule.Public {
		t.Errorf("rule %+v", Rule)
	}
	roles := authz.SortRoles(Rule.AnyRole)
	if !slices.Equal(roles, authz.SortRoles([]string{authz.RoleHospitalAdmin, authz.RoleAnalyst})) || slices.Contains(roles, authz.RoleClinician) {
		t.Errorf("roles %v", roles)
	}
}

// parse: one period exactly, the contract's filters, nothing else.
func TestParse(t *testing.T) {
	for q, want := range map[string]error{
		"period=month&start=2026-10-01":             nil,
		"from=2026-10-01&to=2026-10-31":             nil,
		"from=2026-10-01&to=2026-10-07":             nil,
		"from=2026-10-22&to=2026-10-31":             nil,
		"from=2026-10-01&to=2026-12-31":             nil,
		"from=2026-10-05&to=2026-10-11":             errRange,
		"period=month&start=2026-10-01&from=x":      errRange,
		"period=month":                              errRange,
		"period=year&start=2026-01-01":              errRange,
		"period=month&start=2026-10-01&sex=a&sex=b": errQuery,
		"period=month&start=2026-10-01&sex=":        errQuery,
		"period=month&start=2026-10-01&x=1":         errQuery,
	} {
		v, _ := urlValues(q)
		if _, _, _, err := parse(v, true, true); err != want {
			t.Errorf("%s: %v, want %v", q, err, want)
		}
	}
}

func urlValues(q string) (map[string][]string, error) {
	v, err := url.ParseQuery(q)
	return v, err
}
