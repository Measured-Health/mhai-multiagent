package audit_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

// actionArgs are the helpers that take an audit action as a plain argument,
// and the argument's index.
var actionArgs = map[string]int{
	"writeAudit":     3, // auth: (ctx, tx, r, action, ...)
	"auditOnly":      2, // auth: (w, r, action, ...)
	"revokeSessions": 4, // auth: (ctx, r, user, hospital, action, ...)
	"jobEvent":       1, // intel: (row, action, payload)
}

// collectActions walks every non-test Go file under internal/ and cmd/ and
// returns each audit action written as a literal: an `Action: "..."` field,
// a literal passed to one of actionArgs, or a literal assigned to a
// variable named action or a field named Action.
func collectActions(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	found := map[string]string{}
	lit := func(e ast.Expr) (string, bool) {
		b, ok := e.(*ast.BasicLit)
		if !ok || b.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(b.Value)
		return s, err == nil
	}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			add := func(e ast.Expr) {
				if s, ok := lit(e); ok {
					found[s] = fset.Position(e.Pos()).String()
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.KeyValueExpr:
					if k, ok := n.Key.(*ast.Ident); ok && k.Name == "Action" {
						add(n.Value)
					}
				case *ast.CallExpr:
					name := ""
					switch fn := n.Fun.(type) {
					case *ast.Ident:
						name = fn.Name
					case *ast.SelectorExpr:
						name = fn.Sel.Name
					}
					if i, ok := actionArgs[name]; ok && i < len(n.Args) {
						add(n.Args[i])
					}
				case *ast.AssignStmt:
					for i, l := range n.Lhs {
						name := ""
						switch l := l.(type) {
						case *ast.Ident:
							name = l.Name
						case *ast.SelectorExpr:
							name = l.Sel.Name
						}
						if (name == "action" || name == "Action") && i < len(n.Rhs) && len(n.Lhs) == len(n.Rhs) {
							add(n.Rhs[i])
						}
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
	return found
}

// TestEveryActionHasSchema (C21, core part): every audit action in the code
// has a payload schema in the registry.
func TestEveryActionHasSchema(t *testing.T) {
	found := collectActions(t)
	// The walk must see each kind of call site, or it proves nothing.
	for _, want := range []string{
		"query.submitted",             // Action: field
		"auth.login.succeeded",        // writeAudit
		"auth.login.failed",           // auditOnly
		"auth.logout",                 // revokeSessions
		"job.retried",                 // jobEvent
		"auth.mfa.backup_used",        // action := / action, payload =
		"feedback.validated",          // Plan 4
		"correction.submitted",        // Plan 4
		"correction.approved",         // Plan 4
		"correction.rejected",         // Plan 4
		"correction.withdrawn",        // Plan 4
		"correction.decision_refused", // Plan 4
	} {
		if _, ok := found[want]; !ok {
			t.Errorf("the walk didn't find %q", want)
		}
	}
	var missing []string
	for a, at := range found {
		if !audit.Registered(a) {
			missing = append(missing, a+" ("+at+")")
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("no audit schema for %s", m)
	}
}

// TestAuditWriteRefusesBadPayload: an action with no schema, a missing,
// extra or ill-shaped key is refused before the chain is touched (the nil
// transaction is never used), with an error that never echoes a value.
func TestAuditWriteRefusesBadPayload(t *testing.T) {
	const marker = "SYNTHETIC-AUDIT-MARKER-91c2"
	cid, qid, jid := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for name, e := range map[string]audit.Event{
		"no schema":           {Action: "made.up", Payload: map[string]any{}},
		"missing required":    {Action: "correction.approved", Payload: map[string]any{"correction_id": cid}},
		"extra key":           {Action: "correction.withdrawn", Payload: map[string]any{"correction_id": cid, "note": marker}},
		"free text in an id":  {Action: "feedback.validated", Payload: map[string]any{"query_id": marker}},
		"upper-case uuid":     {Action: "feedback.validated", Payload: map[string]any{"query_id": strings.ToUpper(qid)}},
		"number for an id":    {Action: "feedback.validated", Payload: map[string]any{"query_id": 5}},
		"unknown code":        {Action: "correction.rejected", Payload: map[string]any{"correction_id": cid, "reason_code": marker}},
		"refusal code":        {Action: "correction.decision_refused", Payload: map[string]any{"correction_id": cid, "reason": "conflict", "attempted": "approve"}},
		"refusal attempt":     {Action: "correction.decision_refused", Payload: map[string]any{"correction_id": cid, "reason": "forbidden", "attempted": "delete"}},
		"nested object":       {Action: "correction.submitted", Payload: map[string]any{"correction_id": cid, "query_id": map[string]any{"x": qid}}},
		"diagnosis smuggled":  {Action: "correction.submitted", Payload: map[string]any{"correction_id": cid, "query_id": qid, "corrected_diagnosis": marker}},
		"job id not an id":    {Action: "correction.approved", Payload: map[string]any{"correction_id": cid, "job_id": "job-1"}},
		"frozen plan 2 shape": {Action: "auth.login.failed", Payload: map[string]any{"reason": marker}},
		"fraction count":      {Action: "auth.password.changed", Payload: map[string]any{"sessions_revoked": 1.5}},
	} {
		err := audit.Write(context.Background(), nil, e)
		if !errors.Is(err, audit.ErrSchema) {
			t.Errorf("%s: err %v, want ErrSchema", name, err)
		}
		if err != nil && strings.Contains(err.Error(), marker) {
			t.Errorf("%s: the error echoes a value: %v", name, err)
		}
	}
	// The same shapes fit when they are right.
	for name, e := range map[string]audit.Event{
		"validated": {Action: "feedback.validated", Payload: map[string]any{"query_id": qid}},
		"submitted": {Action: "correction.submitted", Payload: map[string]any{"correction_id": cid, "query_id": qid}},
		"approved":  {Action: "correction.approved", Payload: map[string]any{"correction_id": cid, "job_id": jid}},
		"rejected":  {Action: "correction.rejected", Payload: map[string]any{"correction_id": cid, "reason_code": "unsafe"}},
		"withdrawn": {Action: "correction.withdrawn", Payload: map[string]any{"correction_id": cid}},
		"refused":   {Action: "correction.decision_refused", Payload: map[string]any{"correction_id": cid, "reason": "self_approval", "attempted": "reject"}},
		"plan 2":    {Action: "auth.login.succeeded", Payload: map[string]any{"mfa": true, "amr": []string{"pwd", "otp"}}},
		"plan 3":    {Action: "query.submitted", Payload: map[string]any{"query_id": qid, "urgency": "urgent"}},
	} {
		if err := audit.CheckPayload(e); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Register is closed at init: a second schema for an action, or a required
// key with no check, stops the binary.
func TestRegisterRefusesMistakes(t *testing.T) {
	for name, f := range map[string]func(){
		"duplicate":          func() { audit.Register("correction.approved", audit.Schema{}) },
		"required unchecked": func() { audit.Register("test.unchecked", audit.Schema{Required: []string{"x"}}) },
		"malformed name":     func() { audit.Register("Not An Action", audit.Schema{}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			f()
		}()
	}
}
