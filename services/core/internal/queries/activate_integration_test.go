//go:build integration

package queries_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// A hospital made with the CLI (setup_incomplete) can't ask questions;
// once "core hospitals activate" (account.ActivateHospital, as the
// provisioning login) has run, its clinician can.
func TestPostQueriesAfterCLIActivation(t *testing.T) {
	ctx := context.Background()
	prov, _, err := testutil.ProvisionLogin(ctx, pgSrv, super)
	if err != nil {
		t.Fatal(err)
	}
	defer prov.Close()
	code := "A" + uniq()
	hid, err := account.CreateHospital(ctx, prov, account.NewHospital{Name: "Synthetic General", ShortCode: code, TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	u := seedUser(t, hid, authz.RoleClinician)
	before := newEnv(t, 10)
	if r := before.do(t, "POST", "/queries", mint(t, u), question("Synthetic?"), nil); r.code != http.StatusUnauthorized {
		t.Fatalf("setup_incomplete: %d %s", r.code, r.raw)
	}
	if _, already, err := account.ActivateHospital(ctx, prov, code); err != nil || already {
		t.Fatalf("activate: %v %v", already, err)
	}
	// A fresh chain: the guard's 30-second state cache may still hold the
	// old status (plan 2, edge case 3).
	after := newEnv(t, 10)
	r := after.do(t, "POST", "/queries", mint(t, u), question("Synthetic?"), nil)
	if r.code != http.StatusAccepted {
		t.Fatalf("active: %d %s", r.code, r.raw)
	}
	if id, _ := r.body["query_id"].(string); id == "" || rowOf(t, id).createdBy != u.id {
		t.Fatalf("no row for the question: %s", r.raw)
	}
	jobs(t, hid)
}
