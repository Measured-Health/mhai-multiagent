package platform

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// POST /platform/audit/legal-references {export_id, reference, admins,
// mfa_code | backup_code} (design §8.9 step 1; plan Task 26, departure
// 71): a platform admin binds a named export they made, of at most 200
// rows, to one legal reference, naming 1-3 hospital admins of its
// hospital who may resolve it once. Step-up; 10 an hour per person;
// legal_reference.created (flagged by kind, sent bare) in the same
// transaction. Platform staff never see names: only a named admin
// resolves, in the hospital app. Off unless CORE_AUDIT_EXPORTS.

// LegalRefsPerHour is each person's limit.
const LegalRefsPerHour = 10

func registerLegal(m *authz.Mux, d Deps) {
	rule := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin}, Write: true, StepUp: true}
	authz.Route(m, http.MethodPost, "/platform/audit/legal-references", rule, http.HandlerFunc(d.legalCreate))
}

type legalIn struct {
	ExportID   string   `json:"export_id"`
	Reference  string   `json:"reference"`
	Admins     []string `json:"admins"`
	MFACode    string   `json:"mfa_code"`
	BackupCode string   `json:"backup_code"`
}

// LegalReferenceOut is the reply.
type LegalReferenceOut struct {
	ID        string   `json:"id"`
	ExportID  string   `json:"export_id"`
	Reference string   `json:"reference"`
	Admins    []string `json:"admins"`
	CreatedAt string   `json:"created_at"`
}

// legalRefusals are mhai_legal_reference_create's refusals and their codes.
var legalRefusals = map[string]string{
	"not_found":     "not_found",
	"forbidden":     "forbidden",
	"conflict":      "conflict",
	"too_many_rows": "too_many_rows",
	"invalid":       "validation_failed",
	"not_eligible":  "not_eligible",
}

type legalRefused struct{ code string }

func (legalRefused) Error() string { return "platform: legal reference refused" }

func (d Deps) legalCreate(w http.ResponseWriter, r *http.Request) {
	if !d.ExportsOn {
		httpx.Error(w, r, "not_found")
		return
	}
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var in legalIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	if !canonicalUUID(in.ExportID) || !exportReference.MatchString(in.Reference) || pseudonymIn.MatchString(in.Reference) ||
		len(in.Admins) < 1 || len(in.Admins) > 3 {
		httpx.Error(w, r, "validation_failed")
		return
	}
	admins := make([]uuid.UUID, len(in.Admins))
	for i, a := range in.Admins {
		if !canonicalUUID(a) {
			httpx.Error(w, r, "validation_failed")
			return
		}
		admins[i] = uuid.MustParse(a)
	}
	if d.Auth == nil {
		httpx.Error(w, r, "not_ready")
		return
	}
	if !d.rate(w, r, p, "platform.audit.legal_reference.v1", LegalRefsPerHour, time.Hour) ||
		!d.stepUp(w, r, in.MFACode, in.BackupCode) {
		return
	}
	ctx := r.Context()
	export := uuid.MustParse(in.ExportID)
	var out LegalReferenceOut
	var rows int
	err := d.tx(ctx, func(tx pgx.Tx) error {
		if err := stillHolds(ctx, tx, p, authz.RolePlatformAdmin); err != nil {
			return err
		}
		var id, hospital uuid.UUID
		var created time.Time
		err := tx.QueryRow(ctx, `SELECT id, hospital_id, export_rows, created_at FROM mhai_legal_reference_create($1, $2, $3, $4)`,
			export, in.Reference, admins, p.UserID).Scan(&id, &hospital, &rows, &created)
		if pe := pgError(err); pe != nil && pe.Code == "P0001" {
			if code, known := legalRefusals[pe.Message]; known {
				return legalRefused{code: code}
			}
		}
		if err != nil {
			return err
		}
		named := make([]any, len(admins))
		for i, a := range admins {
			named[i] = a.String()
		}
		if err := audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), RequestID: httpx.RequestIDFrom(ctx),
			Action: "legal_reference.created", Entity: "legal_reference", EntityID: id.String(),
			Payload: map[string]any{"export_id": export.String(), "hospital_id": hospital.String(), "admins": named}}); err != nil {
			return err
		}
		out = LegalReferenceOut{ID: id.String(), ExportID: export.String(), Reference: in.Reference, Admins: in.Admins,
			CreatedAt: created.UTC().Format(time.RFC3339Nano)}
		return nil
	})
	var refused legalRefused
	switch {
	case err == nil:
		httpx.JSON(w, http.StatusOK, out)
	case errors.As(err, &refused) && refused.code == "too_many_rows":
		// The count, read in a new transaction: the refusal ended this one.
		var n int
		if rerr := d.tx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT rows FROM audit_exports WHERE id = $1`, export).Scan(&n)
		}); rerr != nil {
			d.fail(w, r, p, "legal reference")
			return
		}
		httpx.ErrorRows(w, r, n)
	case errors.As(err, &refused):
		httpx.Error(w, r, refused.code)
	case errors.Is(err, errNotReader):
		httpx.Error(w, r, "forbidden")
	case pgError(err) != nil && pgError(err).Code == "23505":
		// The same reference bound at once by another export.
		httpx.Error(w, r, "conflict")
	default:
		d.fail(w, r, p, "legal reference")
	}
}
