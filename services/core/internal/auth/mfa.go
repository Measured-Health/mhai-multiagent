package auth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// TrustedDeviceTTL is how long a trusted device stands in for the code.
const TrustedDeviceTTL = 30 * 24 * time.Hour

// otpIssuer is the issuer shown in authenticator apps.
const otpIssuer = "MHAI"

const maxCodeLen = 64

type challengeRequest struct {
	MFAToken    string `json:"mfa_token"`
	Code        string `json:"code"`
	TrustDevice bool   `json:"trust_device,omitempty"`
}

type recoveryCodesResponse struct {
	RecoveryCodes []string `json:"recovery_codes"`
}

type enrollResponse struct {
	OTPAuthURI    string   `json:"otpauth_uri"`
	RecoveryCodes []string `json:"recovery_codes"`
}

// challenge: POST /auth/mfa/challenge {mfa_token, code, trust_device?}.
//
// Each call first spends one of the token's 5 attempts (fail closed). For
// an account with two-step on, it then counts a code check against the
// account (5 wrong codes in 15 minutes lock it, across tokens, verify and
// step-up), checks a TOTP code (six digits) or a backup code, and burns it.
// On success the token is consumed and a session opens with mfa=true and
// amr [pwd, otp|backup]; with trust_device, and only when the hospital
// allows it and no role forbids it, a device token is issued too.
//
// An account that must use two-step but hasn't set it up gets, with no
// code, a session limited to enrolment (mfa=false, amr [pwd]): its token
// reaches only enrol, verify, password change and logout.
func (s *Service) challenge(w http.ResponseWriter, r *http.Request) {
	var in challengeRequest
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.MFAToken == "" || len(in.MFAToken) > 128 || len(in.Code) > maxCodeLen {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	rid := httpx.RequestIDFrom(ctx)
	store := s.MFATokens()
	rec, err := store.Attempt(ctx, in.MFAToken)
	if errors.Is(err, ErrMFAToken) {
		if spent, ok := store.Exhausted(ctx, in.MFAToken); ok {
			s.auditOnly(w, r, "auth.mfa.locked", spent.UserID, "", map[string]any{"reason": "token_attempts"})
			s.log().Info("auth: mfa token out of attempts", "request_id", rid, "user_id", spent.UserID)
			httpx.Error(w, r, "mfa_locked")
			return
		}
		httpx.Error(w, r, "mfa_invalid")
		return
	}
	if err != nil {
		s.fail(w, r, "not_ready", "auth: mfa token store unavailable")
		return
	}
	uid, err := uuid.Parse(rec.UserID)
	if err != nil {
		httpx.Error(w, r, "mfa_invalid")
		return
	}
	keys, err := s.mfaKeys()
	if err != nil {
		s.fail(w, r, "not_ready", "auth: mfa key not configured")
		return
	}
	var acct *acctRec
	if err := s.withAuth(ctx, func(tx pgx.Tx) error {
		a, err := accountByID(ctx, tx, uid)
		if err != nil && !errors.Is(err, errNoAccount) {
			return err
		}
		acct = a
		return nil
	}); err != nil {
		s.fail(w, r, "internal_error", "auth: mfa account lookup failed", "user_id", rec.UserID)
		return
	}
	if acct == nil || !acct.usable() {
		httpx.Error(w, r, "unauthenticated")
		return
	}
	// The token is good only on the audience it was issued on, for an
	// account still on that side, and only until the password changes.
	// Checked before anything is consumed and before either branch.
	platform := httpx.Audience(ctx) == httpx.AudiencePlatform
	if platform != rec.Platform || hasPlatformRole(acct.Roles) != rec.Platform ||
		!hmac.Equal([]byte(keys.pwFingerprint(acct.PasswordHash)), []byte(rec.PwGen)) {
		s.auditOnly(w, r, "auth.mfa.failed", rec.UserID, acct.hospitalID(), map[string]any{"method": "token_mismatch"})
		s.log().Info("auth: mfa token refused for this audience or password", "request_id", rid, "user_id", rec.UserID)
		httpx.Error(w, r, "mfa_invalid")
		return
	}

	if !acct.MFAEnabled {
		// Nothing to check: the password was checked at sign-in. The session
		// meets the policy only if the account doesn't need two-step;
		// otherwise RequireToken limits it to enrolment.
		if _, err := store.Consume(ctx, in.MFAToken); err != nil {
			httpx.Error(w, r, "mfa_invalid")
			return
		}
		s.startSession(w, r, acct, false, []string{"pwd"}, nil)
		return
	}

	locked, err := s.reserveMFA(ctx, rec.UserID)
	if err != nil {
		s.fail(w, r, "not_ready", "auth: mfa attempts unavailable", "user_id", rec.UserID)
		return
	}
	if locked {
		s.auditOnly(w, r, "auth.mfa.locked", rec.UserID, acct.hospitalID(), map[string]any{"reason": "account_attempts"})
		s.log().Info("auth: mfa locked", "request_id", rid, "user_id", rec.UserID)
		httpx.Error(w, r, "mfa_locked")
		return
	}
	totp, backup := "", in.Code
	if looksLikeTOTP(in.Code) {
		totp, backup = in.Code, ""
	}
	var (
		kind codeKind
		ok   bool
	)
	err = s.withAuth(ctx, func(tx pgx.Tx) error {
		row, err := loadMFA(ctx, tx, uid)
		if err != nil || row == nil || !row.enabled {
			return err
		}
		kind, ok, err = s.checkCode(ctx, tx, keys, uid, row, totp, backup)
		return err
	})
	if err != nil {
		s.fail(w, r, "internal_error", "auth: mfa check failed", "user_id", rec.UserID)
		return
	}
	if !ok {
		s.auditOnly(w, r, "auth.mfa.failed", rec.UserID, acct.hospitalID(), map[string]any{"method": methodWord(kind)})
		s.log().Info("auth: mfa code refused", "request_id", rid, "user_id", rec.UserID)
		httpx.Error(w, r, "mfa_invalid")
		return
	}
	if _, err := store.Consume(ctx, in.MFAToken); err != nil {
		httpx.Error(w, r, "mfa_invalid")
		return
	}
	s.clearMFA(ctx, rec.UserID)
	if kind == kindBackup && acct.RecoveryCodesLeft > 0 {
		acct.RecoveryCodesLeft--
	}

	trust := in.TrustDevice && rec.DeviceAllowed && deviceMayStandIn(acct.Roles, acct.policy())
	deviceToken, deviceHash := "", ""
	if trust {
		deviceToken, deviceHash = token.NewRefresh()
	}
	s.startSessionDevice(w, r, acct, true, []string{"pwd", string(kind)}, func(ctx context.Context, tx pgx.Tx) error {
		action, payload := "auth.mfa.verified", map[string]any{"method": "otp"}
		if kind == kindBackup {
			action, payload = "auth.mfa.backup_used", map[string]any{"left": acct.RecoveryCodesLeft}
		}
		if err := writeAudit(ctx, tx, r, action, acct.ID.String(), acct.hospitalID(), "user", acct.ID.String(), payload); err != nil {
			return err
		}
		if !trust {
			return nil
		}
		devID, now := uuid.New(), s.now()
		if _, err := tx.Exec(ctx, `INSERT INTO trusted_devices (id, user_id, token_hash, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5)`, devID, acct.ID, deviceHash, now, now.Add(TrustedDeviceTTL)); err != nil {
			return errors.New("insert trusted device failed")
		}
		return writeAudit(ctx, tx, r, "auth.device.trusted", acct.ID.String(), acct.hospitalID(), "trusted_device", devID.String(),
			map[string]any{"days": int(TrustedDeviceTTL / (24 * time.Hour))})
	}, deviceToken)
}

func methodWord(k codeKind) string {
	if k == "" {
		return "none"
	}
	return string(k)
}

// decodeEmpty accepts an empty body or an empty JSON object; anything else
// is 422 validation_failed.
func decodeEmpty(w http.ResponseWriter, r *http.Request) bool {
	if r.Body == nil {
		return true
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, httpx.MaxBody+1))
	if err != nil || len(raw) > httpx.MaxBody {
		httpx.Error(w, r, "validation_failed")
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var v struct{}
	return httpx.Decode(w, r, &v)
}

// enroll: POST /auth/mfa/enroll → {otpauth_uri, recovery_codes[10]}. Makes
// a new secret (sealed with AES-256-GCM) and 10 backup codes (stored as
// keyed hashes), replacing any enrolment not yet verified. Two-step stays
// off until POST /auth/mfa/verify. An account with two-step already on gets
// 409 conflict: replacing it takes an admin reset.
func (s *Service) enroll(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
	if !decodeEmpty(w, r) {
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
	codes := newBackupCodes()
	hashes := make([]string, len(codes))
	for i, code := range codes {
		hashes[i] = keys.hashBackup(code)
	}
	var (
		email           string
		found, conflict bool
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
		tag, err := tx.Exec(ctx, `
			INSERT INTO user_mfa (user_id, secret_enc, enabled, last_used_step, backup_code_hashes)
			VALUES ($1, $2, false, NULL, $3)
			ON CONFLICT (user_id) DO UPDATE
			SET secret_enc = EXCLUDED.secret_enc, enabled = false, last_used_step = NULL,
			    backup_code_hashes = EXCLUDED.backup_code_hashes
			WHERE NOT user_mfa.enabled`, uid, sealed, hashes)
		if err != nil {
			return errors.New("store mfa enrolment failed")
		}
		if conflict = tag.RowsAffected() != 1; conflict {
			return nil
		}
		return writeAudit(ctx, tx, r, "auth.mfa.enrolled", c.Sub, c.Hid, "user", c.Sub, map[string]any{"confirmed": false})
	})
	if err != nil {
		s.fail(w, r, "internal_error", "auth: mfa enrol failed", "user_id", c.Sub)
		return
	}
	if !found {
		httpx.Error(w, r, "unauthenticated")
		return
	}
	if conflict {
		httpx.Error(w, r, "conflict")
		return
	}
	httpx.JSON(w, http.StatusOK, enrollResponse{OTPAuthURI: otpauthURI(email, secret), RecoveryCodes: codes})
}

// otpauthURI is the Key Uri Format authenticator apps scan.
func otpauthURI(account string, secret []byte) string {
	q := url.Values{}
	q.Set("secret", b32.EncodeToString(secret))
	q.Set("issuer", otpIssuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	u := url.URL{Scheme: "otpauth", Host: "totp", Path: "/" + otpIssuer + ":" + account, RawQuery: q.Encode()}
	return u.String()
}

type verifyRequest struct {
	Code string `json:"code"`
}

// verify: POST /auth/mfa/verify {code} → 204. Checks a TOTP code against
// the enrolled secret (counted like any code check), turns two-step on if
// it was pending, and marks the caller's session as proven by a code
// (sessions.mfa, amr + otp), so its next refresh mints a full token. No
// enrolment is 409 conflict.
func (s *Service) verify(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
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
	uid, err1 := uuid.Parse(c.Sub)
	sid, err2 := uuid.Parse(c.Sid)
	if err1 != nil || err2 != nil {
		httpx.Error(w, r, "unauthenticated")
		return
	}
	keys, err := s.mfaKeys()
	if err != nil {
		s.fail(w, r, "not_ready", "auth: mfa key not configured", "user_id", c.Sub)
		return
	}
	var found, pending bool
	if err := s.withAuth(ctx, func(tx pgx.Tx) error {
		row, err := loadMFA(ctx, tx, uid)
		found = row != nil
		return err
	}); err != nil {
		s.fail(w, r, "internal_error", "auth: mfa lookup failed", "user_id", c.Sub)
		return
	}
	if !found {
		httpx.Error(w, r, "conflict")
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
		row, err := loadMFA(ctx, tx, uid)
		if err != nil || row == nil {
			return err
		}
		if _, ok, err = s.checkCode(ctx, tx, keys, uid, row, in.Code, ""); err != nil || !ok {
			return err
		}
		pending = !row.enabled
		if _, err := tx.Exec(ctx, `UPDATE user_mfa SET enabled = true WHERE user_id = $1`, uid); err != nil {
			return errors.New("enable mfa failed")
		}
		if _, err := tx.Exec(ctx, `UPDATE sessions SET mfa = true,
			amr = CASE WHEN 'otp' = ANY (amr) THEN amr ELSE array_append(amr, 'otp') END
			WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, sid, uid); err != nil {
			return errors.New("mark session failed")
		}
		action := "auth.mfa.verified"
		if pending {
			action = "auth.mfa.enrolled"
		}
		return writeAudit(ctx, tx, r, action, c.Sub, c.Hid, "user", c.Sub, map[string]any{"confirmed": true})
	})
	if err != nil {
		s.fail(w, r, "internal_error", "auth: mfa verify failed", "user_id", c.Sub)
		return
	}
	if !ok {
		s.auditOnly(w, r, "auth.mfa.failed", c.Sub, c.Hid, map[string]any{"method": "otp"})
		s.log().Info("auth: mfa verify refused", "request_id", rid, "user_id", c.Sub)
		httpx.Error(w, r, "mfa_invalid")
		return
	}
	s.clearMFA(ctx, c.Sub)
	w.WriteHeader(http.StatusNoContent)
}

type recoveryCodesRequest struct {
	MFACode    string `json:"mfa_code,omitempty"`
	BackupCode string `json:"backup_code,omitempty"`
}

// errStepUpRefused rolls the regenerate transaction back after a refused
// step-up; the step-up error itself is kept aside.
var errStepUpRefused = errors.New("auth: step-up refused")

// recoveryCodes: POST /auth/mfa/recovery-codes {mfa_code | backup_code} →
// {recovery_codes[10]}. Needs a step-up code; the old codes stop working.
func (s *Service) recoveryCodes(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
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
	codes := newBackupCodes()
	hashes := make([]string, len(codes))
	for i, code := range codes {
		hashes[i] = keys.hashBackup(code)
	}
	var stepErr error
	err = s.withAuth(ctx, func(tx pgx.Tx) error {
		if stepErr = s.StepUp(ctx, tx, c.Sub, in.MFACode, in.BackupCode); stepErr != nil {
			return errStepUpRefused
		}
		tag, err := tx.Exec(ctx, `UPDATE user_mfa SET backup_code_hashes = $2 WHERE user_id = $1 AND enabled`, uid, hashes)
		if err != nil || tag.RowsAffected() != 1 {
			return errors.New("replace backup codes failed")
		}
		return writeAudit(ctx, tx, r, "auth.mfa.codes_regenerated", c.Sub, c.Hid, "user", c.Sub, map[string]any{"count": len(codes)})
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
		s.fail(w, r, "internal_error", "auth: regenerate backup codes failed", "user_id", c.Sub)
		return
	}
	httpx.JSON(w, http.StatusOK, recoveryCodesResponse{RecoveryCodes: codes})
}
