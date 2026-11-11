package internal

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// K12 (P8): the emr binary links nothing of core: not its auth, signing or
// API code. Go already forbids core's internal/ packages across modules;
// this also pins that the module requires no core module at all.
func TestEMRImportsNoCoreModule(t *testing.T) {
	root := filepath.Join("..")
	cmd := exec.Command("go", "list", "-deps", "-test", "./...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, p := range strings.Split(string(out), "\n") {
		if strings.Contains(p, "mhai-multiagent/services/core") {
			t.Errorf("the emr module depends on %s", p)
		}
	}
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mod), "services/core") {
		t.Error("services/emr/go.mod names services/core")
	}
}
