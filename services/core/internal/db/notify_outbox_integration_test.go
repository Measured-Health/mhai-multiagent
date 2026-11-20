//go:build integration

package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// outboxWorld is two synthetic hospitals with one user each.
type outboxWorld struct {
	pool   *pgxpool.Pool
	h, h2  uuid.UUID
	u, u2  uuid.UUID
	url    string
	ctx    context.Context
	insert func(t *testing.T, h, u uuid.UUID, kind, extra string) uuid.UUID
}

func newOutboxWorld(t *testing.T) *outboxWorld {
	t.Helper()
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	w := &outboxWorld{pool: pool, url: pg.URL, ctx: ctx, h: uuid.New(), h2: uuid.New()}
	for i, id := range []uuid.UUID{w.h, w.h2} {
		if _, err := pool.Exec(ctx, `INSERT INTO hospitals (id, name, short_code) VALUES ($1, 'Synth Notify', $2)`,
			id, "N"+string(rune('A'+i))+id.String()[:6]); err != nil {
			t.Fatal(err)
		}
		var u uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO users (hospital_id, email, display_name) VALUES ($1, $2, 'Synth N') RETURNING id`,
			id, "synth-n-53-"+id.String()[:8]+"@example.test").Scan(&u); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			w.u = u
		} else {
			w.u2 = u
		}
	}
	// insert writes an outbox row as the superuser; extra is SQL for
	// created_at or published_at.
	w.insert = func(t *testing.T, h, u uuid.UUID, kind, extra string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		q := `INSERT INTO message_outbox (hospital_id, recipient_id, kind, channel, created_by) VALUES ($1, $2, $3, 'log', $2) RETURNING id`
		if err := pool.QueryRow(ctx, q, h, u, kind).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if extra != "" {
			if _, err := pool.Exec(ctx, `UPDATE message_outbox SET `+extra+` WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	return w
}

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// asRoleTx runs fn as role (no hospital) in a transaction that commits.
func asRoleTx(ctx context.Context, pool *pgxpool.Pool, role string, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Migration 00053: the outbox keeps working for the gateway under both
// names and its hospital only; the new columns hold ids only; the
// dedupe key is unique per hospital; the drainer's role reaches the outbox
// only through its three definers, which never hand out a link kind or an
// old row; the gateway reads its hospital's delivery outcomes and nothing
// else of them.
func TestMigration00053Outbox(t *testing.T) {
	w := newOutboxWorld(t)
	ctx, pool := w.ctx, w.pool
	// The gateway writes through the table and through the old name, sees
	// only its own hospital's rows, and may not mark one published.
	err := db.WithHospital(ctx, pool, w.h, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO account_messages (hospital_id, recipient_id, kind, channel, created_by)
			VALUES ($1, $2, 'mfa_reminder', 'log', $2)`, w.h, w.u); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO message_outbox (hospital_id, recipient_id, kind, channel, created_by, dedupe_key)
			VALUES ($1, $2, 'mfa_reset', 'email', $2, 'mfa_reset:`+w.u.String()+`')`, w.h, w.u); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("gateway writes: %v", err)
	}
	w.insert(t, w.h2, w.u2, "mfa_reminder", "")
	for _, name := range []string{"message_outbox", "account_messages"} {
		err := db.WithHospital(ctx, pool, w.h, func(tx pgx.Tx) error {
			var n, other int
			if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE hospital_id <> $1) FROM `+name, w.h).
				Scan(&n, &other); err != nil {
				return err
			}
			if n != 2 || other != 0 {
				t.Errorf("%s: the gateway sees %d rows, %d of another hospital", name, n, other)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`UPDATE message_outbox SET published_at = now()`,
		`DELETE FROM message_outbox`,
		`SELECT mhai_outbox_due(10)`,
		`SELECT notify_claim(gen_random_uuid(), interval '1 minute')`,
		`SELECT lease_id FROM message_deliveries`,
		`UPDATE message_deliveries SET status = 'sent'`,
	} {
		if err := db.WithHospital(ctx, pool, w.h, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err }); err == nil {
			t.Errorf("the gateway may: %s", q)
		}
	}
	// Shapes: a channel the outbox doesn't know, free text in the dedupe
	// key, and a second row with one hospital's key are refused; another
	// hospital may use the same key.
	key := "review_reminder:" + uuid.NewString() + ":1"
	for _, tc := range []struct {
		q    string
		args []any
		ok   bool
	}{
		{`INSERT INTO message_outbox (hospital_id, recipient_id, kind, channel, created_by) VALUES ($1, $2, 'mfa_reset', 'smtp', $2)`,
			[]any{w.h, w.u}, false},
		{`INSERT INTO message_outbox (hospital_id, recipient_id, kind, channel, created_by, dedupe_key) VALUES ($1, $2, 'mfa_reset', 'log', $2, 'Reset for Synthetic Patient')`,
			[]any{w.h, w.u}, false},
		{`INSERT INTO message_outbox (hospital_id, recipient_id, kind, channel, created_by, dedupe_key) VALUES ($1, $2, 'mfa_reset', 'log', $2, $3)`,
			[]any{w.h, w.u, key}, true},
		{`INSERT INTO message_outbox (hospital_id, recipient_id, kind, channel, created_by, dedupe_key) VALUES ($1, $2, 'mfa_reset', 'log', $2, $3)`,
			[]any{w.h, w.u, key}, false},
		{`INSERT INTO message_outbox (hospital_id, recipient_id, kind, channel, created_by, dedupe_key) VALUES ($1, $2, 'mfa_reset', 'log', $2, $3)`,
			[]any{w.h2, w.u2, key}, true},
	} {
		_, err := pool.Exec(ctx, tc.q, tc.args...)
		if (err == nil) != tc.ok {
			t.Errorf("%s %v: %v, want ok %v", tc.q[60:], tc.args[len(tc.args)-1], err, tc.ok)
		}
	}

	// The drainer's role: no direct read, only the definers.
	if err := asRoleTx(ctx, pool, "mhai_notify_outbox", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT 1 FROM message_outbox`)
		return err
	}); sqlState(err) != "42501" {
		t.Fatalf("mhai_notify_outbox reads the outbox: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM message_outbox`); err != nil {
		t.Fatal(err)
	}
	fresh := w.insert(t, w.h, w.u, "mfa_reminder", "")
	other := w.insert(t, w.h2, w.u2, "mfa_reset", "")
	w.insert(t, w.h, w.u, "invite", "")
	w.insert(t, w.h, w.u, "password_reset", "")
	w.insert(t, w.h, w.u, "mfa_reminder", "created_at = now() - interval '25 hours'")
	w.insert(t, w.h, w.u, "mfa_reminder", "published_at = now()")
	old := w.insert(t, w.h, w.u, "mfa_reminder", "created_at = now() - interval '91 days'")
	var due map[uuid.UUID]uuid.UUID
	if err := asRoleTx(ctx, pool, "mhai_notify_outbox", func(tx pgx.Tx) error {
		due = map[uuid.UUID]uuid.UUID{}
		rows, err := tx.Query(ctx, `SELECT id, hospital_id FROM mhai_outbox_due(100)`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, h uuid.UUID
			if err := rows.Scan(&id, &h); err != nil {
				return err
			}
			due[id] = h
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 || due[fresh] != w.h || due[other] != w.h2 {
		t.Fatalf("due: %v (want the two fresh unpublished non-link rows)", due)
	}
	var marked, removed int
	if err := asRoleTx(ctx, pool, "mhai_notify_outbox", func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT mhai_outbox_published($1)`, []uuid.UUID{fresh, other, fresh}).Scan(&marked); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT mhai_outbox_retention()`).Scan(&removed)
	}); err != nil {
		t.Fatal(err)
	}
	if marked != 2 || removed != 1 {
		t.Fatalf("marked %d, removed %d", marked, removed)
	}
	var gone bool
	if err := pool.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM message_outbox WHERE id = $1)`, old).Scan(&gone); err != nil || !gone {
		t.Fatalf("the 91-day-old row stays: %v", err)
	}
	if err := asRoleTx(ctx, pool, "mhai_notify_outbox", func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM mhai_outbox_due(100)`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("%d rows still due after publishing", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The gateway reads its hospital's delivery outcomes.
	if _, err := pool.Exec(ctx, `INSERT INTO message_deliveries (message_id, hospital_id, recipient_id, kind, channel, status, outcome_code, sent_at)
		VALUES ($1, $2, $3, 'mfa_reminder', 'log', 'sent', 'accepted', now()), (gen_random_uuid(), $4, $5, 'mfa_reset', 'log', 'claimed', NULL, NULL)`,
		fresh, w.h, w.u, w.h2, w.u2); err != nil {
		t.Fatal(err)
	}
	if err := db.WithHospital(ctx, pool, w.h, func(tx pgx.Tx) error {
		var n int
		var status string
		if err := tx.QueryRow(ctx, `SELECT count(*), max(status) FROM message_deliveries`).Scan(&n, &status); err != nil {
			return err
		}
		if n != 1 || status != "sent" {
			t.Errorf("the gateway sees %d deliveries (%s)", n, status)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A finished delivery never changes.
	if _, err := pool.Exec(ctx, `UPDATE message_deliveries SET status = 'failed', outcome_code = 'provider_down', sent_at = NULL
		WHERE message_id = $1`, fresh); err == nil {
		t.Error("a sent delivery changed")
	}

	// Down below 00053 and up again, with rows in place.
	if err := db.MigrateDownTo(ctx, w.url, 52); err != nil {
		t.Fatalf("down: %v", err)
	}
	var tables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_name IN ('message_outbox', 'message_deliveries', 'message_send_counts')`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("after down: %d %v", tables, err)
	}
	if err := db.MigrateUp(ctx, w.url); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
