//go:build integration

package scan_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/envelope"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/kek"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/s3"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/s3/s3test"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/scan"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/storage"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/testpg"
)

// Migration 00048's definers and the scan against Postgres and MinIO.
// Synthetic hospitals, documents and keys only.

type db struct {
	url          string
	super, login *pgxpool.Pool
	hospA, hospB uuid.UUID
	conn         uuid.UUID
}

func newDB(t *testing.T) db {
	t.Helper()
	url, super := testpg.Migrated(t)
	login, err := pgxpool.New(context.Background(), testpg.Login(t, url, super, "documents_app", "mhai_documents_app"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(login.Close)
	d := db{url: url, super: super, login: login, hospA: testpg.Hospital(t, super), hospB: testpg.Hospital(t, super)}
	if err := super.QueryRow(context.Background(), `INSERT INTO emr_connections (hospital_id, type, source_label, base_url)
		VALUES ($1, 'document_archive', 'Synthetic archive', 'https://archive.example.test') RETURNING id`, d.hospA).Scan(&d.conn); err != nil {
		t.Fatal(err)
	}
	return d
}

// doc inserts a browser document in status.
func (d db) doc(t *testing.T, h uuid.UUID, purpose, mime, status string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := d.super.QueryRow(context.Background(), `INSERT INTO documents (hospital_id, purpose, via, uploaded_by, mime_type,
		size_bytes, status) VALUES ($1, $2, 'user', gen_random_uuid(), $3, 100, $4) RETURNING id`, h, purpose, mime, status).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// archive inserts an archive document whose listing gave sum.
func (d db) archive(t *testing.T, sum []byte) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := d.super.QueryRow(context.Background(), `INSERT INTO documents (hospital_id, purpose, via, connection_id,
		archive_file_id, archive_revision, mime_type, size_bytes, status, sha256)
		VALUES ($1, 'archive', 'emr', $2, gen_random_uuid()::text, 'r1', 'application/pdf', 100, 'uploaded', $3) RETURNING id`,
		d.hospA, d.conn, sum).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (d db) status(t *testing.T, id uuid.UUID) (string, *string) {
	t.Helper()
	var s string
	var r *string
	if err := d.super.QueryRow(context.Background(), `SELECT status, reason FROM documents WHERE id = $1`, id).Scan(&s, &r); err != nil {
		t.Fatal(err)
	}
	return s, r
}

func code(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func objects(n int) []scan.Object {
	out := []scan.Object{{Kind: envelope.Original}}
	for i := 1; i <= n; i++ {
		out = append(out, scan.Object{Kind: envelope.Page, N: i})
	}
	for i := range out {
		out[i].Sealed = envelope.Sealed{WrappedDEK: make([]byte, 60), KEKVersion: 1, Size: 10}
	}
	return out
}

func TestScanDefinersClaimAndLease(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	rows := scan.PGRows{Pool: d.login}
	a := d.doc(t, d.hospA, "clinical", "application/pdf", "uploaded")
	lease := uuid.New()
	c, err := rows.Claim(ctx, d.hospA, a, lease)
	if err != nil || c.Outcome != scan.ClaimClaimed || c.Purpose != "clinical" || c.MimeType != "application/pdf" || c.SHA256 != nil {
		t.Fatalf("claim: %+v %v", c, err)
	}
	if s, _ := d.status(t, a); s != "scanning" {
		t.Fatalf("status %s", s)
	}
	// A live lease isn't taken over; another hospital never sees the row
	// (DS1); a row not uploaded isn't claimed (B6: a later post is never
	// read).
	if c, err := rows.Claim(ctx, d.hospA, a, uuid.New()); err != nil || c.Outcome != scan.ClaimNotClaimable {
		t.Fatalf("second claim: %+v %v", c, err)
	}
	b := d.doc(t, d.hospA, "clinical", "application/pdf", "uploaded")
	if c, err := rows.Claim(ctx, d.hospB, b, uuid.New()); err != nil || c.Outcome != scan.ClaimNotClaimable {
		t.Fatalf("another hospital: %+v %v", c, err)
	}
	for _, st := range []string{"uploading", "ready", "cancelled"} {
		id := d.doc(t, d.hospA, "clinical", "application/pdf", st)
		if c, err := rows.Claim(ctx, d.hospA, id, uuid.New()); err != nil || c.Outcome != scan.ClaimNotClaimable {
			t.Fatalf("%s: %+v %v", st, c, err)
		}
	}
	// Renew and release only with the lease.
	if ok, err := rows.Renew(ctx, d.hospA, a, uuid.New()); err != nil || ok {
		t.Fatalf("renew with another lease: %v %v", ok, err)
	}
	if ok, err := rows.Renew(ctx, d.hospA, a, lease); err != nil || !ok {
		t.Fatalf("renew: %v %v", ok, err)
	}
	if ok, err := rows.Release(ctx, d.hospB, a, lease); err != nil || ok {
		t.Fatalf("release from another hospital: %v %v", ok, err)
	}
	if ok, err := rows.Release(ctx, d.hospA, a, lease); err != nil || !ok {
		t.Fatalf("release: %v %v", ok, err)
	}
	// Released: the next scan claims it at once, still scanning.
	lease2 := uuid.New()
	if c, err := rows.Claim(ctx, d.hospA, a, lease2); err != nil || c.Outcome != scan.ClaimClaimed {
		t.Fatalf("claim after release: %+v %v", c, err)
	}
	// The old lease can't finish it.
	if ok, err := rows.Quarantine(ctx, d.hospA, a, lease, "malware", nil); err != nil || ok {
		t.Fatalf("quarantine with a released lease: %v %v", ok, err)
	}
	// §5.3: six hours scanning, lease expired: the claim fails it.
	if _, err := d.super.Exec(ctx, `UPDATE documents SET scan_started_at = now() - interval '6 hours 1 minute',
		scan_lease_until = now() - interval '1 second' WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	if c, err := rows.Claim(ctx, d.hospA, a, uuid.New()); err != nil || c.Outcome != scan.ClaimFailed {
		t.Fatalf("six hours: %+v %v", c, err)
	}
	if s, r := d.status(t, a); s != "failed" || r == nil || *r != "scan_unavailable" {
		t.Fatalf("six hours: %s %v", s, r)
	}
}

func TestScanDefinersFinish(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	rows := scan.PGRows{Pool: d.login}
	sum := sha256.Sum256([]byte("synthetic"))

	// Quarantine: a closed reason only.
	q := d.doc(t, d.hospA, "clinical", "image/png", "uploaded")
	lease := uuid.New()
	if _, err := rows.Claim(ctx, d.hospA, q, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := rows.Quarantine(ctx, d.hospA, q, lease, "scan_unavailable", nil); code(err) != "22023" {
		t.Fatalf("a failed reason as quarantine: %v", err)
	}
	if ok, err := rows.Quarantine(ctx, d.hospA, q, lease, "malware", &sum); err != nil || !ok {
		t.Fatalf("quarantine: %v %v", ok, err)
	}
	var lid *uuid.UUID
	var stored []byte
	if err := d.super.QueryRow(ctx, `SELECT scan_lease_id, sha256 FROM documents WHERE id = $1`, q).Scan(&lid, &stored); err != nil ||
		lid != nil || !bytes.Equal(stored, sum[:]) {
		t.Fatalf("quarantined row: %v %x %v", lid, stored, err)
	}

	// Ready: the original and pages 1..n exactly, under the lease.
	r := d.doc(t, d.hospA, "clinical", "application/pdf", "uploaded")
	lease = uuid.New()
	if _, err := rows.Claim(ctx, d.hospA, r, lease); err != nil {
		t.Fatal(err)
	}
	for name, objs := range map[string][]scan.Object{
		"a page missing":    objects(2)[:2],
		"no original":       objects(2)[1:],
		"a page twice":      append(objects(1), scan.Object{Kind: envelope.Page, N: 1, Sealed: objects(1)[1].Sealed}),
		"page out of range": {objects(1)[0], {Kind: envelope.Page, N: 3, Sealed: objects(1)[1].Sealed}},
	} {
		pages := len(objs) - 1
		if name == "a page missing" {
			pages = 2
		}
		if _, err := rows.Ready(ctx, d.hospA, r, lease, sum, pages, objs); code(err) == "" {
			t.Errorf("%s: accepted", name)
		}
	}
	if ok, err := rows.Ready(ctx, d.hospA, r, uuid.New(), sum, 2, objects(2)); err != nil || ok {
		t.Fatalf("ready with another lease: %v %v", ok, err)
	}
	if ok, err := rows.Ready(ctx, d.hospB, r, lease, sum, 2, objects(2)); err != nil || ok {
		t.Fatalf("ready from another hospital: %v %v", ok, err)
	}
	if ok, err := rows.Ready(ctx, d.hospA, r, lease, sum, 2, objects(2)); err != nil || !ok {
		t.Fatalf("ready: %v %v", ok, err)
	}
	var keys []string
	krows, err := d.super.Query(ctx, `SELECT object_key FROM document_objects WHERE document_id = $1 ORDER BY kind, n`, r)
	if err != nil {
		t.Fatal(err)
	}
	keys, err = pgx.CollectRows(krows, pgx.RowTo[string])
	h, doc := d.hospA.String(), r.String()
	if err != nil || len(keys) != 3 || keys[0] != "originals/"+h+"/"+doc || keys[2] != "pages/"+h+"/"+doc+"/2" {
		t.Fatalf("keys %v %v", keys, err)
	}
	if s, _ := d.status(t, r); s != "ready" {
		t.Fatalf("status %s", s)
	}

	// An archive row keeps the listing's SHA-256: other bytes aren't ready.
	arch := d.archive(t, sum[:])
	lease = uuid.New()
	c, err := rows.Claim(ctx, d.hospA, arch, lease)
	if err != nil || !bytes.Equal(c.SHA256, sum[:]) || c.Purpose != "archive" {
		t.Fatalf("archive claim: %+v %v", c, err)
	}
	other := sha256.Sum256([]byte("other"))
	if ok, err := rows.Ready(ctx, d.hospA, arch, lease, other, 1, objects(1)); err != nil || ok {
		t.Fatalf("archive with other bytes: %v %v", ok, err)
	}
}

// The definers are mhai_documents' only; the sweeper's role sees ids,
// status and times of due rows only, across hospitals.
func TestScanGrantsAndSweep(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	a := d.doc(t, d.hospA, "clinical", "application/pdf", "uploaded")
	b := d.doc(t, d.hospB, "clinical", "image/png", "uploaded")
	fresh := d.doc(t, d.hospA, "clinical", "image/png", "uploaded")
	ready := d.doc(t, d.hospA, "clinical", "image/png", "ready")
	if _, err := d.super.Exec(ctx, `UPDATE documents SET received_at = now() - interval '3 minutes' WHERE id = ANY($1)`,
		[]uuid.UUID{a, b, ready}); err != nil {
		t.Fatal(err)
	}
	rows := scan.PGRows{Pool: d.login}
	due, err := rows.Due(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]uuid.UUID{}
	for _, x := range due {
		got[x.Document] = x.Hospital
	}
	if len(got) != 2 || got[a] != d.hospA || got[b] != d.hospB {
		t.Fatalf("due %v (fresh %v ready %v)", due, fresh, ready)
	}
	// A scanning row with an expired lease is due; a live one isn't.
	if _, err := rows.Claim(ctx, d.hospA, a, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if due, _ := rows.Due(ctx, 50); len(due) != 1 {
		t.Fatalf("live lease due: %v", due)
	}
	if _, err := d.super.Exec(ctx, `UPDATE documents SET scan_lease_until = now() - interval '1 second' WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	if due, _ := rows.Due(ctx, 50); len(due) != 2 {
		t.Fatalf("expired lease not due: %v", due)
	}
	as := func(role, q string, args ...any) error {
		tx, err := d.super.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, d.hospA.String()); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, q, args...)
		return err
	}
	for _, q := range []string{`SELECT reason FROM documents`, `SELECT sha256 FROM documents`, `SELECT source_name FROM documents`,
		`UPDATE documents SET status = 'ready'`, `SELECT * FROM document_objects`,
		`SELECT * FROM documents_scan_claim(gen_random_uuid(), gen_random_uuid())`} {
		if err := as("mhai_documents_sweep", q); code(err) != "42501" {
			t.Errorf("sweep %q: %v", q, err)
		}
	}
	for _, role := range []string{"mhai_gateway", "mhai_worker", "mhai_emr", "mhai_documents_app"} {
		if err := as(role, `SELECT * FROM documents_scan_claim(gen_random_uuid(), gen_random_uuid())`); code(err) != "42501" {
			t.Errorf("%s may claim: %v", role, err)
		}
	}
	// mhai_documents still writes nothing directly.
	if err := as("mhai_documents", `UPDATE documents SET status = 'ready'`); code(err) != "42501" {
		t.Errorf("mhai_documents updates directly: %v", err)
	}
	// The login is still fit (it may hold the sweep role).
	var n int
	if err := d.login.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname = 'mhai_documents_sweep'
		AND pg_has_role(session_user, oid, 'MEMBER')`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("sweep membership %d %v", n, err)
	}
}

// 00048 down and up again.
func TestScanMigrationDown(t *testing.T) {
	url, super := testpg.Migrated(t)
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := stdlib.OpenDB(*cfg)
	defer func(x *sql.DB) { _ = x.Close() }(sqlDB)
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS(testpg.MigrationsDir()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 46); err != nil {
		t.Fatalf("down: %v", err)
	}
	var fn, col, member bool
	if err := super.QueryRow(ctx, `SELECT to_regprocedure('documents_scan_claim(uuid,uuid)') IS NOT NULL,
		EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'documents' AND column_name = 'scan_lease_id'),
		pg_has_role('mhai_documents_app', 'mhai_documents_sweep', 'MEMBER')`).Scan(&fn, &col, &member); err != nil || fn || col || member {
		t.Fatalf("after down: fn %v col %v member %v %v", fn, col, member, err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// fakes for the parts that aren't Postgres or the store.
type clean struct{ scanned int }

func (c *clean) Ready(context.Context) error { return nil }
func (c *clean) Scan(context.Context, []byte) (bool, error) {
	c.scanned++
	return false, nil
}

type onePage struct{}

func (onePage) Ready(context.Context) error { return nil }
func (onePage) Render(context.Context, []byte, string, int) ([][]byte, string, error) {
	return [][]byte{[]byte("synthetic page")}, "", nil
}

// The whole scan against Postgres and MinIO: the quarantine object is read,
// sealed under the hospital's key, stored at id-only keys whose wrapped
// keys are in document_objects, and removed from quarantine; the original
// opens again from what the row holds. A second post to the same key
// afterwards (B6, DS5) is never read: the row is no longer uploaded.
func TestScanEndToEnd(t *testing.T) {
	d := newDB(t)
	st := s3test.Start(t, true)
	ctx := context.Background()
	ring := filepath.Join(t.TempDir(), "keyring.json")
	if err := kek.InitFile(ring); err != nil {
		t.Fatal(err)
	}
	keys := kek.NewFile(ring, kek.Create)
	store := &storage.Store{Objects: st.Client, Keys: keys}
	virus := &clean{}
	s := &scan.Scanner{Rows: scan.PGRows{Pool: d.login}, Quarantine: st.Client, Store: store, Virus: virus, Renderer: onePage{},
		HospitalKey:  func(context.Context, uuid.UUID) error { return nil },
		StorageReady: (&storage.Readiness{Objects: st.Client}).Check}
	id := d.doc(t, d.hospA, "clinical", "application/pdf", "uploaded")
	file := []byte("%PDF-1.7\nsynthetic document\n")
	qkey := envelope.QuarantineKey(d.hospA, id)
	if err := st.Client.Put(ctx, qkey, file, "application/pdf"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Scan(ctx, d.hospA, id)
	if err != nil || res.Outcome != scan.OutcomeReady || res.Pages != 1 {
		t.Fatalf("scan: %+v %v", res, err)
	}
	if _, err := st.Client.Head(ctx, qkey); !errors.Is(err, s3.ErrNotFound) {
		t.Fatalf("quarantine object left: %v", err)
	}
	var wrapped []byte
	var version int
	if err := d.super.QueryRow(ctx, `SELECT wrapped_dek, kek_version FROM document_objects WHERE document_id = $1 AND kind = 'original'`,
		id).Scan(&wrapped, &version); err != nil {
		t.Fatal(err)
	}
	rc, err := store.Open(ctx, envelope.Ref{Hospital: d.hospA, Document: id, Kind: envelope.Original}, wrapped, version)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, file) {
		t.Fatal("the stored original differs")
	}
	var pages int
	var sum []byte
	if err := d.super.QueryRow(ctx, `SELECT pages, sha256 FROM documents WHERE id = $1`, id).Scan(&pages, &sum); err != nil ||
		pages != 1 || !bytes.Equal(sum, func() []byte { s := sha256.Sum256(file); return s[:] }()) {
		t.Fatalf("row: %d %x %v", pages, sum, err)
	}
	// B6: a second post after the scan; a redelivered message scans nothing.
	if err := st.Client.Put(ctx, qkey, []byte("%PDF-1.7\nsecond synthetic post"), "application/pdf"); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Scan(ctx, d.hospA, id); err != nil || res.Outcome != scan.OutcomeSkipped || virus.scanned != 1 {
		t.Fatalf("second post: %+v %v scanned %d", res, err, virus.scanned)
	}
	// A refusal stores nothing outside quarantine and removes the object.
	bad := d.doc(t, d.hospA, "clinical", "application/pdf", "uploaded")
	bkey := envelope.QuarantineKey(d.hospA, bad)
	if err := st.Client.Put(ctx, bkey, []byte("PK\x03\x04 synthetic zip"), "application/pdf"); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Scan(ctx, d.hospA, bad); err != nil || res.Reason != "unsupported_type" {
		t.Fatalf("zip: %+v %v", res, err)
	}
	var n int
	if err := d.super.QueryRow(ctx, `SELECT count(*) FROM document_objects WHERE document_id = $1`, bad).Scan(&n); err != nil || n != 0 {
		t.Fatalf("objects for a refusal: %d %v", n, err)
	}
	if _, err := st.Client.Head(ctx, bkey); !errors.Is(err, s3.ErrNotFound) {
		t.Fatalf("refused quarantine object left: %v", err)
	}
}
