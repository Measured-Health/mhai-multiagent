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

// dbTxAllowedDirs may open a transaction themselves. Everything else,
// handlers above all, goes through authz.Tx.
var dbTxAllowedDirs = map[string]string{
	"internal/db":    "defines the transactions",
	"internal/authz": "Tx and the principal middleware's state lookup",
	"internal/auth":  "sign-in runs before a principal exists (mhai_auth)",
}

// dbTxAllowedFiles are single files outside those packages that may, each
// for a stated reason. A new file in the same package is checked.
var dbTxAllowedFiles = map[string]string{
	// Infrastructure that can't import authz (authz imports it):
	"internal/httpx/idempotency.go": "the idempotency store scopes its own rows by the principal it is given",
	// The chain check (Plan 4 §5.4): the walk reads the whole chain as
	// mhai_platform under its budget, off the request path; the daily
	// schedule and the finish run with no principal. A person's start
	// goes through authz.Tx (StartRunTx).
	"internal/audit/verify.go": "the chain walk, run finish and daily schedule (db.WithPlatform, the schedule's advisory lock)",
	// Provisioning CLI on the owner/migrate connection (SET LOCAL ROLE
	// mhai_provision), never on a request path.
	"internal/account/create.go": "the users/hospitals create CLI",
	// Core's background job loop, never on a request path: it runs with no
	// principal, reads and updates job rows of every hospital (job columns
	// only, as mhai_jobs) and re-checks a job's creator (as mhai_auth)
	// before re-minting. Request paths reserve in-flight slots and append
	// facts inside the caller's authz.Tx instead.
	"internal/intel/sweeper.go": "the publish sweeper (db.WithJobs) and Republish's creator re-check (db.WithAuth)",
	// The two-person check PublishJob makes before it mints a second_person
	// job: it reads the job's subject under the Caller's hospital (the
	// token's hid) and runs for the sweeper's Republish too, which has no
	// principal, so it can't use authz.Tx.
	"internal/intel/subjects.go": "the subject loader's read of a second_person job (db.WithHospital)",
	// Core's audit drainer: moves every hospital's outbox facts onto the
	// chain as mhai_audit_drain, with no principal (F2, F28).
	"internal/intel/drain.go": "the audit drainer (db.WithAuditDrain)",
	// Core's dead-letter consumer: looks a letter's job up by id and marks
	// it, as mhai_jobs, with no principal; a letter is untrusted input and
	// never acts beyond its own subject's hospital (F5, F28).
	"internal/intel/deadletter.go": "the dead-letter consumer (db.WithJobs)",
	// The platform's retry and cancel of failed jobs (Plan 4 §6.2): a
	// platform principal acts on job rows of every hospital, which only
	// mhai_jobs may touch (job columns only), and the call's audit row is
	// the admin's, appended as mhai_platform in the same transaction
	// (db.WithJobsThenPlatform). The actor comes from the handler's
	// principal; the creator re-check is Republish's (db.WithAuth).
	"internal/intel/retry.go": "the platform's job retry and cancel (db.WithJobs, db.WithJobsThenPlatform)",
	// The grant endpoint audits token and session refusals before a
	// principal exists, as mhai_auth, as sign-in failures are (F7).
	// Everything after the principal goes through authz.Tx.
	"internal/grant/grant.go": "grant refusals before a principal exists (db.WithAuth, mhai_auth)",
	// Feedback and corrections run as the caller's hospital and user
	// (db.WithHospitalUser), so 00011's two-person triggers and insert
	// policy see the verified user; authz.Tx sets the hospital only. The
	// store takes both ids from the handler's principal, never a body.
	"internal/feedback/store.go": "feedback and corrections as the token's hospital and user (db.WithHospitalUser)",
	// The health sampler (Plan 4 §6.1): a background loop with no
	// principal, one core copy at a time (a session advisory lock on a
	// connection it takes out of the pool), reading the health functions
	// as mhai_platform. GET /platform/health reads the KV bucket only.
	"internal/platform/sampler.go": "the health sampler's leader lock (Acquire) and its reads (db.WithPlatformSnapshot)",
}

// TestNoDirectDBTx walks every non-test Go file of the module and fails on
// a call to db.With*, or a raw Begin* or Acquire, outside the allowed
// packages and files.
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
		if _, ok := dbTxAllowedDirs[rel]; ok {
			return nil
		}
		if _, ok := dbTxAllowedFiles[rel+"/"+filepath.Base(path)]; ok {
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
			if strings.HasPrefix(sel.Sel.Name, "Begin") || sel.Sel.Name == "Acquire" {
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
