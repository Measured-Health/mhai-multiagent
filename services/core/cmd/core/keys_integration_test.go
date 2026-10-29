//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// F29, spec §7.5: core keys new --kind request adding a second kid, and
// core keys untrust --emergency, each append intelligence.key_rotated
// through the provisioning login (no actor, no hospital, ids only).
func TestKeysRotationAuditedInDatabase(t *testing.T) {
	pg, pool := testutil.MigratedPostgres(t)
	_, provURL, err := testutil.ProvisionLogin(context.Background(), pg, pool)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	trusted := filepath.Join(dir, "request-keys.json")
	a, b := filepath.Join(dir, "a.pem"), filepath.Join(dir, "b.pem")
	var out bytes.Buffer
	audit := dbRotationAudit(provURL)
	for _, p := range []string{a, b} {
		if err := keysWith([]string{"new", "--kind", "request", "--out", p, "--trusted-out", trusted}, &out, audit); err != nil {
			t.Fatal(err)
		}
	}
	sa, _ := reqtoken.LoadSigner(a)
	sb, _ := reqtoken.LoadSigner(b)
	if err := keysWith([]string{"untrust", "--kid", sa.KeyID, "--trusted-out", trusted, "--emergency"}, &out, audit); err != nil {
		t.Fatal(err)
	}
	type row struct{ kid, rotation, change string }
	rows, err := pool.Query(context.Background(), `SELECT entity_id, payload->>'rotation', payload->>'change' FROM audit_log
		WHERE action = 'intelligence.key_rotated' AND actor_id IS NULL AND hospital_id IS NULL AND entity = 'request_key' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.kid, &r.rotation, &r.change); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	rows.Close()
	want := []row{{sb.KeyID, "planned", "added"}, {sa.KeyID, "emergency", "removed"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("audit rows %+v, want %+v", got, want)
	}
}

// Review fix 5, with the real database: a rotation whose audit commit fails
// (here a deferred check that refuses the row at COMMIT) leaves the key and
// trusted files unchanged.
func TestKeysRotationRealCommitFailure(t *testing.T) {
	pg, pool := testutil.MigratedPostgres(t)
	_, provURL, err := testutil.ProvisionLogin(context.Background(), pg, pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE FUNCTION test_refuse_at_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'synthetic commit failure'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE CONSTRAINT TRIGGER test_refuse_rotation AFTER INSERT ON audit_log
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.action = 'intelligence.key_rotated')
		EXECUTE FUNCTION test_refuse_at_commit()`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	trusted := filepath.Join(dir, "request-keys.json")
	a, b := filepath.Join(dir, "a.pem"), filepath.Join(dir, "b.pem")
	var out bytes.Buffer
	audit := dbRotationAudit(provURL)
	if err := keysWith([]string{"new", "--kind", "request", "--out", a, "--trusted-out", trusted}, &out, audit); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(trusted)
	if err := keysWith([]string{"new", "--kind", "request", "--out", b, "--trusted-out", trusted}, &out, audit); err == nil {
		t.Fatal("the rotation went ahead although its audit didn't commit")
	}
	after, _ := os.ReadFile(trusted)
	if string(before) != string(after) || exists(b) {
		t.Fatal("files changed although the audit didn't commit")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 2 {
		t.Fatalf("left behind: %v", ents)
	}
}
