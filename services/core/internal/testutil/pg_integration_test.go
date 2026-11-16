//go:build integration

package testutil

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Two MigratedPostgres databases are apart: what one test writes, the next
// never sees; each is fully migrated, with the migrations' roles and row
// security; the app login works on it; and the clone is dropped when its
// test ends.
func TestMigratedPostgresClonesAreApart(t *testing.T) {
	ctx := context.Background()
	var first *PG
	t.Run("write", func(t *testing.T) {
		pg, super := MigratedPostgres(t)
		first = pg
		if _, err := super.Exec(ctx, `INSERT INTO hospitals (name, short_code) VALUES ('Clone A', 'CLONEA')`); err != nil {
			t.Fatal(err)
		}
	})
	pg, super := MigratedPostgres(t)
	if pg.URL == first.URL {
		t.Fatal("two tests got the same database")
	}
	var n int
	if err := super.QueryRow(ctx, `SELECT count(*) FROM hospitals WHERE short_code = 'CLONEA'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the second database sees %d of the first's hospitals (%v)", n, err)
	}
	var version int64
	if err := super.QueryRow(ctx, `SELECT max(version_id) FROM goose_db_version`).Scan(&version); err != nil || version == 0 {
		t.Fatalf("the clone isn't migrated: version %d (%v)", version, err)
	}
	var rls bool
	if err := super.QueryRow(ctx, `SELECT relrowsecurity FROM pg_class WHERE oid = 'hospitals'::regclass`).Scan(&rls); err != nil || !rls {
		t.Fatalf("row security on hospitals: %v (%v)", rls, err)
	}
	app, err := AppLogin(ctx, pg, super)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	var db string
	if err := app.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil || !strings.HasSuffix(pg.URL, "/"+db+"?sslmode=disable") {
		t.Fatalf("the app login is on %q, not the clone %s (%v)", db, pg.URL, err)
	}
	var left int
	if err := super.QueryRow(ctx, `SELECT count(*) FROM pg_database WHERE datname = $1`, first.name).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the first test's database is still there (%d, %v)", left, err)
	}
}

// Clones made at once, as parallel tests make them, all succeed.
func TestMigratedPostgresConcurrentClones(t *testing.T) {
	var wg sync.WaitGroup
	pools := make([]*pgxpool.Pool, 4)
	for i := range pools {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t.Run("clone", func(t *testing.T) {
				_, pools[i] = MigratedPostgres(t)
				if err := pools[i].Ping(context.Background()); err != nil {
					t.Error(err)
				}
			})
		}()
	}
	wg.Wait()
}

func TestWithDatabase(t *testing.T) {
	got := withDatabase("postgres://u:p@localhost:5432/mhai?sslmode=disable", "test_x")
	if got != "postgres://u:p@localhost:5432/test_x?sslmode=disable" {
		t.Fatalf("withDatabase: %s", got)
	}
	if got := withDatabase("%zz", "x"); got != "%zz" {
		t.Fatalf("an unparsable url comes back as it was: %s", got)
	}
}
