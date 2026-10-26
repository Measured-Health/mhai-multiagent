package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestSeedRefusesPasswordFlag(t *testing.T) {
	var out bytes.Buffer
	err := seed(context.Background(), []string{"users", "create", "--email", "a@b.example", "--name", "A",
		"--roles", "platform_admin", "--password", "hunter2hunter2"}, "postgres://x", strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("--password accepted")
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "a@b.example") {
		t.Fatalf("error echoes input: %v", err)
	}
}

func TestSeedRolesChecked(t *testing.T) {
	var out bytes.Buffer
	for _, args := range [][]string{
		{"users", "create", "--email", "a@b.example", "--name", "A", "--roles", "clinician,platform_admin"},
		{"users", "create", "--email", "a@b.example", "--name", "A", "--roles", "platform_admin", "--hospital", "HAV"},
		{"users", "create", "--email", "a@b.example", "--name", "A", "--roles", "clinician"},
	} {
		// Fails on validation before any connection is made.
		if err := seed(context.Background(), args, "", strings.NewReader("tangerine-orbit-77-quill\n"), &out); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestReadPasswordFromStdin(t *testing.T) {
	pw, err := readPassword(strings.NewReader("  tangerine-orbit-77-quill \r\nignored\n"))
	if err != nil || pw != "  tangerine-orbit-77-quill " {
		t.Fatalf("got %q, %v", pw, err)
	}
	if _, err := readPassword(strings.NewReader("")); err == nil {
		t.Fatal("empty stdin accepted")
	}
}

func TestReadPasswordRefusesLongAndInvalid(t *testing.T) {
	if _, err := readPassword(strings.NewReader(strings.Repeat("a", 2000) + "\n")); err == nil {
		t.Fatal("2000-byte password accepted")
	}
	if _, err := readPassword(strings.NewReader("abc\xff\xfedefghijklmnop\n")); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestSeedChecksEmailAndNameBeforePassword(t *testing.T) {
	// No stdin: if the password were read first, the error would be about it.
	err := seed(context.Background(), []string{"users", "create", "--email", "not-an-email", "--name", "A", "--roles", "platform_admin"},
		"", strings.NewReader(""), &bytes.Buffer{})
	if err == nil || strings.Contains(err.Error(), "stdin") {
		t.Fatalf("got %v", err)
	}
}
