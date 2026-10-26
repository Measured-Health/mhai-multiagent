//go:build integration

package audit_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/gateway/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/gateway/internal/db"
	"github.com/Measured-Health/mhai-multiagent/gateway/internal/testutil"
)

// The chain is global, so these tests run one at a time against one server
// (no t.Parallel) and each checks the whole chain.
var super, app *pgxpool.Pool

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
		if super, err = db.Open(ctx, pg.URL); err != nil {
			fmt.Fprintln(os.Stderr, "open:", err)
			return 1
		}
		defer super.Close()
		if app, err = testutil.AppLogin(ctx, pg, super); err != nil {
			fmt.Fprintln(os.Stderr, "app login:", err)
			return 1
		}
		defer app.Close()
		return m.Run()
	}()
	os.Exit(code)
}

func seedHospital(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	code := "H" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	if err := super.QueryRow(context.Background(),
		`INSERT INTO hospitals (name, short_code) VALUES ('Synthetic Hospital', $1) RETURNING id`, code).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func event(h uuid.UUID, action string) audit.Event {
	return audit.Event{
		ActorID: uuid.NewString(), RequestID: "req-" + uuid.NewString()[:8],
		Action: action, Entity: "session", EntityID: uuid.NewString(),
		Payload: map[string]any{
			"reason": "synthetic <&>", "n": 3, "big": int64(9007199254740993), "f": 1.5, "tiny": 1e-9,
			"nested": map[string]any{"z": true, "a": []any{1, "two", nil}},
		},
		HospitalID: hospitalString(h),
	}
}

func hospitalString(h uuid.UUID) string {
	if h == uuid.Nil {
		return ""
	}
	return h.String()
}

// writeAs appends one event as the gateway (with a hospital), or as the
// auth or platform role (without).
func writeAs(t *testing.T, role string, e audit.Event) {
	t.Helper()
	ctx := context.Background()
	fn := func(tx pgx.Tx) error { return audit.Write(ctx, tx, e) }
	var err error
	switch role {
	case "gateway":
		err = db.WithHospital(ctx, app, uuid.MustParse(e.HospitalID), fn)
	case "auth":
		err = db.WithAuth(ctx, app, fn)
	case "platform":
		err = db.WithPlatform(ctx, app, fn)
	}
	if err != nil {
		t.Fatalf("write as %s: %v", role, err)
	}
}

func verify(t *testing.T) (bool, int64) {
	t.Helper()
	ok, broken, err := audit.Verify(context.Background(), app)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return ok, broken
}

func lastID(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := super.QueryRow(context.Background(), `SELECT max(id) FROM audit_log`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Writers in different hospitals and roles extend one chain, even though
// the gateway can see only its own hospital's rows.
func TestAuditChainVerifies(t *testing.T) {
	a, b := seedHospital(t), seedHospital(t)
	writeAs(t, "gateway", event(a, "auth.login.succeeded"))
	writeAs(t, "gateway", event(b, "auth.login.succeeded"))
	writeAs(t, "auth", event(uuid.Nil, "auth.login.failed"))
	writeAs(t, "platform", event(uuid.Nil, "platform.hospital.viewed"))
	writeAs(t, "gateway", event(a, "auth.logout"))
	writeAs(t, "gateway", audit.Event{HospitalID: a.String(), Action: "auth.refresh"})

	if ok, broken := verify(t); !ok {
		t.Fatalf("chain broken at %d", broken)
	}
	// Each row links to the one before it.
	rows, err := super.Query(context.Background(), `SELECT prev_hash, hash FROM audit_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	prev := strings.Repeat("0", 64)
	for rows.Next() {
		var p, h string
		if err := rows.Scan(&p, &h); err != nil {
			t.Fatal(err)
		}
		if p != prev || len(h) != 64 {
			t.Fatalf("link: prev %q want %q, hash %q", p, prev, h)
		}
		prev = h
	}
}

func TestAuditConcurrentWrites(t *testing.T) {
	hs := []uuid.UUID{seedHospital(t), seedHospital(t)}
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			h := hs[i%2]
			errs <- db.WithHospital(ctx, app, h, func(tx pgx.Tx) error {
				return audit.Write(ctx, tx, event(h, "test.concurrent"))
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if ok, broken := verify(t); !ok {
		t.Fatalf("chain broken at %d", broken)
	}
}

// Postgres jsonb reorders keys and rewrites numbers; the hash must not
// depend on that.
func TestAuditPayloadRoundTrip(t *testing.T) {
	h := seedHospital(t)
	e := event(h, "test.payload")
	e.Payload["huge"] = 1e300
	e.Payload["neg"] = -0.000001
	e.Payload["unicode"] = "Ŝynthetic   line"
	writeAs(t, "gateway", e)
	if ok, broken := verify(t); !ok {
		t.Fatalf("chain broken at %d", broken)
	}
}

func TestAuditTamperDetected(t *testing.T) {
	h := seedHospital(t)
	for i := 0; i < 3; i++ {
		writeAs(t, "gateway", event(h, "test.tamper"))
	}
	target := lastID(t) - 1
	writeAs(t, "gateway", event(h, "test.tamper"))
	if ok, _ := verify(t); !ok {
		t.Fatal("chain broken before tampering")
	}
	ctx := context.Background()
	// The audit is append-only for app roles, so tampering needs a superuser.
	if _, err := super.Exec(ctx, `UPDATE audit_log SET payload = '{"reason":"edited"}' WHERE id = $1`, target); err != nil {
		t.Fatal(err)
	}
	ok, broken := verify(t)
	if ok || broken != target {
		t.Fatalf("verify = %v, %d; want false, %d", ok, broken, target)
	}
	// Restore so later tests start from a valid chain.
	if _, err := super.Exec(ctx, `DELETE FROM audit_log`); err != nil {
		t.Fatal(err)
	}
}

func TestAuditTamperEveryColumn(t *testing.T) {
	ctx := context.Background()
	h := seedHospital(t)
	other := seedHospital(t)
	edits := []string{
		`ts = ts + interval '1 second'`,
		`actor_id = gen_random_uuid()`,
		`hospital_id = '` + other.String() + `'`,
		`request_id = 'req-edited'`,
		`action = 'test.edited'`,
		`entity = 'edited'`,
		`entity_id = 'edited'`,
		`payload = payload || '{"n": 4}'`,
		`hash = repeat('f', 64)`,
		`prev_hash = repeat('e', 64)`,
	}
	for _, edit := range edits {
		if _, err := super.Exec(ctx, `DELETE FROM audit_log`); err != nil {
			t.Fatal(err)
		}
		writeAs(t, "gateway", event(h, "test.column"))
		target := lastID(t)
		writeAs(t, "gateway", event(h, "test.column"))
		if _, err := super.Exec(ctx, `UPDATE audit_log SET `+edit+` WHERE id = $1`, target); err != nil {
			t.Fatalf("%s: %v", edit, err)
		}
		ok, broken := verify(t)
		if ok || broken != target {
			t.Errorf("%s: verify = %v, %d; want false, %d", edit, ok, broken, target)
		}
	}
	if _, err := super.Exec(ctx, `DELETE FROM audit_log`); err != nil {
		t.Fatal(err)
	}
}

// Deleting a row breaks the link of the row after it.
func TestAuditDeleteDetected(t *testing.T) {
	ctx := context.Background()
	h := seedHospital(t)
	for i := 0; i < 3; i++ {
		writeAs(t, "gateway", event(h, "test.delete"))
	}
	last := lastID(t)
	if _, err := super.Exec(ctx, `DELETE FROM audit_log WHERE id = $1`, last-1); err != nil {
		t.Fatal(err)
	}
	if ok, broken := verify(t); ok || broken != last {
		t.Fatalf("verify = %v, %d; want false, %d", ok, broken, last)
	}
	if _, err := super.Exec(ctx, `DELETE FROM audit_log`); err != nil {
		t.Fatal(err)
	}
}

// The gateway can't write another hospital's audit rows (RLS), and a
// transaction that doesn't read committed data is refused, since its head
// could be stale.
func TestAuditWriteGuards(t *testing.T) {
	ctx := context.Background()
	a, b := seedHospital(t), seedHospital(t)
	err := db.WithHospital(ctx, app, a, func(tx pgx.Tx) error {
		return audit.Write(ctx, tx, event(b, "test.cross"))
	})
	if err == nil {
		t.Fatal("wrote another hospital's audit row")
	}
	err = db.WithHospital(ctx, app, a, func(tx pgx.Tx) error {
		return audit.Write(ctx, tx, audit.Event{HospitalID: a.String()})
	})
	if err == nil {
		t.Fatal("wrote an event without an action")
	}
	err = db.WithHospital(ctx, app, a, func(tx pgx.Tx) error {
		return audit.Write(ctx, tx, audit.Event{HospitalID: "not-a-uuid", Action: "x"})
	})
	if err == nil {
		t.Fatal("wrote an event with a malformed hospital id")
	}
	tx, err := app.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_auth`); err != nil {
		t.Fatal(err)
	}
	if err := audit.Write(ctx, tx, event(uuid.Nil, "test.iso")); !errors.Is(err, audit.ErrIsolation) {
		t.Fatalf("repeatable read: %v", err)
	}
}
