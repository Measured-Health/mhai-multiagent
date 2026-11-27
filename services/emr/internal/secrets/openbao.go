package secrets

// OpenBao is the production secret store (plan Task 12; H3, chosen by the
// owner on 27 Nov 2026): OpenBao's KV v2 engine mounted at mhai/, so a
// Path's String() is the secret's own path (mhai/<env>/emr/<hospital>/
// <connection>/<field> is read at mhai/data/<env>/emr/...).
//
//   - TLS 1.3 to the server with our private CA only, and this service's
//     client certificate (the server requires one from that CA).
//   - AppRole login: role_id and secret_id from a credentials file (0400,
//     this user's), read again at every login so a rotation needs no
//     restart. The token stays in memory only, is used until shortly
//     before its end, and is dropped on any 403 (one login and one retry).
//   - The policy (deploy/openbao/emr-policy.hcl) allows this environment's
//     emr paths and one connection's field list, nothing else.
//   - Versions are KV v2's, named kv-<n>. Put never drops one; the caller
//     prunes after its metadata commits, as with the file store. The
//     current version is the newest that is neither deleted nor destroyed.
//   - Every failure is one of the closed errors: no path, value, token,
//     address or server message reaches an error. Unreachable, sealed,
//     refused or malformed is ErrStore: the store fails closed.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// The KV v2 mount (deploy/openbao/configure.sh).
const openBaoMount = "mhai"

const (
	// openBaoTimeout bounds one request.
	openBaoTimeout = 5 * time.Second
	// maxResponse bounds a response body (a field's value is at most
	// seal.MaxValue; base64 and JSON add little).
	maxResponse = 1 << 20
	// maxCredentialsFile bounds the credentials file.
	maxCredentialsFile = 4096
	// tokenSlack: a token this close to its end is replaced first.
	tokenSlack = time.Minute
)

var kvVersion = regexp.MustCompile(`^kv-([1-9][0-9]{0,9})$`)

// IsOpenBaoVersion reports whether v names a version of this store (the
// move from the file store skips rows that already do).
func IsOpenBaoVersion(v string) bool { return kvVersion.MatchString(v) }

// OpenBaoConfig is what NewOpenBao needs; every file path is read at once
// (the certificates) or at each login (the credentials).
type OpenBaoConfig struct {
	// Addr is the server: https://host[:port], no path, no credentials.
	Addr string
	// CAFile is the CA the server's certificate must chain to (ours only).
	CAFile string
	// CertFile and KeyFile are this service's client certificate.
	CertFile, KeyFile string
	// CredentialsFile holds {"role_id": ..., "secret_id": ...}.
	CredentialsFile string
	// Env is APP_ENV: the only environment this store reads or writes.
	Env string
	// Timeout bounds one request; zero is 5 s.
	Timeout time.Duration
}

// OpenBao is the OpenBao store.
type OpenBao struct {
	base    *url.URL
	creds   string
	env     string
	timeout time.Duration
	client  *http.Client
	// Now dates the keep window; nil is time.Now.
	Now func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewOpenBao checks the settings and loads the TLS files. It doesn't reach
// the server: the service starts while OpenBao is sealed or down, and
// Ready reports it.
func NewOpenBao(c OpenBaoConfig) (*OpenBao, error) {
	if !envName.MatchString(c.Env) {
		return nil, ErrStore
	}
	u, err := url.Parse(c.Addr)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.Fragment != "" {
		return nil, ErrStore
	}
	u.Path = ""
	if c.CAFile == "" || c.CredentialsFile == "" || (c.CertFile == "") != (c.KeyFile == "") {
		return nil, ErrStore
	}
	pem, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, ErrStore
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, ErrStore
	}
	tc := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}
	if c.CertFile != "" {
		pair, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, ErrStore
		}
		tc.Certificates = []tls.Certificate{pair}
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = openBaoTimeout
	}
	tr := &http.Transport{
		// Never a proxy: the store is on its own internal network, and a
		// proxy from the environment would see the token.
		Proxy:                 nil,
		TLSClientConfig:       tc,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
	return &OpenBao{
		base: u, creds: c.CredentialsFile, env: c.Env, timeout: timeout,
		client: &http.Client{
			Transport: tr,
			Timeout:   timeout,
			// A redirect could carry the token elsewhere: refused.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (s *OpenBao) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// rel is p's path under the mount, after the engine's segment (data,
// metadata or destroy): <env>/emr/<hospital>/<connection>/<field>.
func (s *OpenBao) rel(p Path) (string, error) {
	if !p.valid() || p.Env != s.env {
		return "", ErrPath
	}
	return p.Env + "/emr/" + p.Hospital.String() + "/" + p.Connection.String() + "/" + p.Field, nil
}

// do sends one request and decodes a JSON reply into out (when non-nil and
// the status is 2xx). It returns the status; the error is ErrStore for
// anything but a reply.
func (s *OpenBao) do(ctx context.Context, method, path, token string, query url.Values, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, ErrStore
		}
		rd = bytes.NewReader(b)
		defer clear(b)
	}
	u := *s.base
	u.Path = "/v1/" + path
	u.RawQuery = query.Encode()
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return 0, ErrStore
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, ErrStore
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	defer clear(b)
	if err != nil || len(b) > maxResponse {
		return resp.StatusCode, ErrStore
	}
	if out != nil && resp.StatusCode/100 == 2 && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, ErrStore
		}
	}
	return resp.StatusCode, nil
}

// credentials reads the AppRole credentials: a regular file (not a link),
// not readable by group or others, small, with both ids.
func (s *OpenBao) credentials() (roleID, secretID string, err error) {
	f, err := os.OpenFile(s.creds, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", "", ErrStore
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 || st.Size() > maxCredentialsFile {
		return "", "", ErrStore
	}
	b, err := io.ReadAll(io.LimitReader(f, maxCredentialsFile+1))
	defer clear(b)
	if err != nil {
		return "", "", ErrStore
	}
	var c struct {
		RoleID   string `json:"role_id"`
		SecretID string `json:"secret_id"`
	}
	if json.Unmarshal(b, &c) != nil || c.RoleID == "" || c.SecretID == "" {
		return "", "", ErrStore
	}
	return c.RoleID, c.SecretID, nil
}

// login gets a token by AppRole.
func (s *OpenBao) login(ctx context.Context) (string, error) {
	roleID, secretID, err := s.credentials()
	if err != nil {
		return "", err
	}
	var out struct {
		Auth *struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int64  `json:"lease_duration"`
		} `json:"auth"`
	}
	code, err := s.do(ctx, http.MethodPost, "auth/approle/login", "", nil,
		map[string]string{"role_id": roleID, "secret_id": secretID}, &out)
	if err != nil || code != http.StatusOK || out.Auth == nil || out.Auth.ClientToken == "" || out.Auth.LeaseDuration <= 0 {
		return "", ErrStore
	}
	s.token = out.Auth.ClientToken
	s.expires = s.now().Add(time.Duration(out.Auth.LeaseDuration) * time.Second)
	return s.token, nil
}

// tok is a usable token: the one held, or a new login.
func (s *OpenBao) tok(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.now().Add(tokenSlack).Before(s.expires) {
		return s.token, nil
	}
	s.token = ""
	return s.login(ctx)
}

// drop forgets token t (refused by the server), unless another login
// already replaced it.
func (s *OpenBao) drop(t string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == t {
		s.token = ""
	}
}

// call is do with a token: a 403 drops it, and the call is tried once more
// with a new login (a token revoked or expired early). A 403 then is
// ErrStore.
func (s *OpenBao) call(ctx context.Context, method, path string, query url.Values, body any, out any) (int, error) {
	for attempt := 0; ; attempt++ {
		t, err := s.tok(ctx)
		if err != nil {
			return 0, err
		}
		code, err := s.do(ctx, method, path, t, query, body, out)
		if err != nil {
			return code, err
		}
		if code == http.StatusForbidden {
			s.drop(t)
			if attempt == 0 {
				continue
			}
			return code, ErrStore
		}
		return code, nil
	}
}

// kvVersionMeta is a field's KV v2 metadata: its live versions, oldest first.
type kvVersionMeta struct {
	n       int
	created time.Time
}

func (s *OpenBao) live(ctx context.Context, rel string) ([]kvVersionMeta, error) {
	var out struct {
		Data *struct {
			Versions map[string]struct {
				CreatedTime  string `json:"created_time"`
				DeletionTime string `json:"deletion_time"`
				Destroyed    bool   `json:"destroyed"`
			} `json:"versions"`
		} `json:"data"`
	}
	code, err := s.call(ctx, http.MethodGet, openBaoMount+"/metadata/"+rel, nil, nil, &out)
	if err != nil {
		return nil, err
	}
	switch {
	case code == http.StatusNotFound:
		return nil, nil
	case code != http.StatusOK || out.Data == nil:
		return nil, ErrStore
	}
	var vs []kvVersionMeta
	for k, v := range out.Data.Versions {
		n, err := strconv.Atoi(k)
		if err != nil || n < 1 {
			return nil, ErrStore
		}
		if v.Destroyed || v.DeletionTime != "" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, v.CreatedTime)
		if err != nil {
			return nil, ErrStore
		}
		vs = append(vs, kvVersionMeta{n, t})
	}
	slices.SortFunc(vs, func(a, b kvVersionMeta) int { return a.n - b.n })
	return vs, nil
}

func versionName(n int) string { return "kv-" + strconv.Itoa(n) }

func versionNumber(v string) (int, error) {
	m := kvVersion.FindStringSubmatch(v)
	if m == nil {
		return 0, ErrPath
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, ErrPath
	}
	return n, nil
}

// Put writes v as a new version of p. It never drops a version.
func (s *OpenBao) Put(ctx context.Context, p Path, v seal.Secret) (string, error) {
	rel, err := s.rel(p)
	if err != nil {
		return "", err
	}
	if v.Len() == 0 || v.Len() > seal.MaxValue {
		return "", ErrStore
	}
	enc := base64.StdEncoding.EncodeToString(v.Bytes())
	var out struct {
		Data *struct {
			Version int `json:"version"`
		} `json:"data"`
	}
	code, err := s.call(ctx, http.MethodPost, openBaoMount+"/data/"+rel, nil,
		map[string]any{"data": map[string]string{"value": enc}}, &out)
	if err != nil || code != http.StatusOK || out.Data == nil || out.Data.Version < 1 {
		return "", ErrStore
	}
	return versionName(out.Data.Version), nil
}

// Get reads p's current version: the newest live one.
func (s *OpenBao) Get(ctx context.Context, p Path) (seal.Secret, string, error) {
	rel, err := s.rel(p)
	if err != nil {
		return seal.Secret{}, "", err
	}
	vs, err := s.live(ctx, rel)
	if err != nil {
		return seal.Secret{}, "", err
	}
	if len(vs) == 0 {
		return seal.Secret{}, "", ErrNotFound
	}
	cur := versionName(vs[len(vs)-1].n)
	v, err := s.GetVersion(ctx, p, cur)
	return v, cur, err
}

// GetVersion reads one version of p.
func (s *OpenBao) GetVersion(ctx context.Context, p Path, version string) (seal.Secret, error) {
	rel, err := s.rel(p)
	if err != nil {
		return seal.Secret{}, err
	}
	if versionK.MatchString(version) {
		// The file store's (not moved yet): not set here.
		return seal.Secret{}, ErrNotFound
	}
	n, err := versionNumber(version)
	if err != nil {
		return seal.Secret{}, err
	}
	var out struct {
		Data *struct {
			Data *struct {
				Value string `json:"value"`
			} `json:"data"`
		} `json:"data"`
	}
	code, err := s.call(ctx, http.MethodGet, openBaoMount+"/data/"+rel, url.Values{"version": {strconv.Itoa(n)}}, nil, &out)
	if err != nil {
		return seal.Secret{}, err
	}
	switch {
	case code == http.StatusNotFound:
		return seal.Secret{}, ErrNotFound
	case code != http.StatusOK || out.Data == nil:
		return seal.Secret{}, ErrStore
	case out.Data.Data == nil:
		// A deleted or destroyed version answers 200 with no data on
		// some servers.
		return seal.Secret{}, ErrNotFound
	}
	b, err := base64.StdEncoding.DecodeString(out.Data.Data.Value)
	out.Data.Data.Value = ""
	if err != nil || len(b) == 0 || len(b) > seal.MaxValue {
		clear(b)
		return seal.Secret{}, ErrStore
	}
	sec := seal.NewSecret(b)
	clear(b)
	return sec, nil
}

// destroy drops versions of rel for good.
func (s *OpenBao) destroy(ctx context.Context, rel string, ns []int) error {
	if len(ns) == 0 {
		return nil
	}
	code, err := s.call(ctx, http.MethodPost, openBaoMount+"/destroy/"+rel, nil, map[string][]int{"versions": ns}, nil)
	if err != nil {
		return err
	}
	if code/100 != 2 && code != http.StatusNotFound {
		return ErrStore
	}
	return nil
}

// DeleteVersion drops one version of p; a missing one is fine.
func (s *OpenBao) DeleteVersion(ctx context.Context, p Path, version string) error {
	rel, err := s.rel(p)
	if err != nil {
		return err
	}
	n, err := versionNumber(version)
	if err != nil {
		return err
	}
	return s.destroy(ctx, rel, []int{n})
}

// DeleteAll removes every field of the connection, every version and its
// metadata; idempotent.
func (s *OpenBao) DeleteAll(ctx context.Context, hospital, connection uuid.UUID) error {
	if hospital == uuid.Nil || connection == uuid.Nil {
		return ErrPath
	}
	dir := s.env + "/emr/" + hospital.String() + "/" + connection.String()
	var out struct {
		Data *struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	code, err := s.call(ctx, http.MethodGet, openBaoMount+"/metadata/"+dir+"/", url.Values{"list": {"true"}}, nil, &out)
	if err != nil {
		return err
	}
	switch {
	case code == http.StatusNotFound:
		return nil
	case code != http.StatusOK || out.Data == nil:
		return ErrStore
	}
	for _, k := range out.Data.Keys {
		if !fieldKey.MatchString(k) {
			// Nothing but field names lives under a connection.
			return ErrStore
		}
		code, err := s.call(ctx, http.MethodDelete, openBaoMount+"/metadata/"+dir+"/"+k, nil, nil, nil)
		if err != nil {
			return err
		}
		if code/100 != 2 && code != http.StatusNotFound {
			return ErrStore
		}
	}
	return nil
}

// DeleteVersionsBefore drops p's live versions, other than the newest,
// written more than keep ago.
func (s *OpenBao) DeleteVersionsBefore(ctx context.Context, p Path, keep time.Duration) error {
	rel, err := s.rel(p)
	if err != nil {
		return err
	}
	vs, err := s.live(ctx, rel)
	if err != nil || len(vs) < 2 {
		return err
	}
	cutoff := s.now().Add(-keep)
	var old []int
	for _, v := range vs[:len(vs)-1] {
		if v.created.Before(cutoff) {
			old = append(old, v.n)
		}
	}
	return s.destroy(ctx, rel, old)
}

// Ready checks the server is up and unsealed and that this service can log
// in and use its token.
func (s *OpenBao) Ready(ctx context.Context) error {
	var h struct {
		Initialized bool  `json:"initialized"`
		Sealed      *bool `json:"sealed"`
	}
	code, err := s.do(ctx, http.MethodGet, "sys/health", "", nil, nil, &h)
	if err != nil || code != http.StatusOK || !h.Initialized || h.Sealed == nil || *h.Sealed {
		return ErrStore
	}
	var self struct {
		Data *struct {
			TTL int64 `json:"ttl"`
		} `json:"data"`
	}
	code, err = s.call(ctx, http.MethodGet, "auth/token/lookup-self", nil, nil, &self)
	if err != nil || code != http.StatusOK || self.Data == nil {
		return ErrStore
	}
	return nil
}
