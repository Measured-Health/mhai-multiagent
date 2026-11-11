//go:build integration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The dev compose files still validate as they are today: .env.example
// with no NATS_EMR_* at all, every file and profile CI checks.
func TestComposeValidatesWithoutEMRVariables(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose is not installed here")
	}
	root := filepath.Join("..", "..", "..", "..")
	raw, err := os.ReadFile(filepath.Join(root, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, "NATS_EMR_") {
			kept = append(kept, line)
		}
	}
	envFile := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(envFile, []byte(strings.Join(kept, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "-q"},
		{"--profile", "test", "--profile", "observability", "config", "-q"},
		{"-f", "docker-compose.yml", "-f", "docker-compose.smoke.yml", "config", "-q"},
	} {
		cmd := exec.Command("docker", append([]string{"compose", "--env-file", envFile}, args...)...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "NATS_EMR_CONF=", "NATS_EMR_PASSWORD=")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("compose %v: %v\n%s", args, err, out)
		}
	}
}
