package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDeprecatedSetsHeaders(t *testing.T) {
	dep := time.Date(2027, 1, 10, 0, 0, 0, 0, time.UTC)
	sun := dep.AddDate(0, 0, 90)
	h := Deprecated(dep, sun, "https://docs.example.test/migrate")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Error(w, r, "not_found") // headers also reach error responses
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/x", nil))
	if rec.Code != 404 {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rec.Header().Get("Deprecation"); got != "@1799539200" {
		t.Errorf("Deprecation %q", got)
	}
	if got := rec.Header().Get("Sunset"); got != "Sat, 10 Apr 2027 00:00:00 GMT" {
		t.Errorf("Sunset %q", got)
	}
	if got := rec.Header().Get("Link"); got != `<https://docs.example.test/migrate>; rel="deprecation"; type="text/html"` {
		t.Errorf("Link %q", got)
	}
}

func TestDeprecatedRejectsBadInput(t *testing.T) {
	d := time.Date(2027, 1, 10, 0, 0, 0, 0, time.UTC)
	for name, f := range map[string]func(){
		"sunset before": func() { Deprecated(d, d.Add(-time.Hour), "/x") },
		"sunset equal":  func() { Deprecated(d, d, "/x") },
		"empty link":    func() { Deprecated(d, d.Add(time.Hour), "") },
		"header inject": func() { Deprecated(d, d.Add(time.Hour), "/x\r\nSet-Cookie: a=b") },
		"bracket":       func() { Deprecated(d, d.Add(time.Hour), "/x>; rel=evil") },
		"relative":      func() { Deprecated(d, d.Add(time.Hour), "migrate") },
		"scheme":        func() { Deprecated(d, d.Add(time.Hour), "javascript:alert(1)") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			f()
		}()
	}
}
