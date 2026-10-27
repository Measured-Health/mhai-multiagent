package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// MaxMFAFailures is the number of wrong codes per account in the MFA
// bucket's 15 minutes before every code check gets mfa_locked. The counter
// is shared by the sign-in challenge, verify and step-up, so a fresh
// mfa_token never buys more guesses.
const MaxMFAFailures = 5

// Step-up errors. Write them with WriteStepUpError.
var (
	ErrMFAInvalid  = errors.New("auth: verification code not valid")      // 403 mfa_invalid
	ErrMFALocked   = errors.New("auth: too many verification attempts")   // 429 mfa_locked
	ErrMFARequired = errors.New("auth: two-step verification not set up") // 403 mfa_required
)

// mfaFailKey is the per-account failure counter in the MFA bucket. The key
// holds the user id (a server-made uuid), never an email.
func mfaFailKey(userID string) string { return "mfafail." + userID }

// reserveIn counts one attempt against key in kv by compare-and-set before
// the caller checks anything, so a concurrent burst gets at most max checks.
// locked is true when the counter was already at max; then nothing is
// written (the bucket's lifetime runs from the last write). An error means
// the caller must fail closed.
func reserveIn(ctx context.Context, kv jetstream.KeyValue, key string, max int) (locked bool, err error) {
	for i := 0; i < 16; i++ {
		e, err := kv.Get(ctx, key)
		if bus.IsNotFound(err) {
			_, err := kv.Create(ctx, key, []byte("1"))
			if err == nil {
				return false, nil
			}
			if !errors.Is(err, jetstream.ErrKeyExists) && !isRevisionConflict(err) {
				return false, err
			}
			continue
		}
		if err != nil {
			return false, err
		}
		n, err := strconv.Atoi(string(e.Value()))
		if err != nil || n >= max {
			return true, nil // an unreadable counter counts as locked
		}
		_, err = kv.Update(ctx, key, []byte(strconv.Itoa(n+1)), e.Revision())
		if err == nil {
			return false, nil
		}
		if !isRevisionConflict(err) {
			return false, err
		}
	}
	return false, errors.New("auth: attempt counter contention")
}

// refundIn takes one back from the counter at key, by compare-and-set; at
// one it deletes the key. A missing key is fine.
func refundIn(ctx context.Context, kv jetstream.KeyValue, key string) error {
	for i := 0; i < 16; i++ {
		e, err := kv.Get(ctx, key)
		if bus.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(string(e.Value()))
		if err != nil {
			return err
		}
		if n <= 1 {
			err = kv.Delete(ctx, key, jetstream.LastRevision(e.Revision()))
		} else {
			_, err = kv.Update(ctx, key, []byte(strconv.Itoa(n-1)), e.Revision())
		}
		if err == nil {
			return nil
		}
		if !isRevisionConflict(err) {
			return err
		}
	}
	return errors.New("auth: attempt counter contention")
}

// reserveMFA counts a code check for the account.
func (s *Service) reserveMFA(ctx context.Context, userID string) (bool, error) {
	return reserveIn(ctx, s.Buckets.MFAAttempts, mfaFailKey(userID), MaxMFAFailures)
}

// clearMFA resets the account's counter after a correct code. A failure is
// logged only: the counter then expires on its own.
func (s *Service) clearMFA(ctx context.Context, userID string) {
	if err := s.Buckets.MFAAttempts.Delete(ctx, mfaFailKey(userID)); err != nil && !bus.IsNotFound(err) {
		s.log().Error("auth: mfa counter reset failed", "user_id", userID)
	}
}

func (s *Service) mfaKeys() (*mfaKeys, error) { return newMFAKeys(s.MFAKey) }

// mfaRow is an account's user_mfa row.
type mfaRow struct {
	secretEnc []byte
	enabled   bool
	lastStep  *int64
}

func loadMFA(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (*mfaRow, error) {
	m := &mfaRow{}
	err := tx.QueryRow(ctx, `SELECT secret_enc, enabled, last_used_step FROM user_mfa WHERE user_id = $1`, userID).
		Scan(&m.secretEnc, &m.enabled, &m.lastStep)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("auth: mfa lookup failed")
	}
	return m, nil
}

// codeKind is how a correct code was proven: the amr value it adds.
type codeKind string

const (
	kindOTP    codeKind = "otp"
	kindBackup codeKind = "backup"
)

// errBadSecret means the stored secret can't be opened (wrong key or
// corrupt row): a server fault, not a wrong code.
var errBadSecret = errors.New("auth: stored mfa secret unusable")

// checkCode checks totp (a six-digit code) or backup against the account's
// row, and burns it on success, in tx: a TOTP code moves last_used_step
// forward by a conditional update (so two concurrent uses of one code can't
// both win), and a backup code is removed from the array by a conditional
// update. ok=false is a wrong or reused code.
func (s *Service) checkCode(ctx context.Context, tx pgx.Tx, keys *mfaKeys, userID uuid.UUID, row *mfaRow,
	totp, backup string) (kind codeKind, ok bool, err error) {
	switch {
	case totp != "":
		secret, err := keys.open(userID.String(), row.secretEnc)
		if err != nil {
			return "", false, errBadSecret
		}
		step, match := matchTOTP(secret, totp, totpStep(s.now()), row.lastStep)
		if !match {
			return kindOTP, false, nil
		}
		tag, err := tx.Exec(ctx, `UPDATE user_mfa SET last_used_step = $2
			WHERE user_id = $1 AND (last_used_step IS NULL OR last_used_step < $2)`, userID, step)
		if err != nil {
			return "", false, errors.New("auth: mfa step update failed")
		}
		return kindOTP, tag.RowsAffected() == 1, nil
	case backup != "":
		if len(backup) > 64 {
			return kindBackup, false, nil
		}
		tag, err := tx.Exec(ctx, `UPDATE user_mfa SET backup_code_hashes = array_remove(backup_code_hashes, $2)
			WHERE user_id = $1 AND enabled AND $2 = ANY (backup_code_hashes)`, userID, keys.hashBackup(backup))
		if err != nil {
			return "", false, errors.New("auth: backup code update failed")
		}
		return kindBackup, tag.RowsAffected() == 1, nil
	}
	return "", false, nil
}

// StepUp checks a step-up code for a sensitive write: exactly one of
// mfaCode (a TOTP code) or backupCode. It returns nil on a correct code
// (which is then spent), ErrMFAInvalid for a wrong, reused or missing code,
// ErrMFALocked after MaxMFAFailures wrong codes in 15 minutes, and
// ErrMFARequired when the account has no two-step set up. Any other error
// is a server fault (the caller fails closed with 503 or 500).
//
// tx must be able to read and update user_mfa (an mhai_auth transaction,
// db.WithAuth). With tx nil, StepUp runs in its own mhai_auth transaction,
// which later plans' handlers (running as mhai_gateway) should use. A code
// spent inside the caller's tx is unspent again if that tx rolls back.
func (s *Service) StepUp(ctx context.Context, tx pgx.Tx, userID, mfaCode, backupCode string) error {
	if tx == nil {
		return s.withAuth(ctx, func(tx pgx.Tx) error { return s.StepUp(ctx, tx, userID, mfaCode, backupCode) })
	}
	uid, err := uuid.Parse(userID)
	if err != nil {
		return ErrMFAInvalid
	}
	if (mfaCode == "") == (backupCode == "") || len(mfaCode) > 64 || len(backupCode) > 64 {
		return ErrMFAInvalid
	}
	keys, err := s.mfaKeys()
	if err != nil {
		return err
	}
	row, err := loadMFA(ctx, tx, uid)
	if err != nil {
		return err
	}
	if row == nil || !row.enabled {
		return ErrMFARequired
	}
	locked, err := s.reserveMFA(ctx, userID)
	if err != nil {
		return errors.New("auth: mfa attempts unavailable")
	}
	if locked {
		return ErrMFALocked
	}
	_, ok, err := s.checkCode(ctx, tx, keys, uid, row, mfaCode, backupCode)
	if err != nil {
		return err
	}
	if !ok {
		return ErrMFAInvalid
	}
	s.clearMFA(ctx, userID)
	return nil
}

// WriteStepUpError sends the reply for a StepUp error: 403 mfa_invalid,
// 429 mfa_locked, 403 mfa_required, or 503 not_ready for anything else.
func (s *Service) WriteStepUpError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrMFAInvalid):
		httpx.Error(w, r, "mfa_invalid")
	case errors.Is(err, ErrMFALocked):
		httpx.Error(w, r, "mfa_locked")
	case errors.Is(err, ErrMFARequired):
		httpx.Error(w, r, "mfa_required")
	default:
		s.fail(w, r, "not_ready", "auth: step-up check failed")
	}
}
