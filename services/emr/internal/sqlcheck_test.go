package internal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// patientsRef is a reference to emr.patients in a FROM or JOIN; join cuts a
// query at its next clause.
var (
	patientsJoin = regexp.MustCompile(`(?is)\bjoin\s+emr\.patients\b(\s+\w+)?\s+on\b`)
	patientsList = regexp.MustCompile(`(?is)\bemr\.patients\b(\s+\w+)?\s*,`)
	nextClause   = regexp.MustCompile(`(?is)\b(join|where|group|order|limit|union|returning)\b`)
)

// unsafePatientJoins returns the joins on emr.patients in q whose ON clause
// doesn't name hospital_id, and any comma join of it.
func unsafePatientJoins(q string) []string {
	var bad []string
	for _, loc := range patientsJoin.FindAllStringIndex(q, -1) {
		rest := q[loc[1]:]
		if end := nextClause.FindStringIndex(rest); end != nil {
			rest = rest[:end[0]]
		}
		if !strings.Contains(rest, "hospital_id") {
			bad = append(bad, strings.TrimSpace(q[loc[0]:loc[1]]+rest))
		}
	}
	if patientsList.MatchString(q) {
		bad = append(bad, "a comma join of emr.patients")
	}
	return bad
}

// ADR 0009 rule 5, design §10.1: every SQL string in the emr module that
// joins emr.patients names hospital_id in its join condition, so no query
// can pair patients across hospitals even if RLS were off.
func TestNoCrossHospitalJoinInEMRSQL(t *testing.T) {
	for q, want := range map[string]int{
		`SELECT 1 FROM emr.patient_links l JOIN emr.patients p ON p.id = l.patient_id AND p.hospital_id = l.hospital_id WHERE x`: 0,
		`SELECT 1 FROM emr.patient_links l JOIN emr.patients p ON p.id = l.patient_id WHERE p.hospital_id = $1`:                  1,
		`SELECT 1 FROM emr.patients a, emr.patients b WHERE a.mrn = b.mrn`:                                                       1,
		`SELECT 1 FROM x JOIN emr.patient_links l ON l.patient_id = x.id`:                                                        0,
	} {
		if got := len(unsafePatientJoins(q)); got != want {
			t.Errorf("checker on %q: %d findings, want %d", q, got, want)
		}
	}
	root := ".."
	seen := 0
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "testdata" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || filepath.Base(p) == "sqlcheck_test.go" {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.Contains(s, "emr.patients") {
				return true
			}
			seen++
			for _, b := range unsafePatientJoins(s) {
				t.Errorf("%s: %q", p, b)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Fatal("no SQL naming emr.patients found: the walk proves nothing")
	}
}
