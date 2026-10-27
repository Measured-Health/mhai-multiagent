package auth

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
)

type passwordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// changePassword: POST /auth/password {current_password, new_password} →
// 204.
//
//   - The current password is checked first, and each check counts against
//     the sign-in lockout for (login, client IP): a stolen session can't be
//     used to guess the password. A wrong one is 403 forbidden; the 6th in
//     15 minutes is 429 login_locked.
//   - The new password must pass password.Check (and differ from the
//     current one), else 422 weak_password.
//   - On success: the new hash, must_change_password cleared, every other
//     session of the user revoked and denylisted (sid and access jti), and
//     the user's trusted devices removed, all in one transaction. The
//     caller's own session stays.
func (s *Service) changePassword(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
	var in passwordRequest
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.CurrentPassword == "" || in.NewPassword == "" ||
		len(in.CurrentPassword) > maxPasswordLen || len(in.NewPassword) > maxPasswordLen {
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
	ip := clientIP(r)
	if ip == "" {
		s.fail(w, r, "not_ready", "auth: password change without a client IP; check the BFF configuration", "user_id", c.Sub)
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
		s.fail(w, r, "internal_error", "auth: password account lookup failed", "user_id", c.Sub)
		return
	}
	if acct == nil || !acct.usable() {
		httpx.Error(w, r, "unauthenticated")
		return
	}
	key := LockoutKey(acct.Email, ip)
	locked, err := s.reserveAttempt(ctx, key)
	if err != nil {
		s.fail(w, r, "not_ready", "auth: login attempts unavailable", "user_id", c.Sub)
		return
	}
	if locked {
		s.auditOnly(w, r, "auth.password.failed", c.Sub, c.Hid, map[string]any{"reason": "locked"})
		httpx.Error(w, r, "login_locked")
		return
	}
	ok := false
	if acct.PasswordHash != nil && *acct.PasswordHash != "" {
		ok, _ = password.Verify(in.CurrentPassword, *acct.PasswordHash)
	} else {
		password.DummyVerify(in.CurrentPassword)
	}
	if !ok {
		s.auditOnly(w, r, "auth.password.failed", c.Sub, c.Hid, map[string]any{"reason": "bad_current"})
		s.log().Info("auth: password change refused", "request_id", rid, "user_id", c.Sub)
		httpx.Error(w, r, "forbidden")
		return
	}
	if err := s.Buckets.LoginAttempts.Delete(ctx, key); err != nil && !bus.IsNotFound(err) {
		s.log().Error("auth: login attempts reset failed", "request_id", rid, "user_id", c.Sub)
	}
	if in.NewPassword == in.CurrentPassword || password.Check(in.NewPassword, acct.Email) != nil {
		s.auditOnly(w, r, "auth.password.failed", c.Sub, c.Hid, map[string]any{"reason": "weak"})
		httpx.Error(w, r, "weak_password")
		return
	}
	hash, err := password.Hash(in.NewPassword)
	if err != nil {
		s.fail(w, r, "internal_error", "auth: password hash failed", "user_id", c.Sub)
		return
	}
	jtis := map[string][]string{}
	err = s.withAuth(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, must_change_password = false WHERE id = $1`, uid, hash); err != nil {
			return errors.New("update password failed")
		}
		rows, err := tx.Query(ctx, `UPDATE sessions SET revoked_at = $3
			WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL RETURNING id, access_jti`, uid, sid, s.now())
		if err != nil {
			return errors.New("revoke sessions failed")
		}
		var ids []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			var jti *uuid.UUID
			if err := rows.Scan(&id, &jti); err != nil {
				rows.Close()
				return errors.New("revoke sessions scan failed")
			}
			ids = append(ids, id)
			jtis[id.String()] = []string{uuidString(jti)}
		}
		rows.Close()
		if rows.Err() != nil {
			return errors.New("revoke sessions failed")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM trusted_devices WHERE user_id = $1`, uid); err != nil {
			return errors.New("remove trusted devices failed")
		}
		for _, id := range ids {
			if err := writeAudit(ctx, tx, r, "auth.session.revoked", c.Sub, c.Hid, "session", id.String(),
				map[string]any{"reason": "password_changed"}); err != nil {
				return err
			}
		}
		return writeAudit(ctx, tx, r, "auth.password.changed", c.Sub, c.Hid, "user", c.Sub,
			map[string]any{"sessions_revoked": len(ids)})
	})
	if err != nil {
		s.fail(w, r, "internal_error", "auth: password change failed", "user_id", c.Sub)
		return
	}
	if err := s.denyAll(ctx, jtis); err != nil {
		s.fail(w, r, "not_ready", "auth: denylist write failed", "user_id", c.Sub)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
