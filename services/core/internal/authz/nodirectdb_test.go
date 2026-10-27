package authz

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/Measured-Health/mhai-multiagent/services/core"

// dbTxAllowed are the directories (relative to services/core) that may open
// a transaction themselves. Everything else, handlers above all, goes
// through authz.Tx.
var dbTxAllowed = map[string]string{
	"internal/db":    "defines the transactions",
	"internal/authz": "Tx and the principal middleware's state lookup",
	"internal/auth":  "sign-in runs before a principal exists (mhai_auth)",
	// Infrastructure that can't import authz (authz imports it):
	"internal/httpx": "the idempotency store scopes its own rows by the principal it is given",
	"internal/audit": "Verify reads the whole chain as mhai_platform",
	// Provisioning CLI on the owner/migrate connection (SET LOCAL ROLE
	// mhai_provision), never on a request path.
	"internal/account": "the users/hospitals create CLI",
}

// TestNoDirectDBTx walks every non-test Go file of the module and fails on
// a call to db.WithHospital, db.WithPlatform or db.WithAuth, or a raw
// Begin/BeginTx, outside the allowed packages.
func TestNoDirectDBTx(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || strings.HasPrefix(name, ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		rel = filepath.ToSlash(rel)
		if _, ok := dbTxAllowed[rel]; ok {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		checked++
		dbName := ""
		for _, im := range f.Imports {
			if p, _ := strconv.Unquote(im.Path.Value); p == modulePath+"/internal/db" {
				dbName = "db"
				if im.Name != nil {
					dbName = im.Name.Name
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && dbName != "" && x.Name == dbName && strings.HasPrefix(sel.Sel.Name, "With") {
				t.Errorf("%s: direct %s.%s; use authz.Tx", fset.Position(sel.Pos()), dbName, sel.Sel.Name)
			}
			if sel.Sel.Name == "Begin" || sel.Sel.Name == "BeginTx" {
				t.Errorf("%s: raw %s; use authz.Tx", fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no files checked")
	}
}
