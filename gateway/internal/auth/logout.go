package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/gateway/internal/httpx"
)

// logout: POST /auth/logout. Revokes the caller's session and denylists the
// session, the presented access token and the session's latest one.
func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
	sid, err := uuid.Parse(c.Sid)
	if err != nil {
		httpx.Error(w, r, "unauthenticated")
		return
	}
	ctx := r.Context()
	jtis, err := s.revokeSessions(ctx, r, c.Sub, c.Hid, "auth.logout", func(ctx context.Context, tx pgx.Tx) (pgx.Rows, error) {
		return tx.Query(ctx, `UPDATE sessions SET revoked_at = COALESCE(revoked_at, $3)
			WHERE id = $1 AND user_id = $2 RETURNING id, access_jti`, sid, c.Sub, s.now())
	})
	if err != nil {
		s.fail(w, r, "internal_error", "auth: logout failed", "user_id", c.Sub)
		return
	}
	jtis[sid.String()] = append(jtis[sid.String()], c.Jti)
	if err := s.denyAll(ctx, jtis); err != nil {
		s.fail(w, r, "not_ready", "auth: denylist write failed", "user_id", c.Sub)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// revokeSessions runs update (which must RETURN id, access_jti of the
// sessions it revoked) and writes one audit row per session, all in one
// transaction. It returns each session's access token ids to denylist.
func (s *Service) revokeSessions(ctx context.Context, r *http.Request, userID, hospitalID, action string,
	update func(context.Context, pgx.Tx) (pgx.Rows, error)) (map[string][]string, error) {
	out := map[string][]string{}
	err := s.withAuth(ctx, func(tx pgx.Tx) error {
		rows, err := update(ctx, tx)
		if err != nil {
			return errors.New("revoke sessions failed")
		}
		type rev struct {
			id  uuid.UUID
			jti *uuid.UUID
		}
		var revs []rev
		for rows.Next() {
			var x rev
			if err := rows.Scan(&x.id, &x.jti); err != nil {
				rows.Close()
				return errors.New("revoke sessions scan failed")
			}
			revs = append(revs, x)
		}
		rows.Close()
		if rows.Err() != nil {
			return errors.New("revoke sessions failed")
		}
		for _, x := range revs {
			out[x.id.String()] = []string{uuidString(x.jti)}
			if err := writeAudit(ctx, tx, r, action, userID, hospitalID, "session", x.id.String(), map[string]any{}); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func (s *Service) denyAll(ctx context.Context, jtis map[string][]string) error {
	var failed bool
	for sid, js := range jtis {
		if err := s.denySession(ctx, sid, js...); err != nil {
			failed = true
		}
	}
	if failed {
		return errors.New("auth: denylist write failed")
	}
	return nil
}
