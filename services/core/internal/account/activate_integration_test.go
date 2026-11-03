//go:build integration

package account

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

func newHospital(t *testing.T, prov *pgxpool.Pool, code string) uuid.UUID {
	t.Helper()
	id, err := CreateHospital(context.Background(), prov, NewHospital{Name: "Synthetic General", ShortCode: code, TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func statusOf(t *testing.T, super *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := super.QueryRow(context.Background(), `SELECT status FROM hospitals WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func activations(t *testing.T, super *pgxpool.Pool, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'hospital.activated' AND entity_id = $1`,
		id.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestActivateHospitalAuditsOnceAndIsIdempotent(t *testing.T) {
	super, prov := setup(t)
	ctx := context.Background()
	hid := newHospital(t, prov, "HAV")
	got, already, err := ActivateHospital(ctx, prov, "hav")
	if err != nil || already || got != hid {
		t.Fatalf("activate: %v %v %v", got, already, err)
	}
	if s := statusOf(t, super, hid); s != "active" {
		t.Fatalf("status %q", s)
	}
	var hosp, entity, payload string
	var actor *string
	if err := super.QueryRow(ctx, `SELECT hospital_id::text, actor_id::text, entity, payload::text FROM audit_log
		WHERE action = 'hospital.activated' AND entity_id = $1`, hid.String()).Scan(&hosp, &actor, &entity, &payload); err != nil {
		t.Fatal(err)
	}
	if hosp != hid.String() || actor != nil || entity != "hospital" || payload != `{"via": "cli", "from": "setup_incomplete"}` {
		t.Fatalf("audit row: %s %v %s %s", hosp, actor, entity, payload)
	}
	// Again, by id: a no-op, nothing more on the chain.
	got, already, err = ActivateHospital(ctx, prov, hid.String())
	if err != nil || !already || got != hid {
		t.Fatalf("re-activate: %v %v %v", got, already, err)
	}
	if n := activations(t, super, hid); n != 1 {
		t.Fatalf("%d hospital.activated rows, want 1", n)
	}
	if ok, _, err := audit.Verify(ctx, super); err != nil || !ok {
		t.Fatalf("audit chain: %v %v", ok, err)
	}
}

// Only setup_incomplete goes to active here: a deactivated hospital is the
// platform app's to reactivate, and the removal states never come back.
func TestActivateHospitalRefusesOtherStatuses(t *testing.T) {
	super, prov := setup(t)
	ctx := context.Background()
	for i, st := range []string{"deactivated", "removal_pending", "removal_scheduled", "removed"} {
		hid := newHospital(t, prov, "ST"+string(rune('A'+i)))
		if _, err := super.Exec(ctx, `UPDATE hospitals SET status = $1 WHERE id = $2`, st, hid); err != nil {
			t.Fatal(err)
		}
		_, _, err := ActivateHospital(ctx, prov, hid.String())
		if !errors.Is(err, ErrNotActivatable) {
			t.Fatalf("%s: %v", st, err)
		}
		if s := statusOf(t, super, hid); s != st {
			t.Fatalf("%s became %s", st, s)
		}
		if n := activations(t, super, hid); n != 0 {
			t.Fatalf("%s: %d audit rows", st, n)
		}
	}
	if _, _, err := ActivateHospital(ctx, prov, "NOPE"); !errors.Is(err, ErrNoSuchHospital) {
		t.Fatalf("unknown code: %v", err)
	}
	if _, _, err := ActivateHospital(ctx, prov, uuid.NewString()); !errors.Is(err, ErrNoSuchHospital) {
		t.Fatalf("unknown id: %v", err)
	}
}

// Twenty concurrent activations: one changes the row and audits it, the
// rest report it already active.
func TestActivateHospitalConcurrent(t *testing.T) {
	super, prov := setup(t)
	hid := newHospital(t, prov, "HAV")
	var wg sync.WaitGroup
	var mu sync.Mutex
	fresh := 0
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, already, err := ActivateHospital(context.Background(), prov, "HAV")
			if err != nil {
				t.Error(err)
				return
			}
			if !already {
				mu.Lock()
				fresh++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if fresh != 1 {
		t.Fatalf("%d activations reported, want 1", fresh)
	}
	if n := activations(t, super, hid); n != 1 {
		t.Fatalf("%d hospital.activated rows, want 1", n)
	}
}

// A login that isn't a member of mhai_provision (the core service's own)
// can't activate, through the function or with SQL.
func TestActivateHospitalNeedsProvisionRole(t *testing.T) {
	pg, super := testutil.MigratedPostgres(t)
	ctx := context.Background()
	prov, _, err := testutil.ProvisionLogin(ctx, pg, super)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(prov.Close)
	app, err := testutil.AppLogin(ctx, pg, super)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	hid := newHospital(t, prov, "HAV")
	if _, _, err := ActivateHospital(ctx, app, "HAV"); !errors.Is(err, ErrNotProvisioner) {
		t.Fatalf("app login: %v", err)
	}
	for _, role := range []string{"mhai_app", "mhai_gateway", "mhai_platform", "mhai_auth"} {
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+role); err == nil {
			if _, err := tx.Exec(ctx, `UPDATE hospitals SET status = 'active' WHERE id = $1`, hid); err == nil {
				t.Errorf("%s updated hospitals.status", role)
			}
		}
		_ = tx.Rollback(ctx)
	}
	if s := statusOf(t, super, hid); s != "setup_incomplete" {
		t.Fatalf("status %q", s)
	}
}
