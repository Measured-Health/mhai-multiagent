package audit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// The registry of closed payload schemas is written in one place: non-test
// code calls Register only inside internal/audit (schemas.go). A Register
// elsewhere could open an action's schema from any package, away from the
// review of the chain's vocabulary.
func TestOnlyAuditRegistersSchemas(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	checked, inside := 0, 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			own := strings.HasPrefix(rel, "internal/audit/")
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			checked++
			// The name the file imports the audit package under, if any.
			alias := ""
			for _, imp := range f.Imports {
				if strings.Trim(imp.Path.Value, `"`) == "github.com/Measured-Health/mhai-multiagent/services/core/internal/audit" {
					alias = "audit"
					if imp.Name != nil {
						alias = imp.Name.Name
					}
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if id, ok := x.X.(*ast.Ident); ok && alias != "" && id.Name == alias && x.Sel.Name == "Register" {
						t.Errorf("%s: audit.Register outside internal/audit", fset.Position(x.Pos()))
					}
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "Register" && own {
						inside++
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
	if checked < 50 || inside == 0 {
		t.Fatalf("checked %d files, %d registrations inside audit: the test proves nothing", checked, inside)
	}
}
