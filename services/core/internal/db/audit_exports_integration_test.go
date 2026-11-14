//go:build integration

package db_test

import (
	"context"
	"crypto/sha256"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Migration 00044 (analysis backend plan Task 24): export records and
// their rows, legal references, each audited at commit. The superuser
// seeds the fixtures; the platform and the gateway act through their
// roles. Synthetic hospitals, accounts, letters and subjects only.

var exportRefShape = regexp.MustCompile(`^[0-9a-f]{24}$`)

// exportWorld is a named world whose admin is covered by an active letter
// and has opened one named list (its entry committed), and a platform
// admin with two-step.
type exportWorld struct {
	namedWorld
	letter   uuid.UUID
	list     namedOpen
	platform uuid.UUID
}

func platformAdmin(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO users (hospital_id, email, password_hash, display_name)
		VALUES (NULL, $1, 'synthetic-hash', 'Synthetic Platform Admin') RETURNING id`, uniqueCode("pa")+"@example.test").Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`INSERT INTO user_roles (user_id, role) VALUES ($1, 'platform_admin')`,
		`INSERT INTO user_mfa (user_id, secret_enc, enabled) VALUES ($1, '\x00', true)`} {
		if _, err := pool.Exec(ctx, q, id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func newExportWorld(t *testing.T, pool *pgxpool.Pool, n int) exportWorld {
	t.Helper()
	w := exportWorld{namedWorld: newNamedWorld(t, pool, n)}
	w.letter = activeLetter(t, pool, w.h, 30, w.admin)
	var err error
	if w.list, err = openNamed(context.Background(), pool, w.namedWorld, w.admin, false); err != nil {
		t.Fatal(err)
	}
	w.platform = platformAdmin(t, pool)
	return w
}

// asPlatformTx runs fn as mhai_platform and commits.
func asPlatformTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_platform`); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var zeroSum = strings.Repeat("0", 64)

// insertExport records an export as the platform would, without its
// entry: a named one filtered on its hospital only.
func insertExport(ctx context.Context, tx pgx.Tx, id, owner uuid.UUID, kind string, hospital *uuid.UUID) error {
	f := map[string]any{}
	if hospital != nil {
		f["hospital"] = hospital.String()
	}
	return insertExportF(ctx, tx, id, owner, kind, hospital, f)
}

// insertExportF is insertExport with the record's filters.
func insertExportF(ctx context.Context, tx pgx.Tx, id, owner uuid.UUID, kind string, hospital *uuid.UUID, filters map[string]any) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_exports (id, kind, owner, hospital_id, reason, filters, max_seq, rows, checksum, file_name)
		VALUES ($1, $2, $3, $4, 'internal_review', $6, (SELECT COALESCE(max(id), 0) FROM audit_log), 0, $5, 'audit-synthetic.csv')`,
		id, kind, owner, hospital, zeroSum, filters)
	return err
}

// exportEntry appends action for export id by owner. "exported" is the
// record's entry for its kind.
func exportEntry(ctx context.Context, tx pgx.Tx, action string, id, owner uuid.UUID) error {
	payload := map[string]any{}
	if action == "audit.exported" {
		var kind string
		if err := tx.QueryRow(ctx, `SELECT kind FROM audit_exports WHERE id = $1`, id).Scan(&kind); err == nil && kind == "named_access" {
			action = "audit.named_access_exported"
		}
		payload = map[string]any{"rows": 0, "capped": false, "filters": map[string]any{}}
	}
	return audit.Write(ctx, tx, audit.Event{ActorID: owner.String(), Action: action, Entity: "audit_export", EntityID: id.String(),
		Payload: payload})
}

// namedExport makes a named export of w's hospital by owner, with its rows
// and entry, and returns its id and row count.
func namedExport(t *testing.T, pool *pgxpool.Pool, w exportWorld, owner uuid.UUID) (uuid.UUID, int) {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	var n int
	if err := asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
		if err := insertExport(ctx, tx, id, owner, "named_access", &w.h); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT mhai_audit_export_named($1, 50000)`, id).Scan(&n); err != nil {
			return err
		}
		return exportEntry(ctx, tx, "audit.exported", id, owner)
	}); err != nil {
		t.Fatal(err)
	}
	// Rows aren't the platform's to change: the fixture sets them.
	if _, err := pool.Exec(ctx, `UPDATE audit_exports SET rows = $2 WHERE id = $1`, id, n); err != nil {
		t.Fatal(err)
	}
	return id, n
}

// An export and its download are kept only with their entries, by the
// export's owner, naming that export.
func TestExportRecordAudited(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	owner := platformAdmin(t, pool)
	id := uuid.New()
	err := asPlatformTx(ctx, pool, func(tx pgx.Tx) error { return insertExport(ctx, tx, id, owner, "events", nil) })
	if pgCode(err) != "23514" {
		t.Fatalf("an export without its entry: %v", err)
	}
	err = asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
		if err := insertExport(ctx, tx, id, owner, "events", nil); err != nil {
			return err
		}
		return exportEntry(ctx, tx, "audit.exported", uuid.New(), owner)
	})
	if pgCode(err) != "23514" {
		t.Fatalf("an export with another's entry: %v", err)
	}
	err = asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
		if err := insertExport(ctx, tx, id, owner, "events", nil); err != nil {
			return err
		}
		return exportEntry(ctx, tx, "audit.exported", id, platformAdmin(t, pool))
	})
	if pgCode(err) != "23514" {
		t.Fatalf("an export with someone else's entry: %v", err)
	}
	if err := asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
		if err := insertExport(ctx, tx, id, owner, "events", nil); err != nil {
			return err
		}
		return exportEntry(ctx, tx, "audit.exported", id, owner)
	}); err != nil {
		t.Fatalf("an audited export: %v", err)
	}
	download := func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE audit_exports SET downloaded_at = now(), download_xact = pg_current_xact_id() WHERE id = $1`, id)
		return err
	}
	if err := asPlatformTx(ctx, pool, download); pgCode(err) != "23514" {
		t.Fatalf("a download without its entry: %v", err)
	}
	if err := asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
		if err := download(tx); err != nil {
			return err
		}
		return exportEntry(ctx, tx, "audit.export_downloaded", id, owner)
	}); err != nil {
		t.Fatalf("an audited download: %v", err)
	}
}

// A record is downloaded once, before it expires, by its own transaction;
// nothing else of it changes, and none is deleted.
func TestExportDownloadOnce(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	owner := platformAdmin(t, pool)
	mk := func() uuid.UUID {
		id := uuid.New()
		if err := asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
			if err := insertExport(ctx, tx, id, owner, "events", nil); err != nil {
				return err
			}
			return exportEntry(ctx, tx, "audit.exported", id, owner)
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	down := func(id uuid.UUID, set string) error {
		return asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE audit_exports SET `+set+` WHERE id = $1`, id); err != nil {
				return err
			}
			return exportEntry(ctx, tx, "audit.export_downloaded", id, owner)
		})
	}
	const once = `downloaded_at = now(), download_xact = pg_current_xact_id()`
	id := mk()
	if err := down(id, once); err != nil {
		t.Fatalf("first download: %v", err)
	}
	if err := down(id, once); p0001(err) != "gone" {
		t.Errorf("second download: %v", err)
	}
	// Another time or transaction than now and this one.
	id = mk()
	if err := down(id, `downloaded_at = now() - interval '1 minute', download_xact = pg_current_xact_id()`); p0001(err) != "gone" {
		t.Errorf("a backdated download: %v", err)
	}
	if err := down(id, `downloaded_at = now(), download_xact = '1'::xid8`); p0001(err) != "gone" {
		t.Errorf("another transaction's download: %v", err)
	}
	// Past expires_at (the fixture moves the record back).
	if _, err := pool.Exec(ctx, `UPDATE audit_exports SET created_at = now() - interval '16 minutes',
		expires_at = now() - interval '1 minute' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := down(id, once); p0001(err) != "gone" {
		t.Errorf("an expired download: %v", err)
	}
	// Made already downloaded, or with its own times: never.
	err := asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
		x := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO audit_exports (id, kind, owner, reason, filters, max_seq, rows, checksum, file_name,
			downloaded_at, download_xact) VALUES ($1, 'events', $2, 'internal_review', '{}', 0, 0, $3, 'a.csv', now(), pg_current_xact_id())`,
			x, owner, zeroSum); err != nil {
			return err
		}
		return exportEntry(ctx, tx, "audit.exported", x, owner)
	})
	if p0001(err) != "gone" {
		t.Errorf("made downloaded: %v", err)
	}
	requireDenied(t, asRole(ctx, pool, "mhai_platform", `DELETE FROM audit_exports`), "the platform deleting an export")
	requireDenied(t, asRole(ctx, pool, "mhai_platform", `UPDATE audit_exports SET owner = owner`), "the platform changing an owner")
}

// A named export's rows: one per entry and list row, refs unique and
// opaque; over the max nothing is written; only in the creation
// transaction, once.
func TestExportNamedRows(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newExportWorld(t, pool, 7)
	// A de-identified list's entry of the same hospital isn't taken.
	if _, _, err := open(ctx, pool, w.drill, w.admin, w.week, `{}`, femaleGroup, len(w.subjects), 300, false); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	var over, n int
	err := asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
		if err := insertExport(ctx, tx, id, w.platform, "named_access", &w.h); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT mhai_audit_export_named($1, 6)`, id).Scan(&over); err != nil {
			return err
		}
		return errSkip
	})
	if err != errSkip {
		t.Fatal(err)
	}
	// The record's own filters choose the rows (security review): its
	// person and letter.
	id = uuid.New()
	err = asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
		if err := insertExportF(ctx, tx, id, w.platform, "named_access", &w.h, map[string]any{"hospital": w.h.String(),
			"person": w.admin.String(), "clearance": w.letter.String()}); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT mhai_audit_export_named($1, 50000)`, id).Scan(&n); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT mhai_audit_export_named($1, 50000)`, id)
		if pgCode(err) != "22023" {
			t.Errorf("rows made twice: %v", err)
		}
		return errSkip
	})
	if err != errSkip {
		t.Fatal(err)
	}
	if over != 7 || n != 7 {
		t.Errorf("counted %d over the max, %d rows; want 7 and 7", over, n)
	}
	// Another person's or letter's filter takes nothing.
	id = uuid.New()
	if err := asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
		if err := insertExportF(ctx, tx, id, w.platform, "named_access", &w.h, map[string]any{"hospital": w.h.String(),
			"person": w.analyst.String()}); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT mhai_audit_export_named($1, 50000)`, id).Scan(&n); err != nil {
			return err
		}
		return errSkip
	}); err != errSkip || n != 0 {
		t.Errorf("another person's entries: %d (%v)", n, err)
	}
	// Outside its creation transaction: refused.
	eid, rows := namedExport(t, pool, w, w.platform)
	if rows != 7 {
		t.Fatalf("export rows %d", rows)
	}
	err = asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT mhai_audit_export_named($1, 50000)`, eid)
		return err
	})
	if pgCode(err) != "22023" {
		t.Errorf("rows made outside the creation transaction: %v", err)
	}
	var refs []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(ref ORDER BY ord) FROM audit_export_rows WHERE export_id = $1`, eid).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range refs {
		if !exportRefShape.MatchString(r) || seen[r] || strings.Contains(r, "P-") {
			t.Errorf("ref %q", r)
		}
		seen[r] = true
	}
}

var errSkip = errorString("skip: roll back")

type errorString string

func (e errorString) Error() string { return string(e) }

// Pids leave only in the export's creation transaction or its one
// download transaction.
func TestExportPidsOnlyInTheirTransactions(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newExportWorld(t, pool, 6)
	eid, _ := namedExport(t, pool, w, w.platform)
	count := func(set bool) int {
		var n int
		err := asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
			if set {
				if _, err := tx.Exec(ctx, `UPDATE audit_exports SET downloaded_at = now(), download_xact = pg_current_xact_id() WHERE id = $1`, eid); err != nil {
					return err
				}
				if err := exportEntry(ctx, tx, "audit.export_downloaded", eid, w.platform); err != nil {
					return err
				}
			}
			var pids []string
			rows, err := tx.Query(ctx, `SELECT pid FROM mhai_audit_export_pids($1)`, eid)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var p string
				if err := rows.Scan(&p); err != nil {
					return err
				}
				pids = append(pids, p)
			}
			n = len(pids)
			return rows.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(false); n != 0 {
		t.Errorf("another transaction read %d pids", n)
	}
	// The download's entry comes first: without it, no pid (security
	// review).
	err := asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE audit_exports SET downloaded_at = now(), download_xact = pg_current_xact_id() WHERE id = $1`, eid); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT count(*) FROM mhai_audit_export_pids($1)`, eid)
		return err
	})
	if p0001(err) != "not_audited" {
		t.Errorf("pids in a download without its entry: %v", err)
	}
	// Nor in the creation transaction before its entry.
	fresh := uuid.New()
	err = asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
		if err := insertExport(ctx, tx, fresh, w.platform, "named_access", &w.h); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT mhai_audit_export_named($1, 50000)`, fresh); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT count(*) FROM mhai_audit_export_pids($1)`, fresh)
		return err
	})
	if p0001(err) != "not_audited" {
		t.Errorf("pids in the creation transaction without its entry: %v", err)
	}
	// An entry committed earlier (by anyone) never stands for this
	// transaction's (security review): forge one, then make the export.
	forged := uuid.New()
	if err := asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
		return audit.Write(ctx, tx, audit.Event{ActorID: w.platform.String(), Action: "audit.named_access_exported",
			Entity: "audit_export", EntityID: forged.String(), Payload: map[string]any{"rows": 0, "capped": false,
				"filters": map[string]any{}}})
	}); err != nil {
		t.Fatal(err)
	}
	err = asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
		if err := insertExport(ctx, tx, forged, w.platform, "named_access", &w.h); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT mhai_audit_export_named($1, 50000)`, forged); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT count(*) FROM mhai_audit_export_pids($1)`, forged)
		return err
	})
	if p0001(err) != "not_audited" {
		t.Errorf("pids under an entry from another transaction: %v", err)
	}
	// A record owned by a hospital account: refused.
	err = asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
		return insertExport(ctx, tx, uuid.New(), w.admin, "named_access", &w.h)
	})
	if p0001(err) != "forbidden" {
		t.Errorf("a hospital account's export: %v", err)
	}
	if n := count(true); n != 6 {
		t.Errorf("the download read %d pids, want 6", n)
	}
	if n := count(false); n != 0 {
		t.Errorf("after the download, %d pids", n)
	}
}

// createRef binds export to reference as by, with its entry unless
// noAudit.
func createRef(ctx context.Context, pool *pgxpool.Pool, export uuid.UUID, reference string, admins []uuid.UUID, by uuid.UUID,
	noAudit bool) (uuid.UUID, error) {
	var id uuid.UUID
	err := asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT id FROM mhai_legal_reference_create($1, $2, $3, $4)`, export, reference, admins, by).Scan(&id); err != nil {
			return err
		}
		if noAudit {
			return nil
		}
		named := make([]any, len(admins))
		for i, a := range admins {
			named[i] = a.String()
		}
		var h uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT hospital_id FROM audit_exports WHERE id = $1`, export).Scan(&h); err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Event{ActorID: by.String(), Action: "legal_reference.created", Entity: "legal_reference",
			EntityID: id.String(), Payload: map[string]any{"export_id": export.String(), "hospital_id": h.String(), "admins": named}})
	})
	return id, err
}

// Step 1's rules (design §8.9): who binds, what, to whom, once.
func TestLegalReferenceRules(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newExportWorld(t, pool, 6)
	eid, _ := namedExport(t, pool, w, w.platform)
	admins := []uuid.UUID{w.admin}
	ref := "LR-" + uniqueCode("x")
	for _, c := range []struct {
		name   string
		export uuid.UUID
		admins []uuid.UUID
		by     uuid.UUID
		ref    string
		want   string
	}{
		{"not the export's owner", eid, admins, platformAdmin(t, pool), ref, "forbidden"},
		{"a hospital admin binding", eid, admins, w.admin, ref, "forbidden"},
		{"no such export", uuid.New(), admins, w.platform, ref, "not_found"},
		{"an analyst named", eid, []uuid.UUID{w.analyst}, w.platform, ref, "not_eligible"},
		{"another hospital's admin", eid, []uuid.UUID{staffer(t, pool, onHospital(t, pool, "Africa/Lagos"), staffOpts{roles: []string{"hospital_admin"}})}, w.platform, ref, "not_eligible"},
		{"an admin without two-step", eid, []uuid.UUID{staffer(t, pool, w.h, staffOpts{roles: []string{"hospital_admin"}, noMFA: true})}, w.platform, ref, "not_eligible"},
		{"an admin twice", eid, []uuid.UUID{w.admin, w.admin}, w.platform, ref, "not_eligible"},
		{"four admins", eid, []uuid.UUID{w.admin, uuid.New(), uuid.New(), uuid.New()}, w.platform, ref, "not_eligible"},
		{"no admin", eid, []uuid.UUID{}, w.platform, ref, "not_eligible"},
		{"a bad reference", eid, admins, w.platform, "=cmd()", "invalid"},
	} {
		if _, err := createRef(ctx, pool, c.export, c.ref, c.admins, c.by, false); p0001(err) != c.want {
			t.Errorf("%s: %v, want %s", c.name, err, c.want)
		}
	}
	// An events export isn't a named one.
	ev := uuid.New()
	if err := asPlatformTx(ctx, pool, func(tx pgx.Tx) error {
		if err := insertExport(ctx, tx, ev, w.platform, "events", nil); err != nil {
			return err
		}
		return exportEntry(ctx, tx, "audit.exported", ev, w.platform)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := createRef(ctx, pool, ev, ref, admins, w.platform, false); p0001(err) != "not_found" {
		t.Errorf("an events export: %v", err)
	}
	// 201 rows: refused at step 1.
	big, _ := namedExport(t, pool, w, w.platform)
	if _, err := pool.Exec(ctx, `UPDATE audit_exports SET rows = 201 WHERE id = $1`, big); err != nil {
		t.Fatal(err)
	}
	if _, err := createRef(ctx, pool, big, ref, admins, w.platform, false); p0001(err) != "too_many_rows" {
		t.Errorf("201 rows: %v", err)
	}
	// A record whose row count isn't its rows' (security review: the
	// function counts them).
	lied, _ := namedExport(t, pool, w, w.platform)
	if _, err := pool.Exec(ctx, `UPDATE audit_exports SET rows = 1 WHERE id = $1`, lied); err != nil {
		t.Fatal(err)
	}
	if _, err := createRef(ctx, pool, lied, ref, admins, w.platform, false); p0001(err) != "invalid" {
		t.Errorf("a record that undercounts: %v", err)
	}
	// Without its entry: refused at commit.
	if _, err := createRef(ctx, pool, eid, ref, admins, w.platform, true); pgCode(err) != "23514" {
		t.Errorf("a reference without its entry: %v", err)
	}
	if _, err := createRef(ctx, pool, eid, ref, admins, w.platform, false); err != nil {
		t.Fatalf("a reference: %v", err)
	}
	// One reference per export, one export per reference.
	if _, err := createRef(ctx, pool, eid, ref+"B", admins, w.platform, false); p0001(err) != "conflict" {
		t.Errorf("an export bound twice: %v", err)
	}
	other, _ := namedExport(t, pool, w, w.platform)
	if _, err := createRef(ctx, pool, other, ref, admins, w.platform, false); p0001(err) != "conflict" {
		t.Errorf("a reference used twice: %v", err)
	}
}

// resolve runs mhai_legal_subjects as user with key, appending its entry
// unless noAudit; it returns the rows and the replay's number.
func resolve(ctx context.Context, pool *pgxpool.Pool, h, user, ref uuid.UUID, key string, noAudit bool) (int, int, error) {
	k := sha256.Sum256([]byte(key))
	var n, replays int
	var export, clearance uuid.UUID
	err := asPerson(ctx, pool, h, user, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT ref, ref_enc, export_id, clearance_id, replays FROM mhai_legal_subjects($1, $2)`, ref, k[:])
		if err != nil {
			return err
		}
		for rows.Next() {
			var r string
			var enc []byte
			if err := rows.Scan(&r, &enc, &export, &clearance, &replays); err != nil {
				rows.Close()
				return err
			}
			n++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if noAudit {
			return nil
		}
		return audit.Write(ctx, tx, audit.Event{ActorID: user.String(), HospitalID: h.String(), Action: "analysis.legal_resolved",
			Entity: "legal_reference", EntityID: ref.String(), Payload: map[string]any{"export_id": export.String(),
				"clearance_id": clearance.String(), "rows": n, "replays": replays}})
	})
	return n, replays, err
}

// T29 in SQL: parallel resolutions, exactly one succeeds.
func TestLegalSubjectsOnceUnderRace(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newExportWorld(t, pool, 6)
	eid, _ := namedExport(t, pool, w, w.platform)
	ref, err := createRef(ctx, pool, eid, "LR-"+uniqueCode("r"), []uuid.UUID{w.admin}, w.platform, false)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, used := 0, 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := resolve(ctx, pool, w.h, w.admin, ref, "synthetic-key-"+string(rune('a'+i)), false)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case p0001(err) == "already_used":
				used++
			default:
				t.Errorf("resolution %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 || used != 9 {
		t.Errorf("%d succeeded and %d were already_used; want 1 and 9", ok, used)
	}
}

// Step 2's rules: who, the cover, once, replays by key, audited.
func TestLegalSubjectsRefusals(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	w := newExportWorld(t, pool, 6)
	eid, _ := namedExport(t, pool, w, w.platform)
	other := staffer(t, pool, w.h, staffOpts{roles: []string{"hospital_admin"}})
	ref, err := createRef(ctx, pool, eid, "LR-"+uniqueCode("f"), []uuid.UUID{w.admin, other}, w.platform, false)
	if err != nil {
		t.Fatal(err)
	}
	// Not named, another hospital, no such reference: not_found.
	stranger := staffer(t, pool, w.h, staffOpts{roles: []string{"hospital_admin"}})
	activeLetter(t, pool, w.h, 30, stranger)
	if _, _, err := resolve(ctx, pool, w.h, stranger, ref, "k1", false); p0001(err) != "not_found" {
		t.Errorf("an admin not named: %v", err)
	}
	h2 := onHospital(t, pool, "Africa/Lagos")
	if _, _, err := resolve(ctx, pool, h2, w.admin, ref, "k1", false); p0001(err) != "not_found" {
		t.Errorf("another hospital's session: %v", err)
	}
	if _, _, err := resolve(ctx, pool, w.h, w.admin, uuid.New(), "k1", false); p0001(err) != "not_found" {
		t.Errorf("no such reference: %v", err)
	}
	// Named, but not covered by a letter: not_cleared, nothing used.
	if _, _, err := resolve(ctx, pool, w.h, other, ref, "k1", false); p0001(err) != "not_cleared" {
		t.Errorf("an uncovered admin: %v", err)
	}
	// Without its entry: refused at commit, nothing used.
	if _, _, err := resolve(ctx, pool, w.h, w.admin, ref, "k1", true); pgCode(err) != "23514" {
		t.Errorf("a resolution without its entry: %v", err)
	}
	n, rep, err := resolve(ctx, pool, w.h, w.admin, ref, "k1", false)
	if err != nil || n != 6 || rep != 0 {
		t.Fatalf("the resolution: %d rows, replay %d, %v", n, rep, err)
	}
	// Another key, or another admin: already_used.
	if _, _, err := resolve(ctx, pool, w.h, w.admin, ref, "k2", false); p0001(err) != "already_used" {
		t.Errorf("a new key: %v", err)
	}
	activeLetter(t, pool, w.h, 30, other)
	if _, _, err := resolve(ctx, pool, w.h, other, ref, "k1", false); p0001(err) != "already_used" {
		t.Errorf("the other named admin: %v", err)
	}
	// The same key replays, each audited with its number, at most 3.
	for i := 1; i <= 3; i++ {
		if _, rep, err := resolve(ctx, pool, w.h, w.admin, ref, "k1", false); err != nil || rep != i {
			t.Fatalf("replay %d: %d %v", i, rep, err)
		}
	}
	if _, _, err := resolve(ctx, pool, w.h, w.admin, ref, "k1", false); p0001(err) != "already_used" {
		t.Errorf("a fourth replay: %v", err)
	}
	// Analysis off: not_found before anything.
	if _, err := pool.Exec(ctx, `UPDATE analysis_switch SET position = 'off' WHERE hospital_id = $1`, w.h); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolve(ctx, pool, w.h, w.admin, ref, "k1", false); p0001(err) != "not_found" {
		t.Errorf("analysis off: %v", err)
	}
}

// Only the functions reach the tables; the functions only their roles.
func TestExportTablesGrants(t *testing.T) {
	_, pool := shared(t)
	ctx := context.Background()
	for _, role := range []string{"mhai_gateway", "mhai_auth", "mhai_provision", "mhai_jobs", "mhai_analysis_job", "mhai_worker"} {
		for _, table := range []string{"audit_exports", "audit_export_rows", "legal_references"} {
			requireDenied(t, asRole(ctx, pool, role, `SELECT count(*) FROM `+table), role+" reading "+table)
		}
	}
	for _, table := range []string{"audit_export_rows", "legal_references"} {
		requireDenied(t, asRole(ctx, pool, "mhai_platform", `SELECT count(*) FROM `+table), "the platform reading "+table)
	}
	var got []string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(array_agg(p.proname || ':' || r.role ORDER BY (p.proname || ':' || r.role) COLLATE "C"), '{}') FROM pg_proc p,
		unnest(ARRAY['mhai_gateway', 'mhai_platform', 'mhai_provision', 'mhai_auth', 'mhai_analysis_job', 'mhai_jobs', 'mhai_worker']) r(role)
		WHERE (p.proname LIKE 'mhai_audit_export%' OR p.proname LIKE 'mhai\_legal\_ref%' OR p.proname IN ('mhai_audit_letters', 'mhai_export_exempt', 'mhai_legal_subjects'))
		  AND has_function_privilege(r.role, p.oid, 'EXECUTE')`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := "mhai_audit_export_named:mhai_platform,mhai_audit_export_pids:mhai_platform,mhai_audit_letters:mhai_platform," +
		"mhai_legal_reference_create:mhai_platform,mhai_legal_references_mine:mhai_gateway,mhai_legal_subjects:mhai_gateway"
	if s := strings.Join(got, ","); s != want {
		t.Errorf("EXECUTE grants:\n%s\nwant\n%s", s, want)
	}
	var loose []string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(array_agg(proname::text), '{}') FROM pg_proc
		WHERE (proname LIKE 'mhai_audit_export%' OR proname LIKE 'mhai\_legal\_ref%' OR proname IN ('mhai_audit_letters', 'mhai_export_exempt', 'mhai_legal_subjects'))
		  AND NOT (proconfig @> ARRAY['search_path=pg_catalog, pg_temp'])`).Scan(&loose); err != nil || len(loose) != 0 {
		t.Errorf("functions without a pinned search_path: %v (%v)", loose, err)
	}
	var login, members bool
	if err := pool.QueryRow(ctx, `SELECT rolcanlogin, EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.roleid = r.oid)
		FROM pg_roles r WHERE rolname = 'mhai_export_rules'`).Scan(&login, &members); err != nil || login || members {
		t.Errorf("mhai_export_rules: login %v members %v (%v)", login, members, err)
	}
}

func TestMigration00044DownUp(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.MigrateUpTo(ctx, pg.URL, 43); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	state := func() string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT
			(SELECT string_agg(polname || ':' || COALESCE(pg_get_expr(polqual, polrelid), '') || ':' || COALESCE(pg_get_expr(polwithcheck, polrelid), ''), ';' ORDER BY polname)
			 FROM pg_policy WHERE polrelid = 'audit_log'::regclass)
			|| '|' || (SELECT count(*) FROM pg_class c, unnest(ARRAY['audit_log', 'analysis_views', 'analysis_view_rows',
			   'analysis_pseudonyms', 'analysis_periods', 'analysis_subjects', 'clearances', 'users', 'user_roles', 'user_mfa', 'hospitals']) t(n)
			   WHERE c.relname = t.n AND EXISTS (SELECT 1 FROM pg_roles r WHERE r.rolname = 'mhai_export_rules')
			     AND has_any_column_privilege('mhai_export_rules', c.oid, 'SELECT'))::text`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := state()
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := db.MigrateDownTo(ctx, pg.URL, 43); err != nil {
		t.Fatalf("down to 43: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_class WHERE relname IN ('audit_exports', 'audit_export_rows', 'legal_references',
		   'audit_log_named_idx', 'audit_log_export_idx'))
		+ (SELECT count(*) FROM pg_proc WHERE proname LIKE 'mhai_audit_export%' OR proname IN ('mhai_audit_letters',
		   'mhai_export_exempt', 'mhai_legal_reference_create', 'mhai_legal_reference_audited', 'mhai_legal_reference_rules',
		   'mhai_legal_subjects', 'mhai_legal_references_mine'))`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d objects left after the Down", left)
	}
	if after := state(); after != before {
		t.Errorf("state after Down:\n%s\nwant\n%s", after, before)
	}
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
