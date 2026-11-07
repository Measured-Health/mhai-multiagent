//go:build integration

package dbx

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// A self-contained schema shaped like core's (00003): NOLOGIN roles held
// by row-level security on mhai.hospital_id, a table they don't own.
const schema = `
CREATE ROLE mhai_gateway NOLOGIN;
CREATE ROLE mhai_emr NOLOGIN;
CREATE ROLE mhai_emr_core NOLOGIN;
CREATE ROLE owner_role NOLOGIN;
CREATE TABLE notes (hospital_id uuid NOT NULL, body text NOT NULL);
ALTER TABLE notes OWNER TO owner_role;
ALTER TABLE notes ENABLE ROW LEVEL SECURITY;
ALTER TABLE notes FORCE ROW LEVEL SECURITY;
CREATE POLICY by_hospital ON notes USING (hospital_id = nullif(current_setting('mhai.hospital_id', true), '')::uuid);
GRANT SELECT ON notes TO mhai_gateway, mhai_emr, mhai_emr_core;
`

func startPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("dbx"), tcpostgres.WithUsername("dbx"), tcpostgres.WithPassword("test-only-dbx-pw"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(90*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	url, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1 // one pooled connection: whatever leaks, the next transaction sees
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, schema); err != nil {
		t.Fatal(err)
	}
	return pool
}

// One pooled connection, hospital A then B: no row of A in B's
// transaction, and neither the role nor the hospital outlives its
// transaction (as core's TestRLSWithoutHospitalSeesNothing).
func TestWithHospitalResetsBetweenTransactions(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	a, b := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO notes VALUES ($1, 'synthetic a'), ($2, 'synthetic b')`, a, b); err != nil {
		t.Fatal(err)
	}
	read := func(role Role, hid uuid.UUID) []string {
		t.Helper()
		var out []string
		err := WithHospital(ctx, pool, role, hid, func(tx pgx.Tx) error {
			var who string
			if err := tx.QueryRow(ctx, `SELECT current_user`).Scan(&who); err != nil {
				return err
			}
			if who != string(role) {
				t.Errorf("running as %q, want %q", who, role)
			}
			rows, err := tx.Query(ctx, `SELECT body FROM notes ORDER BY body`)
			if err != nil {
				return err
			}
			out, err = pgx.CollectRows(rows, pgx.RowTo[string])
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, role := range Roles {
		if got := read(role, a); len(got) != 1 || got[0] != "synthetic a" {
			t.Fatalf("%s as A: %v", role, got)
		}
		if got := read(role, b); len(got) != 1 || got[0] != "synthetic b" {
			t.Fatalf("%s as B: %v", role, got)
		}
	}
	// After the transactions, on the same connection: back to the login,
	// with no hospital and no user set.
	var who, hid, uid string
	if err := pool.QueryRow(ctx, `SELECT current_user, coalesce(current_setting('mhai.hospital_id', true), ''),
		coalesce(current_setting('mhai.user_id', true), '')`).Scan(&who, &hid, &uid); err != nil {
		t.Fatal(err)
	}
	if who != "dbx" || hid != "" || uid != "" {
		t.Fatalf("leaked: user %q hospital %q user id %q", who, hid, uid)
	}
}

// fn's error rolls the transaction back and comes back unchanged; the
// user id is set only for WithHospitalUser.
func TestWithHospitalUserAndRollback(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	hid, uid := uuid.New(), uuid.New()
	var got string
	if err := WithHospitalUser(ctx, pool, Gateway, hid, uid, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_setting('mhai.user_id', true)`).Scan(&got)
	}); err != nil || got != uid.String() {
		t.Fatalf("user id %q: %v", got, err)
	}
	if err := WithHospital(ctx, pool, Gateway, hid, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(current_setting('mhai.user_id', true), '')`).Scan(&got)
	}); err != nil || got != "" {
		t.Fatalf("user id without WithHospitalUser: %q %v", got, err)
	}
	boom := errors.New("synthetic failure")
	if err := WithHospital(ctx, pool, EMR, hid, func(tx pgx.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("fn error: %v", err)
	}
	// A role can't write what it wasn't granted: the error comes back and
	// nothing is written.
	if err := WithHospital(ctx, pool, EMR, hid, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO notes VALUES ($1, 'synthetic')`, hid)
		return err
	}); err == nil {
		t.Fatal("mhai_emr inserted without a grant")
	}
}
