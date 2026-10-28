package bus_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repo is the repository root, from services/core/internal/bus.
var repo = filepath.Join("..", "..", "..", "..")

func readConf(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stripComments drops # comments and all whitespace, so two configs can be
// compared by content.
func stripComments(s string) string {
	var out strings.Builder
	for _, line := range strings.Split(s, "\n") {
		// No subject in these configs contains '#', so a # starts a comment.
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		out.WriteString(line)
	}
	return regexp.MustCompile(`\s+`).ReplaceAllString(out.String(), "")
}

// permissionsAfter returns the brace-balanced permissions block that
// follows marker in conf.
func permissionsAfter(t *testing.T, conf, marker string) string {
	t.Helper()
	i := strings.Index(conf, marker)
	if i < 0 {
		t.Fatalf("no %q in config", marker)
	}
	j := strings.Index(conf[i:], "permissions:")
	if j < 0 {
		t.Fatalf("no permissions after %q", marker)
	}
	start := i + j + strings.Index(conf[i+j:], "{")
	depth := 0
	for k := start; k < len(conf); k++ {
		switch conf[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return conf[start : k+1]
			}
		}
	}
	t.Fatalf("unbalanced permissions after %q", marker)
	return ""
}

// F19 / spec §17.2: the test-only bff_direct subject (echo.ping) is only in
// the include file, which the production-shaped config never includes.
func TestTestOnlySubjectOnlyInTestInclude(t *testing.T) {
	const testOnly = "inference.v1.echo.ping"
	inc := readConf(t, "services/nats/bff-clinical.test.conf")
	if !strings.Contains(inc, `"`+testOnly+`"`) {
		t.Fatalf("bff-clinical.test.conf doesn't grant %s", testOnly)
	}
	dev := readConf(t, "services/nats/nats.conf")
	if strings.Contains(dev, testOnly) {
		t.Fatalf("services/nats/nats.conf names %s; it belongs in the test include only", testOnly)
	}
	if !strings.Contains(dev, `include "bff-clinical.test.conf"`) {
		t.Fatal("services/nats/nats.conf doesn't include bff-clinical.test.conf")
	}
	prod := readConf(t, "deploy/nats/nats.conf")
	if strings.Contains(prod, testOnly) || strings.Contains(stripComments(prod), "include") || strings.Contains(prod, ".test.conf\"") {
		t.Fatal("deploy/nats/nats.conf must not grant or include the test-only subject")
	}
}

// The demo's gateway permissions are the dev config's, line for line, so
// the narrowed JetStream rights tested here are the ones deployed.
func TestDeployGatewayPermissionsMatchDev(t *testing.T) {
	dev := permissionsAfter(t, readConf(t, "services/nats/nats.conf"), "password: $NATS_GATEWAY_PASSWORD")
	prod := permissionsAfter(t, readConf(t, "deploy/nats/nats.conf"), "nkey: $NATS_GATEWAY_NKEY")
	if stripComments(dev) != stripComments(prod) {
		t.Fatalf("gateway permissions differ between services/nats/nats.conf and deploy/nats/nats.conf:\ndev:  %s\nprod: %s",
			stripComments(dev), stripComments(prod))
	}
}

// The dev and test config: 1 MB payloads, encrypted JetStream, keyed from
// the environment, and no shared worker user left.
func TestDevConfLimitsAndEncryption(t *testing.T) {
	dev := stripComments(readConf(t, "services/nats/nats.conf"))
	for _, want := range []string{"max_payload:1MB", "cipher:chachapoly", "key:$NATS_JS_KEY"} {
		if !strings.Contains(dev, want) {
			t.Errorf("services/nats/nats.conf lacks %s", want)
		}
	}
	for _, gone := range []string{"user:worker", "NATS_WORKER_PASSWORD", `"$JS.API.CONSUMER.>"`, `"$JS.API.STREAM.CREATE.>"`, "no_auth_user"} {
		if strings.Contains(dev, gone) {
			t.Errorf("services/nats/nats.conf still has %s", gone)
		}
	}
}
