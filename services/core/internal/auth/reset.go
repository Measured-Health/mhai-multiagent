package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// MaxResetAttempts bounds POST /auth/password/reset per client IP in the
// login attempts bucket's 15 minutes. A token carries 256 bits, so this
// isn't what stops a guesser; it keeps a flood off the argon2 gate and the
// database.
const MaxResetAttempts = 10

// maxTokenLen bounds the token field (a token is 43 base64url characters).
const maxTokenLen = 128

var errBadToken = errors.New("auth: no live token")

type resetRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

// liveToken is a live invite or reset token, read by its hash.
type liveToken struct {
	ID, User uuid.UUID
	Purpose  string
}

func readLiveToken(ctx context.Context, tx pgx.Tx, hash string) (liveToken, error) {
	var t liveToken
	// The token's hospital must still be its person's (every writer keeps
	// them equal; checked again where the token is spent).
	err := tx.QueryRow(ctx, `SELECT t.id, t.user_id, t.purpose FROM account_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = $1 AND t.used_at IS NULL AND t.revoked_at IS NULL AND t.expires_at > now()
		  AND u.hospital_id = t.hospital_id`, hash).Scan(&t.ID, &t.User, &t.Purpose)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, errBadToken
	}
	return t, err
}

// resetPassword: POST /auth/password/reset {token, new_password} → 204.
// Public: the link's holder isn't signed in. It redeems both a hospital
// admin's reset link and an invite (the invited person's first password).
//
//   - One attempt per call is counted against the client IP first (429
//     rate_limited past MaxResetAttempts; a counter fault is 503), and
//     given back on success.
//   - A token that is unknown, used, replaced or expired, or whose account
//     can't sign in, is 410 invalid_token, alike: the reply never says
//     which.
//   - The new password must pass password.Check (422 weak_password); the
//     token isn't spent then.
//   - Then, in one transaction as mhai_auth: the token is spent (only one
//     concurrent use wins; a loser is 410), every other live link of the
//     person is revoked, the password set, must_change_password cleared,
//     every session revoked (and denylisted after commit) and the trusted
//     devices forgotten. Audited auth.password.reset with the purpose and
//     the number of sessions revoked. The person then signs in as usual
//     (and enrols two-step if they have none).
func (s *Service) resetPassword(w http.ResponseWriter, r *http.Request) {
	var in resetRequest
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Token == "" || in.NewPassword == "" || len(in.Token) > maxTokenLen || len(in.NewPassword) > maxPasswordLen {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	rid := httpx.RequestIDFrom(ctx)
	ip := clientIP(r)
	if ip == "" {
		s.fail(w, r, "not_ready", "auth: password reset without a client IP; check the BFF configuration")
		return
	}
	if s.Buckets.LoginAttempts == nil {
		s.fail(w, r, "not_ready", "auth: reset attempts unavailable")
		return
	}
	key, err := s.counterName("reset", ip)
	if err != nil {
		s.fail(w, r, "not_ready", "auth: reset attempts unavailable")
		return
	}
	locked, err := reserveIn(ctx, s.Buckets.LoginAttempts, key, MaxResetAttempts)
	if err != nil {
		s.fail(w, r, "not_ready", "auth: reset attempts unavailable")
		return
	}
	if locked {
		httpx.Error(w, r, "rate_limited")
		return
	}
	hash := token.HashRefresh(in.Token)
	var tok liveToken
	var acct *acctRec
	err = s.withAuth(ctx, func(tx pgx.Tx) error {
		t, err := readLiveToken(ctx, tx, hash)
		if err != nil {
			return err
		}
		a, err := accountByID(ctx, tx, t.User)
		if errors.Is(err, errNoAccount) {
			return errBadToken
		}
		tok, acct = t, a
		return err
	})
	if errors.Is(err, errBadToken) || (err == nil && !acct.usable()) {
		s.log().Info("auth: password reset refused", "request_id", rid, "reason", "invalid_token")
		httpx.Error(w, r, "invalid_token")
		return
	}
	if err != nil {
		s.fail(w, r, "internal_error", "auth: password reset lookup failed")
		return
	}
	uid := tok.User.String()
	if password.Check(in.NewPassword, acct.Email) != nil {
		s.log().Info("auth: password reset refused", "request_id", rid, "user_id", uid, "reason", "weak")
		httpx.Error(w, r, "weak_password")
		return
	}
	release, err := s.argon().Acquire(ctx)
	if err != nil {
		s.fail(w, r, "not_ready", "auth: password checks saturated", "user_id", uid)
		return
	}
	pwHash, err := password.Hash(in.NewPassword)
	release()
	if err != nil {
		s.fail(w, r, "internal_error", "auth: password hash failed", "user_id", uid)
		return
	}
	jtis := map[string][]string{}
	err = s.withAuth(ctx, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `UPDATE account_tokens SET used_at = now()
			WHERE id = $1 AND used_at IS NULL AND revoked_at IS NULL AND expires_at > now()`, tok.ID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			return errBadToken // another use won
		}
		if _, err := tx.Exec(ctx, `UPDATE account_tokens SET revoked_at = now()
			WHERE user_id = $1 AND used_at IS NULL AND revoked_at IS NULL`, tok.User); err != nil {
			return err
		}
		ct, err = tx.Exec(ctx, `UPDATE users SET password_hash = $2, must_change_password = false WHERE id = $1 AND is_active`,
			tok.User, pwHash)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			return errBadToken // deactivated since the lookup: nothing is spent
		}
		rows, err := tx.Query(ctx, `UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL
			RETURNING id, access_jti`, tok.User, s.now())
		if err != nil {
			return err
		}
		for rows.Next() {
			var id uuid.UUID
			var jti *uuid.UUID
			if err := rows.Scan(&id, &jti); err != nil {
				rows.Close()
				return err
			}
			jtis[id.String()] = []string{uuidString(jti)}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM trusted_devices WHERE user_id = $1`, tok.User); err != nil {
			return err
		}
		return writeAudit(ctx, tx, r, "auth.password.reset", uid, acct.hospitalID(), "user", uid,
			map[string]any{"purpose": tok.Purpose, "sessions_revoked": len(jtis)})
	})
	if errors.Is(err, errBadToken) {
		httpx.Error(w, r, "invalid_token")
		return
	}
	if err != nil {
		s.fail(w, r, "internal_error", "auth: password reset failed", "user_id", uid)
		return
	}
	if err := refundIn(context.WithoutCancel(ctx), s.Buckets.LoginAttempts, key); err != nil {
		s.log().Error("auth: reset attempts refund failed", "request_id", rid, "user_id", uid)
	}
	if err := s.denyAll(ctx, jtis); err != nil {
		s.log().Error("auth: denylist write failed after password reset", "request_id", rid, "user_id", uid)
	}
	w.WriteHeader(http.StatusNoContent)
}
