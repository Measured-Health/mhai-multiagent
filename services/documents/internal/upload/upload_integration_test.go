//go:build integration

package upload_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/s3/s3test"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/testpg"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/upload"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// DS1 on the real schema, as the documents login: hospital B's token can't
// get a policy for hospital A's document (not_found, exactly as a missing
// one), and A's token gets one for the row's quarantine key, signed by a
// real store's credential.
func TestPresignOnTheRealSchema(t *testing.T) {
	url, super := testpg.Migrated(t)
	ctx := context.Background()
	docs, err := pgxpool.New(ctx, testpg.Login(t, url, super, "documents_app", "mhai_documents_app"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(docs.Close)
	hospA, hospB := testpg.Hospital(t, super), testpg.Hospital(t, super)
	var doc uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO documents (hospital_id, purpose, via, uploaded_by, mime_type, size_bytes)
		VALUES ($1, 'clinical', 'user', $2, 'application/pdf', 1000) RETURNING id`, hospA, uuid.New()).Scan(&doc); err != nil {
		t.Fatal(err)
	}
	rows := upload.PGRows{Pool: docs}
	if r, err := rows.UploadRow(ctx, hospA, doc); err != nil || r != (upload.Row{Purpose: "clinical", Status: "uploading",
		MimeType: "application/pdf"}) {
		t.Fatalf("A's row: %+v %v", r, err)
	}
	if _, err := rows.UploadRow(ctx, hospB, doc); !errors.Is(err, upload.ErrNotFound) {
		t.Fatalf("A's row under B: %v", err)
	}
	st := s3test.Start(t, true)
	p := &upload.Presign{Rows: rows, Signer: st.Client, Ready: func(context.Context) error { return nil }}
	ask := func(hid uuid.UUID) *docspb.PresignUploadReply {
		out, err := p.Handle(ctx, reqtoken.Claims{Sub: reqtoken.SvcCore, Hid: hid.String()},
			&docspb.PresignUploadRequest{DocumentId: doc.String()})
		if err != nil {
			t.Fatal(err)
		}
		return out.(*docspb.PresignUploadReply)
	}
	if r := ask(hospB); r.GetOutcome() != upload.OutcomeNotFound || len(r.GetFields()) != 0 {
		t.Fatalf("B asks for A's document: %v", r)
	}
	r := ask(hospA)
	if r.GetOutcome() != upload.OutcomeOK || r.GetFields()[0].GetValue() != "quarantine/"+hospA.String()+"/"+doc.String() {
		t.Fatalf("A: %v", r)
	}
	// Once the row has moved on, no new policy (DS5: a row not uploading).
	if _, err := super.Exec(ctx, `UPDATE documents SET status = 'uploaded' WHERE id = $1`, doc); err != nil {
		t.Fatal(err)
	}
	if r := ask(hospA); r.GetOutcome() != upload.OutcomeConflict {
		t.Fatalf("after upload: %v", r)
	}
}
