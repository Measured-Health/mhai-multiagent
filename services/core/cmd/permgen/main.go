// Command permgen writes the permission table that the core service serves
// and the operations table to docs/specs/generated/permissions.md (for
// people) and services/intelligence/intelligence/boundary/operations.json
// (read by the Python workers; Python writes no rule), and the Intelligence
// API docs page (from proto/ and the operations table; embedded by
// internal/apidocs). Run it from
// services/core:
//
//	go run ./cmd/permgen
//
// TestGeneratedFilesUpToDate fails when the committed files differ.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/server"
)

// Paths from services/core to the generated files.
var (
	markdownPath   = filepath.Join("..", "..", "docs", "specs", "generated", "permissions.md")
	operationsPath = filepath.Join("..", "intelligence", "intelligence", "boundary", "operations.json")
)

// render returns each generated file's path (relative to services/core)
// and content.
func render() (map[string][]byte, error) {
	table := server.Table()
	js, err := authz.OperationsJSON(authz.Operations())
	if err != nil {
		return nil, err
	}
	page, err := intelligenceHTML()
	if err != nil {
		return nil, err
	}
	return map[string][]byte{markdownPath: authz.Markdown(table), operationsPath: js, intelligencePage: page}, nil
}

func main() {
	files, err := render()
	if err != nil {
		fmt.Fprintln(os.Stderr, "permgen:", err)
		os.Exit(1)
	}
	for p, b := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "permgen:", err)
			os.Exit(1)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "permgen:", err)
			os.Exit(1)
		}
		fmt.Println("wrote", p)
	}
}
