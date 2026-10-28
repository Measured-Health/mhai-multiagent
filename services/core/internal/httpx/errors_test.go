package httpx

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serve runs h behind the full chain (request id, BFF, body limit).
func serve(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rt := NewRouter()
	rt.Handle(http.MethodPost, "/probe", h)
	rt.Handle(http.MethodGet, "/probe", h)
	rec := httptest.NewRecorder()
	rt.Handler([]string{testKey}).ServeHTTP(rec, req)
	return rec
}

const testKey = "test-only-bff-service-key-0123456789abcdef"

func bffRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-MHAI-Service", testKey)
	return req
}

func TestCodesCatalogue(t *testing.T) {
	want := map[string]int{
		"unauthenticated": 401, "token_expired": 401,
		"mfa_required": 403, "mfa_invalid": 403, "mfa_locked": 429,
		"forbidden": 403, "not_platform": 403, "not_found": 404,
		"conflict": 409, "validation_failed": 422, "weak_password": 422,
		"rate_limited": 429, "login_locked": 429, "not_ready": 503,
		"unavailable": 503, "internal_error": 500,
	}
	if len(Codes) != len(want) {
		t.Fatalf("Codes has %d entries, want %d", len(Codes), len(want))
	}
	for code, status := range want {
		if Codes[code] != status {
			t.Errorf("Codes[%q] = %d, want %d", code, Codes[code], status)
		}
	}
}

func TestProblemBody(t *testing.T) {
	rec := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Error(w, r, "forbidden")
	}), bffRequest(http.MethodGet, "/api/v1/probe", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content type %q", ct)
	}
	var p map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	rid := rec.Header().Get("X-Request-ID")
	if rid == "" || p["request_id"] != rid {
		t.Fatalf("request_id %v, header %q", p["request_id"], rid)
	}
	if p["code"] != "forbidden" || p["status"] != float64(403) {
		t.Fatalf("body %v", p)
	}
	if p["detail"] != details["forbidden"] || p["detail"] == "" {
		t.Fatalf("detail %v", p["detail"])
	}
	for _, k := range []string{"type", "title", "status", "code", "detail", "request_id"} {
		if _, ok := p[k]; !ok {
			t.Errorf("missing %q", k)
		}
	}
	if len(p) != 6 {
		t.Errorf("body has extra fields: %v", p)
	}
}

func TestEveryCodeHasFixedText(t *testing.T) {
	for code := range Codes {
		if details[code] == "" || titles[code] == "" {
			t.Errorf("%s: missing title or detail", code)
		}
	}
}

func TestUnknownCodeIsInternalError(t *testing.T) {
	rec := httptest.NewRecorder()
	Error(rec, httptest.NewRequest(http.MethodGet, "/", nil), "no such code <script>")
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), `"internal_error"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "script") {
		t.Fatal("body echoes the code argument")
	}
}

func TestPlatformBodyIsCodeOnly(t *testing.T) {
	req := bffRequest(http.MethodGet, "/api/v1/probe", "")
	req.Header.Set("X-MHAI-Audience", "platform")
	rec := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Error(w, r, "forbidden")
	}), req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rec.Body.String(); got != `{"code":"forbidden"}` {
		t.Fatalf("body %q", got)
	}
}

type loginBody struct {
	Username string `json:"username"`
}

func TestUnknownFieldRejected(t *testing.T) {
	var got loginBody
	rec := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !Decode(w, r, &got) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), bffRequest(http.MethodPost, "/api/v1/probe", `{"username":"a","x":1}`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", rec.Code)
	}
	var p map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p["code"] != "validation_failed" {
		t.Fatalf("code %v", p["code"])
	}
	if strings.Contains(rec.Body.String(), `"x"`) || strings.Contains(rec.Body.String(), "unknown field") {
		t.Fatalf("body echoes the input: %s", rec.Body.String())
	}
}

func TestDecodeRejectsTrailingAndEmpty(t *testing.T) {
	for _, body := range []string{``, `{"username":"a"} {"username":"b"}`, `[1]`, `{"username":`} {
		var got loginBody
		rec := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if Decode(w, r, &got) {
				w.WriteHeader(http.StatusNoContent)
			}
		}), bffRequest(http.MethodPost, "/api/v1/probe", body))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("body %q: status %d", body, rec.Code)
		}
	}
	var got loginBody
	rec := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Decode(w, r, &got) {
			w.WriteHeader(http.StatusNoContent)
		}
	}), bffRequest(http.MethodPost, "/api/v1/probe", `{"username":"a"}`))
	if rec.Code != http.StatusNoContent || got.Username != "a" {
		t.Fatalf("valid body: status %d, %+v", rec.Code, got)
	}
}

func TestBodyLimit(t *testing.T) {
	big := `{"username":"` + strings.Repeat("a", MaxBody) + `"}`
	called := false
	rec := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got loginBody
		if Decode(w, r, &got) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		}
	}), bffRequest(http.MethodPost, "/api/v1/probe", big))
	if called || rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized body: called=%v status %d", called, rec.Code)
	}
}

func TestUnknownRouteIsProblem404(t *testing.T) {
	rec := serve(t, http.NotFoundHandler(), bffRequest(http.MethodGet, "/api/v1/nope", ""))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"not_found"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing X-Request-ID")
	}
}

func TestRouterMountsUnderAPIv1(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusOK, map[string]string{"ok": "yes"})
	})
	if rec := serve(t, ok, bffRequest(http.MethodGet, "/api/v1/probe", "")); rec.Code != 200 {
		t.Fatalf("mounted route: %d", rec.Code)
	}
	if rec := serve(t, ok, bffRequest(http.MethodGet, "/probe", "")); rec.Code != 404 {
		t.Fatalf("unmounted path: %d", rec.Code)
	}
}

func TestJSONHelper(t *testing.T) {
	rec := httptest.NewRecorder()
	JSON(rec, http.StatusCreated, map[string]string{"a": "<b>"})
	if rec.Code != 201 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"a"`)) {
		t.Fatalf("body %s", rec.Body.String())
	}
}

// An unclean path to a real route is not redirected: it is a 404 problem.
func TestUncleanPathIsProblem404(t *testing.T) {
	for _, p := range []string{"/api/v1//probe", "/api/v1/x/../probe", "/api/v1/./probe"} {
		req := bffRequest(http.MethodGet, "/", "")
		req.URL.Path = p
		req.RequestURI = p
		rec := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}), req)
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"not_found"`) {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

// Spec §10.1: 503 unavailable is in the catalogue, with fixed text, and the
// platform audience gets the code only.
func TestUnavailableInCatalogue(t *testing.T) {
	if Codes["unavailable"] != http.StatusServiceUnavailable {
		t.Fatalf("Codes[unavailable] = %d", Codes["unavailable"])
	}
	if titles["unavailable"] != "Unavailable" || details["unavailable"] != "Couldn't get an answer. Try again." {
		t.Fatalf("text %q / %q", titles["unavailable"], details["unavailable"])
	}
	req := bffRequest(http.MethodGet, "/api/v1/probe", "")
	req.Header.Set("X-MHAI-Audience", "platform")
	rec := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Error(w, r, "unavailable")
	}), req)
	if rec.Code != 503 || rec.Body.String() != `{"code":"unavailable"}` {
		t.Fatalf("platform: %d %q", rec.Code, rec.Body.String())
	}
	rec = serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Error(w, r, "unavailable")
	}), bffRequest(http.MethodGet, "/api/v1/probe", ""))
	var p map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || rec.Code != 503 || p["code"] != "unavailable" ||
		p["title"] != "Unavailable" {
		t.Fatalf("clinical: %d %s", rec.Code, rec.Body.String())
	}
}

// ErrorRetryAfter adds Retry-After (whole seconds) to the same body Error
// writes; zero or less adds none.
func TestErrorRetryAfter(t *testing.T) {
	for _, c := range []struct {
		seconds int
		want    string
	}{{5, "5"}, {0, ""}, {-3, ""}} {
		rec := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ErrorRetryAfter(w, r, "unavailable", c.seconds)
		}), bffRequest(http.MethodGet, "/api/v1/probe", ""))
		if rec.Code != 503 || rec.Header().Get("Retry-After") != c.want {
			t.Errorf("%d: status %d Retry-After %q", c.seconds, rec.Code, rec.Header().Get("Retry-After"))
		}
		if !strings.Contains(rec.Body.String(), `"code":"unavailable"`) {
			t.Errorf("%d: body %s", c.seconds, rec.Body.String())
		}
	}
}
