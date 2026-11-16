// Package upload answers documents.v1.ctl.presign_upload (documents design
// §4.2): core, having inserted a documents row (uploading) for a browser
// upload, asks for the S3 POST policy that lets the browser post the file
// straight to the object store. The policy is for exactly
// quarantine/<hid>/<document_id>, a content-length range from 1 byte to
// the purpose's limit, the row's declared type and 10 minutes.
//
// The hospital is the verified token's hid (the op is HospitalBound), and
// the row is read under it with row-level security (dbx.WithHospital as
// mhai_documents): another hospital's document is not_found, exactly like
// one that doesn't exist (DS1). Archive documents get their policy through
// documents.v1.<hid>.intake from the emr service (PR 10), never here.
package upload

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/ctl"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/envelope"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/s3"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// The reply's outcomes.
const (
	OutcomeOK          = "ok"
	OutcomeNotFound    = "not_found"
	OutcomeConflict    = "conflict"
	OutcomeUnavailable = "unavailable"
)

// MaxBytes is each browser purpose's size limit (§4.1), the same as the
// documents_limits CHECK.
var MaxBytes = map[string]int64{
	"clinical":  25 << 20,
	"clearance": 10 << 20,
}

// ErrNotFound: no such row in the hospital.
var ErrNotFound = errors.New("upload: no such document")

// Row is what the policy needs of a documents row.
type Row struct{ Purpose, Status, MimeType string }

// Rows reads a documents row under a hospital.
type Rows interface {
	UploadRow(ctx context.Context, hospital, document uuid.UUID) (Row, error)
}

// PGRows reads rows as mhai_documents under the hospital (RLS).
type PGRows struct{ Pool *pgxpool.Pool }

// UploadRow reads the row, or ErrNotFound.
func (p PGRows) UploadRow(ctx context.Context, hospital, document uuid.UUID) (Row, error) {
	var r Row
	err := dbx.WithHospital(ctx, p.Pool, dbx.Documents, hospital, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT purpose, status, mime_type FROM public.documents WHERE id = $1`, document).
			Scan(&r.Purpose, &r.Status, &r.MimeType)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, ErrNotFound
	}
	return r, err
}

// Signer signs POST policies (s3.Client).
type Signer interface {
	PostPolicy(r s3.PolicyRequest, uploadEndpoint string) (s3.Policy, error)
}

// Presign is the handler.
type Presign struct {
	Rows Rows
	// Signer is nil while no object store is configured: unavailable.
	Signer Signer
	// UploadEndpoint is where browsers post ("" for the signer's own).
	UploadEndpoint string
	// Ready reports whether the keyring and the store are ready; the
	// service takes no file it couldn't seal or whose quarantine copy the
	// lifecycle wouldn't remove (§5.4, §6.2).
	Ready func(ctx context.Context) error
	// HospitalKey reports whether the hospital's KEK is there (kek
	// Provider.Has). A provisioned keyring without it: unavailable, so no
	// file is taken that couldn't be sealed; other hospitals are served.
	// Nil is unavailable for everyone.
	HospitalKey func(ctx context.Context, hospital uuid.UUID) error
}

func reply(outcome string) *docspb.PresignUploadReply {
	return &docspb.PresignUploadReply{Outcome: outcome}
}

// Handle serves documents.v1.ctl.presign_upload.
func (p *Presign) Handle(ctx context.Context, c reqtoken.Claims, req proto.Message) (proto.Message, error) {
	hospital, err := ctl.Hospital(c)
	if err != nil {
		return nil, err
	}
	in, ok := req.(*docspb.PresignUploadRequest)
	if !ok {
		return nil, errors.New("upload: not a presign request")
	}
	doc, err := uuid.Parse(in.GetDocumentId())
	if err != nil || doc == uuid.Nil || doc.String() != in.GetDocumentId() {
		return reply(OutcomeNotFound), nil
	}
	if p.Signer == nil || p.Ready == nil || p.Ready(ctx) != nil || p.HospitalKey == nil || p.HospitalKey(ctx, hospital) != nil {
		return reply(OutcomeUnavailable), nil
	}
	row, err := p.Rows.UploadRow(ctx, hospital, doc)
	switch {
	case errors.Is(err, ErrNotFound):
		return reply(OutcomeNotFound), nil
	case err != nil:
		return reply(OutcomeUnavailable), nil
	}
	max, browser := MaxBytes[row.Purpose]
	if !browser {
		// An archive row is the emr service's (S4): to core it doesn't exist.
		return reply(OutcomeNotFound), nil
	}
	if row.Status != "uploading" {
		return reply(OutcomeConflict), nil
	}
	pol, err := p.Signer.PostPolicy(s3.PolicyRequest{Key: envelope.QuarantineKey(hospital, doc), ContentType: row.MimeType,
		MaxBytes: max}, p.UploadEndpoint)
	if err != nil {
		return reply(OutcomeUnavailable), nil
	}
	out := &docspb.PresignUploadReply{Outcome: OutcomeOK, Url: pol.URL, ExpiresAt: pol.ExpiresAt.Unix()}
	for _, f := range pol.Fields {
		out.Fields = append(out.Fields, &docspb.FormField{Name: f.Name, Value: f.Value})
	}
	return out, nil
}
