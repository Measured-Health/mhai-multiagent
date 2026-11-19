// Package pages answers documents.v1.ctl.page_image (documents design
// §10.1; §13 PR 5): one stored page of a document of the token's hospital,
// opened from its sealed object with the wrapped key the row holds,
// checked against the SHA-256 the scan recorded, scaled to at most
// MaxWidth wide, stamped across with the viewer's display name and the
// time (D11: a deterrent to screenshots leaving the hospital), and encoded
// as a JPEG of at most docops.MaxPageJPEG, so the reply fits NATS's 1 MB
// payload.
//
// Core asks only after its own checks (who may see the page) and writes
// the view's audit entry before it sends the bytes on; this package checks
// only that the document is the hospital's (the op is HospitalBound, the
// row read under that hospital with row-level security: another
// hospital's document is not_found, DS1) and that its pages may be shown.
// The viewer's name is drawn onto the pixels and never logged.
package pages

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/ctl"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/envelope"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docops"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// The reply's outcomes.
const (
	OutcomeOK          = "ok"
	OutcomeNotFound    = "not_found"
	OutcomeUnavailable = "unavailable"
)

const (
	// MaxWidth is the widest page served (design §10.1).
	MaxWidth = 1600
	// maxStoredSide bounds a stored page's side: the renderer's (sandbox
	// MaxSide), checked again before a page is decoded.
	maxStoredSide = 4000
	// maxStoredBytes bounds a stored page's PNG.
	maxStoredBytes = 64 << 20
	// MaxViewerName bounds the name stamped, in characters.
	MaxViewerName = 120
)

// shown are the statuses whose pages may be served: scanned clean and not
// ended by a cancel, a rejection or a purge.
var shown = map[string]bool{"ready": true, "queued": true, "extracting": true, "in_review": true,
	"finalising": true, "completed": true}

// ErrNotFound: no such page in the hospital.
var ErrNotFound = errors.New("pages: no such page")

// Row is what serving a page needs of its document and object rows.
type Row struct {
	Status     string
	Wrapped    []byte
	KEKVersion int
	SHA256     []byte
}

// Rows reads a page's rows under a hospital.
type Rows interface {
	PageRow(ctx context.Context, hospital, document uuid.UUID, n int) (Row, error)
}

// PGRows reads as mhai_documents under the hospital (RLS).
type PGRows struct{ Pool *pgxpool.Pool }

// PageRow reads the document's status and the page's object row, or
// ErrNotFound.
func (p PGRows) PageRow(ctx context.Context, hospital, document uuid.UUID, n int) (Row, error) {
	var r Row
	err := dbx.WithHospital(ctx, p.Pool, dbx.Documents, hospital, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT d.status, o.wrapped_dek, o.kek_version, o.sha256
			FROM public.documents d JOIN public.document_objects o
			  ON o.hospital_id = d.hospital_id AND o.document_id = d.id AND o.kind = 'page' AND o.n = $2
			WHERE d.id = $1`, document, n).Scan(&r.Status, &r.Wrapped, &r.KEKVersion, &r.SHA256)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, ErrNotFound
	}
	return r, err
}

// Opener opens a sealed object (storage.Store).
type Opener interface {
	Open(ctx context.Context, ref envelope.Ref, wrapped []byte, kekVersion int) (io.ReadCloser, error)
}

// Handler serves page_image.
type Handler struct {
	Rows Rows
	// Store is nil while no object store or keyring is configured:
	// unavailable.
	Store Opener
	// Ready reports whether the keyring and the store are ready.
	Ready func(ctx context.Context) error
}

func reply(outcome string) *docspb.PageImageReply { return &docspb.PageImageReply{Outcome: outcome} }

// validName is a display name to stamp: 1 to MaxViewerName characters of
// valid UTF-8 with no control character.
func validName(s string) bool {
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > MaxViewerName {
		return false
	}
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) {
			return false
		}
	}
	return true
}

// Handle serves documents.v1.ctl.page_image. A request that isn't well
// formed is not_found (it names nothing that exists); an error opening the
// page is unavailable, with nothing of it in the reply.
func (h *Handler) Handle(ctx context.Context, c reqtoken.Claims, req proto.Message) (proto.Message, error) {
	hospital, err := ctl.Hospital(c)
	if err != nil {
		return nil, err
	}
	in, ok := req.(*docspb.PageImageRequest)
	if !ok {
		return nil, errors.New("pages: not a page request")
	}
	doc, err := uuid.Parse(in.GetDocumentId())
	n := int(in.GetN())
	if err != nil || doc == uuid.Nil || doc.String() != in.GetDocumentId() || n < 1 || n > envelope.MaxPages ||
		!validName(in.GetViewerName()) || in.GetAt() <= 0 {
		return reply(OutcomeNotFound), nil
	}
	if h.Store == nil || h.Ready == nil || h.Ready(ctx) != nil {
		return reply(OutcomeUnavailable), nil
	}
	row, err := h.Rows.PageRow(ctx, hospital, doc, n)
	switch {
	case errors.Is(err, ErrNotFound):
		return reply(OutcomeNotFound), nil
	case err != nil:
		return reply(OutcomeUnavailable), nil
	case !shown[row.Status]:
		return reply(OutcomeNotFound), nil
	}
	raw, err := h.open(ctx, envelope.Ref{Hospital: hospital, Document: doc, Kind: envelope.Page, N: n}, row)
	if err != nil {
		return reply(OutcomeUnavailable), nil
	}
	out, w, ht, err := Stamp(raw, in.GetViewerName(), time.Unix(in.GetAt(), 0))
	if err != nil {
		return reply(OutcomeUnavailable), nil
	}
	return &docspb.PageImageReply{Outcome: OutcomeOK, Jpeg: out, Width: uint32(w), Height: uint32(ht)}, nil
}

// open reads the page's plaintext and checks it is what the scan stored.
func (h *Handler) open(ctx context.Context, ref envelope.Ref, row Row) ([]byte, error) {
	rc, err := h.Store.Open(ctx, ref, row.Wrapped, row.KEKVersion)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, maxStoredBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxStoredBytes {
		return nil, errStored
	}
	sum := sha256.Sum256(raw)
	if len(row.SHA256) != len(sum) || subtle.ConstantTimeCompare(sum[:], row.SHA256) != 1 {
		return nil, errStored
	}
	return raw, nil
}

var errStored = errors.New("pages: the stored page is not what the scan stored")

// Stamp decodes a stored page (a PNG the service encoded itself, PR 4),
// scales it to at most MaxWidth wide, stamps it with name and at, and
// encodes the JPEG at the highest quality that fits docops.MaxPageJPEG,
// scaling down further if no quality fits. It returns the JPEG and its
// size.
func Stamp(stored []byte, name string, at time.Time) ([]byte, int, int, error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(stored))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxStoredSide || cfg.Height > maxStoredSide {
		return nil, 0, 0, errStored
	}
	src, err := png.Decode(bytes.NewReader(stored))
	if err != nil {
		return nil, 0, 0, errStored
	}
	text := stampText(name, at)
	width := min(cfg.Width, MaxWidth)
	for range 4 {
		page := scaled(src, width)
		stampOnto(page, text)
		for _, q := range []int{90, 80, 70, 60, 50, 40} {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, page, &jpeg.Options{Quality: q}); err != nil {
				return nil, 0, 0, err
			}
			if buf.Len() <= docops.MaxPageJPEG {
				b := page.Bounds()
				return buf.Bytes(), b.Dx(), b.Dy(), nil
			}
		}
		width = width * 3 / 4
	}
	return nil, 0, 0, errTooBig
}

var errTooBig = errors.New("pages: the page doesn't fit the reply")

// scaled is src at width w (never wider than it is), the height in
// proportion, as RGBA.
func scaled(src image.Image, w int) *image.RGBA {
	b := src.Bounds()
	if w > b.Dx() {
		w = b.Dx()
	}
	h := max(1, b.Dy()*w/b.Dx())
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	scale(dst, src)
	return dst
}
