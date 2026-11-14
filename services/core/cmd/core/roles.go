package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
)

const rolesUsage = `usage:
  core users roles <email> --add analyst --by <admin email>
  core users roles <email> --remove analyst --by <admin email>

Grants or removes the analyst role (Patient Analysis, design §6) through
the same database function and rules as the app: --by must be an active
hospital admin of the person's hospital, and not the person; a grant also
needs the person to be an active clinician with two-step on, the
hospital's analysis switch on, and fewer than 10 analysts. Each change is
audited (analyst.granted or analyst.removed, by the --by admin, via cli)
in the same transaction. Connects with DATABASE_URL as the provisioning
login (mhai_provision). Prints the person's id only.`

var (
	errRolesArgs  = fmt.Errorf("%s", rolesUsage)
	errRolesWrite = errors.New("could not change the analyst role; nothing was changed")
)

// usersRoles is "core users roles <email> --add|--remove analyst --by
// <admin email>". notify is told of each grant after commit. Errors and
// output never hold an email.
func usersRoles(ctx context.Context, args []string, url string, out io.Writer, notify analysis.Notifier) error {
	if len(args) != 5 || strings.HasPrefix(args[0], "-") {
		return errRolesArgs
	}
	person := account.NormaliseEmail(args[0])
	var add, remove bool
	by := ""
	for rest := args[1:]; len(rest) > 0; rest = rest[2:] {
		switch {
		case rest[0] == "--add" && rest[1] == "analyst" && !add && !remove:
			add = true
		case rest[0] == "--remove" && rest[1] == "analyst" && !add && !remove:
			remove = true
		case rest[0] == "--by" && by == "" && !strings.HasPrefix(rest[1], "-"):
			by = account.NormaliseEmail(rest[1])
		default:
			return errRolesArgs
		}
	}
	if (!add && !remove) || by == "" || !strings.Contains(person, "@") || !strings.Contains(by, "@") {
		return errRolesArgs
	}
	pool, err := openOwner(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	var id, hospital uuid.UUID
	var changed bool
	err = provisionTx(ctx, pool, func(tx pgx.Tx) error {
		var admin uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT person, hospital_id, by_user FROM mhai_analyst_cli_users($1, $2)`, person, by).
			Scan(&id, &hospital, &admin); err != nil {
			return analysis.AnalystRefusal(err)
		}
		// The grantor the rules and the audit policy read, for this
		// transaction only, once the lookup has checked it.
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.operator_id', $1, true)`, admin.String()); err != nil {
			return errRolesWrite
		}
		var err error
		if add {
			changed, err = analysis.GrantAnalyst(ctx, tx, id, "")
		} else {
			changed, err = analysis.RemoveAnalyst(ctx, tx, id, "")
		}
		return err
	})
	if err != nil {
		for _, known := range []error{analysis.ErrAnalystForbidden, analysis.ErrAnalystNotFound, analysis.ErrAnalystOff,
			analysis.ErrAnalystSelf, analysis.ErrAnalystNotEligible, analysis.ErrAnalystCap, account.ErrNotProvisioner} {
			if errors.Is(err, known) {
				return err
			}
		}
		return errRolesWrite
	}
	verb := "added"
	if remove {
		verb = "removed"
	}
	if !changed {
		verb = "unchanged"
	}
	if add && changed && notify != nil {
		notify(ctx, hospital, id)
	}
	fmt.Fprintf(out, "roles %s analyst %s\n", id, verb)
	return nil
}
