//go:build integration

package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00049 (the hospital admin's account and review actions): the
// outbox is hospital-scoped for the gateway, append-only, refuses a
// review message without its correction and a removed account that is
// active; down then up again with rows in place.
func TestMigration00049(t *testing.T) {
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
	users := map[uuid.UUID]uuid.UUID{}
	for i, id := range []uuid.UUID{h, h2} {
		if _, err := pool.Exec(ctx, `INSERT INTO hospitals (id, name, short_code) VALUES ($1, 'Synth Outbox', $2)`,
			id, "O"+string(rune('A'+i))+id.String()[:6]); err != nil {
			t.Fatal(err)
		}
		var u uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO users (hospital_id, email, display_name) VALUES ($1, $2, 'Synth O') RETURNING id`,
			id, "synth-o-49-"+id.String()[:8]+"@example.test").Scan(&u); err != nil {
			t.Fatal(err)
		}
		users[id] = u
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_messages (hospital_id, recipient_id, kind, channel, created_by)
		VALUES ($1, $2, 'mfa_reminder', 'log', $2)`, h2, users[h2]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_messages (hospital_id, recipient_id, kind, channel, created_by)
		VALUES ($1, $2, 'review_reminder', 'log', $2)`, h, users[h]); err == nil {
		t.Error("a review message without its correction")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_messages (hospital_id, recipient_id, kind, channel, created_by)
		VALUES ($1, $2, 'mfa_reminder', 'smtp', $2)`, h, users[h]); err == nil {
		t.Error("an unknown channel")
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET removed_at = now() WHERE id = $1`, users[h]); err == nil {
		t.Error("a removed account left active")
	}
	err = db.WithHospital(ctx, pool, h, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM account_messages`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("the gateway of h sees %d of h2's messages", n)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO account_messages (hospital_id, recipient_id, kind, channel, created_by)
			VALUES ($1, $2, 'mfa_reminder', 'log', $2)`, h, users[h]); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO account_messages (hospital_id, recipient_id, kind, channel, created_by) VALUES ('` + h2.String() + `', '` + users[h2].String() + `', 'mfa_reminder', 'log', '` + users[h2].String() + `')`,
		`UPDATE account_messages SET kind = 'mfa_reset'`,
		`DELETE FROM account_messages`,
	} {
		err := db.WithHospital(ctx, pool, h, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
		if err == nil {
			t.Errorf("the gateway may: %s", q[:30])
		}
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 48); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'account_messages'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after down: %d %v", n, err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
}

// Migration 00050 (invite and reset links): the gateway reads, adds and
// revokes its own hospital's tokens only; the sign-in role reads by hash
// and marks used or revoked, nothing else; one live token per person and
// purpose; a lifetime past 7 days is refused; down then up.
func TestMigration00050(t *testing.T) {
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
	users := map[uuid.UUID]uuid.UUID{}
	for i, id := range []uuid.UUID{h, h2} {
		if _, err := pool.Exec(ctx, `INSERT INTO hospitals (id, name, short_code) VALUES ($1, 'Synth Tokens', $2)`,
			id, "T"+string(rune('A'+i))+id.String()[:6]); err != nil {
			t.Fatal(err)
		}
		var u uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO users (hospital_id, email, display_name, invited_at) VALUES ($1, $2, 'Synth T', now()) RETURNING id`,
			id, "synth-t-50-"+id.String()[:8]+"@example.test").Scan(&u); err != nil {
			t.Fatal(err)
		}
		users[id] = u
	}
	ins := `INSERT INTO account_tokens (hospital_id, user_id, purpose, token_hash, created_by, expires_at) VALUES ($1, $2, 'invite', $3, $2, now() + $4::interval)`
	hash := func(c byte) string { return strings.Repeat(string(c), 64) }
	if _, err := pool.Exec(ctx, ins, h2, users[h2], hash('a'), "1 day"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, ins, h2, users[h2], hash('b'), "1 day"); err == nil {
		t.Error("two live tokens for one person and purpose")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_tokens (hospital_id, user_id, purpose, token_hash, created_by, expires_at)
		VALUES ($1, $2, 'password_reset', $3, $2, now() + interval '8 days')`, h2, users[h2], hash('c')); err == nil {
		t.Error("a token living past 7 days")
	}
	if _, err := pool.Exec(ctx, ins, h2, users[h2], "not-a-hash", "1 day"); err == nil {
		t.Error("a token stored in the clear")
	}
	err = db.WithHospital(ctx, pool, h, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM account_tokens`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("the gateway of h sees %d of h2's tokens", n)
		}
		ct, err := tx.Exec(ctx, `UPDATE account_tokens SET revoked_at = now()`)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 0 {
			t.Error("the gateway revoked another hospital's token")
		}
		_, err = tx.Exec(ctx, ins, h, users[h], hash('d'), "1 day")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE account_tokens SET used_at = now()`,
		`UPDATE account_tokens SET expires_at = now() + interval '1 day'`,
		`DELETE FROM account_tokens`,
	} {
		if err := db.WithHospital(ctx, pool, h, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err }); err == nil {
			t.Errorf("the gateway may: %s", q)
		}
	}
	err = db.WithAuth(ctx, pool, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM account_tokens WHERE token_hash = $1`, hash('a')).Scan(&n); err != nil || n != 1 {
			t.Errorf("sign-in reads by hash: %d %v", n, err)
		}
		_, err := tx.Exec(ctx, `UPDATE account_tokens SET used_at = now() WHERE token_hash = $1`, hash('a'))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO account_tokens (hospital_id, user_id, purpose, token_hash, created_by, expires_at) VALUES ('` + h.String() + `', '` + users[h].String() + `', 'password_reset', '` + hash('e') + `', '` + users[h].String() + `', now() + interval '1 hour')`,
		`UPDATE account_tokens SET expires_at = now() + interval '1 day'`,
		`DELETE FROM account_tokens`,
		`SELECT created_by FROM account_tokens`,
	} {
		if err := db.WithAuth(ctx, pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err }); err == nil {
			t.Errorf("the sign-in role may: %s", q[:40])
		}
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 49); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
}
