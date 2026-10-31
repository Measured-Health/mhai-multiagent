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

// The pipeline's login (spec §8.2): INTELLIGENCE_DB_PASSWORD provisions
// INTELLIGENCE_DB_USER (default intelligence_app) as a member of
// mhai_worker_app only; a placeholder or short password is refused by
// variable name, never by value.
func TestEnsureLoginWorkerApp(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	reqs, err := loginRequests(env(map[string]string{"INTELLIGENCE_DB_PASSWORD": "test-only-intelligence-db-password"}))
	if err != nil || len(reqs) != 1 || reqs[0].user != "intelligence_app" || reqs[0].role != "mhai_worker_app" {
		t.Fatalf("default user: %+v %v", reqs, err)
	}
	reqs, err = loginRequests(env(map[string]string{
		"INTELLIGENCE_DB_USER": "pipeline_db", "INTELLIGENCE_DB_PASSWORD": "test-only-intelligence-db-password",
	}))
	if err != nil || len(reqs) != 1 || reqs[0].user != "pipeline_db" || reqs[0].role != "mhai_worker_app" {
		t.Fatalf("named user: %+v %v", reqs, err)
	}
	for _, pw := range []string{"changemeReplaceWithLettersAndDigits", "placeholder-intelligence-pw", "short"} {
		_, err := loginRequests(env(map[string]string{"INTELLIGENCE_DB_PASSWORD": pw}))
		if err == nil || !strings.Contains(err.Error(), "INTELLIGENCE_DB_PASSWORD") || strings.Contains(err.Error(), pw) {
			t.Errorf("password %d chars: want a refusal naming INTELLIGENCE_DB_PASSWORD only, got %v", len(pw), err)
		}
	}
	// A user without a password is a mistake, not a default.
	if _, err := loginRequests(env(map[string]string{"INTELLIGENCE_DB_USER": "pipeline_db"})); err == nil ||
		!strings.Contains(err.Error(), "INTELLIGENCE_DB_PASSWORD") {
		t.Errorf("user without password: %v", err)
	}
}

// Two variables naming the same login are refused by name: the second
// EnsureLogin would take the first one's membership away.
func TestLoginRequestsRefuseOneUserTwice(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, m := range []map[string]string{
		{"CORE_DB_USER": "intelligence_app", "CORE_DB_PASSWORD": "test-only-core-db-password",
			"INTELLIGENCE_DB_PASSWORD": "test-only-intelligence-db-password"},
		{"CORE_DB_USER": "shared_login", "CORE_DB_PASSWORD": "test-only-core-db-password",
			"PROVISION_DB_USER": "shared_login", "PROVISION_DB_PASSWORD": "test-only-provision-db-password"},
	} {
		_, err := loginRequests(env(m))
		if err == nil || !strings.Contains(err.Error(), "CORE_DB_USER") || !strings.Contains(err.Error(), "same database login") {
			t.Errorf("%v: want a refusal naming the variables, got %v", m, err)
			continue
		}
		if strings.Contains(err.Error(), "test-only") {
			t.Errorf("error carries a password: %v", err)
		}
	}
}
