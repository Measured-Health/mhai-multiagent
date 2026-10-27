package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type refreshResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

// refreshOutcome is what the locked transaction decided.
type refreshOutcome int

const (
	refreshOK       refreshOutcome = iota
	refreshInvalid                 // unknown token: 401
	refreshRevoked                 // session already revoked: 401, denylist written again
	refreshRace                    // already rotated within the grace window: 409, nothing revoked
	refreshReuse                   // already rotated longer ago: family revoked, 401
	refreshEndedNow                // idle, unusable account or MFA no longer met: session revoked now, 401
)

// refresh: POST /auth/refresh. The token row is locked FOR UPDATE, so two
// concurrent refreshes of one token serialise: the first rotates, the
// second sees rotated_at and gets 409 conflict (within ReuseGrace) without
// revoking anything. A reuse after the grace window revokes the session
// (the whole family) and denylists its access tokens.
func (s *Service) refresh(w http.ResponseWriter, r *http.Request) {
	var in refreshRequest
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.RefreshToken == "" || len(in.RefreshToken) > 256 {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	rid := httpx.RequestIDFrom(ctx)
	now := s.now()
	oldHash := token.HashRefresh(in.RefreshToken)

	var (
		outcome            refreshOutcome
		userID, hospitalID string
		sid                uuid.UUID
		oldJTI             *uuid.UUID
		access, newRefresh string
	)
	err := s.withAuth(ctx, func(tx pgx.Tx) error {
		var (
			rtID      uuid.UUID
			rotatedAt *time.Time
			revokedAt *time.Time
			lastSeen  time.Time
			uid       uuid.UUID
			mfa       bool
			amr       []string
			accessJTI *uuid.UUID
		)
		err := tx.QueryRow(ctx, `
			SELECT rt.id, rt.rotated_at, s.id, s.revoked_at, s.last_seen_at, s.user_id, s.mfa, s.amr, s.access_jti
			FROM refresh_tokens rt JOIN sessions s ON s.id = rt.session_id
			WHERE rt.token_hash = $1
			FOR UPDATE OF rt, s`, oldHash).
			Scan(&rtID, &rotatedAt, &sid, &revokedAt, &lastSeen, &uid, &mfa, &amr, &accessJTI)
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = refreshInvalid
			return nil
		}
		if err != nil {
			return errors.New("refresh lookup failed")
		}
		userID, oldJTI = uid.String(), accessJTI
		if revokedAt != nil {
			// Write the denylist again: a retry after a lost denylist write
			// (for example on the reuse path) must still cut the session off.
			outcome = refreshRevoked
			return nil
		}
		acct, err := accountByID(ctx, tx, uid)
		if err != nil && !errors.Is(err, errNoAccount) {
			return err
		}
		if acct != nil {
			hospitalID = acct.hospitalID()
		}
		if rotatedAt != nil {
			if now.Sub(*rotatedAt) <= s.grace() {
				outcome = refreshRace
				return nil
			}
			outcome = refreshReuse
			if err := revokeSession(ctx, tx, sid, now); err != nil {
				return err
			}
			return writeAudit(ctx, tx, r, "auth.refresh.reuse", userID, hospitalID, "session", sid.String(),
				map[string]any{"revoked": true})
		}
		idle := defaultIdleMinutes
		if acct != nil {
			idle = acct.IdleMinutes
		}
		if acct == nil || !acct.usable() || now.Sub(lastSeen) > time.Duration(idle)*time.Minute ||
			!sessionSatisfiesMFA(acct.Roles, acct.policy(), mfa, amr) {
			outcome = refreshEndedNow
			return revokeSession(ctx, tx, sid, now)
		}

		jti := uuid.New()
		var exp time.Time
		access, exp, err = s.issuer().Access(token.Claims{Sub: userID, Hid: hospitalID, Roles: acct.Roles,
			MFA: mfa, AMR: amr, Sid: sid.String(), Jti: jti.String()})
		if err != nil {
			return err
		}
		var newHash string
		newRefresh, newHash = token.NewRefresh()
		newID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO refresh_tokens (id, session_id, token_hash, issued_at) VALUES ($1, $2, $3, $4)`,
			newID, sid, newHash, now); err != nil {
			return errors.New("insert refresh token failed")
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET rotated_at = $2, successor_id = $3 WHERE id = $1`,
			rtID, now, newID); err != nil {
			return errors.New("rotate refresh token failed")
		}
		if _, err := tx.Exec(ctx, `UPDATE sessions SET last_seen_at = $2, access_jti = $3, access_expires_at = $4 WHERE id = $1`,
			sid, now, jti, exp); err != nil {
			return errors.New("update session failed")
		}
		outcome = refreshOK
		return nil
	})
	if err != nil {
		s.fail(w, r, "internal_error", "auth: refresh failed", "user_id", userID)
		return
	}
	switch outcome {
	case refreshOK:
		httpx.JSON(w, http.StatusOK, refreshResponse{AccessToken: access, ExpiresIn: int(token.AccessTTL / time.Second),
			RefreshToken: newRefresh})
	case refreshRace:
		s.log().Info("auth: refresh race", "request_id", rid, "user_id", userID)
		httpx.Error(w, r, "conflict")
	case refreshReuse, refreshEndedNow, refreshRevoked:
		if outcome == refreshReuse {
			s.log().Warn("auth: refresh token reused; session revoked", "request_id", rid, "user_id", userID, "session_id", sid.String())
		}
		if err := s.denySession(ctx, sid.String(), uuidString(oldJTI)); err != nil {
			s.fail(w, r, "not_ready", "auth: denylist write failed", "user_id", userID)
			return
		}
		httpx.Error(w, r, "unauthenticated")
	default:
		httpx.Error(w, r, "unauthenticated")
	}
}

// revokeSession marks the session revoked; its refresh tokens die with it
// (every refresh checks the session).
func revokeSession(ctx context.Context, tx pgx.Tx, sid uuid.UUID, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, sid, now); err != nil {
		return errors.New("revoke session failed")
	}
	return nil
}

func uuidString(u *uuid.UUID) string {
	if u == nil {
		return ""
	}
	return u.String()
}
