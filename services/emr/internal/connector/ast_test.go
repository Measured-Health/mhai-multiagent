package connector_test

import (
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

const (
	credsType  = "github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector.Creds"
	secretType = "github.com/Measured-Health/mhai-multiagent/services/gokit/seal.Secret"
)

// A3 (design §4): no connector code hands Creds, a seal.Secret, or anything
// taken from them, to a logger (log, log/slog, a *slog.Logger or
// *log.Logger method) or a print or error function of fmt. The check is
// typed, so a value renamed or passed through a variable is still caught;
// the fixture in testdata/leaky proves it finds each kind.
func TestNoConnectorLogsCreds(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks from source")
	}
	build.Default.CgoEnabled = false
	var dirs []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "testdata" {
			return filepath.SkipDir
		}
		if d.IsDir() {
			if m, _ := filepath.Glob(filepath.Join(path, "*.go")); len(m) > 0 {
				dirs = append(dirs, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) < 2 {
		t.Fatalf("found only %v", dirs)
	}
	for _, d := range dirs {
		for _, f := range credLeaks(t, d) {
			t.Errorf("%s hands a credential to a logger or print function", f)
		}
	}
	got := credLeaks(t, filepath.Join("testdata", "leaky"))
	if len(got) != 6 {
		t.Fatalf("the fixture's six leaks: found %d: %v", len(got), got)
	}
}

// credLeaks type-checks the non-test files in dir and returns the position
// of every call that passes a credential-typed expression to a sink.
func credLeaks(t *testing.T, dir string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var files []*ast.File
	matches, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	for _, m := range matches {
		if strings.HasSuffix(m, "_test.go") {
			continue
		}
		abs, err := filepath.Abs(m)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, abs, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	if _, err := conf.Check(files[0].Name.Name, fset, files, info); err != nil {
		t.Fatalf("type-check %s: %v", dir, err)
	}
	var out []string
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isSink(info, call) {
				return true
			}
			for _, a := range call.Args {
				if carriesCred(info, a) {
					out = append(out, fset.Position(call.Pos()).String())
					break
				}
			}
			return true
		})
	}
	return out
}

// isSink: a function of log, log/slog or fmt's print and error family, or
// a method of *log.Logger or *slog.Logger.
func isSink(info *types.Info, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if id, ok := sel.X.(*ast.Ident); ok {
		if pn, ok := info.Uses[id].(*types.PkgName); ok {
			switch pn.Imported().Path() {
			case "log", "log/slog":
				return true
			case "fmt":
				n := sel.Sel.Name
				return strings.HasPrefix(n, "Print") || strings.HasPrefix(n, "Fprint") ||
					strings.HasPrefix(n, "Sprint") || strings.HasPrefix(n, "Append") || n == "Errorf"
			}
			return false
		}
	}
	s, ok := info.Selections[sel]
	if !ok || s.Kind() != types.MethodVal {
		return false
	}
	recv := s.Recv()
	if p, ok := recv.(*types.Pointer); ok {
		recv = p.Elem()
	}
	named, ok := recv.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	path := named.Obj().Pkg().Path()
	return named.Obj().Name() == "Logger" && (path == "log" || path == "log/slog")
}

// carriesCred: any expression inside e has a credential type (or a
// pointer to one), so creds.Get(k).Bytes() is caught at creds.Get(k).
// creds.Has(k) and secret.Len() are not credentials.
func carriesCred(info *types.Info, e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		// creds.Has(k) and secret.Len() say nothing of the value.
		if c, ok := n.(*ast.CallExpr); ok {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Has" || sel.Sel.Name == "Len") {
				if tv, ok := info.Types[sel.X]; ok && isCredType(tv.Type) {
					return false
				}
			}
		}
		x, ok := n.(ast.Expr)
		if !ok || found {
			return !found
		}
		if tv, ok := info.Types[x]; ok && isCredType(tv.Type) {
			found = true
		}
		return !found
	})
	return found
}

func isCredType(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	full := named.Obj().Pkg().Path() + "." + named.Obj().Name()
	return full == credsType || full == secretType
}
