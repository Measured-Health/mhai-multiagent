//go:build integration

package internal

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/testpg"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
)

// Migration 00046 (documents design §7.1, §7.2; §13 PR 3). Synthetic
// hospitals, documents and keys only.

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

const (
	denied = "42501"
	check  = "23514"
	fk     = "23503"
)

type world struct {
	super     *pgxpool.Pool
	docs      *pgxpool.Pool
	hospA     uuid.UUID
	hospB     uuid.UUID
	user      uuid.UUID
	conn      uuid.UUID
	clinicalA uuid.UUID
}

func newWorld(t *testing.T) world {
	t.Helper()
	url, super := testpg.Migrated(t)
	docsURL := testpg.Login(t, url, super, "documents_app", "mhai_documents_app")
	docs, err := pgxpool.New(context.Background(), docsURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(docs.Close)
	w := world{super: super, docs: docs, hospA: testpg.Hospital(t, super), hospB: testpg.Hospital(t, super), user: uuid.New()}
	ctx := context.Background()
	if err := super.QueryRow(ctx, `INSERT INTO emr_connections (hospital_id, type, source_label, base_url)
		VALUES ($1, 'document_archive', 'Synthetic archive', 'https://archive.example.test') RETURNING id`, w.hospA).Scan(&w.conn); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	w.clinicalA = w.insert(t, w.hospA, "clinical", "application/pdf", 1000)
	return w
}

// insert adds a browser document (as the owner: core's definer comes in PR 5).
func (w world) insert(t *testing.T, h uuid.UUID, purpose, mime string, size int64) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := w.super.QueryRow(context.Background(), `INSERT INTO documents (hospital_id, purpose, via, uploaded_by, mime_type, size_bytes)
		VALUES ($1, $2, 'user', $3, $4, $5) RETURNING id`, h, purpose, w.user, mime, size).Scan(&id); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	return id
}

func TestDocumentsRoleSeesItsHospitalOnly(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.insert(t, w.hospB, "clearance", "image/png", 10)
	count := func(h uuid.UUID, table string) (n int) {
		if err := dbx.WithHospital(ctx, w.docs, dbx.Documents, h, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
		}); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	if count(w.hospA, "documents") != 1 || count(w.hospB, "documents") != 1 || count(uuid.New(), "documents") != 0 {
		t.Fatal("documents: RLS doesn't separate the hospitals")
	}
	// A's id under B's hospital: nothing (DS1).
	var n int
	if err := dbx.WithHospital(ctx, w.docs, dbx.Documents, w.hospB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM documents WHERE id = $1`, w.clinicalA).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("A's document under B: %d %v", n, err)
	}
	// An object row of each hospital, seen only by its own.
	for _, h := range []uuid.UUID{w.hospA, w.hospB} {
		var d uuid.UUID
		if err := w.super.QueryRow(ctx, `SELECT id FROM documents WHERE hospital_id = $1`, h).Scan(&d); err != nil {
			t.Fatal(err)
		}
		if _, err := w.super.Exec(ctx, `INSERT INTO document_objects (hospital_id, document_id, kind, n, object_key, wrapped_dek,
			kek_version, sha256, size_bytes) VALUES ($1, $2, 'original', 0, $3, $4, 1, $5, 10)`,
			h, d, "originals/"+h.String()+"/"+d.String(), make([]byte, 60), make([]byte, 32)); err != nil {
			t.Fatalf("seed object: %v", err)
		}
	}
	if count(w.hospA, "document_objects") != 1 || count(w.hospB, "document_objects") != 1 {
		t.Fatal("document_objects: RLS doesn't separate the hospitals")
	}
	// Without a hospital set, the documents service sees nothing.
	tx, err := w.docs.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_documents`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM documents`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("no hospital: %d %v", n, err)
	}
}

// §7.2: mhai_documents reads; it never reads source_name, the uploader or
// the patient hint, and never writes (its writes are PR 4's definers). The
// login alone holds nothing until SET ROLE. No other role holds anything
// on these tables yet.
func TestDocumentsGrantsAreNarrow(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	as := func(role string, h uuid.UUID, q string, args ...any) error {
		tx, err := w.super.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, h.String()); err != nil {
			t.Fatal(err)
		}
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		rows.Close()
		return rows.Err()
	}
	if err := as("mhai_documents", w.hospA, `SELECT id, purpose, status, reason, via, mime_type, size_bytes, sha256, pages,
		received_at, scanned_at, version FROM documents`); err != nil {
		t.Fatalf("the granted columns: %v", err)
	}
	for _, q := range []string{
		`SELECT source_name FROM documents`,
		`SELECT uploaded_by FROM documents`,
		`SELECT patient_hint_id FROM documents`,
		`SELECT * FROM documents`,
		`UPDATE documents SET status = 'uploaded'`,
		`DELETE FROM documents`,
		`DELETE FROM document_objects`,
		`INSERT INTO document_objects (hospital_id, document_id, kind, n, object_key, wrapped_dek, kek_version, sha256, size_bytes)
		 VALUES (mhai_current_hospital(), gen_random_uuid(), 'original', 0, 'x', '\x00', 1, '\x00', 1)`,
	} {
		if err := as("mhai_documents", w.hospA, q); pgCode(err) != denied {
			t.Errorf("mhai_documents %q: %v, want denied", q, err)
		}
	}
	if err := as("mhai_documents", w.hospA, `INSERT INTO documents (hospital_id, purpose, via, uploaded_by, mime_type, size_bytes)
		VALUES (mhai_current_hospital(), 'clinical', 'user', gen_random_uuid(), 'application/pdf', 1)`); pgCode(err) != denied {
		t.Errorf("mhai_documents insert: %v", err)
	}
	for _, role := range []string{"mhai_gateway", "mhai_worker", "mhai_emr", "mhai_platform", "mhai_emr_core", "mhai_jobs",
		"mhai_documents_app"} {
		for _, table := range []string{"documents", "document_objects"} {
			if err := as(role, w.hospA, `SELECT hospital_id FROM `+table); pgCode(err) != denied {
				t.Errorf("%s reads %s: %v", role, table, err)
			}
		}
	}
	// The login holds nothing by itself (NOINHERIT, WITH INHERIT FALSE).
	if _, err := w.docs.Exec(ctx, `SELECT id FROM documents`); pgCode(err) != denied {
		t.Fatalf("the login reads documents without SET ROLE: %v", err)
	}
}

// §4.1, §4.4, §6.1: what the tables refuse, whoever writes.
func TestDocumentsConstraints(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	exec := func(q string, args ...any) error {
		_, err := w.super.Exec(ctx, q, args...)
		return err
	}
	ins := `INSERT INTO documents (hospital_id, purpose, via, uploaded_by, connection_id, archive_file_id, archive_revision,
		source_name, mime_type, size_bytes, status, reason) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`
	u, a := w.user, w.hospA
	var none *uuid.UUID
	type row struct {
		purpose, via   string
		by, conn       any
		file, rev, src any
		mime           string
		size           int64
		status         string
		reason         any
	}
	good := []row{
		{"clinical", "user", u, none, nil, nil, nil, "image/tiff", 26214400, "uploading", nil},
		{"clearance", "user", u, none, nil, nil, nil, "image/jpeg", 10485760, "uploading", nil},
		{"archive", "emr", none, w.conn, "file-1", "r1", "SYNTHETIC-NAME.pdf", "application/pdf", 5, "uploading", nil},
		{"clinical", "user", u, none, nil, nil, nil, "image/png", 5, "quarantined", "malware"},
		{"clinical", "user", u, none, nil, nil, nil, "image/png", 5, "failed", "scan_unavailable"},
		{"archive", "emr", none, w.conn, "file-2", "r1", nil, "application/pdf", 5, "cancelled", "source_removed"},
		{"clinical", "user", u, none, nil, nil, nil, "image/png", 5, "cancelled", nil},
	}
	for i, r := range good {
		if err := exec(ins, a, r.purpose, r.via, r.by, r.conn, r.file, r.rev, r.src, r.mime, r.size, r.status, r.reason); err != nil {
			t.Fatalf("good row %d: %v", i, err)
		}
	}
	bad := map[string]row{
		"clinical over 25 MB":         {"clinical", "user", u, none, nil, nil, nil, "image/png", 26214401, "uploading", nil},
		"clearance over 10 MB":        {"clearance", "user", u, none, nil, nil, nil, "image/png", 10485761, "uploading", nil},
		"clearance as tiff":           {"clearance", "user", u, none, nil, nil, nil, "image/tiff", 5, "uploading", nil},
		"html":                        {"clinical", "user", u, none, nil, nil, nil, "text/html", 5, "uploading", nil},
		"empty":                       {"clinical", "user", u, none, nil, nil, nil, "image/png", 0, "uploading", nil},
		"archive by a user":           {"archive", "user", u, none, nil, nil, nil, "image/png", 5, "uploading", nil},
		"clinical by emr":             {"clinical", "emr", none, w.conn, "f", "r", nil, "image/png", 5, "uploading", nil},
		"archive without connection":  {"archive", "emr", none, none, "f", "r", nil, "image/png", 5, "uploading", nil},
		"archive without file id":     {"archive", "emr", none, w.conn, nil, "r", nil, "image/png", 5, "uploading", nil},
		"user upload with a name":     {"clinical", "user", u, none, nil, nil, "SYNTHETIC-NAME.pdf", "image/png", 5, "uploading", nil},
		"user upload without a user":  {"clinical", "user", none, none, nil, nil, nil, "image/png", 5, "uploading", nil},
		"quarantined without reason":  {"clinical", "user", u, none, nil, nil, nil, "image/png", 5, "quarantined", nil},
		"quarantined, failed reason":  {"clinical", "user", u, none, nil, nil, nil, "image/png", 5, "quarantined", "scan_unavailable"},
		"failed, quarantine reason":   {"clinical", "user", u, none, nil, nil, nil, "image/png", 5, "failed", "malware"},
		"ready with a reason":         {"clinical", "user", u, none, nil, nil, nil, "image/png", 5, "ready", "malware"},
		"unknown status":              {"clinical", "user", u, none, nil, nil, nil, "image/png", 5, "deleted", nil},
		"unknown purpose":             {"letter", "user", u, none, nil, nil, nil, "image/png", 5, "uploading", nil},
		"cancelled, other reason":     {"clinical", "user", u, none, nil, nil, nil, "image/png", 5, "cancelled", "malware"},
		"another hospital's conn":     {"archive", "emr", none, uuid.New(), "f", "r", nil, "image/png", 5, "uploading", nil},
		"archive name over 512 chars": {"archive", "emr", none, w.conn, "f9", "r", strings.Repeat("n", 513), "image/png", 5, "uploading", nil},
	}
	for name, r := range bad {
		err := exec(ins, a, r.purpose, r.via, r.by, r.conn, r.file, r.rev, r.src, r.mime, r.size, r.status, r.reason)
		if c := pgCode(err); c != check && c != fk && c != "22P02" {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
	// The same archive file and revision twice: refused (§4.3).
	if err := exec(ins, a, "archive", "emr", none, w.conn, "file-1", "r1", nil, "application/pdf", 5, "uploading", nil); pgCode(err) != "23505" {
		t.Fatalf("duplicate archive file: %v", err)
	}
	// B's document can't use A's connection.
	if err := exec(ins, w.hospB, "archive", "emr", none, w.conn, "file-9", "r1", nil, "application/pdf", 5, "uploading", nil); pgCode(err) != fk {
		t.Fatalf("another hospital's connection: %v", err)
	}
	// A clearance letter names no patient and has at most 20 pages.
	if err := exec(`UPDATE documents SET pages = 21 WHERE purpose = 'clearance'`); pgCode(err) != check {
		t.Fatalf("clearance with 21 pages: %v", err)
	}

	objIns := `INSERT INTO document_objects (hospital_id, document_id, kind, n, object_key, wrapped_dek, kek_version, sha256, size_bytes)
		VALUES ($1, $2, $3, $4, $5, $6, 1, $7, 10)`
	d, h := w.clinicalA.String(), w.hospA.String()
	dek, sum := make([]byte, 60), make([]byte, 32)
	for name, tc := range map[string]struct {
		hosp     uuid.UUID
		kind     string
		n        int
		key      string
		dek, sum []byte
	}{
		"key with a name":        {w.hospA, "original", 0, "originals/" + h + "/report.pdf", dek, sum},
		"key in hospital B":      {w.hospA, "original", 0, "originals/" + w.hospB.String() + "/" + d, dek, sum},
		"page key for original":  {w.hospA, "original", 0, "pages/" + h + "/" + d + "/0", dek, sum},
		"original numbered":      {w.hospA, "original", 1, "originals/" + h + "/" + d, dek, sum},
		"page 0":                 {w.hospA, "page", 0, "pages/" + h + "/" + d + "/0", dek, sum},
		"page 51":                {w.hospA, "page", 51, "pages/" + h + "/" + d + "/51", dek, sum},
		"short wrapped key":      {w.hospA, "original", 0, "originals/" + h + "/" + d, make([]byte, 39), sum},
		"short sha":              {w.hospA, "original", 0, "originals/" + h + "/" + d, dek, make([]byte, 31)},
		"quarantine kind":        {w.hospA, "quarantine", 0, "quarantine/" + h + "/" + d, dek, sum},
		"object of B on A's doc": {w.hospB, "original", 0, "originals/" + w.hospB.String() + "/" + d, dek, sum},
		"object for missing doc": {w.hospA, "original", 0, "originals/" + h + "/" + uuid.NewString(), dek, sum},
	} {
		doc := w.clinicalA
		if name == "object for missing doc" {
			doc, _ = uuid.Parse(strings.TrimPrefix(tc.key, "originals/"+h+"/"))
		}
		err := exec(objIns, tc.hosp, doc, tc.kind, tc.n, tc.key, tc.dek, tc.sum)
		if c := pgCode(err); c != check && c != fk {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
	if err := exec(objIns, w.hospA, w.clinicalA, "page", 3, "pages/"+h+"/"+d+"/3", dek, sum); err != nil {
		t.Fatalf("a good page: %v", err)
	}
	if err := exec(objIns, w.hospA, w.clinicalA, "original", 0, "originals/"+h+"/"+d, dek, sum); err != nil {
		t.Fatalf("a good original: %v", err)
	}
	// Deleting the document deletes its wrapped keys (the crypto-shred, §6.3).
	if err := exec(`DELETE FROM documents WHERE id = $1`, w.clinicalA); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := w.super.QueryRow(ctx, `SELECT count(*) FROM document_objects WHERE document_id = $1`, w.clinicalA).Scan(&n); err != nil || n != 0 {
		t.Fatalf("wrapped keys left: %d %v", n, err)
	}
}

// Every table: owner mhai_migrate, RLS enabled and forced; the new roles
// can't log in or bypass RLS; down takes the tables and grants back, and
// up again works.
func TestMigrationShapeAndDown(t *testing.T) {
	url, super := testpg.Migrated(t)
	ctx := context.Background()
	for _, table := range []string{"documents", "document_objects"} {
		var owner string
		var rls, force bool
		if err := super.QueryRow(ctx, `SELECT pg_get_userbyid(relowner), relrowsecurity, relforcerowsecurity FROM pg_class
			WHERE oid = $1::regclass`, "public."+table).Scan(&owner, &rls, &force); err != nil {
			t.Fatal(err)
		}
		if owner != "mhai_migrate" || !rls || !force {
			t.Errorf("%s: owner %s rls %v force %v", table, owner, rls, force)
		}
	}
	for _, role := range []string{"mhai_documents", "mhai_documents_app"} {
		var login, super_, bypass bool
		if err := super.QueryRow(ctx, `SELECT rolcanlogin, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = $1`, role).
			Scan(&login, &super_, &bypass); err != nil || login || super_ || bypass {
			t.Errorf("%s: login %v super %v bypass %v %v", role, login, super_, bypass, err)
		}
	}
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDB(*cfg)
	defer func(d *sql.DB) { _ = d.Close() }(db)
	p, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS(testpg.MigrationsDir()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 45); err != nil {
		t.Fatalf("down: %v", err)
	}
	var exists bool
	if err := super.QueryRow(ctx, `SELECT to_regclass('public.documents') IS NOT NULL OR to_regclass('public.document_objects') IS NOT NULL`).
		Scan(&exists); err != nil || exists {
		t.Fatalf("tables after down: %v %v", exists, err)
	}
	var member bool
	if err := super.QueryRow(ctx, `SELECT pg_has_role('mhai_documents_app', 'mhai_documents', 'MEMBER')`).Scan(&member); err != nil || member {
		t.Fatalf("membership after down: %v %v", member, err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := super.QueryRow(ctx, `SELECT to_regclass('public.documents') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Fatalf("tables after up: %v %v", exists, err)
	}
}
