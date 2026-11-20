package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func emailEnv() map[string]string {
	m := prodEnv()
	m["NOTIFY_CHANNELS"] = "email"
	m["NOTIFY_APP_URL"] = "https://app.example.test"
	m["NOTIFY_SMTP_HOST"] = "smtp.example.test"
	m["NOTIFY_SMTP_PORT"] = "587"
	m["NOTIFY_SMTP_USERNAME"] = "no-reply@example.test"
	m["NOTIFY_SMTP_PASSWORD_FILE"] = "/run/secrets/smtp-password"
	m["NOTIFY_SMTP_FROM"] = "no-reply@example.test"
	m["NOTIFY_SMTP_REPLY_TO"] = "support@example.test"
	return m
}

// PR 4: email is allowed in production with every SMTP setting; the port
// is 465 or 587 only (no plaintext submission); the password comes only
// from a file; the app's address is https.
func TestEmailSettings(t *testing.T) {
	s, err := FromEnv(env(emailEnv()))
	if err != nil || s.SMTP == nil || s.SMTP.Port != 587 || s.SMTP.FromName != "MHAI" || s.AppURL != "https://app.example.test" {
		t.Fatalf("%+v %v", s, err)
	}
	m := emailEnv()
	m["NOTIFY_SMTP_PORT"], m["NOTIFY_SMTP_FROM_NAME"], m["NOTIFY_SMTP_CA_FILE"] = "465", "Measured Health", "/etc/ca.pem"
	if s, err := FromEnv(env(m)); err != nil || s.SMTP.Port != 465 || s.SMTP.FromName != "Measured Health" {
		t.Fatalf("465: %+v %v", s, err)
	}
	if s, err := FromEnv(env(devEnv())); err != nil || s.SMTP != nil {
		t.Fatalf("no email: %+v %v", s, err)
	}
	pw := "synthetic-" + uuid.NewString()
	for name, tc := range map[string]struct {
		edit func(map[string]string)
		want string
	}{
		"port 25":          {func(m map[string]string) { m["NOTIFY_SMTP_PORT"] = "25" }, "NOTIFY_SMTP_PORT"},
		"no port":          {func(m map[string]string) { delete(m, "NOTIFY_SMTP_PORT") }, "NOTIFY_SMTP_PORT"},
		"no host":          {func(m map[string]string) { delete(m, "NOTIFY_SMTP_HOST") }, "NOTIFY_SMTP_HOST"},
		"host with port":   {func(m map[string]string) { m["NOTIFY_SMTP_HOST"] = "smtp.example.test:25" }, "NOTIFY_SMTP_HOST"},
		"no user":          {func(m map[string]string) { delete(m, "NOTIFY_SMTP_USERNAME") }, "NOTIFY_SMTP_USERNAME"},
		"no password file": {func(m map[string]string) { delete(m, "NOTIFY_SMTP_PASSWORD_FILE") }, "NOTIFY_SMTP_PASSWORD_FILE"},
		"relative file":    {func(m map[string]string) { m["NOTIFY_SMTP_PASSWORD_FILE"] = "pw.txt" }, "NOTIFY_SMTP_PASSWORD_FILE"},
		"password in env":  {func(m map[string]string) { m["NOTIFY_SMTP_PASSWORD"] = pw }, "NOTIFY_SMTP_PASSWORD"},
		"no from":          {func(m map[string]string) { delete(m, "NOTIFY_SMTP_FROM") }, "NOTIFY_SMTP_FROM"},
		"no reply-to":      {func(m map[string]string) { delete(m, "NOTIFY_SMTP_REPLY_TO") }, "NOTIFY_SMTP_REPLY_TO"},
		"no app url":       {func(m map[string]string) { delete(m, "NOTIFY_APP_URL") }, "NOTIFY_APP_URL"},
		"http app url":     {func(m map[string]string) { m["NOTIFY_APP_URL"] = "http://app.example.test" }, "NOTIFY_APP_URL"},
		"relative CA":      {func(m map[string]string) { m["NOTIFY_SMTP_CA_FILE"] = "ca.pem" }, "NOTIFY_SMTP_CA_FILE"},
	} {
		m := emailEnv()
		tc.edit(m)
		_, err := FromEnv(env(m))
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), pw) {
			t.Errorf("%s: %v", name, err)
		}
	}
	m = devEnv()
	m["NOTIFY_SMTP_PASSWORD"] = pw
	if _, err := FromEnv(env(m)); err == nil {
		t.Fatal("NOTIFY_SMTP_PASSWORD accepted with email off")
	}
}

func TestReadSMTPPassword(t *testing.T) {
	dir := t.TempDir()
	write := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pw := "synthetic-" + uuid.NewString()
	if got, err := ReadSMTPPassword(write("good", pw+"\n")); err != nil || got != pw {
		t.Fatalf("good: %v", err)
	}
	for name, path := range map[string]string{"missing": filepath.Join(dir, "none"), "short": write("short", "abc"),
		"placeholder": write("ph", "changemeReplaceWithLettersAndDigits"), "two lines": write("two", pw+"\nsecond"),
		"a directory": dir} {
		_, err := ReadSMTPPassword(path)
		if err == nil || !strings.Contains(err.Error(), "NOTIFY_SMTP_PASSWORD_FILE") || strings.Contains(err.Error(), dir) ||
			strings.Contains(err.Error(), pw) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
