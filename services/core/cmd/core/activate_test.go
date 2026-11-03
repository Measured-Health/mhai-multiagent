package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
)

// Bad arguments fail before any connection (url is empty: connecting would
// say DATABASE_URL is required).
func TestHospitalsActivateArgsCheckedFirst(t *testing.T) {
	for _, args := range [][]string{
		{"hospitals", "activate"},
		{"hospitals", "activate", "HAV", "EXTRA"},
		{"hospitals", "activate", "--short-code", "HAV"},
		{"hospitals", "activate", "-h"},
	} {
		err := seed(context.Background(), args, "", strings.NewReader(""), &bytes.Buffer{})
		if !errors.Is(err, errActivateArgs) {
			t.Errorf("%v: %v", args, err)
		}
	}
	err := seed(context.Background(), []string{"hospitals", "activate", "not a code"}, "", strings.NewReader(""), &bytes.Buffer{})
	if !errors.Is(err, account.ErrBadHospitalRef) {
		t.Fatalf("bad ref: %v", err)
	}
	err = seed(context.Background(), []string{"hospitals", "activate", "HAV"}, "", strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("good ref, no url: %v", err)
	}
}
