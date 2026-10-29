//go:build integration

package main

import (
	"bytes"
	"context"
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
