package secrets

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// fakeBao is a TLS server that answers the login and lets each test choose
// the rest; it requires a client certificate from the test CA, as the real
// listener does.
type fakeBao struct {
	srv      *httptest.Server
	logins   atomic.Int32
	roleID   string
	secretID string
	token    string
	h        atomic.Pointer[http.HandlerFunc]
}

// setHandle swaps the handler (a slow request of the case before may still
// be reading it).
func (f *fakeBao) setHandle(h http.HandlerFunc) { f.h.Store(&h) }

func newFake(t *testing.T, p pki) *fakeBao {
	t.Helper()
	f := &fakeBao{roleID: randomID(t), secretID: randomID(t), token: randomID(t)}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/approle/login" {
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["role_id"] != f.roleID || in["secret_id"] != f.secretID {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.logins.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": f.token, "lease_duration": 900}})
			return
		}
		if r.URL.Path != "/v1/sys/health" && r.Header.Get("X-Vault-Token") != f.token {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		(*f.h.Load())(w, r)
	}))
	pair, err := tls.LoadX509KeyPair(p.ServerCert, p.ServerKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM, _ := os.ReadFile(p.CA)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS13}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

// okHandler answers health (unsealed) and lookup-self.
func okHandler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/sys/health":
		_, _ = w.Write([]byte(`{"initialized":true,"sealed":false}`))
	default:
		_, _ = w.Write([]byte(`{"data":{"ttl":900}}`))
	}
}

func (f *fakeBao) store(t *testing.T, p pki) *OpenBao {
	t.Helper()
	s, err := NewOpenBao(OpenBaoConfig{Addr: f.srv.URL, CAFile: p.CA, CertFile: p.ClientCert, KeyFile: p.ClientKey,
		CredentialsFile: writeCreds(t, f.roleID, f.secretID, 0o400), Env: "development", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The settings are checked before anything is reached; the errors are the
// closed ones and name no address or file.
func TestOpenBaoConfigRefusals(t *testing.T) {
	p := newPKI(t, 0o600)
	creds := writeCreds(t, randomID(t), randomID(t), 0o400)
	good := OpenBaoConfig{Addr: "https://openbao:8200", CAFile: p.CA, CertFile: p.ClientCert, KeyFile: p.ClientKey,
		CredentialsFile: creds, Env: "production"}
	if _, err := NewOpenBao(good); err != nil {
		t.Fatalf("good settings refused: %v", err)
	}
	for name, edit := range map[string]func(c *OpenBaoConfig){
		"plain http":             func(c *OpenBaoConfig) { c.Addr = "http://openbao:8200" },
		"credentials in the url": func(c *OpenBaoConfig) { c.Addr = "https://emr:" + randomID(t) + "@openbao:8200" },
		"a path":                 func(c *OpenBaoConfig) { c.Addr = "https://openbao:8200/v1/mhai" },
		"a query":                func(c *OpenBaoConfig) { c.Addr = "https://openbao:8200?x=1" },
		"no host":                func(c *OpenBaoConfig) { c.Addr = "https://" },
		"no CA":                  func(c *OpenBaoConfig) { c.CAFile = "" },
		"a CA file that is not":  func(c *OpenBaoConfig) { c.CAFile = creds },
		"cert without key":       func(c *OpenBaoConfig) { c.KeyFile = "" },
		"a key that doesn't fit": func(c *OpenBaoConfig) { c.KeyFile = p.OtherClientK },
		"no credentials file":    func(c *OpenBaoConfig) { c.CredentialsFile = "" },
		"unknown env":            func(c *OpenBaoConfig) { c.Env = "prod" },
	} {
		c := good
		edit(&c)
		_, err := NewOpenBao(c)
		if !errors.Is(err, ErrStore) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if strings.Contains(err.Error(), "openbao") || strings.Contains(err.Error(), p.dir) {
			t.Errorf("%s: the error names the address or a file: %v", name, err)
		}
	}
}

// Sealed, down, refused, slow, redirected or malformed: ErrStore, every
// time (the connection is then unreachable, design table). Nothing of the
// path, the token or the server's message reaches the error.
func TestOpenBaoFailsClosed(t *testing.T) {
	p := newPKI(t, 0o600)
	f := newFake(t, p)
	s := f.store(t, p)
	ctx := context.Background()
	pp := path()
	cases := map[string]func(w http.ResponseWriter, r *http.Request){
		"sealed": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"errors":["Vault is sealed"]}`))
		},
		"server error": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"forbidden":    func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
		"not json":     func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://elsewhere.invalid/steal", http.StatusTemporaryRedirect)
		},
		"slow": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(5 * time.Second):
			case <-r.Context().Done():
			}
		},
		"huge": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":{"data":{"value":"` + strings.Repeat("A", maxResponse+10) + `"}}}`))
		},
		"bad base64": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":{"data":{"value":"%%%"},"metadata":{"version":1}}}`))
		},
	}
	for name, h := range cases {
		f.setHandle(h)
		t0 := time.Now()
		_, err := s.GetVersion(ctx, pp, "kv-1")
		if !errors.Is(err, ErrStore) {
			t.Errorf("%s: GetVersion %v", name, err)
		}
		if time.Since(t0) > 4*time.Second {
			t.Errorf("%s: took %v (the timeout is 2 s)", name, time.Since(t0))
		}
		if _, err := s.Put(ctx, pp, seal.NewSecret([]byte(canary))); !errors.Is(err, ErrStore) {
			t.Errorf("%s: Put %v", name, err)
		}
		if err := s.Ready(ctx); !errors.Is(err, ErrStore) {
			t.Errorf("%s: Ready %v", name, err)
		}
		for _, e := range []error{err} {
			if e != nil && (strings.Contains(e.Error(), pp.Hospital.String()) || strings.Contains(e.Error(), f.token) ||
				strings.Contains(e.Error(), "sealed") || strings.Contains(e.Error(), canary)) {
				t.Errorf("%s: the error says too much: %v", name, e)
			}
		}
	}
	// Down altogether.
	f.srv.Close()
	if err := s.Ready(ctx); !errors.Is(err, ErrStore) {
		t.Errorf("down: %v", err)
	}
}

// A refused token is dropped and the call tried once more after a new
// login (the token was revoked); a second refusal is ErrStore, with no
// loop.
func TestOpenBaoForbiddenLogsInOnce(t *testing.T) {
	p := newPKI(t, 0o600)
	f := newFake(t, p)
	s := f.store(t, p)
	var calls atomic.Int32
	f.setHandle(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"data":{"value":"` + "c3ludGhldGlj" + `"}}}`))
	})
	if _, err := s.GetVersion(context.Background(), path(), "kv-3"); err != nil {
		t.Fatalf("retry after a 403: %v", err)
	}
	if f.logins.Load() != 2 || calls.Load() != 2 {
		t.Fatalf("logins %d calls %d, want 2 and 2", f.logins.Load(), calls.Load())
	}
	f.setHandle(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	calls.Store(0)
	if _, err := s.GetVersion(context.Background(), path(), "kv-3"); !errors.Is(err, ErrStore) || calls.Load() != 0 {
		// calls counts only the first handler; the 403 one isn't counted.
		t.Fatalf("always 403: %v", err)
	}
}

// The credentials file must be this user's alone; a looser one, a link or
// one without both ids is refused, and nothing is sent.
func TestOpenBaoCredentialsFile(t *testing.T) {
	p := newPKI(t, 0o600)
	f := newFake(t, p)
	f.setHandle(okHandler)
	ctx := context.Background()
	mk := func(file string) *OpenBao {
		s, err := NewOpenBao(OpenBaoConfig{Addr: f.srv.URL, CAFile: p.CA, CertFile: p.ClientCert, KeyFile: p.ClientKey,
			CredentialsFile: file, Env: "development"})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	loose := writeCreds(t, f.roleID, f.secretID, 0o644)
	link := loose + ".link"
	if err := os.Symlink(writeCreds(t, f.roleID, f.secretID, 0o400), link); err != nil {
		t.Fatal(err)
	}
	half := writeCreds(t, f.roleID, "", 0o400)
	for name, file := range map[string]string{"0644": loose, "symlink": link, "no secret_id": half, "missing": loose + ".nope"} {
		if err := mk(file).Ready(ctx); !errors.Is(err, ErrStore) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if f.logins.Load() != 0 {
		t.Fatalf("%d logins with a refused file", f.logins.Load())
	}
	// The rotation: the file is read again at each login.
	good := writeCreds(t, f.roleID, f.secretID, 0o400)
	if err := mk(good).Ready(ctx); err != nil {
		t.Fatal(err)
	}
}

// TLS: our CA only, and the server wants a client certificate.
func TestOpenBaoTLS(t *testing.T) {
	p := newPKI(t, 0o600)
	f := newFake(t, p)
	f.setHandle(okHandler)
	creds := writeCreds(t, f.roleID, f.secretID, 0o400)
	ctx := context.Background()
	for name, c := range map[string]OpenBaoConfig{
		"another CA":             {CAFile: p.OtherCA, CertFile: p.ClientCert, KeyFile: p.ClientKey},
		"no client certificate":  {CAFile: p.CA},
		"a client of another CA": {CAFile: p.CA, CertFile: p.OtherClientCert, KeyFile: p.OtherClientK},
	} {
		c.Addr, c.CredentialsFile, c.Env = f.srv.URL, creds, "development"
		s, err := NewOpenBao(c)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Ready(ctx); !errors.Is(err, ErrStore) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if f.logins.Load() != 0 {
		t.Fatalf("a login got through: %d", f.logins.Load())
	}
	if err := f.store(t, p).Ready(ctx); err != nil {
		t.Fatalf("our CA and client certificate: %v", err)
	}
}

// The environment and the path are checked before any request.
func TestOpenBaoPaths(t *testing.T) {
	p := newPKI(t, 0o600)
	f := newFake(t, p)
	var hits atomic.Int32
	f.setHandle(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) })
	s := f.store(t, p)
	ctx := context.Background()
	other := path()
	other.Env = "production"
	bad := path()
	bad.Field = "../api_key"
	for _, q := range []Path{other, bad} {
		if _, err := s.Put(ctx, q, seal.NewSecret([]byte(canary))); !errors.Is(err, ErrPath) {
			t.Errorf("%+v: %v", q, err)
		}
	}
	for _, v := range []string{"", "kv-0", "kv-01", "v1", "kv-1/../x", "kv-99999999999"} {
		if _, err := s.GetVersion(ctx, path(), v); !errors.Is(err, ErrPath) {
			t.Errorf("version %q: %v", v, err)
		}
	}
	// A file store version (not moved yet) is not set here; nothing is sent.
	if _, err := s.GetVersion(ctx, path(), "v0000000000000000001-00000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a file store version: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests for refused paths", hits.Load())
	}
}
