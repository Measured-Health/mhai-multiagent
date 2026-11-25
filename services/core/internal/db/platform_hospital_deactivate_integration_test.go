//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00060: mhai_platform deactivates and reactivates a hospital,
// and nothing else with its status. Every name here is synthetic.

func platformAccount(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	u := seedUser(t, pool, nil)
	if _, err := pool.Exec(context.Background(), `INSERT INTO user_roles (user_id, role) VALUES ($1, 'platform_admin')`, u); err != nil {
		t.Fatal(err)
	}
	return u
}

func hospitalStatusOf(t *testing.T, pool *pgxpool.Pool, h uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM hospitals WHERE id = $1`, h).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// The two moves work as mhai_platform, with their record; the queued
// question is cancelled by 00009's trigger; the staff's sessions can be
// revoked only once the hospital is deactivated.
func TestPlatformDeactivatesAndReactivates(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	h, q := seedHospital(t, pool, uniqueCode("D"))
	staff := seedClinician(t, pool, h)
	sid := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (id, user_id) VALUES ($1, $2)`, sid, staff); err != nil {
		t.Fatal(err)
	}
	adm := platformAccount(t, pool)
	revoke := `UPDATE sessions SET revoked_at = now() WHERE id = $1`

	// Not while the hospital is active: the policy hides the row.
	err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, revoke, sid)
		if err == nil && tag.RowsAffected() != 0 {
			t.Error("revoked a session of an active hospital's account")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	err = db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE hospitals SET status = 'deactivated' WHERE id = $1`, h); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO hospital_deactivations (hospital_id, from_status, reason, deactivated_by)
			VALUES ($1, 'active', 'contract_ended', $2)`, h, adm); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, revoke, sid)
		if err == nil && tag.RowsAffected() != 1 {
			t.Errorf("revoked %d sessions", tag.RowsAffected())
		}
		return err
	})
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	var qs string
	if err := pool.QueryRow(ctx, `SELECT status FROM clinical_queries WHERE id = $1`, q).Scan(&qs); err != nil || qs != "cancelled" {
		t.Errorf("question %s %v, want cancelled", qs, err)
	}

	err = db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE hospitals SET status = 'active' WHERE id = $1`, h); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE hospital_deactivations SET reactivated_by = $2, reactivated_at = now(),
			reactivation_reason = 'Synthetic' WHERE hospital_id = $1 AND reactivated_at IS NULL`, h, adm)
		return err
	})
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if s := hospitalStatusOf(t, pool, h); s != "active" {
		t.Errorf("status %s", s)
	}
}

// Every other status write is refused, as is a record that doesn't match
// the hospital, by a hospital account, or a second open one.
func TestPlatformStatusGuard(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	adm := platformAccount(t, pool)
	set := func(h uuid.UUID, from, to string) error {
		if _, err := pool.Exec(ctx, `UPDATE hospitals SET status = $2 WHERE id = $1`, h, from); err != nil {
			t.Fatal(err)
		}
		return db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE hospitals SET status = $2 WHERE id = $1`, h, to)
			return err
		})
	}
	h, _ := seedHospital(t, pool, uniqueCode("G"))
	for _, c := range [][2]string{
		{"setup_incomplete", "active"}, {"active", "removal_pending"}, {"active", "removed"},
		{"deactivated", "active"}, {"deactivated", "removal_pending"}, {"removal_pending", "deactivated"},
		{"active", "setup_incomplete"},
	} {
		requireDenied(t, set(h, c[0], c[1]), c[0]+" -> "+c[1])
		if s := hospitalStatusOf(t, pool, h); s != c[0] {
			t.Errorf("%s -> %s: now %s", c[0], c[1], s)
		}
	}

	// A record left by setup_incomplete lets it back only to set-up.
	if _, err := pool.Exec(ctx, `INSERT INTO hospital_deactivations (hospital_id, from_status, reason, deactivated_by)
		VALUES ($1, 'setup_incomplete', 'contract_ended', $2)`, h, adm); err != nil {
		t.Fatal(err)
	}
	requireDenied(t, set(h, "deactivated", "active"), "back to active from a set-up record")

	// The record's own rules: one open record a hospital, by a platform
	// account.
	err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO hospital_deactivations (hospital_id, from_status, reason, deactivated_by)
			VALUES ($1, 'active', 'contract_ended', $2)`, h, adm)
		return err
	})
	if pgCode(err) != uniqueViolation {
		t.Errorf("second open record: %v", err)
	}
	h2, _ := seedHospital(t, pool, uniqueCode("B"))
	staff := seedClinician(t, pool, h2)
	if _, err := pool.Exec(ctx, `UPDATE hospitals SET status = 'deactivated' WHERE id = $1`, h2); err != nil {
		t.Fatal(err)
	}
	err = db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO hospital_deactivations (hospital_id, from_status, reason, deactivated_by)
			VALUES ($1, 'active', 'contract_ended', $2)`, h2, staff)
		return err
	})
	requireDenied(t, err, "a record by a hospital account")
	active, _ := seedHospital(t, pool, uniqueCode("A"))
	err = db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO hospital_deactivations (hospital_id, from_status, reason, deactivated_by)
			VALUES ($1, 'active', 'contract_ended', $2)`, active, adm)
		return err
	})
	requireDenied(t, err, "a record for an active hospital")
	if _, err := pool.Exec(ctx, `INSERT INTO hospital_deactivations (hospital_id, from_status, reason, deactivated_by)
		VALUES ($1, 'active', 'other', $2)`, active, adm); err == nil {
		t.Error("other without a note accepted")
	}

	// A clinical role can't change a status at all.
	err = db.WithHospital(ctx, pool, h, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE hospitals SET status = 'active' WHERE id = $1`, h)
		return err
	})
	requireDenied(t, err, "gateway status write")
}

// 00060 goes down cleanly and up again.
func TestMigration00060DownUp(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.MigrateDownTo(ctx, pg.URL, 59); err != nil {
		t.Fatalf("down to 59: %v", err)
	}
	var tables, fns int
	var status bool
	if err := pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM pg_tables WHERE tablename = 'hospital_deactivations'),
			(SELECT count(*) FROM pg_proc WHERE proname = 'mhai_platform_status_guard'),
			has_column_privilege('mhai_platform', 'hospitals', 'status', 'UPDATE')`).Scan(&tables, &fns, &status); err != nil {
		t.Fatal(err)
	}
	if tables != 0 || fns != 0 || status {
		t.Fatalf("after down: %d tables, %d functions, status update %v", tables, fns, status)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
