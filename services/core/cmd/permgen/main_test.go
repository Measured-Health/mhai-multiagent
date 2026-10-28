package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The committed generated files match what permgen produces now (CI runs
// this too). Fix a failure with: cd services/core && go run ./cmd/permgen
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
