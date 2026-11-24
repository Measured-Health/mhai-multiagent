package auth

import (
	"testing"
	"time"
)

func TestPendingExpired(t *testing.T) {
	created := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		after time.Duration
		want  bool
	}{
		{0, false},
		{14 * time.Minute, false},
		{PendingSecretTTL, false},
		{PendingSecretTTL + time.Nanosecond, true},
		{time.Hour, true},
	} {
		if got := pendingExpired(created, created.Add(c.after)); got != c.want {
			t.Errorf("after %v: expired %v, want %v", c.after, got, c.want)
		}
	}
	if PendingSecretTTL != 15*time.Minute {
		t.Fatalf("PendingSecretTTL %v, the contract says 15 minutes", PendingSecretTTL)
	}
}

func TestStepUpMethod(t *testing.T) {
	if stepUpMethod("123456") != "otp" || stepUpMethod("") != "backup" {
		t.Fatal("step-up method words")
	}
}
