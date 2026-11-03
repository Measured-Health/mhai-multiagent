package feedback

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// C15 (spec §4.4): the masked consultation has one reader. Only
// feedback/store.go may name masked_question or masked_patient_json in a
// string (SQL) of non-test code, and that reader never names the raw
// consultation columns clinical_question or patient_data_json.
func TestOnlyCorrectionsReaderSelectsMaskedColumns(t *testing.T) {
	root := filepath.Join("..", "..")
	const reader = "internal/feedback/store.go"
	masked := regexp.MustCompile(`(?i)\bmasked_(question|patient_json)\b`)
	raw := regexp.MustCompile(`(?i)\b(clinical_question|patient_data_json)\b`)
	fset := token.NewFileSet()
	checked, readerSeen := 0, false
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
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			checked++
			ast.Inspect(f, func(n ast.Node) bool {
				b, ok := n.(*ast.BasicLit)
				if !ok || b.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(b.Value)
				if err != nil {
					return true
				}
				if masked.MatchString(s) {
					if rel != reader {
						t.Errorf("%s names the masked consultation; only %s may", fset.Position(b.Pos()), reader)
					} else {
						readerSeen = true
					}
				}
				if rel == reader && raw.MatchString(s) {
					t.Errorf("%s: the corrections reader names a raw consultation column", fset.Position(b.Pos()))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked == 0 || !readerSeen {
		t.Fatalf("checked %d files, reader seen %v: the test proves nothing", checked, readerSeen)
	}
}
