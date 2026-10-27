package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Requests that idempotency doesn't apply to never touch the database (the
// pool is nil here).
func TestIdempotentPassThrough(t *testing.T) {
	cases := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/", nil),
		httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`)),
	}
	get := httptest.NewRequest(http.MethodGet, "/", nil)
	get.Header.Set("Idempotency-Key", "k1")
	cases = append(cases, get)
	for _, req := range cases {
		n := 0
		rec := httptest.NewRecorder()
		Idempotent(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n++
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(rec, req)
		if n != 1 || rec.Code != http.StatusNoContent {
			t.Errorf("%s key=%q: n=%d status %d", req.Method, req.Header.Get("Idempotency-Key"), n, rec.Code)
		}
	}
}

func TestIdempotencyKeyValidated(t *testing.T) {
	for _, key := range []string{strings.Repeat("a", 256), "has space", "tab\tkey", "ünicode"} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		called := false
		Idempotent(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })).ServeHTTP(rec, req)
		if called || rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("key %q: called=%v status %d", key, called, rec.Code)
		}
	}
}

// Anonymous requests without a trusted client IP have no scope, so they
// run without idempotency rather than share one scope.
func TestIdempotentAnonWithoutIPRunsPlain(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Idempotency-Key", "k1")
	n := 0
	rec := httptest.NewRecorder()
	IdempotentAnon(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rec, req)
	if n != 1 || rec.Code != http.StatusNoContent {
		t.Fatalf("n=%d status %d", n, rec.Code)
	}
}

func TestCredentialKeys(t *testing.T) {
	for _, k := range []string{"password", "secret", "token", "access_token", "Refresh-Token", "mfa_token",
		"device_token", "mfa_code", "backup_code", "backup_codes", "recovery_codes", "otp", "totp",
		"Authorization", "cookie", "set-cookie", "API_KEY"} {
		if !isCredentialKey(k) {
			t.Errorf("%q not a credential", k)
		}
	}
	for _, k := range []string{"email", "code", "reason", "user_id"} {
		if isCredentialKey(k) {
			t.Errorf("%q is a credential", k)
		}
	}
}

// Idempotent sits inside the principal middleware. A keyed write that
// reaches it without a principal is a wiring fault: refused, never run, and
// never scoped by client IP (M3).
func TestIdempotentRequiresPrincipal(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Idempotency-Key", "k1")
	ran := false
	rec := httptest.NewRecorder()
	Idempotent(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ran = true })).ServeHTTP(rec, req)
	if ran || rec.Code != http.StatusInternalServerError {
		t.Fatalf("ran=%v status %d", ran, rec.Code)
	}
}

// IdempotentAnon without a trusted client IP runs without idempotency (no
// database touched: the pool is nil).
func TestIdempotentAnonWithoutIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Idempotency-Key", "k1")
	req = req.WithContext(WithPrincipal(req.Context(), Principal{UserID: "u"}))
	n := 0
	rec := httptest.NewRecorder()
	IdempotentAnon(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rec, req)
	if n != 1 || rec.Code != http.StatusNoContent {
		t.Fatalf("n=%d status %d", n, rec.Code)
	}
}

// I4: a credential key is caught by the words it contains, in any case and
// separator, at any depth of the body. Plain email and code stay storable;
// a harmless match such as token_count is tombstoned, never stored.
func TestCredentialKeySubstring(t *testing.T) {
	for _, c := range []struct {
		key      string
		storable bool
	}{
		{"session_token", false}, {"inviteToken", false}, {"reset-token", false},
		{"new_password", false}, {"newPassword", false}, {"client_secret", false},
		{"api_key_secret", false}, {"db_passwd", false}, {"Passcode", false},
		{"one_time_otp", false}, {"X-Auth-Cookie", false}, {"proxy_authorization", false},
		{"apiKey", false}, {"backup_codes_new", false}, {"recoveryCodes", false},
		{"MFA-Code", false}, {"token_count", false},
		{"email", true}, {"code", true}, {"hospital_code", true}, {"reason", true},
		{"user_id", true}, {"status", true},
	} {
		if got := !isCredentialKey(c.key); got != c.storable {
			t.Errorf("isCredentialKey(%q) storable=%v, want %v", c.key, got, c.storable)
		}
		body := `{"outer":{"list":[{"` + c.key + `":"x"}]}}`
		if got := storable([]byte(body)); got != c.storable {
			t.Errorf("storable(%s) = %v, want %v", body, got, c.storable)
		}
	}
}
