package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// POST /auth/password/reset refuses a malformed body, and fails closed
// without the client IP or the attempt counters, before reading any token.
func TestResetPasswordRefusalsBeforeAnyLookup(t *testing.T) {
	key := "unit-test-bff-key-" + strings.Repeat("k", 24)
	s := &Service{} // no pool: a lookup would panic
	h := httpx.BFF([]string{key})(http.HandlerFunc(s.resetPassword))
	long := strings.Repeat("t", maxTokenLen+1)
	// Built at run time: synthetic, low-entropy, and not secret-shaped.
	pw := strings.Repeat("synth ", 3)
	ok := func(token string) string { return `{"token": "` + token + `", "new_password": "` + pw + `"}` }
	for _, c := range []struct {
		name, body, ip, code string
		status               int
	}{
		{"unknown field", `{"token": "x", "new_password": "y", "user_id": "z"}`, "198.51.100.9", "validation_failed", 422},
		{"no token", `{"new_password": "` + pw + `"}`, "198.51.100.9", "validation_failed", 422},
		{"no password", `{"token": "x"}`, "198.51.100.9", "validation_failed", 422},
		{"token too long", ok(long), "198.51.100.9", "validation_failed", 422},
		{"no client IP", ok("x"), "", "not_ready", 503},
		{"no counters", ok("x"), "198.51.100.9", "not_ready", 503},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password/reset", strings.NewReader(c.body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-MHAI-Service", key)
		if c.ip != "" {
			r.Header.Set("X-MHAI-Client-IP", c.ip)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.status || !strings.Contains(w.Body.String(), `"`+c.code+`"`) {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body.String())
		}
	}
}

// After a link works, the reply names the app to sign in to: a platform
// account (no hospital) the platform admin app, a hospital's the clinical
// app.
func TestResetSignInApp(t *testing.T) {
	h := uuid.New()
	if got := resetSignIn(&acctRec{HospitalID: &h, Roles: []string{"clinician"}}); got != "clinical" {
		t.Fatalf("hospital account: %q", got)
	}
	if got := resetSignIn(&acctRec{Roles: []string{"platform_admin"}}); got != "platform" {
		t.Fatalf("platform account: %q", got)
	}
}
