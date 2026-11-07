// Package dbx runs a transaction as one of the hospital-scoped database
// roles, with mhai.hospital_id set so row-level security shows only that
// hospital's rows (plan departure 6). Core (as mhai_gateway) and the emr
// service (as mhai_emr, mhai_emr_core) share it.
//
// The role is one of a closed set of constants, never input: it is put
// into SET LOCAL ROLE as text, so anything else is refused before any SQL.
// The settings are transaction-local, so a pooled connection carries
// neither the role nor the hospital into its next use.
package dbx

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Role is a hospital-scoped database role.
type Role string

// The roles WithHospital may run as.
const (
	Gateway Role = "mhai_gateway"  // core's requests
	EMR     Role = "mhai_emr"      // the emr service
	EMRCore Role = "mhai_emr_core" // core's reads of EMR findings
)

// Roles is the closed set.
var Roles = []Role{Gateway, EMR, EMRCore}

var (
	// ErrNoHospital: the nil hospital id.
	ErrNoHospital = errors.New("db: no hospital id")
	// ErrNoUser: the nil user id (WithHospitalUser).
	ErrNoUser = errors.New("db: no user id")
	// ErrRole: a role outside Roles.
	ErrRole = errors.New("db: not a hospital-scoped role")
)

// WithHospital runs fn in a transaction as role, with mhai.hospital_id set
// (transaction-local) to hospitalID. It commits if fn returns nil and rolls
// back otherwise, returning fn's error unchanged.
func WithHospital(ctx context.Context, pool *pgxpool.Pool, role Role, hospitalID uuid.UUID, fn func(pgx.Tx) error) error {
	if !slices.Contains(Roles, role) {
		return ErrRole
	}
	if hospitalID == uuid.Nil {
		return ErrNoHospital
	}
	return run(ctx, pool, role, hospitalID, nil, fn)
}

// WithHospitalUser is WithHospital plus mhai.user_id set (transaction-local)
// to userID, for the writes whose database guards need to know who is
// acting. The id comes from a verified token, never from a request body.
func WithHospitalUser(ctx context.Context, pool *pgxpool.Pool, role Role, hospitalID, userID uuid.UUID, fn func(pgx.Tx) error) error {
	if !slices.Contains(Roles, role) {
		return ErrRole
	}
	if hospitalID == uuid.Nil {
		return ErrNoHospital
	}
	if userID == uuid.Nil {
		return ErrNoUser
	}
	return run(ctx, pool, role, hospitalID, &userID, fn)
}

func run(ctx context.Context, pool *pgxpool.Pool, role Role, hospital uuid.UUID, user *uuid.UUID, fn func(pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	// role is one of the constants above, checked by the caller.
	if _, err = tx.Exec(ctx, "SET LOCAL ROLE "+string(role)); err != nil {
		return fmt.Errorf("db: set role: %w", err)
	}
	if _, err = tx.Exec(ctx, "SELECT set_config('mhai.hospital_id', $1, true)", hospital.String()); err != nil {
		return fmt.Errorf("db: set hospital: %w", err)
	}
	if user != nil {
		if _, err = tx.Exec(ctx, "SELECT set_config('mhai.user_id', $1, true)", user.String()); err != nil {
			return fmt.Errorf("db: set user: %w", err)
		}
	}
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}
