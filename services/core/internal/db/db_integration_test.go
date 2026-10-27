//go:build integration

package db_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// One migrated server is shared by the behaviour tests; each test seeds its
// own hospitals, so they don't see each other's rows.
var (
	sharedPG   *testutil.PG
	sharedPool *pgxpool.Pool
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	pg, stop, err := testutil.NewPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start postgres:", err)
		os.Exit(1)
	}
	code := func() int {
		defer stop()
		if err := db.MigrateUp(ctx, pg.URL); err != nil {
			fmt.Fprintln(os.Stderr, "migrate up:", err)
			return 1
		}
		pool, err := db.Open(ctx, pg.URL)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open:", err)
			return 1
		}
		defer pool.Close()
		sharedPG, sharedPool = pg, pool
		return m.Run()
	}()
	os.Exit(code)
}

func shared(t *testing.T) (*testutil.PG, *pgxpool.Pool) {
	t.Helper()
	return sharedPG, sharedPool
}

const permissionDenied = "42501"

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func requireDenied(t *testing.T, err error, what string) {
	t.Helper()
	if pgCode(err) != permissionDenied {
		t.Fatalf("%s: want permission denied (42501), got %v", what, err)
	}
}

// seedHospital inserts a hospital and one clinical query for it, as superuser.
func seedHospital(t *testing.T, pool *pgxpool.Pool, code string) (hospitalID, queryID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	hospitalID, queryID = uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO hospitals (id, name, short_code, status, time_zone) VALUES ($1, $2, $3, 'active', 'Africa/Lagos')`,
		hospitalID, "Synthetic Hospital "+code, code); err != nil {
		t.Fatalf("seed hospital: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency) VALUES ($1, $2, 'synthetic question', 'routine')`,
		queryID, hospitalID); err != nil {
		t.Fatalf("seed query: %v", err)
	}
	return hospitalID, queryID
}

func uniqueCode(prefix string) string {
	return prefix + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
}

func TestMigrateUpFromEmpty(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	// A second run is a no-op.
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	st, err := db.Status(ctx, pg.URL)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st) != 6 {
		t.Fatalf("want 6 migrations, got %d", len(st))
	}
	for i, s := range st {
		if s.Source.Version != int64(i+1) || s.State != goose.StateApplied {
			t.Fatalf("migration %d: version %d state %s", i, s.Source.Version, s.State)
		}
	}
	// Down to empty and back up again.
	if err := db.MigrateDownTo(ctx, pg.URL, 0); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("migrate up after down: %v", err)
	}
}

func TestAppRolesAreNotOwnersAndCannotBypassRLS(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	rows, err := pool.Query(ctx,
		`SELECT rolname, rolsuper, rolbypassrls, rolcanlogin FROM pg_roles
		 WHERE rolname IN ('mhai_gateway','mhai_platform','mhai_auth','mhai_worker','mhai_migrate','mhai_app','mhai_provision')`)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for rows.Next() {
		var name string
		var super, bypass, login bool
		if err := rows.Scan(&name, &super, &bypass, &login); err != nil {
			t.Fatal(err)
		}
		seen[name] = true
		if super || bypass || login {
			t.Errorf("%s: super=%v bypassrls=%v login=%v, want all false", name, super, bypass, login)
		}
	}
	if len(seen) != 7 {
		t.Fatalf("roles found: %v", seen)
	}

	// Every table and view is owned by mhai_migrate; every table with
	// hospital_id has RLS enabled and forced. (platform_users is a
	// security_invoker view, so the users policies apply through it.)
	rows, err = pool.Query(ctx, `
		SELECT c.relname, pg_get_userbyid(c.relowner), c.relrowsecurity, c.relforcerowsecurity,
		       c.relkind = 'r' AND EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attname = 'hospital_id' AND NOT a.attisdropped)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r','v') AND c.relname <> 'goose_db_version'`)
	if err != nil {
		t.Fatal(err)
	}
	tables := 0
	for rows.Next() {
		var name, owner string
		var rls, force, hasHospital bool
		if err := rows.Scan(&name, &owner, &rls, &force, &hasHospital); err != nil {
			t.Fatal(err)
		}
		tables++
		if owner != "mhai_migrate" {
			t.Errorf("%s owned by %s, want mhai_migrate", name, owner)
		}
		if hasHospital && (!rls || !force) {
			t.Errorf("%s has hospital_id but rls=%v force=%v", name, rls, force)
		}
	}
	if tables < 18 {
		t.Fatalf("only %d tables and views found", tables)
	}
}

func TestRLSHidesOtherHospital(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, qa := seedHospital(t, pool, uniqueCode("A"))
	b, qb := seedHospital(t, pool, uniqueCode("B"))

	err := db.WithHospital(ctx, pool, a, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM clinical_queries`).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("hospital A sees %d queries, want 1", n)
		}
		var found int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM clinical_queries WHERE id = $1`, qb).Scan(&found); err != nil {
			return err
		}
		if found != 0 {
			t.Errorf("hospital A found hospital B's query")
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM clinical_queries WHERE id = $1`, qa).Scan(&found); err != nil {
			return err
		}
		if found != 1 {
			t.Errorf("hospital A can't find its own query")
		}
		// An update aimed at B's row touches nothing.
		tag, err := tx.Exec(ctx, `UPDATE clinical_queries SET status = 'done' WHERE id = $1`, qb)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Errorf("update of B's query affected %d rows", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithHospital: %v", err)
	}

	// Writing a row for another hospital is refused by the policy.
	err = db.WithHospital(ctx, pool, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency) VALUES ($1, $2, 'x', 'routine')`,
			uuid.New(), b)
		return err
	})
	if pgCode(err) != "42501" {
		t.Fatalf("insert for other hospital: want RLS violation (42501), got %v", err)
	}
}

func TestRLSWithoutHospitalSeesNothing(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("A"))
	seedHospital(t, pool, uniqueCode("B"))

	// One connection, so the second transaction reuses the session that
	// had mhai.hospital_id set by the first.
	cfg := pool.Config().Copy()
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()

	if err := db.WithHospital(ctx, one, a, func(pgx.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"clinical_queries", "users", "hospital_settings", "audit_log", "kb_chunks"} {
		tx, err := one.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_gateway`); err != nil {
			t.Fatal(err)
		}
		var n int
		err = tx.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE hospital_id IS NOT NULL`).Scan(&n)
		_ = tx.Rollback(ctx)
		if err != nil {
			t.Fatalf("%s without hospital: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s without hospital: %d rows, want 0", table, n)
		}
	}

	if err := db.WithHospital(ctx, one, uuid.Nil, func(pgx.Tx) error { return nil }); err == nil {
		t.Fatal("WithHospital accepted the nil hospital id")
	}
}

// The gateway's login role holds no table privileges of its own: a handler
// that forgets db.With* fails instead of reading anything.
func TestLoginRoleOutsideWithFails(t *testing.T) {
	pg, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("A"))
	if _, err := pool.Exec(ctx, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gw_login_test') THEN
			CREATE ROLE gw_login_test LOGIN PASSWORD 'test-only-login-password' IN ROLE mhai_app;
		END IF; END $$`); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User = "gw_login_test"
	cfg.ConnConfig.Password = "test-only-login-password"
	app, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	for _, table := range []string{"clinical_queries", "users", "audit_log", "hospitals", "sessions"} {
		var n int
		err := app.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
		requireDenied(t, err, "login role reading "+table)
	}
	err = db.WithHospital(ctx, app, a, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM clinical_queries`).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("login role in WithHospital sees %d queries, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("login role WithHospital: %v", err)
	}
}

func TestSharedKBVisible(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("A"))
	b, _ := seedHospital(t, pool, uniqueCode("B"))
	shared := uniqueCode("S")
	own := uniqueCode("O")
	if _, err := pool.Exec(ctx,
		`INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type) VALUES ($1, NULL, 'shared guideline', 'guideline'), ($2, $3, 'A only', 'guideline')`,
		shared, own, a); err != nil {
		t.Fatalf("seed kb: %v", err)
	}

	for _, h := range []uuid.UUID{a, b} {
		err := db.WithHospital(ctx, pool, h, func(tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM kb_chunks WHERE chunk_id = $1`, shared).Scan(&n); err != nil {
				return err
			}
			if n != 1 {
				t.Errorf("hospital %s can't see the shared chunk", h)
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM kb_chunks WHERE chunk_id = $1`, own).Scan(&n); err != nil {
				return err
			}
			if want := map[bool]int{true: 1, false: 0}[h == a]; n != want {
				t.Errorf("hospital %s sees %d of A's chunks, want %d", h, n, want)
			}
			for _, q := range []string{
				`UPDATE kb_chunks SET content = 'tampered' WHERE chunk_id = $1`,
				`DELETE FROM kb_chunks WHERE chunk_id = $1`,
			} {
				tag, err := tx.Exec(ctx, q, shared)
				if err != nil {
					return err
				}
				if tag.RowsAffected() != 0 {
					t.Errorf("%q on the shared chunk affected %d rows", q, tag.RowsAffected())
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("WithHospital(%s): %v", h, err)
		}
	}

	// A hospital can't create shared content.
	err := db.WithHospital(ctx, pool, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type) VALUES ($1, NULL, 'x', 'guideline')`, uniqueCode("N"))
		return err
	})
	if pgCode(err) != "42501" {
		t.Fatalf("insert of a shared chunk: want RLS violation, got %v", err)
	}

	// Shared content never holds patient data.
	_, err = pool.Exec(ctx, `INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type, source_patient_id) VALUES ($1, NULL, 'x', 'patient', 'P1')`, uniqueCode("P"))
	if pgCode(err) != "23514" {
		t.Fatalf("shared chunk with a patient id: want check violation, got %v", err)
	}
}

func TestAuditAppendOnly(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("A"))
	err := db.WithHospital(ctx, pool, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO audit_log (hospital_id, action, entity, payload, prev_hash, hash) VALUES ($1, 'test.event', 'test', '{}', '', 'h1')`, a)
		return err
	})
	if err != nil {
		t.Fatalf("gateway audit insert: %v", err)
	}
	for _, q := range []string{
		`UPDATE audit_log SET action = 'tampered'`,
		`DELETE FROM audit_log`,
		`TRUNCATE audit_log`,
	} {
		err := db.WithHospital(ctx, pool, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q)
			return err
		})
		requireDenied(t, err, "gateway "+q)
		for _, with := range []func(context.Context, *pgxpool.Pool, func(pgx.Tx) error) error{db.WithPlatform, db.WithAuth} {
			err := with(ctx, pool, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, q)
				return err
			})
			requireDenied(t, err, q)
		}
		requireDenied(t, asRole(ctx, pool, "mhai_worker", q), "worker "+q)
	}
}

// The audit chain is global, but the gateway sees only its hospital's
// rows; audit_chain_head() gives it the last id and hash, nothing else.
func TestAuditChainHeadAcrossHospitals(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("A"))
	b, _ := seedHospital(t, pool, uniqueCode("B"))
	var lastID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO audit_log (hospital_id, action, entity, payload, prev_hash, hash) VALUES ($1, 'test.b', 'test', '{}', '', 'head-of-b') RETURNING id`, b).
		Scan(&lastID); err != nil {
		t.Fatal(err)
	}
	err := db.WithHospital(ctx, pool, a, func(tx pgx.Tx) error {
		var id int64
		var hash string
		if err := tx.QueryRow(ctx, `SELECT id, hash FROM audit_chain_head()`).Scan(&id, &hash); err != nil {
			return err
		}
		if id != lastID || hash != "head-of-b" {
			t.Errorf("chain head = (%d, %q), want (%d, head-of-b)", id, hash, lastID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chain head: %v", err)
	}
}

func TestPlatformRoleNoClinical(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("A"))
	b, _ := seedHospital(t, pool, uniqueCode("B"))
	for _, h := range []uuid.UUID{a, b} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (hospital_id, email, password_hash, display_name) VALUES ($1, $2, 'synthetic-hash', 'Synthetic User')`,
			h, uniqueCode("u")+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}

	for _, table := range []string{"clinical_queries", "clinical_decisions", "hga_evaluations", "feedback_events", "kb_chunks", "user_mfa", "refresh_tokens", "trusted_devices", "hospital_settings"} {
		err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
			var n int
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
		})
		requireDenied(t, err, "platform reading "+table)
	}
	err := db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		var s string
		return tx.QueryRow(ctx, `SELECT password_hash FROM users LIMIT 1`).Scan(&s)
	})
	requireDenied(t, err, "platform reading password_hash")

	err = db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM platform_users WHERE hospital_id = ANY($1)`, []uuid.UUID{a, b}).Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			t.Errorf("platform_users shows %d of the 2 users", n)
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM hospitals WHERE id = ANY($1)`, []uuid.UUID{a, b}).Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			t.Errorf("platform sees %d of the 2 hospitals", n)
		}
		for _, table := range []string{"user_roles", "sessions", "audit_log"} {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
				return fmt.Errorf("%s: %w", table, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("platform reads: %v", err)
	}
	var cols int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_name = 'platform_users' AND column_name = 'password_hash'`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 0 {
		t.Fatal("platform_users exposes password_hash")
	}
}

func TestAuthRoleNoClinical(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	a, _ := seedHospital(t, pool, uniqueCode("A"))
	b, _ := seedHospital(t, pool, uniqueCode("B"))
	for _, table := range []string{"clinical_queries", "clinical_decisions", "hga_evaluations", "feedback_events", "kb_chunks"} {
		err := db.WithAuth(ctx, pool, func(tx pgx.Tx) error {
			var n int
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
		})
		requireDenied(t, err, "auth reading "+table)
	}
	// Sign-in looks a user up by email before any hospital is known.
	email := uniqueCode("Mixed") + "@Example.Test"
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (hospital_id, email, password_hash, display_name) VALUES ($1, $2, 'synthetic-hash', 'Synthetic User')`, b, email); err != nil {
		t.Fatal(err)
	}
	err := db.WithAuth(ctx, pool, func(tx pgx.Tx) error {
		var h uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT hospital_id FROM users WHERE email = $1`, strings.ToLower(email)).Scan(&h); err != nil {
			return err
		}
		if h != b {
			t.Errorf("auth lookup found hospital %s", h)
		}
		var n int
		return tx.QueryRow(ctx, `SELECT count(*) FROM hospitals WHERE id = ANY($1)`, []uuid.UUID{a, b}).Scan(&n)
	})
	if err != nil {
		t.Fatalf("auth sign-in reads: %v", err)
	}
	err = db.WithAuth(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE hospitals SET name = 'x' WHERE id = $1`, a)
		return err
	})
	requireDenied(t, err, "auth updating hospitals")
}

// asRole runs q in a transaction as role, with no hospital set.
func asRole(ctx context.Context, pool *pgxpool.Pool, role, q string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+pgx.Identifier{role}.Sanitize()); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, q)
	return err
}
