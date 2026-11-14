package analysis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

// The analyst grant (analysis backend design §6; plan Tasks 15-16,
// migration 00030). The rules live in the database, in triggers on
// user_roles, for every writer: these helpers only call the one pair of
// definer functions and append the audit entries the database demands
// before it lets the transaction commit.

// The rules' refusals (P0001 messages of 00030). The route maps them to
// problem codes (Task 17: forbidden 403, not_found 404, analysis_off 409,
// analyst_cap 409, self_change 403, not_eligible 422); the CLI prints
// them.
var (
	ErrAnalystForbidden   = errors.New("analysis: the grantor is not an active hospital admin of the person's hospital")
	ErrAnalystNotFound    = errors.New("analysis: no such person in this hospital")
	ErrAnalystOff         = errors.New("analysis: the hospital's analysis switch is off")
	ErrAnalystSelf        = errors.New("analysis: nobody grants a role to themselves")
	ErrAnalystNotEligible = errors.New("analysis: the person needs the clinician role, an active account and two-step")
	ErrAnalystCap         = errors.New("analysis: the hospital already has 10 analysts")
)

var analystRefusals = map[string]error{
	"forbidden":    ErrAnalystForbidden,
	"not_found":    ErrAnalystNotFound,
	"analysis_off": ErrAnalystOff,
	"self_change":  ErrAnalystSelf,
	"not_eligible": ErrAnalystNotEligible,
	"analyst_cap":  ErrAnalystCap,
}

// AnalystRefusal maps a database error to one of the refusals above, or
// returns it unchanged.
func AnalystRefusal(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "P0001" {
		if e, ok := analystRefusals[pe.Message]; ok {
			return e
		}
	}
	return err
}

// GrantAnalyst adds the analyst role to person in tx, under the database's
// rules, and appends its analyst.granted entry. tx is a READ COMMITTED
// transaction as mhai_gateway (hospital and user set from the token: the
// grantor is the user) or as mhai_provision (mhai.operator_id set to the
// checked --by admin). changed is false when the person already holds it.
func GrantAnalyst(ctx context.Context, tx pgx.Tx, person uuid.UUID, requestID string) (changed bool, err error) {
	return analystChange(ctx, tx, `SELECT mhai_grant_analyst($1)`, person, requestID)
}

// RemoveAnalyst takes the analyst role from person in tx (the grantor an
// active hospital admin of the person's hospital) and appends its
// analyst.removed entry. changed is false when the person doesn't hold it.
func RemoveAnalyst(ctx context.Context, tx pgx.Tx, person uuid.UUID, requestID string) (changed bool, err error) {
	return analystChange(ctx, tx, `SELECT mhai_remove_analyst($1)`, person, requestID)
}

func analystChange(ctx context.Context, tx pgx.Tx, q string, person uuid.UUID, requestID string) (bool, error) {
	var changed bool
	if err := tx.QueryRow(ctx, q, person).Scan(&changed); err != nil {
		return false, AnalystRefusal(err)
	}
	if _, err := AuditAnalystChanges(ctx, tx, requestID); err != nil {
		return false, err
	}
	return changed, nil
}

// AuditAnalystChanges appends one entry per grant or removal this
// transaction made that has none yet: the ones GrantAnalyst and
// RemoveAnalyst make, and an analyst role that left with the clinician
// role (reason clinician_removed). A writer that removes 'clinician'
// calls it before commit, or the commit is refused. It returns how many
// it appended.
func AuditAnalystChanges(ctx context.Context, tx pgx.Tx, requestID string) (int, error) {
	rows, err := tx.Query(ctx, `SELECT change_id, hospital_id, user_id, change, reason, via, by_user FROM mhai_analyst_pending()`)
	if err != nil {
		return 0, fmt.Errorf("analysis: pending analyst changes: %w", err)
	}
	type pending struct {
		id, hospital, user  uuid.UUID
		change, reason, via string
		by                  *uuid.UUID
	}
	var ps []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.hospital, &p.user, &p.change, &p.reason, &p.via, &p.by); err != nil {
			rows.Close()
			return 0, fmt.Errorf("analysis: pending analyst changes: %w", err)
		}
		ps = append(ps, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("analysis: pending analyst changes: %w", err)
	}
	for _, p := range ps {
		action := "analyst.granted"
		payload := map[string]any{"change_id": p.id.String(), "via": p.via}
		if p.change == "removed" {
			action = "analyst.removed"
			payload["reason"] = p.reason
		}
		actor := ""
		if p.by != nil {
			actor = p.by.String()
		}
		if err := audit.Write(ctx, tx, audit.Event{ActorID: actor, HospitalID: p.hospital.String(), RequestID: requestID,
			Action: action, Entity: "user", EntityID: p.user.String(), Payload: payload}); err != nil {
			return 0, err
		}
	}
	return len(ps), nil
}

// Notifier tells a hospital's admins of an analyst grant (design §6.3).
type Notifier func(ctx context.Context, hospital, person uuid.UUID)

// NotifyAnalystGranted is the notice to every hospital admin of a grant.
// Until the notifications service exists it records the ids only (never a
// name or an email); callers call it once per grant, after commit.
func NotifyAnalystGranted(log *slog.Logger) Notifier {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return func(ctx context.Context, hospital, person uuid.UUID) {
		log.InfoContext(ctx, "analyst granted; notice pending the notifications service",
			"hospital_id", hospital.String(), "user_id", person.String())
	}
}
