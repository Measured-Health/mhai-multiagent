package bus_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
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
	for _, want := range []string{"max_payload:1MB", "max_file_store:1GB", "cipher:chachapoly", "key:$NATS_JS_KEY"} {
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

// JOBS and DEAD reserve their MaxBytes in max_file_store. The demo's store
// is 512 MB (deploy/nats/nats.conf); the streams must fit with room left
// for the KV buckets (at least 128 MiB), or core serve can't start there.
func TestStreamReservationsFitTheDemoStore(t *testing.T) {
	if !strings.Contains(stripComments(readConf(t, "deploy/nats/nats.conf")), "max_file_store:512MB") {
		t.Fatal("deploy/nats/nats.conf max_file_store changed: recheck this budget")
	}
	if bus.StreamsMaxBytes > 512<<20-128<<20 {
		t.Fatalf("JOBS+DEAD reserve %d bytes, too much for the demo's 512 MB store", bus.StreamsMaxBytes)
	}
}

// Review round 2 (A): one subject's worst case (its message limit times the
// largest job) is at most a sixteenth of JOBS, so one hospital can't fill
// the stream for the others.
func TestJobsPerSubjectWorstCaseIsASixteenth(t *testing.T) {
	if worst := bus.JobsMaxPerSubject * bus.JobsMaxMsgSize; worst > bus.JobsMaxBytes/16 {
		t.Fatalf("one subject can hold %d bytes, more than JobsMaxBytes/16 = %d", worst, bus.JobsMaxBytes/16)
	}
}

// Spec §17.1, §17.4 (Plan 3 T15): the demo's clinical BFF user has the
// dev include's permissions minus the test-only bff_direct subjects, so the
// two can't drift; nothing else is granted.
func TestDeployBFFClinicalMatchesDevWithoutTestOnly(t *testing.T) {
	dev := stripComments(readConf(t, "services/nats/bff-clinical.test.conf"))
	i := strings.Index(dev, "permissions:")
	if i < 0 {
		t.Fatal("no permissions in bff-clinical.test.conf")
	}
	dev = strings.ReplaceAll(dev[i+len("permissions:"):], `"inference.v1.echo.ping"`, "")
	prod := stripComments(permissionsAfter(t, readConf(t, "deploy/nats/nats.conf"), "nkey: $NATS_BFF_NKEY"))
	if dev != prod {
		t.Fatalf("bff_clinical permissions differ:\ndev (no test-only): %s\nprod: %s", dev, prod)
	}
}

// Spec §17.1, §17.4: the demo's platform BFF user has the dev config's
// bff_platform permissions exactly (the dev one has no test-only grants),
// so the two can't drift; nothing else is granted.
func TestDeployBFFPlatformMatchesDev(t *testing.T) {
	dev := stripComments(permissionsAfter(t, readConf(t, "services/nats/nats.conf"), "user: bff_platform"))
	prod := stripComments(permissionsAfter(t, readConf(t, "deploy/nats/nats.conf"), "nkey: $NATS_BFF_PLATFORM_NKEY"))
	if dev != prod {
		t.Fatalf("bff_platform permissions differ:\ndev: %s\nprod: %s", dev, prod)
	}
	want := `permissions:{publish:{allow:["api.v1.platform"]}subscribe:{allow:["_INBOX_bff.platform.>"]}}`
	if got := "permissions:" + prod; got != want {
		t.Fatalf("bff_platform must publish api.v1.platform and subscribe _INBOX_bff.platform.> only:\n%s", got)
	}
}

// The two BFF users in the demo config are separate NKeys: neither may be
// the other's variable, and neither may carry the other's subjects.
func TestDeployBFFUsersAreSeparate(t *testing.T) {
	prod := readConf(t, "deploy/nats/nats.conf")
	clin := stripComments(permissionsAfter(t, prod, "nkey: $NATS_BFF_NKEY"))
	plat := stripComments(permissionsAfter(t, prod, "nkey: $NATS_BFF_PLATFORM_NKEY"))
	if strings.Contains(clin, "platform") || strings.Contains(plat, "clinical") {
		t.Fatalf("a BFF user holds the other app's subject:\nclinical: %s\nplatform: %s", clin, plat)
	}
}

// Plan 4 spec §10.5: the pipeline user's JetStream rights are exactly its
// two durables' three subjects each (pipeline for run_query,
// pipeline_learning for approve_correction): no stream API, no other
// consumer, no create or delete.
func TestPipelineUserJSSubjectsAreItsTwoConsumersOnly(t *testing.T) {
	perms := permissionsAfter(t, readConf(t, "services/nats/nats.conf"), "password: $NATS_PIPELINE_PASSWORD")
	got := map[string]bool{}
	for _, m := range regexp.MustCompile(`"(\$JS[^"]*)"`).FindAllStringSubmatch(perms, -1) {
		got[m[1]] = true
	}
	want := map[string]bool{}
	for _, c := range []string{bus.PipelineConsumer, bus.LearningConsumer} {
		want["$JS.API.CONSUMER.INFO.JOBS."+c] = true
		want["$JS.API.CONSUMER.MSG.NEXT.JOBS."+c] = true
		want["$JS.ACK.JOBS."+c+".>"] = true
	}
	if len(got) != len(want) {
		t.Fatalf("pipeline $JS subjects = %v, want %v", got, want)
	}
	for s := range want {
		if !got[s] {
			t.Fatalf("pipeline lacks %s (has %v)", s, got)
		}
	}
	if strings.Contains(perms, "jobs.v1") || strings.Contains(perms, "$JS.API.STREAM") || strings.Contains(perms, "$JS.API.>") {
		t.Fatalf("pipeline may publish jobs or reach the stream API: %s", stripComments(perms))
	}
	if bus.LearningFilter == bus.PipelineFilter || bus.LearningFilter != "jobs.v1.*.approve_correction" {
		t.Fatalf("learning filter %q", bus.LearningFilter)
	}
}

// Plan 4 spec §10.5: the health bucket is the gateway's alone. In both
// configs, no user but the gateway names it, and the gateway names exactly
// its KV subjects, its stream's create, update and info, and its reads.
func TestHealthBucketIsTheGatewaysOnly(t *testing.T) {
	want := []string{
		`"$KV.health.>"`,
		`"$JS.API.STREAM.CREATE.KV_health"`, `"$JS.API.STREAM.UPDATE.KV_health"`, `"$JS.API.STREAM.INFO.KV_health"`,
		`"$JS.API.STREAM.MSG.GET.KV_health"`, `"$JS.API.DIRECT.GET.KV_health.>"`,
	}
	for _, c := range []struct{ file, gateway string }{
		{"services/nats/nats.conf", "password: $NATS_GATEWAY_PASSWORD"},
		{"deploy/nats/nats.conf", "nkey: $NATS_GATEWAY_NKEY"},
	} {
		conf := readConf(t, c.file)
		gw := permissionsAfter(t, conf, c.gateway)
		got := regexp.MustCompile(`"[^"]*health[^"]*"`).FindAllString(stripComments(gw), -1)
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s: gateway's health subjects %v, want %v", c.file, got, want)
		}
		// Every other occurrence of the bucket's subjects is the gateway's:
		// remove its block and nothing names the bucket.
		rest := stripComments(strings.Replace(conf, gw, "", 1))
		if strings.Contains(rest, "KV.health") || strings.Contains(rest, "KV_health") {
			t.Errorf("%s: a user other than the gateway names the health bucket", c.file)
		}
	}
	// Nor the include, where the clinical BFF lives in dev.
	if inc := readConf(t, "services/nats/bff-clinical.test.conf"); strings.Contains(inc, "health") {
		t.Error("bff-clinical.test.conf names the health bucket")
	}
	if bus.HealthBucket != "health" || bus.HealthTTL != 5*time.Minute {
		t.Errorf("health bucket %q, ttl %v", bus.HealthBucket, bus.HealthTTL)
	}
}

// With EMR on, JOBS, DEAD and EMR together still leave the KV buckets at
// least 128 MiB of the demo's 512 MB store.
func TestEMRStreamFitsTheDemoStore(t *testing.T) {
	if bus.StreamsMaxBytes+bus.EMRMaxBytes > 512<<20-128<<20 {
		t.Fatalf("JOBS+DEAD+EMR reserve %d bytes", bus.StreamsMaxBytes+bus.EMRMaxBytes)
	}
}

// The emr NATS user is an include: emr.conf (on) or emr-off.conf (the dev
// default), which needs no variable and lets no one in or do anything.
func TestEMRUserIsAnOptInInclude(t *testing.T) {
	dev := readConf(t, "services/nats/nats.conf")
	if !strings.Contains(dev, `{ include "emr.conf" }`) || strings.Contains(stripComments(dev), "NATS_EMR_PASSWORD") ||
		strings.Contains(stripComments(dev), "user:emr") {
		t.Fatal("services/nats/nats.conf must take the emr user from the emr.conf include only")
	}
	off := stripComments(readConf(t, "services/nats/emr-off.conf"))
	if strings.Contains(off, "$") || strings.Contains(off, "password") || !strings.Contains(off, `publish:{deny:[">"]}`) ||
		!strings.Contains(off, `subscribe:{deny:[">"]}`) || strings.Contains(off, "allow") {
		t.Fatalf("emr-off.conf must need no variable and deny everything: %s", off)
	}
	on := stripComments(readConf(t, "services/nats/emr.conf"))
	if !strings.Contains(on, "user:emr") || !strings.Contains(on, "password:$NATS_EMR_PASSWORD") {
		t.Fatal("emr.conf must define the emr user with NATS_EMR_PASSWORD")
	}
	if strings.Contains(readConf(t, "deploy/nats/nats.conf"), "emr.conf") {
		t.Fatal("the deploy config must not include an emr user yet (NATS_EMR_NKEY isn't provisioned)")
	}
}
