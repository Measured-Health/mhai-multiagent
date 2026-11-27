package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// prodServices are the services production mode runs that reach Postgres or
// NATS: each must be overridden in compose.production.yml (the demo profile's
// one-shots are not supported in production mode and Postgres refuses them).
func loadProduction(t *testing.T) deployCompose {
	t.Helper()
	return loadComposeAs(t, filepath.Join("deploy", "compose.production.yml"))
}

// verifyFullTo reports whether u is a postgres URL to the postgres service
// with exactly sslmode=verify-full and an absolute sslrootcert.
func verifyFullTo(u string) bool {
	if !strings.HasPrefix(u, "postgres://") || !strings.Contains(u, "@postgres:5432/") && !strings.HasPrefix(u, "postgres://postgres:5432/") {
		return false
	}
	q := u[strings.LastIndex(u, "?")+1:]
	parts := strings.Split(q, "&")
	return strings.Count(u, "sslmode=") == 1 && slices.Contains(parts, "sslmode=verify-full") &&
		slices.ContainsFunc(parts, func(p string) bool { return strings.HasPrefix(p, "sslrootcert=/") })
}

// Production mode (docs/deploy/production-tls-emr.md): every service of
// deploy/compose.yml that connects without TLS (sslmode=disable, nats://),
// outside the demo profile, is switched to verify-full and tls:// with a
// client certificate by compose.production.yml. A service added later with
// a plaintext link fails here until the overlay covers it.
func TestProductionOverlayCoversEveryPlaintextClient(t *testing.T) {
	base := loadDeployCompose(t)
	prod := loadProduction(t)
	for name, s := range base.Services {
		if slices.Contains(s.Profiles, "demo") {
			continue
		}
		for k, v := range s.Environment {
			if !strings.Contains(v, "sslmode=disable") && !strings.HasPrefix(v, "nats://") {
				continue
			}
			o, ok := prod.Services[name]
			if !ok {
				t.Errorf("%s connects without TLS (%s) and compose.production.yml doesn't override it", name, k)
				continue
			}
			got := o.Environment[k]
			switch {
			case strings.Contains(v, "sslmode="):
				if !verifyFullTo(got) {
					t.Errorf("production %s %s = %q: want verify-full with sslrootcert", name, k, got)
				}
			case got != "tls://nats:4222":
				t.Errorf("production %s %s = %q: want tls://nats:4222", name, k, got)
			}
		}
	}
	// The emr service is TLS-only already (it needs production mode).
	emr := base.Services["emr"]
	if emr.Environment["APP_ENV"] != "staging" || emr.Environment["EMR_NATS_URL"] != "tls://nats:4222" ||
		!verifyFullTo(emr.Environment["EMR_DATABASE_URL"]) {
		t.Errorf("emr: APP_ENV %q, NATS %q, database %q: want staging, tls:// and verify-full",
			emr.Environment["APP_ENV"], emr.Environment["EMR_NATS_URL"], emr.Environment["EMR_DATABASE_URL"])
	}
}

// Each Go and Python client shows its own NATS client certificate (NATS
// verifies one from everyone), from its own tls/<service> folder mounted
// read-only; core runs as production, the pipeline leaves single-host mode
// and notifications runs as production.
func TestProductionClientsHaveTheirOwnCertificate(t *testing.T) {
	prod := loadProduction(t)
	for svc, vars := range map[string][3]string{
		"core":          {"NATS_TLS_CA_PATH", "NATS_TLS_CERT_PATH", "NATS_TLS_KEY_PATH"},
		"pipeline":      {"NATS_TLS_CA_PATH", "NATS_TLS_CERT_PATH", "NATS_TLS_KEY_PATH"},
		"notifications": {"NOTIFY_NATS_TLS_CA_FILE", "NOTIFY_NATS_TLS_CERT_FILE", "NOTIFY_NATS_TLS_KEY_FILE"},
	} {
		s := prod.Services[svc]
		if s.Environment[vars[0]] != "/run/tls/ca.pem" || s.Environment[vars[1]] != "/run/tls/"+svc+".pem" ||
			s.Environment[vars[2]] != "/run/tls/"+svc+"-key.pem" {
			t.Errorf("%s: TLS files %q %q %q", svc, s.Environment[vars[0]], s.Environment[vars[1]], s.Environment[vars[2]])
		}
		if !slices.Contains(s.Volumes, "./tls/"+svc+":/run/tls:ro") {
			t.Errorf("%s: mounts %v, want ./tls/%s read-only", svc, s.Volumes, svc)
		}
	}
	emr := loadDeployCompose(t).Services["emr"]
	if emr.Environment["EMR_NATS_TLS_CERT_FILE"] != "/run/tls/emr.pem" || !slices.Contains(emr.Volumes, "./tls/emr:/run/tls:ro") {
		t.Errorf("emr: TLS cert %q, mounts %v", emr.Environment["EMR_NATS_TLS_CERT_FILE"], emr.Volumes)
	}
	// A pool big enough for core's leader loops, each of which holds a
	// connection while its run takes another (the rehearsal's hang).
	if !strings.Contains(prod.Services["core"].Environment["DATABASE_URL"], "&pool_max_conns=16") {
		t.Error("production core DATABASE_URL must size its pool (pool_max_conns=16)")
	}
	if prod.Services["core"].Environment["CORE_ENV"] != "production" || prod.Services["core"].Environment["CORE_ALLOW_SINGLE_NODE"] != "true" {
		t.Error("production core: CORE_ENV must be production, CORE_ALLOW_SINGLE_NODE true (one host)")
	}
	if v, ok := prod.Services["pipeline"].Environment["INTELLIGENCE_SINGLE_HOST"]; !ok || v != "" {
		t.Errorf("production pipeline INTELLIGENCE_SINGLE_HOST = %q (set %v): want empty, off", v, ok)
	}
	if prod.Services["notifications"].Environment["APP_ENV"] != "production" {
		t.Error("production notifications: APP_ENV must be production")
	}
	if got := prod.Services["nats"].Command; !slices.Equal(got, []string{"-c", "/etc/nats/tls.conf"}) {
		t.Errorf("production nats command %v: want tls.conf", got)
	}
	pg := strings.Join(prod.Services["postgres"].Command, " ")
	for _, want := range []string{"ssl=on", "ssl_min_protocol_version=TLSv1.2", "hba_file=/etc/postgresql/pg_hba.conf"} {
		if !strings.Contains(pg, want) {
			t.Errorf("production postgres command lacks %s", want)
		}
	}
	// No service in production mode is published on more than loopback, and
	// the overlay adds no port and no network.
	for name, s := range prod.Services {
		if len(s.Ports) != 0 || len(s.networkNames()) != 0 {
			t.Errorf("compose.production.yml: %s adds ports %v or networks %v", name, s.Ports, s.networkNames())
		}
	}
}

// Postgres in production mode: TLS and a password from its own networks,
// everything else refused; no trust but the container's own socket.
func TestProductionPgHBA(t *testing.T) {
	var rules [][]string
	for _, l := range strings.Split(readRepoFile(t, filepath.Join("deploy", "postgres", "pg_hba.production.conf")), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			rules = append(rules, strings.Fields(l))
		}
	}
	if len(rules) != 3 {
		t.Fatalf("rules %v", rules)
	}
	if !slices.Equal(rules[0], []string{"local", "all", "all", "trust"}) ||
		!slices.Equal(rules[1], []string{"hostssl", "all", "all", "samenet", "scram-sha-256"}) ||
		!slices.Equal(rules[2], []string{"host", "all", "all", "all", "reject"}) {
		t.Errorf("rules %v", rules)
	}
}

// The EMR overlay gives core the sealing PUBLIC key and its HMAC key, never
// the emr service's folders (the sealing private key, the secret store).
func TestEMROverlayForCore(t *testing.T) {
	c := loadComposeAs(t, filepath.Join("deploy", "compose.emr.yml"))
	if len(c.Services) != 1 {
		t.Fatalf("compose.emr.yml overrides %d services, want core only", len(c.Services))
	}
	core := c.Services["core"]
	if core.Environment["CORE_EMR_ENABLED"] != "true" || core.Environment["CORE_EMR_CONNECTOR_TYPES"] != "${CORE_EMR_CONNECTOR_TYPES:-fhir_r4,file_export}" {
		t.Errorf("core EMR settings %v", core.Environment)
	}
	for _, v := range core.Volumes {
		if !strings.HasSuffix(v, ":ro") || strings.HasPrefix(v, "./emr/") || strings.Contains(v, "emr-proxy") {
			t.Errorf("core mounts %q", v)
		}
	}
	for _, v := range loadDeployCompose(t).Services["emr"].Volumes {
		if strings.HasPrefix(v, "./emr/seal:") && !strings.HasSuffix(v, ":ro") {
			t.Errorf("emr mounts its sealing keys writable: %q", v)
		}
	}
}

// The deploy ships production mode's and the EMR's files with every release,
// checks production mode before anything restarts, and runs the EMR step
// after core is healthy. The step reads the start line without grep -q
// (PR #289: -q closes the pipe and fails the log stream).
func TestDeployShipsProductionAndEMR(t *testing.T) {
	wf := readRepoFile(t, filepath.Join(".github", "workflows", "deploy.yml"))
	for _, f := range []string{"compose.production.yml", "compose.emr.yml", "nats/tls.conf", "postgres/pg_hba.production.conf",
		"production/enable-production.sh", "production/prod-lib.sh", "production/check-mode.sh", "emr/enable-emr.sh",
		"emr/emr.sh", "tls/make-ca.sh", "tls/issue.sh", "tls/split-lib.sh", "file: services/emr/Dockerfile"} {
		if !strings.Contains(wf, f) {
			t.Errorf("deploy.yml does not ship %s", f)
		}
	}
	check, healthy, emr := strings.Index(wf, "bash production/check-mode.sh"), strings.Index(wf, "Backend healthy."), strings.Index(wf, "bash emr/emr.sh")
	if check < 0 || check > strings.Index(wf, "docker compose up -d --remove-orphans core") {
		t.Error("deploy.yml must check production mode before core restarts")
	}
	if emr < healthy {
		t.Error("deploy.yml runs the EMR step before core is healthy")
	}
	for _, f := range []string{"emr/emr.sh", "production/enable-production.sh", "production/prod-lib.sh"} {
		if s := readRepoFile(t, filepath.Join("deploy", f)); strings.Contains(s, "| grep -q") {
			t.Errorf("deploy/%s pipes into grep -q", f)
		}
	}
	if s := readRepoFile(t, filepath.Join("deploy", "emr", "emr.sh")); !strings.Contains(s, `"$(env_get EMR_ENABLED)" != true`) ||
		!strings.Contains(s, "grep 'emr serving' >/dev/null") {
		t.Error("emr/emr.sh must be gated on EMR_ENABLED=true and read the start line with grep >/dev/null")
	}
}
