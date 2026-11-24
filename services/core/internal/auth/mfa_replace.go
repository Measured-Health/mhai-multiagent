package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// PendingSecretTTL is how long a new authenticator app has to be confirmed
// after POST /auth/mfa/replace.
const PendingSecretTTL = 15 * time.Minute

type replaceResponse struct {
	OTPAuthURI string `json:"otpauth_uri"`
}

// pendingExpired reports whether a pending secret made at created is past
// PendingSecretTTL at now.
func pendingExpired(created, now time.Time) bool {
	return now.Sub(created) > PendingSecretTTL
}

// fullSession reports whether the request's session was proven by a
// second factor. The principal middleware already requires it of an
// account with two-step on; this is checked again in the handler because
// the middleware's account state may be up to 30 s old.
func fullSession(r *http.Request) bool {
	p, ok := authz.FromContext(r.Context())
	return ok && p.MFA
}

// stepUpMethod is the replace_started audit word for the step-up given.
func stepUpMethod(mfaCode string) string {
	if mfaCode != "" {
		return string(kindOTP)
	}
	return string(kindBackup)
}

// replaceStart: POST /auth/mfa/replace {mfa_code | backup_code} →
// {otpauth_uri}. Moves two-step to a new authenticator app, step one.
//
//   - An account without two-step on gets 409 conflict (nothing to move),
//     before any code is checked. A session not proven by a second factor
//     gets 403 mfa_required (a session limited to enrolment never reaches
//     this route: it isn't MFAEnrol).
//   - The step-up code is checked (and spent) in the same transaction as
//     the write, as POST /auth/mfa/recovery-codes does, with the same
//     refusals and audit.
//   - A new secret, sealed as enroll seals one, is kept as the pending
//     secret, replacing any earlier one. The current secret keeps working
//     until POST /auth/mfa/replace/confirm.
func (s *Service) replaceStart(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	var in recoveryCodesRequest
	if !httpx.Decode(w, r, &in) {
		return
	}
	ctx := r.Context()
	uid, err := uuid.Parse(c.Sub)
	if err != nil {
		httpx.Error(w, r, "unauthenticated")
		return
	}
	keys, err := s.mfaKeys()
	if err != nil {
		s.fail(w, r, "not_ready", "auth: mfa key not configured", "user_id", c.Sub)
		return
	}
	secret := newSecret()
	sealed, err := keys.seal(uid.String(), secret)
	if err != nil {
		s.fail(w, r, "internal_error", "auth: seal mfa secret failed", "user_id", c.Sub)
		return
	}
	var (
		email                    string
		found, conflict, partial bool
		stepErr                  error
	)
	err = s.withAuth(ctx, func(tx pgx.Tx) error {
		acct, err := accountByID(ctx, tx, uid)
		if errors.Is(err, errNoAccount) {
			return nil
		}
		if err != nil {
			return err
		}
		if found = acct.usable(); !found {
			return nil
		}
		email = acct.Email
		row, err := loadMFA(ctx, tx, uid)
		if err != nil {
			return err
		}
		if conflict = row == nil || !row.enabled; conflict {
			return nil
		}
		if partial = !fullSession(r); partial {
			return nil
		}
		if stepErr = s.StepUp(ctx, tx, c.Sub, in.MFACode, in.BackupCode); stepErr != nil {
			return errStepUpRefused
		}
		tag, err := tx.Exec(ctx, `UPDATE user_mfa SET pending_secret_enc = $2, pending_created_at = $3
			WHERE user_id = $1 AND enabled`, uid, sealed, s.now())
		if err != nil || tag.RowsAffected() != 1 {
			return errors.New("store pending mfa secret failed")
		}
		return writeAudit(ctx, tx, r, "auth.mfa.replace_started", c.Sub, c.Hid, "user", c.Sub,
			map[string]any{"method": stepUpMethod(in.MFACode)})
	})
	if errors.Is(err, errStepUpRefused) {
		switch {
		case errors.Is(stepErr, ErrMFAInvalid):
			s.auditOnly(w, r, "auth.mfa.failed", c.Sub, c.Hid, map[string]any{"method": "step_up"})
		case errors.Is(stepErr, ErrMFALocked):
			s.auditOnly(w, r, "auth.mfa.locked", c.Sub, c.Hid, map[string]any{"reason": "account_attempts"})
		}
		s.WriteStepUpError(w, r, stepErr)
		return
	}
	if err != nil {
		s.fail(w, r, "internal_error", "auth: mfa replace failed", "user_id", c.Sub)
		return
	}
	switch {
	case !found:
		httpx.Error(w, r, "unauthenticated")
	case conflict:
		httpx.Error(w, r, "conflict")
	case partial:
		httpx.Error(w, r, "mfa_required")
	default:
		httpx.JSON(w, http.StatusOK, replaceResponse{OTPAuthURI: otpauthURI(email, secret)})
	}
}

// errReplaceGone rolls a confirm back when its pending secret was taken by
// a concurrent confirm or replaced by a new start.
var errReplaceGone = errors.New("auth: pending mfa secret changed")

// replaceConfirm: POST /auth/mfa/replace/confirm {code} → 204. Moves
// two-step to a new authenticator app, step two.
//
//   - No pending secret, or one older than PendingSecretTTL (which is then
//     cleared), is 409 conflict, as is an account without two-step on.
//   - The code is counted against the account's lockout before it is
//     checked (429 mfa_locked), like every code check. It must be a
//     current code of the pending secret; a wrong one is 403 mfa_invalid.
//   - On success, in one transaction: the pending secret becomes the
//     secret, its step is recorded as used (so the same code can't sign in
//     or confirm again), the pending secret is cleared, and the account's
//     trusted devices are removed (they stood in for the old app). Backup
//     codes and sessions are unchanged. Only the first of concurrent
//     confirms wins: the update is conditional on the pending secret read.
func (s *Service) replaceConfirm(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	var in verifyRequest
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Code == "" || len(in.Code) > maxCodeLen {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	rid := httpx.RequestIDFrom(ctx)
	uid, err := uuid.Parse(c.Sub)
	if err != nil {
		httpx.Error(w, r, "unauthenticated")
		return
	}
	keys, err := s.mfaKeys()
	if err != nil {
		s.fail(w, r, "not_ready", "auth: mfa key not configured", "user_id", c.Sub)
		return
	}
	// Is there a live pending secret? An expired one is cleared here.
	var conflict bool
	if err := s.withAuth(ctx, func(tx pgx.Tx) error {
		p, err := loadPending(ctx, tx, uid)
		if err != nil {
			return err
		}
		if conflict = p == nil; conflict {
			return nil
		}
		if conflict = pendingExpired(p.created, s.now()); conflict {
			if _, err := tx.Exec(ctx, `UPDATE user_mfa SET pending_secret_enc = NULL, pending_created_at = NULL
				WHERE user_id = $1 AND pending_secret_enc = $2`, uid, p.sealed); err != nil {
				return errors.New("clear expired pending mfa secret failed")
			}
		}
		return nil
	}); err != nil {
		s.fail(w, r, "internal_error", "auth: mfa replace lookup failed", "user_id", c.Sub)
		return
	}
	if conflict {
		httpx.Error(w, r, "conflict")
		return
	}
	if !fullSession(r) {
		httpx.Error(w, r, "mfa_required")
		return
	}
	locked, err := s.reserveMFA(ctx, c.Sub)
	if err != nil {
		s.fail(w, r, "not_ready", "auth: mfa attempts unavailable", "user_id", c.Sub)
		return
	}
	if locked {
		s.auditOnly(w, r, "auth.mfa.locked", c.Sub, c.Hid, map[string]any{"reason": "account_attempts"})
		httpx.Error(w, r, "mfa_locked")
		return
	}
	var ok bool
	err = s.withAuth(ctx, func(tx pgx.Tx) error {
		p, err := loadPending(ctx, tx, uid)
		if err != nil {
			return err
		}
		if p == nil || pendingExpired(p.created, s.now()) {
			return errReplaceGone
		}
		secret, err := keys.open(uid.String(), p.sealed)
		if err != nil {
			return errBadSecret
		}
		step, match := matchTOTP(secret, in.Code, totpStep(s.now()), nil)
		if ok = match; !ok {
			return nil
		}
		tag, err := tx.Exec(ctx, `UPDATE user_mfa
			SET secret_enc = pending_secret_enc, last_used_step = $3, pending_secret_enc = NULL, pending_created_at = NULL
			WHERE user_id = $1 AND enabled AND pending_secret_enc = $2`, uid, p.sealed, step)
		if err != nil {
			return errors.New("replace mfa secret failed")
		}
		if tag.RowsAffected() != 1 {
			return errReplaceGone
		}
		dev, err := tx.Exec(ctx, `DELETE FROM trusted_devices WHERE user_id = $1`, uid)
		if err != nil {
			return errors.New("remove trusted devices failed")
		}
		return writeAudit(ctx, tx, r, "auth.mfa.replaced", c.Sub, c.Hid, "user", c.Sub,
			map[string]any{"trusted_devices_revoked": dev.RowsAffected()})
	})
	if errors.Is(err, errReplaceGone) {
		httpx.Error(w, r, "conflict")
		return
	}
	if err != nil {
		s.fail(w, r, "internal_error", "auth: mfa replace confirm failed", "user_id", c.Sub)
		return
	}
	if !ok {
		s.auditOnly(w, r, "auth.mfa.failed", c.Sub, c.Hid, map[string]any{"method": "replace"})
		s.log().Info("auth: mfa replace code refused", "request_id", rid, "user_id", c.Sub)
		httpx.Error(w, r, "mfa_invalid")
		return
	}
	s.clearMFA(ctx, c.Sub)
	w.WriteHeader(http.StatusNoContent)
}

// pendingSecret is an account's pending (not yet confirmed) secret.
type pendingSecret struct {
	sealed  []byte
	created time.Time
}

// loadPending returns the pending secret of an account with two-step on,
// or nil when there is none.
func loadPending(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (*pendingSecret, error) {
	var (
		sealed  []byte
		created *time.Time
	)
	err := tx.QueryRow(ctx, `SELECT pending_secret_enc, pending_created_at FROM user_mfa WHERE user_id = $1 AND enabled`, userID).
		Scan(&sealed, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("auth: pending mfa lookup failed")
	}
	if len(sealed) == 0 || created == nil {
		return nil, nil
	}
	return &pendingSecret{sealed: bytes.Clone(sealed), created: *created}, nil
}
