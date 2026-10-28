package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	// The test runs in cmd/permgen; render's paths are from services/core.
	for p, want := range files {
		got, err := os.ReadFile(filepath.Join("..", "..", p))
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
	old := filepath.Join("..", "..", "..", "intelligence", "intelligence", "security", "permissions.json")
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
	md := string(files[markdownPath])
	for _, want := range []string{"## Operations", "`pipeline.run_query`", "`echo.ping`"} {
		if !strings.Contains(md, want) {
			t.Errorf("permissions.md lacks %q", want)
		}
	}
}
