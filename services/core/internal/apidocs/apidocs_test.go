package apidocs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/Measured-Health/mhai-multiagent/services/core"
)

const bundleSHA256 = "dcaf76612bc4a3fbcc923a8966dee2f6146a5f32e5ce1b6f02dd60cbbf89500b"

func handler(enabled bool) http.Handler {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Next", "1")
		w.WriteHeader(http.StatusTeapot)
	})
	return Wrap(enabled, core.OpenAPI, next)
}

func get(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(method, "http://core.test/", nil)
	r.URL.Path = path
	r.URL.RawPath = ""
	h.ServeHTTP(rec, r)
	return rec
}

func TestOnServesPageBundleAndSpec(t *testing.T) {
	h := handler(true)
	for path, ct := range map[string]string{
		PagePath:   "text/html; charset=utf-8",
		BundlePath: "text/javascript; charset=utf-8",
		SpecPath:   "application/yaml; charset=utf-8",
	} {
		rec := get(h, http.MethodGet, path)
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != ct {
			t.Errorf("%s: content type %q, want %q", path, got, ct)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty body", path)
		}
	}
}

func TestHeaders(t *testing.T) {
	h := handler(true)
	want := map[string]string{
		"Content-Security-Policy": CSP,
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "no-store",
	}
	for _, path := range []string{PagePath, BundlePath, SpecPath} {
		rec := get(h, http.MethodGet, path)
		for k, v := range want {
			if got := rec.Header().Get(k); got != v {
				t.Errorf("%s: %s = %q, want %q", path, k, got, v)
			}
		}
	}
	for _, need := range []string{"script-src 'self';", "frame-ancestors 'none'", "default-src 'none'", "base-uri 'none'"} {
		if !strings.Contains(CSP, need) {
			t.Errorf("CSP lacks %s", need)
		}
	}
	// scripts must never be loosened; only styles may be inline.
	if strings.Contains(CSP, "script-src 'self' ") || strings.Contains(CSP, "unsafe-eval") || strings.Contains(CSP, "http") {
		t.Errorf("CSP too loose: %s", CSP)
	}
	body := get(h, http.MethodGet, PagePath).Body.String()
	for _, bad := range []string{"http://", "https://", "<script>", "onload"} {
		if strings.Contains(body, bad) {
			t.Errorf("page contains %q", bad)
		}
	}
}

func TestServedSpecIsTheFile(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := get(handler(true), http.MethodGet, SpecPath).Body.Bytes(); string(got) != string(want) {
		t.Fatal("served openapi.yaml differs from services/core/openapi.yaml")
	}
}

func TestBundleIsThePinnedOne(t *testing.T) {
	sum := sha256.Sum256(get(handler(true), http.MethodGet, BundlePath).Body.Bytes())
	if got := hex.EncodeToString(sum[:]); got != bundleSHA256 {
		t.Fatalf("bundle sha256 %s, want %s (update assets/README.md with it)", got, bundleSHA256)
	}
}

func TestOffIs404Problem(t *testing.T) {
	h := handler(false)
	for _, path := range []string{PagePath, BundlePath, SpecPath, PagePath + "/x"} {
		rec := get(h, http.MethodGet, path)
		if rec.Code != 404 {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s: content type %q", path, ct)
		}
		var p struct{ Code string }
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p.Code != "not_found" {
			t.Errorf("%s: body %q", path, rec.Body)
		}
		if rec.Header().Get("X-Next") != "" {
			t.Errorf("%s reached the API chain", path)
		}
	}
}

func TestRefusedPaths(t *testing.T) {
	h := handler(true)
	for _, path := range []string{
		"/api/docs/", "/api/docs/../", "/api/docs/..", "/api/docs/../v1/healthz", "/api/docs/./redoc.standalone.js",
		"/api/docs/%2e%2e/", "/api/docs//redoc.standalone.js", "/api/docs/assets/../../../etc/passwd",
		"/api/docs/redoc.standalone.js/", "/api/docs/README.md", "/api/docs/redoc.LICENSE",
		"/api/docs/..%2f..%2fopenapi.yaml", "/api/docs/../openapi.yaml",
	} {
		rec := get(h, http.MethodGet, path)
		if rec.Code != 404 || rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("%s: status %d type %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	// Over a real server, where net/http parses the request line.
	srv := httptest.NewServer(h)
	defer srv.Close()
	for _, raw := range []string{"/api/docs/../", "/api/docs/%2e%2e/", "/api/docs/..%2f"} {
		resp, err := http.Get(srv.URL + raw)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Errorf("%s served", raw)
		}
	}
}

func TestMethodsAndPassthrough(t *testing.T) {
	h := handler(true)
	if rec := get(h, http.MethodPost, PagePath); rec.Code != 404 {
		t.Errorf("POST: %d", rec.Code)
	}
	if rec := get(h, http.MethodHead, PagePath); rec.Code != 200 || rec.Body.Len() != 0 {
		t.Errorf("HEAD: %d, %d body bytes", rec.Code, rec.Body.Len())
	}
	for _, path := range []string{"/api/v1/healthz", "/api/docsx", "/api/openapi.yaml.bak", "/api/openapi.yaml/", "/api"} {
		if rec := get(h, http.MethodGet, path); rec.Code != http.StatusTeapot {
			t.Errorf("%s: %d, want pass-through", path, rec.Code)
		}
	}
}
