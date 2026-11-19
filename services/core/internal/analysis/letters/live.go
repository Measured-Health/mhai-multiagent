package letters

import (
	"context"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/documents"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
)

// The live letter (documents design §10; §13 PR 5; §17 departure 37): the
// letter's metadata comes as JSON with the file's type and size, the file
// goes straight to the object store by the documents service's POST
// policy, and the letter waits in checking until the scan's outcome
// (core_documents, Scanned) makes it waiting or refused. Off unless
// CORE_DOCUMENTS_LIVE_LETTERS is on, which waits on the legal lead's
// letter template (H3): until then these routes are 404 and the multipart
// route stays the demo's.

// UploadedRule: the uploader says the file is posted; no fresh code (the
// upload's was checked when the letter was made).
var UploadedRule = authz.Rule{AnyRole: []string{authz.RoleHospitalAdmin}, NeedsMFA: true, Write: true}

type liveIn struct {
	SignedBy        string   `json:"signed_by"`
	SignerTitle     string   `json:"signer_title"`
	LetterDate      string   `json:"letter_date"`
	EndDate         string   `json:"end_date"`
	Covered         []string `json:"covered"`
	CoveredPlatform []string `json:"covered_platform"`
	Renews          *string  `json:"renews"`
	ContentType     string   `json:"content_type"`
	SizeBytes       int64    `json:"size_bytes"`
	stepUpIn
}

// LiveOut is the live upload's reply: the letter (checking) and the POST
// policy for its file.
type LiveOut struct {
	Clearance ClearanceOut        `json:"clearance"`
	Upload    documents.UploadOut `json:"upload"`
}

// parseLive checks the live upload's body as parseUpload checks the
// multipart one, plus the file's declared type and size (§4.1: a PDF, PNG
// or JPEG of at most 10 MB).
func parseLive(in liveIn) (uploadIn, bool) {
	var out uploadIn
	for _, v := range []string{in.SignedBy, in.SignerTitle} {
		if !utf8.ValidString(v) || !textShape.MatchString(v) || utf8.RuneCountInString(v) > 120 || v != trimmed(v) {
			return out, false
		}
	}
	out.signedBy, out.signerTitle = in.SignedBy, in.SignerTitle
	var ok bool
	if out.letterDate, ok = date(in.LetterDate); !ok {
		return out, false
	}
	if out.endDate, ok = date(in.EndDate); !ok {
		return out, false
	}
	if in.Renews != nil {
		u, ok := canonicalID(*in.Renews)
		if !ok {
			return out, false
		}
		out.renews = &u
	}
	for _, c := range []string{in.MFACode, in.BackupCode} {
		if c != "" && !codeShape.MatchString(c) {
			return out, false
		}
	}
	out.mfaCode, out.backupCode = in.MFACode, in.BackupCode
	if n := len(in.Covered) + len(in.CoveredPlatform); n < 1 || n > MaxCovered {
		return out, false
	}
	ids := map[uuid.UUID]bool{}
	for _, s := range in.Covered {
		u, ok := canonicalID(s)
		if !ok || ids[u] {
			return out, false
		}
		ids[u] = true
		out.covered = append(out.covered, u)
	}
	emails := map[string]bool{}
	for _, e := range in.CoveredPlatform {
		if !emailShape.MatchString(e) || len(e) > 254 || emails[lower(e)] {
			return out, false
		}
		emails[lower(e)] = true
		out.coveredPlatform = append(out.coveredPlatform, e)
	}
	if !documents.LetterTypes[in.ContentType] || in.SizeBytes < 1 || in.SizeBytes > documents.MaxLetterBytes {
		return out, false
	}
	return out, true
}

// live is POST /hospital/clearances/live. Order: off (404); the gates (404,
// 403); the key and the body (422); the code (403); a check slot (429);
// the letter's checks (422, as the multipart route's) and the document row
// (429 past the uploads limit); the POST policy (503 unavailable: the
// document is cancelled and no letter is made); then the letter, checking,
// with its entries. 201 {clearance, upload}, never stored for replay.
func (d Deps) live(w http.ResponseWriter, r *http.Request) {
	if !d.Live || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	ctx := r.Context()
	if err := d.tx(ctx, func(pgx.Tx) error { return nil }); err != nil {
		d.fail(w, r, "live", err)
		return
	}
	if !keyed(r) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var raw liveIn
	if !httpx.Decode(w, r, &raw) {
		return
	}
	in, ok := parseLive(raw)
	if !ok {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !d.stepUp(w, r, stepUpIn{MFACode: in.mfaCode, BackupCode: in.backupCode}) {
		return
	}
	slot, ok, err := d.reserveCheck(ctx)
	if err != nil {
		d.fail(w, r, "live", err)
		return
	}
	if !ok {
		httpx.Error(w, r, "rate_limited")
		return
	}
	// The checks and the document row, before anything is signed.
	var (
		doc             uuid.UUID
		platformIDs     []uuid.UUID
		bad             []string
		badCode         string
		platformRefused bool
	)
	err = d.tx(ctx, func(tx pgx.Tx) error {
		var badStaff []uuid.UUID
		var badEmails []string
		var renewsOK bool
		if err := tx.QueryRow(ctx, `SELECT bad_staff, bad_emails, platform_ids, renews_ok
			FROM mhai_clearance_check_live($1, $2, $3, $4, $5::citext[], $6)`,
			in.letterDate, in.endDate, in.renews, in.covered, in.coveredPlatform, slot).
			Scan(&badStaff, &badEmails, &platformIDs, &renewsOK); err != nil {
			return err
		}
		switch {
		case len(badStaff) > 0:
			badCode = "not_eligible"
			for _, u := range badStaff {
				bad = append(bad, u.String())
			}
			return refusal{badCode}
		case len(badEmails) > 0:
			platformRefused = true
			return refusal{"not_eligible_platform"}
		case !renewsOK:
			return refusal{"validation_failed"}
		}
		var err error
		doc, err = documents.Start(ctx, tx, "clearance", raw.ContentType, raw.SizeBytes, nil)
		return asRefusal(err)
	})
	if code, _ := codeOf(err); badCode == "" && !platformRefused && code != "rate_limited" {
		d.releaseCheck(ctx)
	}
	if err != nil {
		if badCode != "" {
			httpx.ErrorFields(w, r, badCode, map[string]any{"ids": bad})
			return
		}
		d.fail(w, r, "live", err)
		return
	}
	p, _ := authz.FromContext(ctx)
	upload, err := documents.Presign(ctx, d.Ctl, p.HospitalID, doc)
	if err != nil {
		documents.CancelUnavailable(ctx, d.Pool, d.log(), "clearance", doc)
		d.fail(w, r, "live", asRefusal(err))
		return
	}
	var out LiveOut
	out.Upload = upload
	err = d.tx(ctx, func(tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT mhai_clearance_create_live($1, $2, $3, $4, $5, $6, $7, $8)`, in.signedBy,
			in.signerTitle, in.letterDate, in.endDate, in.renews, in.covered, platformIDs, doc).Scan(&id); err != nil {
			return err
		}
		if _, err := AuditPending(ctx, tx, httpx.RequestIDFrom(ctx)); err != nil {
			return err
		}
		var err error
		out.Clearance, err = one(ctx, tx, id)
		return err
	})
	if err != nil {
		documents.CancelUnavailable(ctx, d.Pool, d.log(), "clearance", doc)
		d.fail(w, r, "live", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusCreated, out)
}

// asRefusal maps a documents refusal to the letters' refusal.
func asRefusal(err error) error {
	var rf documents.ErrRefused
	if errors.As(err, &rf) {
		return refusal{rf.Code}
	}
	return err
}

// uploaded is POST /hospital/clearances/{id}/uploaded: the uploader says
// the letter's file is posted. Its document becomes uploaded and the scan
// is announced after the commit; again, it changes nothing. A file not
// posted within 30 minutes ends the document, and the letter is refused
// (409 conflict). Another hospital's letter, a demo letter (no document),
// or another admin's upload: 404. 202 with the letter.
func (d Deps) uploaded(w http.ResponseWriter, r *http.Request) {
	id, ok := canonicalID(r.PathValue("id"))
	if !d.Live || !ok || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	ctx := r.Context()
	p, _ := authz.FromContext(ctx)
	var (
		out     ClearanceOut
		doc     uuid.UUID
		changed bool
		expired bool
	)
	err := d.tx(ctx, func(tx pgx.Tx) error {
		var docID *uuid.UUID
		err := tx.QueryRow(ctx, `SELECT document_id FROM clearances WHERE id = $1`, id).Scan(&docID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && docID == nil) {
			return refusal{"not_found"}
		}
		if err != nil {
			return err
		}
		doc = *docID
		if changed, err = documents.MarkUploaded(ctx, tx, "clearance", doc); err != nil {
			return asRefusal(err)
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM documents WHERE id = $1`, doc).Scan(&status); err != nil {
			return err
		}
		if !changed && status == "cancelled" {
			expired = true
			if err := Scanned(ctx, tx, doc); err != nil {
				return err
			}
		}
		out, err = one(ctx, tx, id)
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
		documents.Announce(ctx, d.Announce, d.log(), p.HospitalID, doc)
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusAccepted, out)
}

// Scanned moves a live letter on from its document's row
// (mhai_clearance_scanned) and appends the letter's entries, in tx (the
// gateway under the letter's hospital, with or without a user): ready
// makes it waiting, anything else refused. A letter that isn't checking,
// or whose scan hasn't finished, is unchanged. core_documents calls it.
func Scanned(ctx context.Context, tx pgx.Tx, document uuid.UUID) error {
	var outcome string
	if err := tx.QueryRow(ctx, `SELECT mhai_clearance_scanned($1)`, document).Scan(&outcome); err != nil {
		return err
	}
	if outcome != "done" {
		return nil
	}
	_, err := AuditPending(ctx, tx, httpx.RequestIDFrom(ctx))
	return err
}

// livePage is a live letter's page n, from the documents service, stamped
// with the viewer's display name and the time; the view's entry
// (clearance.viewed) commits before the bytes are sent. Not ready or no
// answer: 503 unavailable, and no entry.
func (d Deps) livePage(w http.ResponseWriter, r *http.Request, id, doc uuid.UUID, n int) {
	ctx := r.Context()
	p, _ := authz.FromContext(ctx)
	var name string
	if err := d.tx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT display_name FROM mhai_clearance_names(ARRAY[$1]::uuid[])`, p.UserID).Scan(&name)
	}); err != nil || name == "" {
		d.fail(w, r, "page", refusal{"not_ready"})
		return
	}
	if d.Ctl == nil {
		httpx.Error(w, r, "unavailable")
		return
	}
	var rep docspb.PageImageReply
	err := d.Ctl.Request(ctx, p.HospitalID, documents.RequestID(ctx), "page_image", &docspb.PageImageRequest{
		DocumentId: doc.String(), N: uint32(n), ViewerName: name, At: time.Now().Unix()}, &rep)
	switch {
	case err != nil || rep.GetOutcome() == "unavailable":
		d.fail(w, r, "page", refusal{"unavailable"})
		return
	case rep.GetOutcome() == "not_found":
		d.fail(w, r, "page", refusal{"not_found"})
		return
	case rep.GetOutcome() != "ok" || len(rep.GetJpeg()) == 0:
		d.fail(w, r, "page", refusal{"unavailable"})
		return
	}
	if err := d.tx(ctx, func(tx pgx.Tx) error {
		return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Action: "clearance.viewed", Entity: "clearance", EntityID: id.String(),
			Payload: map[string]any{"page": n}})
	}); err != nil {
		d.fail(w, r, "page", err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Disposition", "inline")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rep.GetJpeg())
}
