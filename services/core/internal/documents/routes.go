// Package documents is core's side of the documents service for clinical
// uploads (documents design §4.2, §10, §11; §13 PR 5; ADR 0016): the
// routes POST /documents, POST /documents/{id}/uploaded, GET
// /documents/{id} and POST /documents/{id}/cancel, the uploaded
// announcement, and core_documents, the consumer of the scan's outcomes.
//
// Off by default: with CORE_DOCUMENTS_ENABLED false every route is 404 and
// nothing is consumed. The file never crosses core: core inserts the row
// (uploading) through a definer that checks the limits and the patient,
// then asks the documents service for a POST policy for exactly that row's
// quarantine key, and the browser posts the file to the object store. The
// policy is passed through to the caller and never stored (NoStore) or
// logged.
//
// The hospital and the uploader are the session's (authz.TxUser), never
// the body's; another hospital's document, or another person's, is 404.
// Every write and its audit entry commit together; entries carry ids and
// codes only, never a file name, a value or a policy.
package documents

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/docsctl"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
)

// Rules: the people who may upload a clinical document (design §4.1,
// documents.upload): clinicians and hospital admins, never a platform
// role. The records-clerk role comes with review (PR 6).
var (
	ReadRule  = authz.Rule{AnyRole: []string{authz.RoleClinician, authz.RoleHospitalAdmin}}
	WriteRule = authz.Rule{AnyRole: []string{authz.RoleClinician, authz.RoleHospitalAdmin}, Write: true}
)

// The purposes' types and limits (design §4.1), as migration 00051's
// documents_upload_start checks them again.
var (
	ClinicalTypes = map[string]bool{"application/pdf": true, "image/png": true, "image/jpeg": true, "image/tiff": true}
	LetterTypes   = map[string]bool{"application/pdf": true, "image/png": true, "image/jpeg": true}
)

const (
	MaxClinicalBytes = 25 << 20
	MaxLetterBytes   = 10 << 20
)

// Announcer publishes documents.v1.<hid>.uploaded after the commit.
type Announcer interface {
	Uploaded(ctx context.Context, hospital, document uuid.UUID) error
}

// Deps are what the routes need. A nil Pool registers the routes for the
// permission table only.
type Deps struct {
	Pool *pgxpool.Pool
	// On is CORE_DOCUMENTS_ENABLED: off, every route is 404.
	On       bool
	Ctl      docsctl.Requester
	Announce Announcer
	Log      *slog.Logger
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Register adds the routes to m.
func Register(m *authz.Mux, d Deps) {
	authz.Route(m, http.MethodPost, "/documents", WriteRule, httpx.NoStore(http.HandlerFunc(d.start)))
	authz.Route(m, http.MethodGet, "/documents/{id}", ReadRule, http.HandlerFunc(d.get))
	authz.Route(m, http.MethodPost, "/documents/{id}/uploaded", WriteRule, http.HandlerFunc(d.uploaded))
	authz.Route(m, http.MethodPost, "/documents/{id}/cancel", WriteRule, http.HandlerFunc(d.cancel))
}

// UploadOut is the POST policy the browser posts the file with: every
// field, in order, then the file last.
type UploadOut struct {
	URL       string     `json:"url"`
	Fields    []FieldOut `json:"fields"`
	ExpiresAt string     `json:"expires_at"`
}

// FieldOut is one form field of the policy.
type FieldOut struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// DocumentOut is one document as its uploader sees it: status and codes,
// never a file name or a value.
type DocumentOut struct {
	ID         string  `json:"id"`
	Purpose    string  `json:"purpose"`
	Status     string  `json:"status"`
	Reason     *string `json:"reason"`
	Pages      *int    `json:"pages"`
	ReceivedAt string  `json:"received_at"`
	ScannedAt  *string `json:"scanned_at"`
	Version    int     `json:"version"`
}

// StartOut is POST /documents's reply.
type StartOut struct {
	Document DocumentOut `json:"document"`
	Upload   UploadOut   `json:"upload"`
}

// ErrRefused is a definer's refusal: its problem code.
type ErrRefused struct{ Code string }

func (e ErrRefused) Error() string { return "documents: refused: " + e.Code }

// codeOf is the problem code for err and whether it is a refusal.
func codeOf(err error) (string, bool) {
	var rf ErrRefused
	if errors.As(err, &rf) {
		return rf.Code, true
	}
	return "not_ready", false
}

// fail answers err, logged with ids, the code and a database error's
// SQLSTATE only.
func (d Deps) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	code, refused := codeOf(err)
	p, _ := authz.FromContext(r.Context())
	attrs := []any{"request_id", httpx.RequestIDFrom(r.Context()), "hospital_id", p.HospitalID.String(),
		"user_id", p.UserID.String(), "reason", code}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		attrs = append(attrs, "sqlstate", pe.Code)
	}
	if refused {
		d.log().Info("documents: "+what+" refused", attrs...)
	} else {
		d.log().Error("documents: "+what+" failed", attrs...)
	}
	httpx.Error(w, r, code)
}

// CanonicalID parses a canonical, non-nil uuid.
func CanonicalID(s string) (uuid.UUID, bool) {
	u, err := uuid.Parse(s)
	if err != nil || u.String() != s || u == uuid.Nil {
		return uuid.Nil, false
	}
	return u, true
}

// RequestID is the request's id, or a fresh one (the documents service's
// token needs one).
func RequestID(ctx context.Context) string {
	if rid := httpx.RequestIDFrom(ctx); docsctl.ValidRID(rid) {
		return rid
	}
	return uuid.NewString()
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

type startIn struct {
	Purpose     string  `json:"purpose"`
	ContentType string  `json:"content_type"`
	SizeBytes   int64   `json:"size_bytes"`
	PatientID   *string `json:"patient_id"`
}

// Start inserts a documents row (uploading) for the caller in tx and
// writes document.upload_started; outcome is the definer's refusal as a
// problem code (ErrRefused).
func Start(ctx context.Context, tx pgx.Tx, purpose, mime string, size int64, patient *uuid.UUID) (uuid.UUID, error) {
	p, _ := authz.FromContext(ctx)
	var outcome string
	var id *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT outcome, document_id FROM documents_upload_start($1, $2, $3, $4)`,
		purpose, mime, size, patient).Scan(&outcome, &id); err != nil {
		return uuid.Nil, err
	}
	switch outcome {
	case "ok":
	case "invalid":
		return uuid.Nil, ErrRefused{"validation_failed"}
	case "rate_limited", "busy", "not_found":
		return uuid.Nil, ErrRefused{outcome}
	default:
		return uuid.Nil, errors.New("documents: unexpected start outcome")
	}
	if id == nil {
		return uuid.Nil, errors.New("documents: no document id")
	}
	return *id, audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
		RequestID: httpx.RequestIDFrom(ctx), Action: "document.upload_started", Entity: "document", EntityID: id.String(),
		Payload: map[string]any{"document_id": id.String(), "purpose": purpose, "via": "user"}})
}

// Presign asks the documents service for document's POST policy. A
// refusal or no answer is ErrRefused{unavailable}.
func Presign(ctx context.Context, ctl docsctl.Requester, hospital, document uuid.UUID) (UploadOut, error) {
	if ctl == nil {
		return UploadOut{}, ErrRefused{"unavailable"}
	}
	var rep docspb.PresignUploadReply
	if err := ctl.Request(ctx, hospital, RequestID(ctx), "presign_upload", &docspb.PresignUploadRequest{
		DocumentId: document.String()}, &rep); err != nil || rep.GetOutcome() != "ok" || rep.GetUrl() == "" ||
		len(rep.GetFields()) == 0 || rep.GetExpiresAt() <= 0 {
		return UploadOut{}, ErrRefused{"unavailable"}
	}
	out := UploadOut{URL: rep.GetUrl(), ExpiresAt: stamp(time.Unix(rep.GetExpiresAt(), 0)),
		Fields: make([]FieldOut, 0, len(rep.GetFields()))}
	for _, f := range rep.GetFields() {
		out.Fields = append(out.Fields, FieldOut{Name: f.GetName(), Value: f.GetValue()})
	}
	return out, nil
}

// CancelUnavailable ends a row whose policy couldn't be had, in its own
// transaction, with its entry (reason unavailable). A failure is logged by
// id: the row then waits for the 30-minute rule.
func CancelUnavailable(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, purpose string, document uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := authz.TxUser(ctx, pool, func(tx pgx.Tx) error {
		return cancelRow(ctx, tx, purpose, document, 1, "unavailable")
	})
	if err != nil {
		log.Error("documents: a row without a policy was not cancelled", "document_id", document.String(),
			"reason", "internal_error")
	}
}

// cancelRow calls documents_cancel and writes document.cancelled.
func cancelRow(ctx context.Context, tx pgx.Tx, purpose string, document uuid.UUID, version int, why string) error {
	p, _ := authz.FromContext(ctx)
	var outcome string
	var status *string
	if err := tx.QueryRow(ctx, `SELECT outcome, status FROM documents_cancel($1, $2, $3)`, document, purpose, version).
		Scan(&outcome, &status); err != nil {
		return err
	}
	switch outcome {
	case "cancelled":
	case "not_found", "conflict":
		return ErrRefused{outcome}
	default:
		return errors.New("documents: unexpected cancel outcome")
	}
	return writeCancelled(ctx, tx, p, purpose, document, why)
}

func writeCancelled(ctx context.Context, tx pgx.Tx, p authz.Principal, purpose string, document uuid.UUID, why string) error {
	return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
		RequestID: httpx.RequestIDFrom(ctx), Action: "document.cancelled", Entity: "document", EntityID: document.String(),
		Payload: map[string]any{"document_id": document.String(), "purpose": purpose, "reason": why}})
}

// MarkUploaded moves the caller's own row from uploading to uploaded and
// writes document.uploaded; a row already past uploading is unchanged
// (changed false, no entry); a row left uploading for 30 minutes is
// cancelled (ErrRefused{conflict} after its entry is written, so the
// caller must still commit).
func MarkUploaded(ctx context.Context, tx pgx.Tx, purpose string, document uuid.UUID) (changed bool, err error) {
	p, _ := authz.FromContext(ctx)
	var outcome string
	var status *string
	if err := tx.QueryRow(ctx, `SELECT outcome, status FROM documents_mark_uploaded($1, $2)`, document, purpose).
		Scan(&outcome, &status); err != nil {
		return false, err
	}
	switch outcome {
	case "uploaded":
		return true, audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Action: "document.uploaded", Entity: "document", EntityID: document.String(),
			Payload: map[string]any{"document_id": document.String(), "purpose": purpose, "via": "user"}})
	case "unchanged":
		return false, nil
	case "expired":
		return false, writeCancelled(ctx, tx, p, purpose, document, "expired")
	case "not_found":
		return false, ErrRefused{"not_found"}
	}
	return false, errors.New("documents: unexpected uploaded outcome")
}

// view reads the caller's own document of purpose.
func view(ctx context.Context, tx pgx.Tx, purpose string, id uuid.UUID) (DocumentOut, error) {
	var (
		out       DocumentOut
		received  time.Time
		scannedAt *time.Time
	)
	err := tx.QueryRow(ctx, `SELECT id, purpose, status, reason, pages, received_at, scanned_at, version FROM documents
		WHERE id = $1 AND purpose = $2 AND via = 'user' AND uploaded_by = mhai_current_user()`, id, purpose).
		Scan(&out.ID, &out.Purpose, &out.Status, &out.Reason, &out.Pages, &received, &scannedAt, &out.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return DocumentOut{}, ErrRefused{"not_found"}
	}
	if err != nil {
		return DocumentOut{}, err
	}
	out.ReceivedAt = stamp(received)
	if scannedAt != nil {
		s := stamp(*scannedAt)
		out.ScannedAt = &s
	}
	return out, nil
}

// start is POST /documents {purpose, content_type, size_bytes, patient_id?}.
// Order: off (404); the body (422: only purpose clinical, a type and size
// of §4.1); the row and its entry (429 past 30 starts an hour, 503 busy
// past the queue cap, 404 for a patient not in the hospital); then the
// policy (503 unavailable, and the row is cancelled). 201 with the
// document and the policy, never stored for replay.
func (d Deps) start(w http.ResponseWriter, r *http.Request) {
	if !d.On {
		httpx.Error(w, r, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var in startIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	var patient *uuid.UUID
	if in.PatientID != nil {
		u, ok := CanonicalID(*in.PatientID)
		if !ok {
			httpx.Error(w, r, "validation_failed")
			return
		}
		patient = &u
	}
	if in.Purpose != "clinical" || !ClinicalTypes[in.ContentType] || in.SizeBytes < 1 || in.SizeBytes > MaxClinicalBytes {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	p, _ := authz.FromContext(ctx)
	var out StartOut
	err := authz.TxUser(ctx, d.Pool, func(tx pgx.Tx) error {
		id, err := Start(ctx, tx, in.Purpose, in.ContentType, in.SizeBytes, patient)
		if err != nil {
			return err
		}
		out.Document, err = view(ctx, tx, in.Purpose, id)
		return err
	})
	if err != nil {
		d.fail(w, r, "start", err)
		return
	}
	doc, _ := uuid.Parse(out.Document.ID)
	if out.Upload, err = Presign(ctx, d.Ctl, p.HospitalID, doc); err != nil {
		CancelUnavailable(ctx, d.Pool, d.log(), in.Purpose, doc)
		d.fail(w, r, "start", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusCreated, out)
}

// get is GET /documents/{id}: the uploader's own clinical document.
func (d Deps) get(w http.ResponseWriter, r *http.Request) {
	id, ok := CanonicalID(r.PathValue("id"))
	if !d.On || !ok || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	ctx := r.Context()
	var out DocumentOut
	err := authz.TxUser(ctx, d.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = view(ctx, tx, "clinical", id)
		return err
	})
	if err != nil {
		d.fail(w, r, "get", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

// uploaded is POST /documents/{id}/uploaded: the uploader says the file is
// posted. uploading becomes uploaded and the scan is announced after the
// commit; a second call changes nothing and announces nothing; a row left
// uploading for 30 minutes is cancelled (409 conflict). 202 with the
// document.
func (d Deps) uploaded(w http.ResponseWriter, r *http.Request) {
	id, ok := CanonicalID(r.PathValue("id"))
	if !d.On || !ok || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	ctx := r.Context()
	p, _ := authz.FromContext(ctx)
	var (
		out     DocumentOut
		changed bool
		expired bool
	)
	err := authz.TxUser(ctx, d.Pool, func(tx pgx.Tx) error {
		var err error
		changed, err = MarkUploaded(ctx, tx, "clinical", id)
		if err != nil {
			return err
		}
		out, err = view(ctx, tx, "clinical", id)
		expired = out.Status == "cancelled" && !changed
		return err
	})
	if err != nil {
		d.fail(w, r, "uploaded", err)
		return
	}
	if expired {
		httpx.Error(w, r, "conflict")
		return
	}
	if changed {
		Announce(ctx, d.Announce, d.log(), p.HospitalID, id)
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusAccepted, out)
}

// Announce publishes uploaded after the commit. A failure is logged by id:
// the documents service's sweeper scans a row left uploaded for 2 minutes
// (design §17 departure 22).
func Announce(ctx context.Context, a Announcer, log *slog.Logger, hospital, document uuid.UUID) {
	if a == nil {
		return
	}
	if err := a.Uploaded(context.WithoutCancel(ctx), hospital, document); err != nil {
		log.Warn("documents: uploaded was not announced; the documents sweeper takes the row over",
			"document_id", document.String(), "reason", "publish_failed")
	}
}

type cancelIn struct {
	Version *int `json:"version"`
}

// cancel is POST /documents/{id}/cancel {version}: the uploader ends their
// document before review (uploading to queued), at the version they saw
// (409 conflict otherwise, or past queued).
func (d Deps) cancel(w http.ResponseWriter, r *http.Request) {
	id, ok := CanonicalID(r.PathValue("id"))
	if !d.On || !ok || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	var in cancelIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Version == nil || *in.Version < 1 {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	var out DocumentOut
	err := authz.TxUser(ctx, d.Pool, func(tx pgx.Tx) error {
		if err := cancelRow(ctx, tx, "clinical", id, *in.Version, "user"); err != nil {
			return err
		}
		var err error
		out, err = view(ctx, tx, "clinical", id)
		return err
	})
	if err != nil {
		d.fail(w, r, "cancel", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}
