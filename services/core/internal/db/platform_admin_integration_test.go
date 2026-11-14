//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Migration 00037 (the platform admin routes): mhai_platform's new writes
// reach platform accounts only, the new tables are row-secured and owned by
// mhai_migrate, and no clinical role holds anything on them.

func TestPlatformAdminTables(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for _, table := range []string{"platform_member_versions", "legal_hold_lifts", "platform_email_prefs"} {
		var rls, force bool
		var owner string
		if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity, pg_get_userbyid(relowner) FROM pg_class
			WHERE oid = $1::regclass`, table).Scan(&rls, &force, &owner); err != nil {
			t.Fatal(err)
		}
		if !rls || !force || owner != "mhai_migrate" {
			t.Errorf("%s: rls %v force %v owner %s", table, rls, force, owner)
		}
		for _, role := range []string{"mhai_gateway", "mhai_worker", "mhai_jobs", "mhai_emr", "mhai_auth", "mhai_provision"} {
			requireDenied(t, asRole(ctx, pool, role, `SELECT count(*) FROM `+table), role+" reading "+table)
		}
	}
}

// The platform role's team writes stop at hospital accounts and clinical
// roles; a lift's approver must differ from its requester.
func TestPlatformAdminWritesScoped(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, _ := seedHospital(t, pool, uniqueCode("PA"))
	clin := seedClinician(t, pool, h)
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (user_id) VALUES ($1)`, clin); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_mfa (user_id, secret_enc, enabled) VALUES ($1, '\x00', true)`, clin); err != nil {
		t.Fatal(err)
	}
	staff := seedUser(t, pool, nil)
	if _, err := pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'auditor')`, staff); err != nil {
		t.Fatal(err)
	}
	as := func(fn func(pgx.Tx) error) error { return asRoleIn(ctx, pool, "mhai_platform", uuid.Nil, fn) }
	exec := func(q string, args ...any) (int64, error) {
		var n int64
		err := as(func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, q, args...)
			n = tag.RowsAffected()
			return err
		})
		return n, err
	}
	// A hospital account: nothing changes (RLS hides it from the writes).
	for _, q := range []string{`UPDATE users SET is_active = false WHERE id = $1`,
		`UPDATE sessions SET revoked_at = now() WHERE user_id = $1`,
		`DELETE FROM user_roles WHERE user_id = $1`} {
		if n, err := exec(q, clin); err != nil || n != 0 {
			t.Errorf("%s on a hospital account: %d rows, %v", q, n, err)
		}
	}
	if err := as(func(tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT count(*) FROM user_mfa WHERE user_id = $1`, clin).Scan(&n)
	}); err != nil {
		t.Errorf("reading user_mfa: %v", err)
	}
	if _, err := exec(`INSERT INTO user_roles (user_id, role) VALUES ($1, 'platform_admin')`, clin); pgCode(err) != permissionDenied {
		t.Errorf("a platform role for a hospital account: %v", err)
	}
	if _, err := exec(`INSERT INTO user_roles (user_id, role) VALUES ($1, 'clinician')`, staff); pgCode(err) != permissionDenied {
		t.Errorf("a clinical role for a platform account: %v", err)
	}
	if _, err := exec(`UPDATE users SET display_name = 'Synthetic Renamed' WHERE id = $1`, staff); pgCode(err) != permissionDenied {
		t.Errorf("a name change: %v", err)
	}
	// A platform account: the writes land.
	if n, err := exec(`UPDATE users SET is_active = false WHERE id = $1`, staff); err != nil || n != 1 {
		t.Errorf("deactivating a platform account: %d %v", n, err)
	}
	// Never the other way, and never a revoked session's time.
	if _, err := exec(`UPDATE users SET is_active = true WHERE id = $1`, staff); pgCode(err) != permissionDenied {
		t.Errorf("reactivating a platform account: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (user_id, revoked_at) VALUES ($1, now() - interval '1 day')`, staff); err != nil {
		t.Fatal(err)
	}
	if n, err := exec(`UPDATE sessions SET revoked_at = now() WHERE user_id = $1`, staff); err != nil || n != 0 {
		t.Errorf("a revoked session's time rewritten: %d %v", n, err)
	}
	// Holds: a hold needs its reason, review date and placer; nothing
	// deletes one.
	if _, err := exec(`INSERT INTO legal_holds (hospital_id, reference) VALUES ($1, 'LH-2026-8801')`, h); pgCode(err) != permissionDenied {
		t.Errorf("a hold without its reason: %v", err)
	}
	var hold uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO legal_holds (hospital_id, reference) VALUES ($1, 'LH-2026-8802') RETURNING id`, h).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	if _, err := exec(`DELETE FROM legal_holds WHERE id = $1`, hold); pgCode(err) != permissionDenied {
		t.Errorf("deleting a hold: %v", err)
	}
	if _, err := exec(`INSERT INTO legal_hold_lifts (hold_id, reason, authority, requested_by, expires_at)
		VALUES ($1, 'other', 'Synthetic letter', $2, now() + interval '1 day')`, hold, staff); err == nil {
		t.Error("a free-text authority was stored")
	}
	var lift uuid.UUID
	if err := as(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO legal_hold_lifts (hold_id, reason, authority, requested_by, expires_at)
			VALUES ($1, 'other', 'CT-2026-8803', $2, now() + interval '1 day') RETURNING id`, hold, staff).Scan(&lift)
	}); err != nil {
		t.Fatalf("a lift request: %v", err)
	}
	if _, err := exec(`UPDATE legal_hold_lifts SET approved_by = requested_by, approved_at = now() WHERE id = $1`, lift); err == nil {
		t.Error("a requester approved their own lift")
	}
	// No hold is lifted without an approved request of two people.
	if _, err := exec(`UPDATE legal_holds SET lifted_at = now() WHERE id = $1`, hold); pgCode(err) != permissionDenied {
		t.Errorf("a hold lifted with no approval: %v", err)
	}
	if n, err := exec(`UPDATE legal_hold_lifts SET cancelled_at = now() WHERE id = $1`, lift); err != nil || n != 1 {
		t.Errorf("cancel: %d %v", n, err)
	}
	if n, err := exec(`UPDATE legal_hold_lifts SET cancelled_at = NULL WHERE id = $1`, lift); err != nil || n != 0 {
		t.Errorf("an ended request changed again: %d %v", n, err)
	}
}
