//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

func TestMigrateUpCommand(t *testing.T) {
	pg := testutil.StartPostgres(t)
	var out bytes.Buffer
	if err := migrate(context.Background(), []string{"up"}, pg.URL, &out); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if got := strings.Count(out.String(), "applied"); got != 6 {
		t.Fatalf("want 6 applied migrations, got output:\n%s", out.String())
	}
}

func TestServeRefusesSuperuserLogin(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := migrate(ctx, []string{"up"}, pg.URL, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	pool, err := openDB(ctx, pg.URL)
	if err == nil {
		pool.Close()
		t.Fatal("serve accepted a superuser login")
	}
	if !strings.Contains(err.Error(), "superuser") || !strings.Contains(err.Error(), "mhai_app") {
		t.Fatalf("unclear refusal: %v", err)
	}
}
