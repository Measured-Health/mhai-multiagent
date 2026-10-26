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
