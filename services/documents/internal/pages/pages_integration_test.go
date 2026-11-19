//go:build integration

package pages_test

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/envelope"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/kek"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/pages"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/s3/s3test"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/storage"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/testpg"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// page_image against Postgres (as mhai_documents, RLS) and MinIO, with a
// page sealed the way the scan seals it. Synthetic hospitals and pages.
func TestPageImageEndToEnd(t *testing.T) {
	url, super := testpg.Migrated(t)
	login, err := pgxpool.New(context.Background(), testpg.Login(t, url, super, "documents_app", "mhai_documents_app"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(login.Close)
	hospA, hospB := testpg.Hospital(t, super), testpg.Hospital(t, super)
	st := s3test.Start(t, true)
	ring := filepath.Join(t.TempDir(), "keyring.json")
	if err := kek.InitFile(ring); err != nil {
		t.Fatal(err)
	}
	keys := kek.NewFile(ring, kek.Create)
	store := &storage.Store{Objects: st.Client, Keys: keys}
	ctx := context.Background()

	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 800, 1000))); err != nil {
		t.Fatal(err)
	}
	var doc uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO documents (hospital_id, purpose, via, uploaded_by, mime_type, size_bytes, status, pages)
		VALUES ($1, 'clearance', 'user', gen_random_uuid(), 'application/pdf', 100, 'ready', 1) RETURNING id`, hospA).Scan(&doc); err != nil {
		t.Fatal(err)
	}
	ref := envelope.Ref{Hospital: hospA, Document: doc, Kind: envelope.Page, N: 1}
	sealed, err := store.Put(ctx, ref, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `INSERT INTO document_objects (hospital_id, document_id, kind, n, object_key, wrapped_dek, kek_version, sha256, size_bytes)
		VALUES ($1, $2, 'page', 1, $3, $4, $5, $6, $7)`, hospA, doc, ref.Key(), sealed.WrappedDEK, sealed.KEKVersion,
		sealed.SHA256[:], sealed.Size); err != nil {
		t.Fatal(err)
	}
	h := &pages.Handler{Rows: pages.PGRows{Pool: login}, Store: store, Ready: func(context.Context) error { return nil }}
	ask := func(hid uuid.UUID, n uint32) *docspb.PageImageReply {
		t.Helper()
		rep, err := h.Handle(ctx, reqtoken.Claims{Hid: hid.String()}, &docspb.PageImageRequest{DocumentId: doc.String(), N: n,
			ViewerName: "Synthetic Viewer", At: time.Now().Unix()})
		if err != nil {
			t.Fatal(err)
		}
		return rep.(*docspb.PageImageReply)
	}
	if r := ask(hospA, 1); r.GetOutcome() != pages.OutcomeOK || len(r.GetJpeg()) == 0 || r.GetWidth() != 800 {
		t.Fatalf("own page: %s %d bytes %d wide", r.GetOutcome(), len(r.GetJpeg()), r.GetWidth())
	}
	// DS1: another hospital's token reads nothing (RLS), the same answer as
	// a page that doesn't exist.
	if r := ask(hospB, 1); r.GetOutcome() != pages.OutcomeNotFound || len(r.GetJpeg()) != 0 {
		t.Fatalf("another hospital: %s", r.GetOutcome())
	}
	if r := ask(hospA, 2); r.GetOutcome() != pages.OutcomeNotFound {
		t.Fatalf("no such page: %s", r.GetOutcome())
	}
	// A cancelled document's pages are no longer shown.
	if _, err := super.Exec(ctx, `UPDATE documents SET status = 'cancelled' WHERE id = $1`, doc); err != nil {
		t.Fatal(err)
	}
	if r := ask(hospA, 1); r.GetOutcome() != pages.OutcomeNotFound {
		t.Fatalf("cancelled: %s", r.GetOutcome())
	}
}
