// Command permgen writes the permission table that the core service serves
// to docs/specs/generated/permissions.md (for people) and
// services/intelligence/intelligence/security/permissions.json (for the
// Python services' checks). Run it from services/core:
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
	markdownPath = filepath.Join("..", "..", "docs", "specs", "generated", "permissions.md")
	jsonPath     = filepath.Join("..", "intelligence", "intelligence", "security", "permissions.json")
)

// render returns each generated file's path (relative to services/core)
// and content.
func render() (map[string][]byte, error) {
	table := server.Table()
	js, err := authz.JSON(table)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{markdownPath: authz.Markdown(table), jsonPath: js}, nil
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
