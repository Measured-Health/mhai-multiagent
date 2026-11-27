//go:build integration

package secrets

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// OpenBaoImage is the server the deploy runs (deploy/compose.openbao.yml
// pins the same digest; TestComposePinsOpenBao checks they agree).
const OpenBaoImage = "openbao/openbao:2.7.3@sha256:ba4b133eb9586630024e71a617e66f37f29cc3534cdd46a6233e098ceded8dc9"

func deployDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "deploy", "openbao")
}

// bao is a real OpenBao started with the deploy's own config, policies and
// configure.sh, initialised and unsealed; root and shares are in memory
// only, made by the server at run time.
type bao struct {
	c      testcontainers.Container
	addr   string
	p      pki
	admin  *http.Client
	root   string
	shares []string
}

func (b *bao) exec(t *testing.T, env map[string]string, cmd ...string) (int, string) {
	t.Helper()
	opts := []tcexec.ProcessOption{tcexec.Multiplexed()}
	var kv []string
	for k, v := range env {
		kv = append(kv, k+"="+v)
	}
	if len(kv) > 0 {
		opts = append(opts, tcexec.WithEnv(kv))
	}
	code, r, err := b.c.Exec(context.Background(), cmd, opts...)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(r)
	return code, string(out)
}

// http is a request to the TLS listener with our client certificate.
func (b *bao) http(t *testing.T, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, b.addr+"/v1/"+path, rd)
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	resp, err := b.admin.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (b *bao) unseal(t *testing.T) {
	t.Helper()
	for _, k := range b.shares[:3] {
		if code, out := b.http(t, http.MethodPut, "sys/unseal", "", map[string]string{"key": k}); code != 200 {
			t.Fatalf("unseal: %d %s", code, out)
		}
	}
}

func (b *bao) credentials(t *testing.T, mode string) (file string, roleID, secretID string) {
	t.Helper()
	code, out := b.exec(t, map[string]string{"BAO_TOKEN": b.root, "MHAI_ENV": "development"}, "sh", "/openbao/config/configure.sh", mode)
	if code != 0 {
		t.Fatalf("configure %s: %d", mode, code)
	}
	var c struct {
		RoleID   string `json:"role_id"`
		SecretID string `json:"secret_id"`
	}
	line := strings.TrimSpace(out[strings.LastIndex(strings.TrimSpace(out), "\n")+1:])
	if err := json.Unmarshal([]byte(line), &c); err != nil || c.RoleID == "" || c.SecretID == "" {
		t.Fatalf("configure %s gave no credentials", mode)
	}
	return writeCreds(t, c.RoleID, c.SecretID, 0o400), c.RoleID, c.SecretID
}

func startBao(t *testing.T) *bao {
	t.Helper()
	ctx := context.Background()
	p := newPKI(t, 0o444)
	files := []testcontainers.ContainerFile{
		{HostFilePath: p.CA, ContainerFilePath: "/openbao/tls/ca.pem", FileMode: 0o444},
		{HostFilePath: p.ServerCert, ContainerFilePath: "/openbao/tls/server.pem", FileMode: 0o444},
		{HostFilePath: p.ServerKey, ContainerFilePath: "/openbao/tls/server-key.pem", FileMode: 0o444},
	}
	for _, f := range []string{"openbao.hcl", "emr-policy.hcl", "backup-policy.hcl", "configure.sh"} {
		files = append(files, testcontainers.ContainerFile{HostFilePath: filepath.Join(deployDir(), f),
			ContainerFilePath: "/openbao/config/" + f, FileMode: 0o444})
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        OpenBaoImage,
			Entrypoint:   []string{"bao"},
			Cmd:          []string{"server", "-config=/openbao/config/openbao.hcl"},
			User:         "100:1000",
			ExposedPorts: []string{"8200/tcp"},
			Files:        files,
			Tmpfs:        map[string]string{"/tmp": "rw"},
			WaitingFor:   wait.ForLog("OpenBao server started").WithStartupTimeout(90 * time.Second),
		}})
	if err != nil {
		if c != nil {
			_ = testcontainers.TerminateContainer(c)
		}
		t.Fatalf("start openbao: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	host, _ := c.Host(ctx)
	if host != "localhost" && host != "127.0.0.1" {
		t.Skipf("docker host %q is not local (the test certificate names localhost)", host)
	}
	port, err := c.MappedPort(ctx, "8200/tcp")
	if err != nil {
		t.Fatal(err)
	}
	caPEM, _ := os.ReadFile(p.CA)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	pair, err := tls.LoadX509KeyPair(p.ClientCert, p.ClientKey)
	if err != nil {
		t.Fatal(err)
	}
	b := &bao{c: c, addr: "https://" + host + ":" + port.Port(), p: p,
		admin: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: pool, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}}}}
	code, out := b.exec(t, nil, "bao", "operator", "init", "-address=http://127.0.0.1:8201", "-format=json",
		"-key-shares=5", "-key-threshold=3")
	if code != 0 {
		t.Fatalf("init: %d", code)
	}
	var init struct {
		Keys []string `json:"unseal_keys_b64"`
		Root string   `json:"root_token"`
	}
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &init); err != nil || len(init.Keys) != 5 || init.Root == "" {
		t.Fatalf("init output: %v", err)
	}
	b.root, b.shares = init.Root, init.Keys
	b.unseal(t)
	if code, out := b.exec(t, map[string]string{"BAO_TOKEN": b.root, "MHAI_ENV": "development"},
		"sh", "/openbao/config/configure.sh", "setup"); code != 0 {
		t.Fatalf("configure setup: %d %s", code, out)
	}
	// Idempotent: a second setup changes nothing and succeeds.
	if code, _ := b.exec(t, map[string]string{"BAO_TOKEN": b.root, "MHAI_ENV": "development"},
		"sh", "/openbao/config/configure.sh", "setup"); code != 0 {
		t.Fatalf("second setup: %d", code)
	}
	return b
}

func (b *bao) store(t *testing.T, creds string) *OpenBao {
	t.Helper()
	s, err := NewOpenBao(OpenBaoConfig{Addr: b.addr, CAFile: b.p.CA, CertFile: b.p.ClientCert, KeyFile: b.p.ClientKey,
		CredentialsFile: creds, Env: "development"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The store against a real OpenBao, set up exactly as the deploy sets it
// up: the same semantics as the file store (versions, the 24 h keep, the
// wipe), the policy's limits, fail closed while sealed, recovery after the
// unseal, and no value in the audit log.
func TestOpenBaoIntegration(t *testing.T) {
	b := startBao(t)
	outer := t // files that outlive a subtest are made in the outer test's folders
	creds, roleID, _ := b.credentials(t, "emr-credentials")
	s := b.store(t, creds)
	ctx := context.Background()
	if err := s.Ready(ctx); err != nil {
		t.Fatalf("ready: %v", err)
	}
	now := time.Now()
	s.Now = func() time.Time { return now }
	p := path()

	t.Run("versions", func(t *testing.T) {
		if _, _, err := s.Get(ctx, p); !errors.Is(err, ErrNotFound) {
			t.Fatalf("get before put: %v", err)
		}
		v1, err := s.Put(ctx, p, seal.NewSecret([]byte(canary)))
		if err != nil {
			t.Fatal(err)
		}
		v2, err := s.Put(ctx, p, seal.NewSecret([]byte(canary+"-2")))
		if err != nil || v1 == v2 || !IsOpenBaoVersion(v1) || !IsOpenBaoVersion(v2) {
			t.Fatalf("versions %q %q %v", v1, v2, err)
		}
		got, ver, err := s.Get(ctx, p)
		if err != nil || ver != v2 || string(got.Bytes()) != canary+"-2" {
			t.Fatalf("get: %q %v", ver, err)
		}
		if old, err := s.GetVersion(ctx, p, v1); err != nil || string(old.Bytes()) != canary {
			t.Fatalf("previous version kept: %v", err)
		}
		// Within 24 h nothing goes; after, the old one goes and the
		// current one stays.
		if err := s.DeleteVersionsBefore(ctx, p, Keep); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetVersion(ctx, p, v1); err != nil {
			t.Fatalf("pruned within 24 h: %v", err)
		}
		now = now.Add(25 * time.Hour)
		if err := s.DeleteVersionsBefore(ctx, p, Keep); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetVersion(ctx, p, v1); !errors.Is(err, ErrNotFound) {
			t.Fatalf("old version after 25 h: %v", err)
		}
		if _, ver, err := s.Get(ctx, p); err != nil || ver != v2 {
			t.Fatalf("current after cleanup: %q %v", ver, err)
		}
		// A rolled-back put: the version before is current again.
		v3, err := s.Put(ctx, p, seal.NewSecret([]byte(canary+"-3")))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteVersion(ctx, p, v3); err != nil {
			t.Fatal(err)
		}
		if _, ver, err := s.Get(ctx, p); err != nil || ver != v2 {
			t.Fatalf("after a rolled-back put: %q %v", ver, err)
		}
		if err := s.DeleteVersion(ctx, p, v2); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Get(ctx, p); !errors.Is(err, ErrNotFound) {
			t.Fatalf("after dropping the only version: %v", err)
		}
		if err := s.DeleteVersion(ctx, p, "kv-999"); err != nil {
			t.Fatalf("dropping a missing version: %v", err)
		}
	})

	t.Run("wipe", func(t *testing.T) {
		other := p
		other.Connection = uuid.MustParse("dddddddd-0000-4000-8000-00000000000d")
		second := p
		second.Field = "client_secret"
		for _, q := range []Path{p, second, other} {
			if _, err := s.Put(ctx, q, seal.NewSecret([]byte(canary))); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.DeleteAll(ctx, p.Hospital, p.Connection); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteAll(ctx, p.Hospital, p.Connection); err != nil {
			t.Fatalf("second wipe: %v", err)
		}
		if err := s.DeleteAll(ctx, uuid.New(), uuid.New()); err != nil {
			t.Fatalf("wipe of nothing: %v", err)
		}
		for _, q := range []Path{p, second} {
			if _, _, err := s.Get(ctx, q); !errors.Is(err, ErrNotFound) {
				t.Fatalf("after wipe: %v", err)
			}
		}
		if _, _, err := s.Get(ctx, other); err != nil {
			t.Fatalf("other connection wiped: %v", err)
		}
	})

	t.Run("policy", func(t *testing.T) {
		code, out := b.http(t, http.MethodPost, "auth/approle/login", "", map[string]string{"role_id": roleID,
			"secret_id": mustSecret(t, creds)})
		var login struct {
			Auth struct {
				Token    string   `json:"client_token"`
				Policies []string `json:"token_policies"`
			} `json:"auth"`
		}
		if code != 200 || json.Unmarshal(out, &login) != nil {
			t.Fatalf("login: %d", code)
		}
		if strings.Join(login.Auth.Policies, ",") != "emr-development" {
			t.Fatalf("policies %v (no default policy)", login.Auth.Policies)
		}
		tok := login.Auth.Token
		h := p.Hospital.String()
		for _, r := range []struct{ method, path string }{
			{http.MethodPost, "mhai/data/staging/emr/" + h + "/" + p.Connection.String() + "/api_key"},
			{http.MethodPost, "mhai/data/production/emr/" + h + "/" + p.Connection.String() + "/api_key"},
			{http.MethodGet, "mhai/metadata/development/emr/" + h + "/?list=true"},
			{http.MethodGet, "mhai/metadata/development/emr/?list=true"},
			{http.MethodPost, "mhai/data/development/emr/" + h + "/api_key"},
			{http.MethodGet, "sys/mounts"},
			{http.MethodGet, "sys/storage/raft/snapshot"},
			{http.MethodGet, "cubbyhole/x"},
			{http.MethodPost, "auth/approle/role/emr-development/secret-id"},
		} {
			code, _ := b.http(t, r.method, r.path, tok, map[string]any{"data": map[string]string{"value": "eA=="}})
			if code != http.StatusForbidden {
				t.Errorf("%s %s: %d, want 403", r.method, r.path, code)
			}
		}
		// The backup role reads a snapshot, nothing else.
		bc, bRole, _ := b.credentials(t, "backup-credentials")
		code, out = b.http(t, http.MethodPost, "auth/approle/login", "", map[string]string{"role_id": bRole,
			"secret_id": mustSecret(t, bc)})
		if code != 200 || json.Unmarshal(out, &login) != nil {
			t.Fatalf("backup login: %d", code)
		}
		if code, snap := b.http(t, http.MethodGet, "sys/storage/raft/snapshot", login.Auth.Token, nil); code != 200 || len(snap) == 0 {
			t.Errorf("backup snapshot: %d", code)
		}
		if code, _ := b.http(t, http.MethodGet, "mhai/data/development/emr/"+h+"/"+p.Connection.String()+"/api_key",
			login.Auth.Token, nil); code != http.StatusForbidden {
			t.Errorf("backup read a secret: %d", code)
		}
	})

	t.Run("rotation", func(t *testing.T) {
		// A new secret_id, the older ones destroyed: the service reads the
		// file again at its next login; the old file no longer works.
		fresh, _, _ := b.credentials(outer, "emr-credentials")
		acc := accessorOf(t, b, fresh)
		if code, out := b.exec(t, map[string]string{"BAO_TOKEN": b.root, "MHAI_ENV": "development"},
			"sh", "/openbao/config/configure.sh", "prune", "emr-development", acc); code != 0 {
			t.Fatalf("prune: %d %s", code, out)
		}
		if err := b.store(t, fresh).Ready(ctx); err != nil {
			t.Fatalf("new credentials: %v", err)
		}
		if err := b.store(t, creds).Ready(ctx); !errors.Is(err, ErrStore) {
			t.Fatalf("old credentials after the rotation: %v", err)
		}
		creds = fresh
		s = b.store(outer, creds)
	})

	t.Run("sealed fails closed, unseal recovers", func(t *testing.T) {
		if _, err := s.Put(ctx, p, seal.NewSecret([]byte(canary))); err != nil {
			t.Fatal(err)
		}
		if code, _ := b.exec(t, map[string]string{"BAO_TOKEN": b.root}, "bao", "operator", "seal", "-address=http://127.0.0.1:8201"); code != 0 {
			t.Fatalf("seal: %d", code)
		}
		if err := s.Ready(ctx); !errors.Is(err, ErrStore) {
			t.Fatalf("ready while sealed: %v", err)
		}
		if _, _, err := s.Get(ctx, p); !errors.Is(err, ErrStore) {
			t.Fatalf("get while sealed: %v", err)
		}
		if _, err := s.Put(ctx, p, seal.NewSecret([]byte(canary))); !errors.Is(err, ErrStore) {
			t.Fatalf("put while sealed: %v", err)
		}
		b.unseal(t)
		// Unsealed, the node still elects itself leader (raft) before it
		// serves: ready within seconds, never before it is active.
		t0 := time.Now()
		for s.Ready(ctx) != nil {
			if time.Since(t0) > 30*time.Second {
				t.Fatal("not ready 30 s after the unseal")
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Logf("ready %v after the unseal", time.Since(t0).Round(10*time.Millisecond))
		if got, _, err := s.Get(ctx, p); err != nil || string(got.Bytes()) != canary {
			t.Fatalf("get after unseal: %v", err)
		}
	})

	t.Run("audit", func(t *testing.T) {
		enc := base64.StdEncoding.EncodeToString([]byte(canary))
		code, _ := b.exec(t, nil, "sh", "-c", "test -s /openbao/file/audit.log && ! grep -F -e '"+enc+"' -e '"+canary+"' /openbao/file/audit.log")
		if code != 0 {
			t.Fatal("the audit log is empty or holds a value in clear")
		}
	})

	t.Run("root revoked", func(t *testing.T) {
		if code, _ := b.exec(t, map[string]string{"BAO_TOKEN": b.root}, "bao", "token", "revoke", "-self", "-address=http://127.0.0.1:8201"); code != 0 {
			t.Fatalf("revoke: %d", code)
		}
		if code, _ := b.http(t, http.MethodGet, "sys/mounts", b.root, nil); code != http.StatusForbidden {
			t.Fatalf("revoked root still works: %d", code)
		}
		if err := s.Ready(ctx); err != nil {
			t.Fatalf("the service after the root is revoked: %v", err)
		}
	})
}

func mustSecret(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		SecretID string `json:"secret_id"`
	}
	_ = json.Unmarshal(b, &c)
	return c.SecretID
}

// accessorOf is the accessor of the secret_id in file.
func accessorOf(t *testing.T, b *bao, file string) string {
	t.Helper()
	code, out := b.http(t, http.MethodPost, "auth/approle/role/emr-development/secret-id/lookup", b.root,
		map[string]string{"secret_id": mustSecret(t, file)})
	var r struct {
		Data struct {
			Accessor string `json:"secret_id_accessor"`
		} `json:"data"`
	}
	if code != 200 || json.Unmarshal(out, &r) != nil || r.Data.Accessor == "" {
		t.Fatalf("lookup: %d", code)
	}
	return r.Data.Accessor
}
