package letters

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// Rules: hospital admins with a two-step session; writes need a fresh
// code (step-up) and an Idempotency-Key.
var (
	ReadRule  = authz.Rule{AnyRole: []string{authz.RoleHospitalAdmin}, NeedsMFA: true}
	WriteRule = authz.Rule{AnyRole: []string{authz.RoleHospitalAdmin}, NeedsMFA: true, Write: true, StepUp: true}
)

// Register adds the hospital side's letter routes to m.
func Register(m *authz.Mux, d Deps) {
	authz.Route(m, http.MethodGet, "/hospital/clearances", ReadRule, http.HandlerFunc(d.list))
	authz.Route(m, http.MethodPost, "/hospital/clearances", WriteRule, http.HandlerFunc(d.upload))
	authz.Route(m, http.MethodGet, "/hospital/clearances/{id}", ReadRule, http.HandlerFunc(d.get))
	authz.Route(m, http.MethodGet, "/hospital/clearances/{id}/pages/{n}", ReadRule, http.HandlerFunc(d.page))
	authz.Route(m, http.MethodPost, "/hospital/clearances/{id}/revoke", WriteRule, http.HandlerFunc(d.revoke))
	authz.Route(m, http.MethodPost, "/hospital/clearances/{id}/narrow", WriteRule, http.HandlerFunc(d.narrow))
}

var errNoPool = errors.New("letters: no database")

// tx runs fn as the caller's hospital and user, after the gate (the two
// switches and the caller still an active hospital admin with two-step,
// re-read in this transaction).
func (d Deps) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	if d.Pool == nil {
		return errNoPool
	}
	return authz.TxUser(ctx, d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT mhai_clearance_gate()`); err != nil {
			return err
		}
		return fn(tx)
	})
}

// fail answers err: its refusal code, or 503 not_ready for anything else,
// logged with ids, the code and a database error's SQLSTATE only.
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
		d.log().Info("letters: "+what+" refused", attrs...)
	} else {
		d.log().Error("letters: "+what+" failed", attrs...)
	}
	httpx.Error(w, r, code)
}

func canonicalID(s string) (uuid.UUID, bool) {
	u, err := uuid.Parse(s)
	if err != nil || u.String() != s || u == uuid.Nil {
		return uuid.Nil, false
	}
	return u, true
}

// list is GET /hospital/clearances: {clearances, today, max_end_date}.
func (d Deps) list(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	var out ClearancesOut
	err := d.tx(ctx, func(tx pgx.Tx) error {
		t, err := readToday(ctx, tx)
		if err != nil {
			return err
		}
		out.Today, out.MaxEndDate = day(t.Today), day(t.MaxEndDate)
		out.Clearances, err = load(ctx, tx, nil)
		return err
	})
	if err != nil {
		d.fail(w, r, "list", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

// one reads letter id of the caller's hospital (not_found otherwise).
func one(ctx context.Context, tx pgx.Tx, id uuid.UUID) (ClearanceOut, error) {
	l, err := load(ctx, tx, &id)
	if err != nil {
		return ClearanceOut{}, err
	}
	if len(l) != 1 {
		return ClearanceOut{}, refusal{"not_found"}
	}
	return l[0], nil
}

// get is GET /hospital/clearances/{id}.
func (d Deps) get(w http.ResponseWriter, r *http.Request) {
	id, ok := canonicalID(r.PathValue("id"))
	if !ok || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	ctx := r.Context()
	var out ClearanceOut
	err := d.tx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = one(ctx, tx, id)
		return err
	})
	if err != nil {
		d.fail(w, r, "get", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

var pageShape = regexp.MustCompile(`^[1-9][0-9]?$`)

// page is GET /hospital/clearances/{id}/pages/{n}: one page image. The
// view is audited (clearance.viewed) and committed before the page is
// sent. Another hospital's letter, or a page it hasn't, is 404; no pages
// yet is 503 not_ready; a letter that isn't a demo letter is 503
// unavailable (its pages are the documents service's).
func (d Deps) page(w http.ResponseWriter, r *http.Request) {
	id, ok := canonicalID(r.PathValue("id"))
	ns := r.PathValue("n")
	if !ok || !pageShape.MatchString(ns) || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	n, _ := strconv.Atoi(ns)
	if n > maxPages {
		httpx.Error(w, r, "not_found")
		return
	}
	ctx := r.Context()
	p, _ := authz.FromContext(ctx)
	err := d.tx(ctx, func(tx pgx.Tx) error {
		var pages int
		var demo bool
		err := tx.QueryRow(ctx, `SELECT pages, demo FROM clearances WHERE id = $1`, id).Scan(&pages, &demo)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return refusal{"not_found"}
		case err != nil:
			return err
		case pages == 0:
			return refusal{"not_ready"}
		case n > pages:
			return refusal{"not_found"}
		case !demo:
			return refusal{"unavailable"}
		}
		return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Action: "clearance.viewed", Entity: "clearance", EntityID: id.String(),
			Payload: map[string]any{"page": n}})
	})
	if err != nil {
		d.fail(w, r, "page", err)
		return
	}
	body := fixturePage(n)
	if len(body) == 0 {
		httpx.Error(w, r, "unavailable")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Disposition", "inline")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// keyed reports whether the request carries one Idempotency-Key (the
// middleware checks its shape); the letter writes require it.
func keyed(r *http.Request) bool { return len(r.Header.Values("Idempotency-Key")) == 1 }

// stepUpIn is the code a write carries.
type stepUpIn struct {
	MFACode    string `json:"mfa_code"`
	BackupCode string `json:"backup_code"`
}

// stepUp checks a fresh code (a refusal is audited by auth); false means
// the reply was sent.
func (d Deps) stepUp(w http.ResponseWriter, r *http.Request, in stepUpIn) bool {
	if in.MFACode == "" && in.BackupCode == "" {
		httpx.Error(w, r, "mfa_required")
		return false
	}
	if d.stepUpCheck != nil {
		if d.stepUpCheck(r, in.MFACode, in.BackupCode) != nil {
			httpx.Error(w, r, "mfa_invalid")
			return false
		}
		return true
	}
	if d.Auth == nil {
		httpx.Error(w, r, "not_ready")
		return false
	}
	if err := d.Auth.StepUpAudited(r, in.MFACode, in.BackupCode); err != nil {
		d.Auth.WriteStepUpError(w, r, err)
		return false
	}
	return true
}

// visible checks, before asking for a code, that the gate passes and the
// letter is the caller's hospital's (404 otherwise).
func (d Deps) visible(ctx context.Context, id uuid.UUID) error {
	return d.tx(ctx, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM clearances WHERE id = $1`, id).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return refusal{"not_found"}
		}
		return nil
	})
}

// note is an optional note: 1-500 characters, no control characters.
func validNote(s *string) bool {
	if s == nil {
		return true
	}
	return utf8.ValidString(*s) && textShape.MatchString(*s) && utf8.RuneCountInString(*s) <= 500
}

var textShape = regexp.MustCompile(`^[^\p{Cc}]+$`)

var revokeReasons = map[string]bool{"no_longer_needed": true, "person_left": true, "uploaded_in_error": true,
	"security_concern": true, "other": true}
var narrowReasons = map[string]bool{"person_left": true, "no_longer_needed": true, "uploaded_in_error": true, "other": true}

type revokeIn struct {
	Reason  string  `json:"reason"`
	Note    *string `json:"note"`
	Version *int    `json:"version"`
	stepUpIn
}

// revoke is POST /hospital/clearances/{id}/revoke: a waiting or active
// letter, at once. Order: the path and the gate (404, 403), the key and
// the body (422), the code (403), then the revoke and its entry.
func (d Deps) revoke(w http.ResponseWriter, r *http.Request) {
	id, ok := canonicalID(r.PathValue("id"))
	if !ok || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	ctx := r.Context()
	if err := d.visible(ctx, id); err != nil {
		d.fail(w, r, "revoke", err)
		return
	}
	var in revokeIn
	if !keyed(r) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if !revokeReasons[in.Reason] || !validNote(in.Note) || (in.Reason == "other" && in.Note == nil) ||
		(in.Version != nil && *in.Version < 1) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !d.stepUp(w, r, in.stepUpIn) {
		return
	}
	d.change(w, r, "revoke", id, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT mhai_clearance_revoke($1, $2, $3, $4)`, id, in.Version, in.Reason, in.Note)
		return err
	})
}

type narrowIn struct {
	Remove  []string `json:"remove"`
	Reason  string   `json:"reason"`
	Note    *string  `json:"note"`
	Version *int     `json:"version"`
	stepUpIn
}

// narrow is POST /hospital/clearances/{id}/narrow: removes people, never
// all of them (422 would_empty), at the version the admin saw (409
// conflict otherwise).
func (d Deps) narrow(w http.ResponseWriter, r *http.Request) {
	id, ok := canonicalID(r.PathValue("id"))
	if !ok || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	ctx := r.Context()
	if err := d.visible(ctx, id); err != nil {
		d.fail(w, r, "narrow", err)
		return
	}
	var in narrowIn
	if !keyed(r) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	remove := make([]uuid.UUID, 0, len(in.Remove))
	seen := map[uuid.UUID]bool{}
	for _, s := range in.Remove {
		u, ok := canonicalID(s)
		if !ok || seen[u] {
			httpx.Error(w, r, "validation_failed")
			return
		}
		seen[u] = true
		remove = append(remove, u)
	}
	if len(remove) < 1 || len(remove) > MaxCovered || !narrowReasons[in.Reason] || !validNote(in.Note) ||
		(in.Reason == "other" && in.Note == nil) || in.Version == nil || *in.Version < 1 {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !d.stepUp(w, r, in.stepUpIn) {
		return
	}
	d.change(w, r, "narrow", id, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT mhai_clearance_narrow($1, $2, $3, $4, $5)`, id, *in.Version, remove, in.Reason, in.Note)
		return err
	})
}

// change runs fn, appends the entries the database demands, and replies
// with the letter as it now is, in one transaction.
func (d Deps) change(w http.ResponseWriter, r *http.Request, what string, id uuid.UUID, fn func(pgx.Tx) error) {
	ctx := r.Context()
	var out ClearanceOut
	err := d.tx(ctx, func(tx pgx.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		if _, err := AuditPending(ctx, tx, httpx.RequestIDFrom(ctx)); err != nil {
			return err
		}
		var err error
		out, err = one(ctx, tx, id)
		return err
	})
	if err != nil {
		d.fail(w, r, what, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

// upload is POST /hospital/clearances (multipart). Order (design §8.1,
// plan Task 19): the gate (404, 403); the key, the media type (415), the
// body's size (413) and shape (422, T19: an unknown field such as a
// hospital or an uploader is refused); the code (403); then, before
// anything is stored, the dates (422 end_date_too_late), the people (422
// not_eligible with the ids, not_eligible_platform with the emails) and
// renews (422; not_eligible_platform without the emails). Outside the demo nothing is stored: 503 unavailable until
// the documents service exists. In the demo the letter, its people and
// its fixture pages are stored with their entries, and the reply is the
// letter (201). The file is never kept, parsed or logged.
func (d Deps) upload(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	if err := d.tx(ctx, func(pgx.Tx) error { return nil }); err != nil {
		d.fail(w, r, "upload", err)
		return
	}
	if !keyed(r) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	in, code := parseUpload(r)
	if code != "" {
		httpx.Error(w, r, code)
		return
	}
	if !d.stepUp(w, r, stepUpIn{MFACode: in.mfaCode, BackupCode: in.backupCode}) {
		return
	}
	sum := sha256.Sum256(in.file)
	in.file = nil
	var out ClearanceOut
	var bad []string
	badCode := ""
	err := d.tx(ctx, func(tx pgx.Tx) error {
		var badStaff []uuid.UUID
		var badEmails []string
		var platformIDs []uuid.UUID
		var renewsOK bool
		if err := tx.QueryRow(ctx, `SELECT bad_staff, bad_emails, platform_ids, renews_ok
			FROM mhai_clearance_check($1, $2, $3, $4, $5::citext[])`,
			in.letterDate, in.endDate, in.renews, in.covered, in.coveredPlatform).
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
			// Without the list (security review): which addresses are
			// active platform admins is the platform's to say, not a lookup.
			return refusal{"not_eligible_platform"}
		case !renewsOK:
			return refusal{"validation_failed"}
		}
		t, err := readToday(ctx, tx)
		if err != nil {
			return err
		}
		if !t.Demo {
			return refusal{"unavailable"}
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT mhai_clearance_create($1, $2, $3, $4, $5, $6, $7)`, in.signedBy, in.signerTitle,
			in.letterDate, in.endDate, in.renews, in.covered, platformIDs).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT mhai_clearance_demo_ready($1, $2, $3)`, id, DemoPages, sum[:]); err != nil {
			return err
		}
		if _, err := AuditPending(ctx, tx, httpx.RequestIDFrom(ctx)); err != nil {
			return err
		}
		out, err = one(ctx, tx, id)
		return err
	})
	if err != nil {
		if badCode != "" {
			httpx.ErrorFields(w, r, badCode, map[string]any{"ids": bad})
			return
		}
		d.fail(w, r, "upload", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusCreated, out)
}
