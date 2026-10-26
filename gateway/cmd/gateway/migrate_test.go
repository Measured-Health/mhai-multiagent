package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestMigrateArgs(t *testing.T) {
	ctx := context.Background()
	for _, args := range [][]string{nil, {"down"}, {"up", "extra"}} {
		if err := migrate(ctx, args, "postgres://x", io.Discard); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("migrate %v: want usage error, got %v", args, err)
		}
	}
	if err := migrate(ctx, []string{"up"}, "", io.Discard); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("missing url: got %v", err)
	}
}
