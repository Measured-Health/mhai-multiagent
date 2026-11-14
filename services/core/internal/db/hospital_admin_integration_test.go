//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00035 (the hospital admin routes): staff and settings versions,
// additive, and down then up again with rows in place. The gateway moves
// them only in its own hospital.
func TestMigration00035DownUp(t *testing.T) {
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
	h, h2 := uuid.New(), uuid.New()
	for i, id := range []uuid.UUID{h, h2} {
		if _, err := pool.Exec(ctx, `INSERT INTO hospitals (id, name, short_code) VALUES ($1, 'Synth Versioned', $2)`,
			id, "V"+string(rune('A'+i))+id.String()[:6]); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO hospital_settings (hospital_id) VALUES ($1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	var u uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO users (hospital_id, email, display_name) VALUES ($1, 'synth-v-35@example.test', 'Synth V')
		RETURNING id`, h2).Scan(&u); err != nil {
		t.Fatal(err)
	}
	var sv, hv int
	if err := pool.QueryRow(ctx, `SELECT u.staff_version, s.version FROM users u, hospital_settings s WHERE u.id = $1 AND s.hospital_id = $2`,
		u, h).Scan(&sv, &hv); err != nil || sv != 1 || hv != 1 {
		t.Fatalf("defaults %d %d %v", sv, hv, err)
	}
	// As the gateway of hospital h: hospital h2's rows are out of reach.
	err = db.WithHospital(ctx, pool, h, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `UPDATE hospital_settings SET version = version + 1 WHERE hospital_id = $1`, h2)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 0 {
			t.Error("the gateway moved another hospital's settings")
		}
		ct, err = tx.Exec(ctx, `UPDATE users SET staff_version = staff_version + 1 WHERE id = $1`, u)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 0 {
			t.Error("the gateway moved another hospital's person")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hospital_settings SET version = 0 WHERE hospital_id = $1`, h); err == nil {
		t.Error("a zero version was accepted")
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 34); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name IN ('users', 'hospital_settings')
		AND column_name IN ('staff_version', 'version', 'updated_by', 'updated_at')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after down: %d columns %v", n, err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT staff_version FROM users WHERE id = $1`, u).Scan(&sv); err != nil || sv != 1 {
		t.Fatalf("after up: %d %v", sv, err)
	}
}
