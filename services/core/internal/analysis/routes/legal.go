package routes

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// The legal path, hospital side (design §8.9 step 2; plan Task 26,
// departures 72-76):
//
//   - GET /hospital/legal-resolutions: the caller's open references.
//   - POST /hospital/legal-resolutions {legal_reference_id, mfa_code |
//     backup_code}: a hospital admin named on a reference, covered by a
//     letter now, with step-up, resolves it once. In one transaction as
//     the person: the role, two-step and account re-read; the reference
//     consumed (or replayed by the same key within 15 minutes) by
//     mhai_legal_subjects; the flagged analysis.legal_resolved appended
//     last; commit. Only then are names read, for exactly the export's
//     subjects, and sent in this one reply: never a file, never kept,
//     logged or traced, and no pid.
//
// Off by default: analysis off is 404, named access off 403
// named_access_off, and a reference exists only for an export the
// platform made under CORE_AUDIT_EXPORTS.

// LegalRule is the resolution's rule: hospital admins with two-step,
// step-up in the body, and no generic idempotency store (the reply holds
// names; the reference's own key handles a retry, departure 72).
var LegalRule = authz.Rule{AnyRole: []string{authz.RoleHospitalAdmin}, NeedsMFA: true, Write: true, StepUp: true,
	NoIdempotency: true}

// LegalReadRule is the list's rule.
var LegalReadRule = authz.Rule{AnyRole: []string{authz.RoleHospitalAdmin}, NeedsMFA: true}

// LegalPerMinute is each person's resolutions a minute (departure 75).
const LegalPerMinute = 5

var (
	errAlreadyUsed = errors.New("routes: legal reference already used")
	// errLegalGone: a row of the export no longer resolves; nothing is sent.
	errLegalGone = errors.New("routes: legal reference rows gone")
	errLegalBody = errors.New("routes: legal body")
)

// legalKeyHash is the stored form of an Idempotency-Key for a resolution:
// bound to the hospital and the person (as departure 57's).
func legalKeyHash(hospital, user uuid.UUID, key string) []byte {
	h := sha256.New()
	h.Write([]byte("analysis/legal-key/v1\x00"))
	h.Write(hospital[:])
	h.Write(user[:])
	h.Write([]byte(key))
	return h.Sum(nil)
}

// legalErr maps mhai_legal_subjects' refusals.
func legalErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "P0001" {
		switch pe.Message {
		case "not_found":
			return analysis.ErrAnalysisOff
		case "named_access_off":
			return analysis.ErrNamedOff
		case "not_cleared":
			return analysis.ErrNotCleared
		case "clearance_ended":
			return analysis.ErrClearanceEnded
		case "forbidden":
			return analysis.ErrNotAllowed
		case "already_used":
			return errAlreadyUsed
		case "unavailable":
			return errLegalGone
		}
	}
	return err
}

type legalReferenceOut struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
	Rows      int       `json:"rows"`
	CreatedAt time.Time `json:"created_at"`
}

// legalList is GET /hospital/legal-resolutions: the caller's open
// references at this hospital (departure 74).
func (h *handlers) legalList(w http.ResponseWriter, r *http.Request) {
	if !h.d.LegalOn || !h.on(r.Context()) {
		httpx.Error(w, r, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	out := struct {
		References []legalReferenceOut `json:"references"`
	}{References: []legalReferenceOut{}}
	err := analysis.PersonTx(ctx, h.d.Pool, func(tx pgx.Tx) error {
		pos, _, err := analysis.NamedState(ctx, tx)
		if err != nil {
			return err
		}
		if pos == "off" {
			return analysis.ErrNamedOff
		}
		rows, err := tx.Query(ctx, `SELECT id, reference, export_rows, created_at FROM mhai_legal_references_mine()`)
		if err != nil {
			return legalErr(err)
		}
		defer rows.Close()
		for rows.Next() {
			var o legalReferenceOut
			if err := rows.Scan(&o.ID, &o.Reference, &o.Rows, &o.CreatedAt); err != nil {
				return err
			}
			out.References = append(out.References, o)
		}
		return rows.Err()
	})
	if err != nil {
		h.namedFail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

type legalIn struct {
	LegalReferenceID string `json:"legal_reference_id"`
	MFACode          string `json:"mfa_code"`
	BackupCode       string `json:"backup_code"`
}

// legalSubject is one export row as mhai_legal_subjects returns it.
type legalSubject struct {
	ref, level string
	start      time.Time
	enc        []byte
	demo       bool
}

type legalPeriod struct {
	Level string `json:"level"`
	Start string `json:"start"`
}

// legalRow is one resolved row: the export's opaque row reference, the
// period, the name and the MRN; no pid (departure 73).
type legalRow struct {
	ExportRowRef string      `json:"export_row_ref"`
	Period       legalPeriod `json:"period"`
	Name         string      `json:"name"`
	MRN          *string     `json:"mrn"`
}

// legalResolve is POST /hospital/legal-resolutions.
func (h *handlers) legalResolve(w http.ResponseWriter, r *http.Request) {
	if !h.d.LegalOn || !h.on(r.Context()) {
		httpx.Error(w, r, "not_found")
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	if !idemKeyOK(keys) || r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var in legalIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	ref, err := uuid.Parse(in.LegalReferenceID)
	if err != nil || ref.String() != in.LegalReferenceID {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*disclose.StatementTimeout+identifyTimeout)
	defer cancel()
	pr, _ := authz.FromContext(ctx)
	if h.d.Keys == nil || (h.d.Auth == nil && h.stepUpCheck == nil) {
		httpx.Error(w, r, "not_ready")
		return
	}
	if ok, err := h.legalAdmit(ctx, pr); err != nil {
		h.fail(w, r, err)
		return
	} else if !ok {
		httpx.ErrorRetryAfter(w, r, "rate_limited", int(time.Minute.Seconds()))
		return
	}
	if in.MFACode == "" && in.BackupCode == "" {
		httpx.Error(w, r, "mfa_required")
		return
	}
	if h.stepUpCheck != nil {
		if h.stepUpCheck(r, in.MFACode, in.BackupCode) != nil {
			httpx.Error(w, r, "mfa_invalid")
			return
		}
	} else if err := h.d.Auth.StepUpAudited(r, in.MFACode, in.BackupCode); err != nil {
		h.d.Auth.WriteStepUpError(w, r, err)
		return
	}
	key := legalKeyHash(pr.HospitalID, pr.UserID, keys[0])
	var subjects []legalSubject
	var export, clearance uuid.UUID
	err = analysis.PersonTx(ctx, h.d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '`+disclose.StatementTimeout.String()+`'`); err != nil {
			return err
		}
		if err := stillAdmin(ctx, tx, pr); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT ref, level, start, ref_enc, demo, clearance_id, export_id, replays
			FROM mhai_legal_subjects($1, $2)`, ref, key)
		if err != nil {
			return legalErr(err)
		}
		replays := 0
		for rows.Next() {
			var s legalSubject
			if err := rows.Scan(&s.ref, &s.level, &s.start, &s.enc, &s.demo, &clearance, &export, &replays); err != nil {
				rows.Close()
				return legalErr(err)
			}
			subjects = append(subjects, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return legalErr(err)
		}
		if len(subjects) == 0 || len(subjects) > disclose.DrillMax {
			return errLegalBody
		}
		if h.afterOpen != nil {
			if err := h.afterOpen(); err != nil {
				return err
			}
		}
		// Last: the chain lock is held until commit. No entry, no names.
		return audit.Write(ctx, tx, audit.Event{ActorID: pr.UserID.String(), HospitalID: pr.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Action: "analysis.legal_resolved", Entity: "legal_reference", EntityID: ref.String(),
			Payload: map[string]any{"export_id": export.String(), "clearance_id": clearance.String(), "rows": len(subjects),
				"replays": replays}})
	})
	switch {
	case err == nil:
	case errors.Is(err, errStepUp):
		httpx.Error(w, r, "mfa_required")
		return
	case errors.Is(err, errRole):
		httpx.Error(w, r, "forbidden")
		return
	case errors.Is(err, errAlreadyUsed):
		h.legalRefused(r.Context(), pr, ref, "already_used")
		httpx.Error(w, r, "already_used")
		return
	case errors.Is(err, errLegalGone):
		h.legalRefused(r.Context(), pr, ref, "unavailable")
		httpx.Error(w, r, "unavailable")
		return
	case errors.Is(err, analysis.ErrNamedOff), errors.Is(err, analysis.ErrNotCleared), errors.Is(err, analysis.ErrClearanceEnded),
		errors.Is(err, analysis.ErrNotAllowed):
		h.legalRefused(r.Context(), pr, ref, coverReason(err))
		h.namedFail(w, r, err)
		return
	case errors.Is(err, errLegalBody):
		h.fail(w, r, analysis.ErrFailed)
		return
	default:
		h.namedFail(w, r, err)
		return
	}

	// Committed and audited: now, and only now, the names.
	out, err := h.legalIdentify(ctx, pr, ref, clearance, subjects)
	if err == nil {
		err = h.legalStillCovered(ctx, pr, clearance)
	}
	if err != nil {
		h.legalFailed(r.Context(), pr, ref, export)
		httpx.Error(w, r, "unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, struct {
		LegalReferenceID uuid.UUID  `json:"legal_reference_id"`
		Rows             []legalRow `json:"rows"`
	}{ref, out})
}

// legalAdmit reserves one of the person's resolutions this minute, in its
// own committed transaction (one upsert on the person's counter: parallel
// requests queue on its row).
func (h *handlers) legalAdmit(ctx context.Context, pr authz.Principal) (bool, error) {
	ok := false
	err := analysis.PersonTx(ctx, h.d.Pool, func(tx pgx.Tx) error {
		var n int
		err := tx.QueryRow(ctx, `
			INSERT INTO idempotency_keys AS k (scope, key, hospital_id, request_hash, status, expires_at)
			VALUES ('analysis.legal.rate', $1, $2, $1, 1, now() + interval '1 minute')
			ON CONFLICT (scope, key) DO UPDATE
			SET status = CASE WHEN k.expires_at <= now() THEN 1 ELSE k.status + 1 END,
			    expires_at = CASE WHEN k.expires_at <= now() THEN now() + interval '1 minute' ELSE k.expires_at END
			WHERE k.expires_at <= now() OR k.status < $3
			RETURNING k.status`, "legal:"+pr.UserID.String(), pr.HospitalID, LegalPerMinute).Scan(&n)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		ok = true
		return nil
	})
	return ok, err
}

// legalIdentify resolves exactly the export's distinct subjects, each
// reference opened for this call only, and checks the reply (T20).
func (h *handlers) legalIdentify(ctx context.Context, pr authz.Principal, ref, clearance uuid.UUID,
	subjects []legalSubject) ([]legalRow, error) {
	refs := make([]string, len(subjects))
	var asked []string
	demo := true
	for i, s := range subjects {
		if len(s.enc) == 0 {
			return nil, analysis.ErrIdentityUnavailable
		}
		x, err := h.d.Keys.OpenRef(pr.HospitalID, s.enc)
		if err != nil || x == "" {
			return nil, analysis.ErrIdentityUnavailable
		}
		refs[i] = x
		if !slices.Contains(asked, x) {
			asked = append(asked, x)
		}
		demo = demo && s.demo
	}
	ictx, cancel := context.WithTimeout(ctx, identifyTimeout)
	defer cancel()
	ids, err := h.resolver().Resolve(ictx, analysis.IdentityRequest{Hospital: pr.HospitalID, User: pr.UserID, View: ref,
		Clearance: clearance, Demo: demo, Refs: slices.Clone(asked)})
	if err == nil {
		err = analysis.CheckIdentities(asked, ids)
	}
	if err != nil {
		return nil, err
	}
	out := make([]legalRow, len(subjects))
	for i, s := range subjects {
		id := ids[refs[i]]
		out[i] = legalRow{ExportRowRef: s.ref, Period: legalPeriod{Level: s.level, Start: s.start.Format(time.DateOnly)}, Name: id.Name()}
		if m := id.MRN(); m != "" {
			out[i].MRN = &m
		}
	}
	return out, nil
}

// legalStillCovered re-reads the person's role, two-step and cover after
// the identity call: still named, by the same letter.
func (h *handlers) legalStillCovered(ctx context.Context, pr authz.Principal, clearance uuid.UUID) error {
	var c analysis.Cover
	err := analysis.PersonTx(ctx, h.d.Pool, func(tx pgx.Tx) error {
		if err := stillAdmin(ctx, tx, pr); err != nil {
			return err
		}
		var err error
		c, err = analysis.NamedCover(ctx, tx, pr.UserID)
		return err
	})
	if err != nil {
		return err
	}
	if !c.Named || c.Clearance == nil || *c.Clearance != clearance {
		return analysis.ErrNotCleared
	}
	return nil
}

// legalFailed records, in its own transaction, that a committed
// resolution sent no names (flagged by kind; departure 76). Logged with
// ids and a fixed reason only.
func (h *handlers) legalFailed(parent context.Context, pr authz.Principal, ref, export uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 2*disclose.StatementTimeout)
	defer cancel()
	err := analysis.PersonTx(authz.WithPrincipal(ctx, pr), h.d.Pool, func(tx pgx.Tx) error {
		return audit.Write(ctx, tx, audit.Event{ActorID: pr.UserID.String(), HospitalID: pr.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Action: "analysis.legal_resolve_failed", Entity: "legal_reference",
			EntityID: ref.String(), Payload: map[string]any{"export_id": export.String()}})
	})
	h.log().Error("analysis: legal resolution sent no names", "request_id", httpx.RequestIDFrom(ctx),
		"hospital_id", pr.HospitalID.String(), "reason", "unavailable", "recorded", err == nil)
}

// coverReason is a cover refusal's code.
func coverReason(err error) string {
	switch {
	case errors.Is(err, analysis.ErrNamedOff):
		return "named_access_off"
	case errors.Is(err, analysis.ErrClearanceEnded):
		return "clearance_ended"
	case errors.Is(err, analysis.ErrNotAllowed):
		return "forbidden"
	}
	return "not_cleared"
}

// stillAdmin is stillAllowed for the legal path's own rule: the account
// active at this hospital, two-step on, and still a hospital admin
// (security review: an analyst is not enough).
func stillAdmin(ctx context.Context, tx pgx.Tx, pr authz.Principal) error {
	if err := stillAllowed(ctx, tx, pr); err != nil {
		return err
	}
	var admin bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_roles WHERE user_id = $1 AND role = $2)`,
		pr.UserID, authz.RoleHospitalAdmin).Scan(&admin); err != nil {
		return err
	}
	if !admin || !slices.Contains(pr.Roles, authz.RoleHospitalAdmin) {
		return errRole
	}
	return nil
}

// legalRefused records, in its own transaction, a refused resolution of a
// reference the person is named on (flagged by kind; the code only).
func (h *handlers) legalRefused(parent context.Context, pr authz.Principal, ref uuid.UUID, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 2*disclose.StatementTimeout)
	defer cancel()
	err := analysis.PersonTx(authz.WithPrincipal(ctx, pr), h.d.Pool, func(tx pgx.Tx) error {
		return audit.Write(ctx, tx, audit.Event{ActorID: pr.UserID.String(), HospitalID: pr.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Action: "analysis.legal_resolve_refused", Entity: "legal_reference",
			EntityID: ref.String(), Payload: map[string]any{"reason": reason}})
	})
	if err != nil {
		h.log().Error("analysis: legal refusal not recorded", "request_id", httpx.RequestIDFrom(ctx),
			"hospital_id", pr.HospitalID.String(), "reason", reason)
	}
}
