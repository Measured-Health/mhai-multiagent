package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
)

// render reads proto/ with paths from services/core, so the tests run there.
func TestMain(m *testing.M) {
	if err := os.Chdir(filepath.Join("..", "..")); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// The committed generated files match what permgen produces now (CI runs
// this too; A23). That covers permissions.md with its operations section
// and boundary/operations.json: removing a role from an operation in Go
// without regenerating fails here. Fix a failure with:
// cd services/core && go run ./cmd/permgen
func TestGeneratedFilesUpToDate(t *testing.T) {
	files, err := render()
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range files {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s: %v (run go run ./cmd/permgen)", p, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is out of date: run go run ./cmd/permgen in services/core", p)
		}
	}
}

// permissions.json went away with the operations table: nothing generates
// it and nothing may read it.
func TestPermissionsJSONIsGone(t *testing.T) {
	files, err := render()
	if err != nil {
		t.Fatal(err)
	}
	for p := range files {
		if filepath.Base(p) == "permissions.json" {
			t.Errorf("permgen still generates %s", p)
		}
	}
	old := filepath.Join("..", "intelligence", "intelligence", "security", "permissions.json")
	if _, err := os.Stat(old); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s must not exist (err %v)", old, err)
	}
}

func TestGeneratedFilesIncludeOperations(t *testing.T) {
	files, err := render()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[operationsPath]; !ok {
		t.Fatalf("render has no %s", operationsPath)
	}
	if !strings.HasSuffix(filepath.ToSlash(operationsPath), "intelligence/boundary/operations.json") {
		t.Errorf("operations.json path %s", operationsPath)
	}
	if _, ok := files[cataloguePath]; !ok {
		t.Fatalf("render has no %s", cataloguePath)
	}
	if !strings.HasSuffix(filepath.ToSlash(cataloguePath), "intelligence/boundary/catalogue.json") {
		t.Errorf("catalogue.json path %s", cataloguePath)
	}
	md := string(files[markdownPath])
	for _, want := range []string{"## Operations", "`pipeline.run_query`", "`echo.ping`"} {
		if !strings.Contains(md, want) {
			t.Errorf("permissions.md lacks %q", want)
		}
	}
}

// The Intelligence API page is generated and committed with the rest; the
// paths in the generator are the ones internal/apidocs serves.
func TestIntelligencePageIsGenerated(t *testing.T) {
	files, err := render()
	if err != nil {
		t.Fatal(err)
	}
	page := string(files[intelligencePage])
	for _, want := range []string{"pipeline.health", "pipeline.run_query", "echo.ping", "test only",
		"inference.v1.pipeline.health", "jobs.v1.&lt;hospital_id&gt;.run_query", "mhai.intelligence.v1.PingRequest",
		"Deadline", "10 minutes", "svc:core", "BFF direct", "query_id", "spec section 7.3"} {
		if !strings.Contains(page, want) {
			t.Errorf("intelligence page lacks %q", want)
		}
	}
	// Comments from the .proto files come through.
	if !strings.Contains(page, "carries the query id only") {
		t.Error("intelligence page lacks the RunQueryJob comment from pipeline.proto")
	}
	if corePagePath != "/api/docs" || intelligencePagePath != "/api/docs/intelligence" || logoPath != "/api/docs/logo.png" {
		t.Error("generator paths drifted from internal/apidocs")
	}
}

func TestOperationNamingUnknownMessageFails(t *testing.T) {
	_, err := operations([]authz.Operation{{Name: "x.y", Service: "x", Op: "y", Kind: reqtoken.KindReq, Timeout: time.Second, RequestSchema: "mhai.intelligence.v1.Nope"}}, nil)
	if err == nil {
		t.Fatal("an operation naming a message no .proto declares must fail")
	}
}
