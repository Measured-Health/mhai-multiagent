package account

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

var (
	ErrBadHospitalRef  = errors.New("give the hospital's id (a uuid) or its short code")
	ErrNoSuchHospital  = errors.New("no hospital with that id or short code")
	ErrNotActivatable  = errors.New("only a setup_incomplete hospital can be activated here")
	ErrActivationAudit = errors.New("could not audit the activation; nothing was changed")
)

// activatableFrom is the one status the CLI activates from (00010's
// provision_activate holds the database to the same). A deactivated
// hospital is reactivated through the platform app, with a reason and
// two-step (frontend §14.2); removal_pending, removal_scheduled and removed
// never go back to active.
const activatableFrom = "setup_incomplete"

// hospitalRef is how a hospital is named on the command line: its id or
// its short code (the slug), never its name.
type hospitalRef struct {
	id   uuid.UUID
	code string
}

func parseHospitalRef(s string) (hospitalRef, error) {
	s = strings.TrimSpace(s)
	if id, err := uuid.Parse(s); err == nil && len(s) == 36 {
		return hospitalRef{id: id}, nil
	}
	if code, ok := NormaliseShortCode(s); ok {
		return hospitalRef{code: code}, nil
	}
	return hospitalRef{}, ErrBadHospitalRef
}

// where is the WHERE clause for r, with $1 its value.
func (r hospitalRef) where() (string, any) {
	if r.code != "" {
		return "short_code = $1", r.code
	}
	return "id = $1", r.id
}

// ActivateHospital moves the hospital named by ref (its id or short code)
// from setup_incomplete to active and appends hospital.activated to the
// audit chain in the same transaction, as the provisioning login
// (mhai_provision, no actor: as hospital.created, the operator is whoever
// holds that login). An already-active hospital is a no-op (already true,
// nothing written). Any other status is ErrNotActivatable, naming it.
//
// The UPDATE goes first and the read only explains a miss, so two
// concurrent activations audit once: the second waits on the row lock,
// then matches nothing and reports already active.
func ActivateHospital(ctx context.Context, pool *pgxpool.Pool, ref string) (id uuid.UUID, already bool, err error) {
	r, err := parseHospitalRef(ref)
	if err != nil {
		return uuid.Nil, false, err
	}
	cond, arg := r.where()
	err = inTx(ctx, pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`UPDATE hospitals SET status = 'active' WHERE `+cond+` AND status = '`+activatableFrom+`' RETURNING id`,
			arg).Scan(&id)
		switch {
		case err == nil:
			if err := audit.Write(ctx, tx, audit.Event{HospitalID: id.String(), Action: "hospital.activated",
				Entity: "hospital", EntityID: id.String(),
				Payload: map[string]any{"via": "cli", "from": activatableFrom}}); err != nil {
				return ErrActivationAudit
			}
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return errors.New("activate hospital failed")
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT id, status FROM hospitals WHERE `+cond, arg).Scan(&id, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNoSuchHospital
			}
			return errors.New("hospital lookup failed")
		}
		if status == "active" {
			already = true
			return nil
		}
		// status is one of the CHECK constraint's values, not input.
		return fmt.Errorf("%w (it is %s)", ErrNotActivatable, status)
	})
	if err != nil {
		return uuid.Nil, false, err
	}
	return id, already, nil
}
