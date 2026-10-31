package apidocs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	core "github.com/Measured-Health/mhai-multiagent/services/core"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
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
		PagePath:         "text/html; charset=utf-8",
		BundlePath:       "text/javascript; charset=utf-8",
		LogoPath:         "image/png",
		ShimPath:         "text/javascript; charset=utf-8",
		SpecPath:         "text/plain; charset=utf-8",
		IntelligencePath: "text/html; charset=utf-8",
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
	for _, path := range []string{PagePath, IntelligencePath, BundlePath, LogoPath, ShimPath, SpecPath} {
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

func TestSpecIsInlineText(t *testing.T) {
	rec := get(handler(true), http.MethodGet, SpecPath)
	if got, want := rec.Header().Get("Content-Disposition"), `inline; filename="openapi.yaml"`; got != want {
		t.Errorf("Content-Disposition %q, want %q", got, want)
	}
	if got := get(handler(true), http.MethodGet, PagePath).Header().Get("Content-Disposition"); got != "" {
		t.Errorf("page has Content-Disposition %q", got)
	}
}

func TestPageLoadsShimBeforeBundleAndHidesLogo(t *testing.T) {
	body := get(handler(true), http.MethodGet, PagePath).Body.String()
	si, bi := strings.Index(body, ShimPath), strings.Index(body, BundlePath)
	if si < 0 || bi < 0 || si > bi {
		t.Errorf("shim must load before the bundle (shim at %d, bundle at %d)", si, bi)
	}
	if !strings.Contains(body, `img[alt="redocly logo"]{display:none}`) {
		t.Error("page does not hide the Redocly logo")
	}
	if !strings.Contains(get(handler(true), http.MethodGet, ShimPath).Body.String(), "cdn.redoc.ly") {
		t.Error("shim does not block the Redocly CDN logo")
	}
}

func TestLogoIsAPNG(t *testing.T) {
	b := get(handler(true), http.MethodGet, LogoPath).Body.Bytes()
	if len(b) < 8 || string(b[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatal("logo is not a PNG")
	}
}

// Everything the page and the spec make the browser fetch must be same-origin
// (img-src 'self'): the page's own references, and the spec's x-logo.
func TestPageAndSpecReferenceOnlySameOrigin(t *testing.T) {
	h := handler(true)
	for _, p := range []string{PagePath, IntelligencePath} {
		page := get(h, http.MethodGet, p).Body.String()
		refs := regexp.MustCompile(`(?:src|href|action)="([^"]*)"`).FindAllStringSubmatch(page, -1)
		if len(refs) == 0 {
			t.Errorf("%s has no references to check", p)
		}
		for _, ref := range refs {
			if !strings.HasPrefix(ref[1], "/api/") && !strings.HasPrefix(ref[1], "#") {
				t.Errorf("%s references %q", p, ref[1])
			}
			if strings.HasPrefix(ref[1], "/api/") && get(h, http.MethodGet, ref[1]).Code != 200 {
				t.Errorf("%s references %q, which is not served", p, ref[1])
			}
		}
		for _, bad := range []string{"http://", "https://", "<script>", "onload", "onclick", "javascript:"} {
			if strings.Contains(page, bad) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
	}
	spec := get(h, http.MethodGet, SpecPath).Body.String()
	m := regexp.MustCompile(`(?m)^  x-logo:\n    url: (\S+)\n`).FindStringSubmatch(spec)
	if m == nil || m[1] != LogoPath {
		t.Fatalf("spec info.x-logo.url = %v, want %s", m, LogoPath)
	}
	if rec := get(h, http.MethodGet, m[1]); rec.Code != 200 {
		t.Errorf("x-logo url status %d", rec.Code)
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
	for _, path := range []string{PagePath, IntelligencePath, BundlePath, SpecPath, PagePath + "/x"} {
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

func TestIntelligencePageHasNoScript(t *testing.T) {
	if body := get(handler(true), http.MethodGet, IntelligencePath).Body.String(); strings.Contains(body, "<script") {
		t.Error("the intelligence page must carry no script")
	}
}

func TestTabsLinkBothPages(t *testing.T) {
	h := handler(true)
	for _, p := range []string{PagePath, IntelligencePath} {
		body := get(h, http.MethodGet, p).Body.String()
		for _, want := range []string{`<nav class="tabs" aria-label="API documentation">`, `href="` + PagePath + `"`, `href="` + IntelligencePath + `"`, `aria-current="page"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %s", p, want)
			}
		}
	}
}

// Drift: the page lists every operation in the table and every message in
// proto/ (the compiled descriptors are generated from the same files).
func TestIntelligencePageListsEveryOperationAndMessage(t *testing.T) {
	body := get(handler(true), http.MethodGet, IntelligencePath).Body.String()
	if len(authz.Operations()) == 0 {
		t.Fatal("no operations")
	}
	for _, o := range authz.Operations() {
		if !strings.Contains(body, "<code>"+o.Name+"</code>") {
			t.Errorf("page lacks operation %s", o.Name)
		}
	}
	for _, f := range []protoreflect.FileDescriptor{
		intelpb.File_mhai_intelligence_v1_authz_proto, intelpb.File_mhai_intelligence_v1_echo_proto,
		intelpb.File_mhai_intelligence_v1_envelope_proto, intelpb.File_mhai_intelligence_v1_pipeline_proto,
	} {
		ms := f.Messages()
		for i := 0; i < ms.Len(); i++ {
			if n := string(ms.Get(i).FullName()); !strings.Contains(body, `id="msg-`+n+`"`) {
				t.Errorf("page lacks message %s", n)
			}
		}
	}
	for _, c := range authz.ErrorCodes() {
		if !strings.Contains(body, "<code>"+c.Code+"</code>") {
			t.Errorf("page lacks error code %s", c.Code)
		}
	}
	for _, want := range []string{"never over HTTP", "core-signed request token", "test only"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestOnlyGetAndHeadServeIntelligence(t *testing.T) {
	h := handler(true)
	if rec := get(h, http.MethodHead, IntelligencePath); rec.Code != 200 || rec.Body.Len() != 0 {
		t.Errorf("HEAD: %d, %d bytes", rec.Code, rec.Body.Len())
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if rec := get(h, m, IntelligencePath); rec.Code != 404 {
			t.Errorf("%s: %d", m, rec.Code)
		}
	}
	if rec := get(h, http.MethodGet, IntelligencePath+"/"); rec.Code != 404 {
		t.Errorf("trailing slash: %d", rec.Code)
	}
}
