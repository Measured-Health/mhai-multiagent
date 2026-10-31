package account

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
)

// NewHospital describes a hospital to create. It starts as setup_incomplete
// with the other hospital_settings columns at their defaults.
type NewHospital struct {
	Name, ShortCode, TimeZone                 string
	RequireMFAForClinicians, AllowTrustDevice bool
}

// NewUser describes an account to create. Password is checked against the
// password policy and stored only as an argon2id hash; the account must
// change it at first sign-in.
type NewUser struct {
	Email, Name, HospitalShortCode, Password string
	Roles                                    []string
}

// CreateHospital inserts the hospital, its settings row and an audit entry in
// one transaction. pool must connect as the owner (see package comment).
func CreateHospital(ctx context.Context, pool *pgxpool.Pool, h NewHospital) (uuid.UUID, error) {
	code, ok := NormaliseShortCode(h.ShortCode)
	if !ok {
		return uuid.Nil, ErrBadShortCode
	}
	name := strings.TrimSpace(h.Name)
	if name == "" {
		return uuid.Nil, ErrBadName
	}
	if err := validTimeZone(h.TimeZone); err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		id = uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO hospitals (id, name, short_code, time_zone) VALUES ($1, $2, $3, $4)`,
			id, name, code, h.TimeZone); err != nil {
			if isUnique(err) {
				return ErrShortCodeTaken
			}
			return errors.New("insert hospital failed")
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO hospital_settings (hospital_id, require_mfa_for_clinicians, allow_trust_device) VALUES ($1, $2, $3)`,
			id, h.RequireMFAForClinicians, h.AllowTrustDevice); err != nil {
			return errors.New("insert hospital settings failed")
		}
		// Ids only: no names, codes or emails in the audit trail.
		return audit.Write(ctx, tx, audit.Event{HospitalID: id.String(), Action: "hospital.created",
			Entity: "hospital", EntityID: id.String(), Payload: map[string]any{"via": "cli"}})
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// CreateUser validates, hashes the password and inserts the account, its
// roles and an audit entry in one transaction. The email is normalised; a
// case-only duplicate is ErrEmailTaken. Errors never contain the email or
// password.
func CreateUser(ctx context.Context, pool *pgxpool.Pool, u NewUser) (uuid.UUID, error) {
	if err := ValidateNewUser(u); err != nil {
		return uuid.Nil, err
	}
	email, name := NormaliseEmail(u.Email), strings.TrimSpace(u.Name)
	code, _ := NormaliseShortCode(u.HospitalShortCode) // "" when none
	if err := password.Check(u.Password, email); err != nil {
		return uuid.Nil, err
	}
	hash, err := password.Hash(u.Password)
	if err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err = inTx(ctx, pool, func(tx pgx.Tx) error {
		var hospital *uuid.UUID
		if code != "" {
			var hid uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM hospitals WHERE short_code = $1`, code).Scan(&hid); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrHospitalNotFound
				}
				return errors.New("hospital lookup failed")
			}
			hospital = &hid
		}
		id = uuid.New() // supplied here: mhai_provision cannot read users back
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, hospital_id, email, password_hash, display_name, must_change_password)
			 VALUES ($1, $2, $3, $4, $5, true)`,
			id, hospital, email, hash, name); err != nil {
			if isUnique(err) {
				return ErrEmailTaken
			}
			return errors.New("insert user failed")
		}
		for _, r := range u.Roles {
			if _, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, id, r); err != nil {
				var pe *pgconn.PgError
				if errors.As(err, &pe) && pe.Code == "23514" { // the pairing trigger, if the Go check was bypassed
					return ErrRoleHospital
				}
				return errors.New("insert role failed")
			}
		}
		ev := audit.Event{Action: "user.created", Entity: "user", EntityID: id.String(), Payload: map[string]any{"via": "cli"}}
		if hospital != nil {
			ev.HospitalID = hospital.String()
		}
		return audit.Write(ctx, tx, ev)
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// inTx runs fn in a READ COMMITTED transaction (audit.Write needs it).
func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return errors.New("begin failed")
	}
	// The provisioning login is a plain member of mhai_provision; every
	// statement below runs with that role's narrow grants and RLS policies.
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_provision`); err != nil {
		_ = tx.Rollback(ctx)
		return ErrNotProvisioner
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.New("commit failed")
	}
	return nil
}

// isUnique reports a unique violation without exposing the row detail.
func isUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// ErrRotationAudit: the key rotation couldn't be audited, so it wasn't made.
var ErrRotationAudit = errors.New("could not audit the key rotation; nothing was changed")

// AuditKeyRotation appends intelligence.key_rotated (spec §7.5, §10.2) for
// request-signing kid kid as the provisioning login (mhai_provision, no
// actor, no hospital), and runs prepare inside the same transaction, after
// the row is written. prepare only stages the file change (temp files
// beside the real ones); the caller moves them into place once this
// returns nil, that is once the row has committed, and removes them
// otherwise. So the files never change unless the rotation is on the chain.
// mode is planned or emergency; change is added or removed. Ids and codes
// only.
func AuditKeyRotation(ctx context.Context, pool *pgxpool.Pool, kid, mode, change string, prepare func() error) error {
	if (mode != "planned" && mode != "emergency") || (change != "added" && change != "removed") || kid == "" || prepare == nil {
		return errors.New("invalid key rotation")
	}
	return inTx(ctx, pool, func(tx pgx.Tx) error {
		if err := audit.Write(ctx, tx, audit.Event{Action: "intelligence.key_rotated", Entity: "request_key", EntityID: kid,
			Payload: map[string]any{"kid": kid, "rotation": mode, "change": change}}); err != nil {
			return ErrRotationAudit
		}
		return prepare()
	})
}
