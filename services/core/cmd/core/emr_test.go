package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/config"
)

// K4: with CORE_EMR_ENABLED off (the default) serve creates no EMR stream:
// ensureEMR doesn't touch JetStream at all (a nil one here). On, it does,
// and a failure stops serve.
func TestEMRStreamNotCreatedWhenDisabled(t *testing.T) {
	if err := ensureEMR(context.Background(), config.Config{}, nil); err != nil {
		t.Fatalf("off: %v", err)
	}
	if err := ensureEMR(context.Background(), config.Config{EMREnabled: true, NATSJSReplicas: 1}, nil); err == nil {
		t.Fatal("on: no error without JetStream")
	}
}

// The EMR CLI checks its arguments before it connects: one of --patient or
// --mrn-stdin, a closed reason, a canonical patient id, and a document
// reference for a legal hold. An MRN is never an argument.
func TestEMRCLIArgs(t *testing.T) {
	ctx := context.Background()
	pid := "6f0c3a52-8d1e-4b7a-9c2d-5e8f1a3b7c90"
	for _, args := range [][]string{
		{},
		{"exclude"},
		{"exclude", "--hospital", "HAV", "--reason", "objection"},
		{"exclude", "--hospital", "HAV", "--patient", pid, "--mrn-stdin", "--reason", "objection"},
		{"exclude", "--hospital", "HAV", "--patient", pid, "--reason", "because"},
		{"exclude", "--hospital", "HAV", "--patient", strings.ToUpper(pid), "--reason", "other"},
		{"exclude", "--hospital", "HAV", "--mrn", "SYN-6837", "--reason", "other"},
		{"remove-hospital-data"},
		{"remove-hospital-data", "--all"},
		{"purge"},
	} {
		if err := emrCmd(ctx, args, "", strings.NewReader(""), io.Discard); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	// Good arguments reach the connection, which is missing here.
	if err := emrCmd(ctx, []string{"exclude", "--hospital", "HAV", "--patient", pid, "--reason", "objection"}, "",
		strings.NewReader(""), io.Discard); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("good exclude: %v", err)
	}
	if err := emrCmd(ctx, []string{"exclude", "--hospital", "HAV", "--mrn-stdin", "--reason", "objection"}, "",
		strings.NewReader("\n"), io.Discard); err == nil || strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("empty MRN on stdin: %v", err)
	}
	for _, args := range [][]string{
		{"place", "HAV"},
		{"place", "HAV", "--reference", "Synthetic Person"},
		{"place", "HAV", "--reference", "lh-2026-0042"},
		{"hold", "HAV", "--reference", "LH-2026-0042"},
		{"lift", "--reference", "LH-2026-0042"},
	} {
		if err := hospitalsLegalHold(ctx, args, "", io.Discard); err == nil || strings.Contains(err.Error(), "DATABASE_URL") {
			t.Errorf("legal-hold %v: %v", args, err)
		}
	}
	if err := hospitalsLegalHold(ctx, []string{"place", "HAV", "--reference", "LH-2026-0042"}, "", io.Discard); err == nil ||
		!strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("good legal-hold: %v", err)
	}
}

func TestReadMRN(t *testing.T) {
	for in, want := range map[string]string{"SYN-6837\n": "SYN-6837", "SYN-6837\r\n": "SYN-6837", "SYN-6837": "SYN-6837"} {
		if got, err := readMRN(strings.NewReader(in)); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", "\n", strings.Repeat("9", 65) + "\n"} {
		if _, err := readMRN(strings.NewReader(in)); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}
