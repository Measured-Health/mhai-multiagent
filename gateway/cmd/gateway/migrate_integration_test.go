//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/gateway/internal/testutil"
)

func TestMigrateUpCommand(t *testing.T) {
	pg := testutil.StartPostgres(t)
	var out bytes.Buffer
	if err := migrate(context.Background(), []string{"up"}, pg.URL, &out); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if got := strings.Count(out.String(), "applied"); got != 3 {
		t.Fatalf("want 3 applied migrations, got output:\n%s", out.String())
	}
}
