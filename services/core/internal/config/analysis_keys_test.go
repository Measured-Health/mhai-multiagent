package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeKeyFile writes content to a 0600 file and returns its path.
func writeKeyFile(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func b64(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }

func analysisEnv(t *testing.T, files map[string]string) {
	t.Helper()
	env := map[string]string{"MFA_ENCRYPTION_KEY": goodKey, "BFF_SERVICE_KEYS": goodKey}
	for k, v := range files {
		env[k] = v
	}
	setEnv(t, env)
}

// N6: the four key files: none set means no analysis keys; otherwise all
// four, each 32 random bytes in base64, distinct, not world-readable and
// not a placeholder. Errors name the variable only.
func TestAnalysisKeyFiles(t *testing.T) {
	dir := t.TempDir()
	good := map[string]string{}
	for i, v := range analysisKeyVars {
		good[v] = writeKeyFile(t, dir, strings.ToLower(v), b64(byte(i+1))+"\n", 0o600)
	}
	analysisEnv(t, nil)
	c, err := Load()
	if err != nil || c.Analysis.Set() {
		t.Fatalf("no key files: set %v (%v)", c.Analysis.Set(), err)
	}
	analysisEnv(t, good)
	c, err = Load()
	if err != nil || !c.Analysis.Set() || !bytes.Equal(c.Analysis.Drill, bytes.Repeat([]byte{3}, 32)) {
		t.Fatalf("good key files: %v", err)
	}
	if s := c.Analysis.String() + strings.Join([]string{c.Analysis.GoString()}, ""); strings.Contains(s, b64(3)) || strings.Contains(s, "[3 3 3") {
		t.Fatalf("the keys print: %s", s)
	}

	bad := map[string]string{
		"missing file": filepath.Join(dir, "absent"),
		"placeholder":  writeKeyFile(t, dir, "ph", "change-me-change-me-change-me-change-me-xx", 0o600),
		"short":        writeKeyFile(t, dir, "short", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 16)), 0o600),
		"not base64":   writeKeyFile(t, dir, "nb64", strings.Repeat("!", 44), 0o600),
		"equal":        good["ANALYSIS_REF_HMAC_KEY_FILE"],
		"world":        writeKeyFile(t, dir, "world", b64(8), 0o644),
	}
	for name, path := range bad {
		env := map[string]string{}
		for k, v := range good {
			env[k] = v
		}
		env["ANALYSIS_DRILL_KEY_FILE"] = path
		analysisEnv(t, env)
		_, err := Load()
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "ANALYSIS_") {
			t.Errorf("%s: the error names no variable: %v", name, err)
		}
		if strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "change-me") || strings.Contains(err.Error(), "!!!!") {
			t.Errorf("%s: the error echoes a path or value: %v", name, err)
		}
	}
	// Only some set.
	analysisEnv(t, map[string]string{"ANALYSIS_DRILL_KEY_FILE": good["ANALYSIS_DRILL_KEY_FILE"]})
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ANALYSIS_") {
		t.Errorf("one of four set: %v", err)
	}
}
