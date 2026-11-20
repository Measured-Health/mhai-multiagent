package internal

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ADR 0018: the notifications binary links nothing of core (its auth,
// signing or API code), nothing of the emr service (its credentials and
// connectors) and nothing of the documents service (its keys). Go already forbids other modules' internal/ packages; this
// also pins that the module requires neither module at all.
func TestNotificationsImportsNoOtherService(t *testing.T) {
	root := filepath.Join("..")
	cmd := exec.Command("go", "list", "-deps", "-test", "./...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, p := range strings.Split(string(out), "\n") {
		for _, other := range []string{"mhai-multiagent/services/core", "mhai-multiagent/services/emr",
			"mhai-multiagent/services/documents"} {
			if strings.Contains(p, other) {
				t.Errorf("the notifications module depends on %s", p)
			}
		}
	}
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{"services/core", "services/emr", "services/documents"} {
		if strings.Contains(string(mod), other) {
			t.Errorf("services/notifications/go.mod names %s", other)
		}
	}
}
