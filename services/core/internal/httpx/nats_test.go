package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func natsRouter(h http.Handler) *Router {
	rt := NewRouter()
	rt.Handle(http.MethodGet, "/probe", h)
	rt.Handle(http.MethodPost, "/probe", h)
	return rt
}

// ViaNATS is the NATS path's proof of origin (spec §17.1): the audience is
// the one given (from the subject), never X-MHAI-Audience, and the client
// IP is read from X-MHAI-Client-IP as BFF does.
func TestViaNATSSetsAudienceAndClientIP(t *testing.T) {
	for _, aud := range []string{AudienceClinical, AudiencePlatform} {
		var s seen
		r := httptest.NewRequest(http.MethodGet, "/api/v1/probe", nil)
		r.Header.Set("X-MHAI-Client-IP", "198.51.100.9")
		other := AudiencePlatform
		if aud == AudiencePlatform {
			other = AudienceClinical
		}
		r.Header.Set("X-MHAI-Audience", other)
		r, err := ViaNATS(r, aud)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		natsRouter(probe(&s)).NATSHandler().ServeHTTP(rec, r)
		if !s.called || rec.Code != http.StatusNoContent || s.audience != aud || s.ip != netip.MustParseAddr("198.51.100.9") {
			t.Fatalf("%s: called=%v %d audience %q ip %v", aud, s.called, rec.Code, s.audience, s.ip)
		}
		if rec.Header().Get("X-Request-ID") == "" {
			t.Fatal("no request id")
		}
	}
	if _, err := ViaNATS(httptest.NewRequest(http.MethodGet, "/", nil), "admin"); err == nil {
		t.Fatal("unknown audience accepted")
	}
}

// NATSHandler has no BFF key check, so it refuses anything that wasn't
// marked by ViaNATS: a request that reaches it some other way (an HTTP
// listener wired to it by mistake) is 401, key or not.
func TestNATSHandlerRefusesUnmarked(t *testing.T) {
	var s seen
	for name, r := range map[string]*http.Request{
		"plain":   httptest.NewRequest(http.MethodGet, "/api/v1/probe", nil),
		"bff key": bffRequest(http.MethodGet, "/api/v1/probe", ""),
	} {
		rec := httptest.NewRecorder()
		natsRouter(probe(&s)).NATSHandler().ServeHTTP(rec, r)
		if s.called || rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: called=%v %d", name, s.called, rec.Code)
		}
	}
}

// The HTTP path still needs the key: a ViaNATS mark doesn't open it.
func TestHTTPHandlerStillNeedsKey(t *testing.T) {
	var s seen
	r, _ := ViaNATS(httptest.NewRequest(http.MethodGet, "/api/v1/probe", nil), AudienceClinical)
	rec := httptest.NewRecorder()
	natsRouter(probe(&s)).Handler([]string{testKey}).ServeHTTP(rec, r)
	if s.called || rec.Code != http.StatusUnauthorized {
		t.Fatalf("called=%v %d", s.called, rec.Code)
	}
}

// The NATS handler keeps the body limit, unknown routes and unclean paths
// of the HTTP chain.
func TestNATSHandlerKeepsChain(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got loginBody
		if Decode(w, r, &got) {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	do := func(method, path, body string) *httptest.ResponseRecorder {
		r, _ := ViaNATS(httptest.NewRequest(method, path, strings.NewReader(body)), AudienceClinical)
		rec := httptest.NewRecorder()
		natsRouter(h).NATSHandler().ServeHTTP(rec, r)
		return rec
	}
	if rec := do(http.MethodPost, "/api/v1/probe", `{"username":"`+strings.Repeat("a", MaxBody)+`"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized: %d", rec.Code)
	}
	if rec := do(http.MethodGet, "/api/v1/nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown: %d", rec.Code)
	}
	if rec := do(http.MethodGet, "/api/v1/x/../probe", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unclean: %d", rec.Code)
	}
}
