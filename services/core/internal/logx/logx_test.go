package logx

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"
)

func TestRedactingHandler(t *testing.T) {
	keys := []string{"password", "token", "code", "email", "access_token", "refresh_token", "mfa_token", "backup_code"}
	for _, k := range keys {
		var buf bytes.Buffer
		New(&buf).Info("evt", k, "s3cret-value", "user_id", "u1")
		if strings.Contains(buf.String(), "s3cret-value") {
			t.Fatalf("%s leaked: %s", k, buf.String())
		}
		var m map[string]any
		if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		if m[k] != "[redacted]" {
			t.Fatalf("%s = %v", k, m[k])
		}
		if m["user_id"] != "u1" {
			t.Fatalf("user_id must pass through: %v", m)
		}
	}
}

func TestRedactingHandlerNested(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.Info("evt", slog.Group("req", slog.String("email", "a@b.example"), slog.Group("inner", slog.String("password", "pw123"))))
	l.With(slog.Group("g", slog.String("token", "tok123"))).WithGroup("w").Info("x", "backup_code", "bc123")
	out := buf.String()
	for _, leak := range []string{"a@b.example", "pw123", "tok123", "bc123"} {
		if strings.Contains(out, leak) {
			t.Fatalf("%s leaked: %s", leak, out)
		}
	}
	if strings.Count(out, "[redacted]") != 4 {
		t.Fatalf("expected 4 redactions: %s", out)
	}
}

func TestRedactingHandlerAnyValues(t *testing.T) {
	type req struct {
		Email string `json:"email"`
		Name  string `json:"name"`
		Auth  struct {
			Password string
		}
	}
	r := req{Email: "e-leak@x.example", Name: "visible"}
	r.Auth.Password = "pw-leak"
	var buf bytes.Buffer
	l := New(&buf)
	l.Info("m", "req", map[string]any{"password": "map-leak", "n": 1, "deep": []any{map[string]any{"token": "deep-leak"}}})
	l.Info("m", "req", map[string]string{"Token": "ms-leak"})
	l.Info("m", "req", url.Values{"code": {"uv-leak"}})
	l.Info("m", "req", r)
	l.Info("m", "req", &r)
	l.Info("m", "req", []req{r})
	l.Info("m", "err", errors.New("plain error"))
	out := buf.String()
	for _, leak := range []string{"map-leak", "deep-leak", "ms-leak", "uv-leak", "e-leak", "pw-leak"} {
		if strings.Contains(out, leak) {
			t.Fatalf("%s leaked: %s", leak, out)
		}
	}
	if !strings.Contains(out, "visible") || !strings.Contains(out, "plain error") {
		t.Fatalf("non-sensitive data lost: %s", out)
	}
}

func TestRedactingHandlerSensitiveGroupName(t *testing.T) {
	var buf bytes.Buffer
	New(&buf).Info("m", slog.Group("token", "v", "t3-leak"), slog.Group("Set-Cookie", "v", "c-leak"))
	out := buf.String()
	if strings.Contains(out, "t3-leak") || strings.Contains(out, "c-leak") {
		t.Fatalf("leaked: %s", out)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["token"] != "[redacted]" || m["Set-Cookie"] != "[redacted]" {
		t.Fatalf("groups not replaced whole: %v", m)
	}
}

func TestRedactingHandlerKeyNormalising(t *testing.T) {
	keys := []string{"Password", "Access_Token", "Access-Token", "accessToken", "REFRESH-TOKEN", "Authorization",
		"cookie", "Set-Cookie", "secret", "OTP", "recovery_codes", "Backup-Code", "mfa_token"}
	for _, k := range keys {
		var buf bytes.Buffer
		New(&buf).Info("m", k, "leak-value")
		if strings.Contains(buf.String(), "leak-value") {
			t.Fatalf("%s leaked: %s", k, buf.String())
		}
	}
}

func TestIsSensitiveKey(t *testing.T) {
	for _, k := range []string{"email", "Refresh-Token", "backup_code", "API_KEY", "code"} {
		if !IsSensitiveKey(k) {
			t.Errorf("%q not sensitive", k)
		}
	}
	for _, k := range []string{"reason", "user_id", "hospital_id", "request_id"} {
		if IsSensitiveKey(k) {
			t.Errorf("%q sensitive", k)
		}
	}
}

func TestSensitiveKeyInJSON(t *testing.T) {
	cases := map[string]string{
		`{"email":"x"}`:                         "email",
		`{"meta":{"Refresh-Token":"x"}}`:        "Refresh-Token",
		`{"items":[{"ok":1},{"password":"x"}]}`: "password",
		`[[{"otp":1}]]`:                         "otp",
	}
	for doc, want := range cases {
		if got, ok := SensitiveKeyInJSON([]byte(doc)); !ok || got != want {
			t.Errorf("%s: got %q %v, want %q", doc, got, ok, want)
		}
	}
	for _, doc := range []string{`{"reason":"email token code"}`, `[1,"password"]`, `"token"`, `{}`} {
		if got, ok := SensitiveKeyInJSON([]byte(doc)); ok {
			t.Errorf("%s: flagged %q", doc, got)
		}
	}
	if _, ok := SensitiveKeyInJSON([]byte(`{not json`)); !ok {
		t.Error("invalid JSON must fail closed")
	}
}
