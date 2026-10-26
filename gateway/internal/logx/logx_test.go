package logx

import (
	"bytes"
	"encoding/json"
	"log/slog"
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
