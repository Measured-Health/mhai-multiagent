package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// seen records what a handler behind BFF read from the context.
type seen struct {
	called   bool
	ip       netip.Addr
	audience string
}

func probe(s *seen) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.called = true
		s.ip = ClientIP(r.Context())
		s.audience = Audience(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
}

func TestBFFRejectsMissingKey(t *testing.T) {
	var s seen
	rec := httptest.NewRecorder()
	BFF([]string{testKey})(probe(&s)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if s.called || rec.Code != http.StatusUnauthorized {
		t.Fatalf("called=%v status %d", s.called, rec.Code)
	}
	var p map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p["code"] != "unauthenticated" {
		t.Fatalf("body %s (%v)", rec.Body.String(), err)
	}
}

func TestBFFRejectsWrongKeys(t *testing.T) {
	for name, set := range map[string]func(h http.Header){
		"wrong":       func(h http.Header) { h.Set("X-MHAI-Service", "not-the-key") },
		"prefix":      func(h http.Header) { h.Set("X-MHAI-Service", testKey[:len(testKey)-1]) },
		"longer":      func(h http.Header) { h.Set("X-MHAI-Service", testKey+"x") },
		"empty":       func(h http.Header) { h.Set("X-MHAI-Service", "") },
		"two headers": func(h http.Header) { h.Add("X-MHAI-Service", testKey); h.Add("X-MHAI-Service", "other") },
	} {
		var s seen
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		set(req.Header)
		rec := httptest.NewRecorder()
		BFF([]string{testKey})(probe(&s)).ServeHTTP(rec, req)
		if s.called || rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: called=%v status %d", name, s.called, rec.Code)
		}
	}
}

func TestBFFNoKeysRejectsAll(t *testing.T) {
	for _, keys := range [][]string{nil, {""}} {
		var s seen
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-MHAI-Service", "")
		rec := httptest.NewRecorder()
		BFF(keys)(probe(&s)).ServeHTTP(rec, req)
		if s.called || rec.Code != http.StatusUnauthorized {
			t.Errorf("keys %q: called=%v status %d", keys, s.called, rec.Code)
		}
	}
}

func TestBFFAcceptsAnyConfiguredKey(t *testing.T) {
	other := "test-only-second-bff-key-fedcba9876543210"
	for _, k := range []string{testKey, other} {
		var s seen
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-MHAI-Service", k)
		rec := httptest.NewRecorder()
		BFF([]string{testKey, other})(probe(&s)).ServeHTTP(rec, req)
		if !s.called || rec.Code != http.StatusNoContent {
			t.Errorf("key rejected: status %d", rec.Code)
		}
	}
}

// Trusted headers on a request without the key are never read: the request
// is refused, and the refusal is a clinical problem body even when it claims
// the platform audience.
func TestClientIPIgnoredWithoutKey(t *testing.T) {
	var s seen
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-MHAI-Client-IP", "203.0.113.7")
	req.Header.Set("X-MHAI-Audience", "platform")
	req.Header.Set("X-MHAI-Service", "wrong")
	rec := httptest.NewRecorder()
	BFF([]string{testKey})(probe(&s)).ServeHTTP(rec, req)
	if s.called || rec.Code != http.StatusUnauthorized {
		t.Fatalf("called=%v status %d", s.called, rec.Code)
	}
	if rec.Body.String() == `{"code":"unauthenticated"}` {
		t.Fatal("audience header honoured without the key")
	}
	// Outside BFF the context holds nothing, whatever the headers say.
	if ip := ClientIP(req.Context()); ip.IsValid() {
		t.Fatalf("ClientIP without BFF = %v", ip)
	}
	if a := Audience(req.Context()); a != "clinical" {
		t.Fatalf("Audience without BFF = %q", a)
	}
}

func TestBFFReadsTrustedHeaders(t *testing.T) {
	cases := []struct {
		ip, aud string
		wantIP  string
		wantAud string
	}{
		{"203.0.113.7", "platform", "203.0.113.7", "platform"},
		{" 2001:db8::1 ", "clinical", "2001:db8::1", "clinical"},
		{"::ffff:198.51.100.9", "", "198.51.100.9", "clinical"},
		{"fe80::1%eth0", "PLATFORM", "fe80::1", "clinical"},
		{"not-an-ip", "other", "", "clinical"},
		{"203.0.113.7, 10.0.0.1", "", "", "clinical"},
		{"", "", "", "clinical"},
	}
	for _, c := range cases {
		var s seen
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-MHAI-Service", testKey)
		if c.ip != "" {
			req.Header.Set("X-MHAI-Client-IP", c.ip)
		}
		if c.aud != "" {
			req.Header.Set("X-MHAI-Audience", c.aud)
		}
		rec := httptest.NewRecorder()
		BFF([]string{testKey})(probe(&s)).ServeHTTP(rec, req)
		if !s.called {
			t.Fatalf("%q: not called", c.ip)
		}
		gotIP := ""
		if s.ip.IsValid() {
			gotIP = s.ip.String()
		}
		if gotIP != c.wantIP || s.audience != c.wantAud {
			t.Errorf("ip %q aud %q: got %q %q, want %q %q", c.ip, c.aud, gotIP, s.audience, c.wantIP, c.wantAud)
		}
	}
}

func TestBFFTwoClientIPHeadersIsMissing(t *testing.T) {
	var s seen
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-MHAI-Service", testKey)
	req.Header.Add("X-MHAI-Client-IP", "203.0.113.7")
	req.Header.Add("X-MHAI-Client-IP", "198.51.100.1")
	BFF([]string{testKey})(probe(&s)).ServeHTTP(httptest.NewRecorder(), req)
	if s.ip.IsValid() {
		t.Fatalf("got %v", s.ip)
	}
}

func TestRequestIDOnRejection(t *testing.T) {
	rt := NewRouter()
	rec := httptest.NewRecorder()
	rt.Handler([]string{testKey}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/x", nil))
	if rec.Code != 401 || rec.Header().Get("X-Request-ID") == "" {
		t.Fatalf("status %d, request id %q", rec.Code, rec.Header().Get("X-Request-ID"))
	}
	var p map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p["request_id"] != rec.Header().Get("X-Request-ID") {
		t.Fatalf("request_id %v", p["request_id"])
	}
}
