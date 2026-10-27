package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

const maxTermsVersionLen = 64

type termsRequest struct {
	Version string `json:"version"`
}

// terms: POST /auth/terms {version} → 204. Records the version and time.
func (s *Service) terms(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	var in termsRequest
	if !httpx.Decode(w, r, &in) {
		return
	}
	v := strings.TrimSpace(in.Version)
	if v == "" || len(v) > maxTermsVersionLen || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	var found bool
	err := s.withAuth(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET terms_version = $2, terms_accepted_at = $3 WHERE id = $1`, c.Sub, v, s.now())
		if err != nil {
			return errors.New("update terms failed")
		}
		if found = tag.RowsAffected() == 1; !found {
			return nil
		}
		return writeAudit(ctx, tx, r, "auth.terms.accepted", c.Sub, c.Hid, "user", c.Sub,
			map[string]any{"version": v})
	})
	if err != nil {
		s.fail(w, r, "internal_error", "auth: terms failed", "user_id", c.Sub)
		return
	}
	if !found {
		httpx.Error(w, r, "unauthenticated")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
