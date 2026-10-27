package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

type sessionView struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	UASummary  *string   `json:"ua_summary"`
	IPPrefix   *string   `json:"ip_prefix"`
	Current    bool      `json:"current"`
}

// listSessions: GET /auth/sessions → {"sessions": [...]}, the caller's
// sessions that are not revoked, most recently seen first.
func (s *Service) listSessions(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
	ctx := r.Context()
	list := []sessionView{}
	err := s.withAuth(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, created_at, last_seen_at, ua_summary, ip_prefix FROM sessions
			WHERE user_id = $1 AND revoked_at IS NULL ORDER BY last_seen_at DESC, id`, c.Sub)
		if err != nil {
			return errors.New("list sessions failed")
		}
		defer rows.Close()
		for rows.Next() {
			var v sessionView
			var id uuid.UUID
			if err := rows.Scan(&id, &v.CreatedAt, &v.LastSeenAt, &v.UASummary, &v.IPPrefix); err != nil {
				return errors.New("list sessions scan failed")
			}
			v.ID, v.Current = id.String(), id.String() == c.Sid
			list = append(list, v)
		}
		return rows.Err()
	})
	if err != nil {
		s.fail(w, r, "internal_error", "auth: list sessions failed", "user_id", c.Sub)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"sessions": list})
}

// deleteSession: DELETE /auth/sessions/{id} → 204. Another user's session,
// an unknown id or a malformed one is 404.
func (s *Service) deleteSession(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, r, "not_found")
		return
	}
	ctx := r.Context()
	var found bool
	err = s.withAuth(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE id = $1 AND user_id = $2)`, id, c.Sub).Scan(&found)
	})
	if err != nil {
		s.fail(w, r, "internal_error", "auth: delete session failed", "user_id", c.Sub)
		return
	}
	if !found {
		httpx.Error(w, r, "not_found")
		return
	}
	jtis, err := s.revokeSessions(ctx, r, c.Sub, c.Hid, "auth.session.revoked", func(ctx context.Context, tx pgx.Tx) (pgx.Rows, error) {
		return tx.Query(ctx, `UPDATE sessions SET revoked_at = $3
			WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL RETURNING id, access_jti`, id, c.Sub, s.now())
	})
	if err != nil {
		s.fail(w, r, "internal_error", "auth: delete session failed", "user_id", c.Sub)
		return
	}
	if len(jtis) == 0 { // already revoked: make sure it is denylisted
		jtis[id.String()] = nil
	}
	if err := s.denyAll(ctx, jtis); err != nil {
		s.fail(w, r, "not_ready", "auth: denylist write failed", "user_id", c.Sub)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// revokeAll: POST /auth/sessions/revoke-all → 204. Revokes every session of
// the caller, the current one included.
func (s *Service) revokeAll(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
	ctx := r.Context()
	jtis, err := s.revokeSessions(ctx, r, c.Sub, c.Hid, "auth.session.revoked", func(ctx context.Context, tx pgx.Tx) (pgx.Rows, error) {
		return tx.Query(ctx, `UPDATE sessions SET revoked_at = $2
			WHERE user_id = $1 AND revoked_at IS NULL RETURNING id, access_jti`, c.Sub, s.now())
	})
	if err != nil {
		s.fail(w, r, "internal_error", "auth: revoke all failed", "user_id", c.Sub)
		return
	}
	jtis[c.Sid] = append(jtis[c.Sid], c.Jti)
	if err := s.denyAll(ctx, jtis); err != nil {
		s.fail(w, r, "not_ready", "auth: denylist write failed", "user_id", c.Sub)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
