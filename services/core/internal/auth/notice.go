package auth

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/delivery"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// notice records a security notice of kind for user in tx (the change's own
// transaction, so a change that rolls back tells nobody). It never fails
// the change: with no sender configured nothing is recorded, and a failure
// to record is logged by ids and a closed reason, never the address. It
// returns the recipient's hospital when a row was written (uuid.Nil
// otherwise), for afterNotice.
func (s *Service) notice(ctx context.Context, tx pgx.Tx, user uuid.UUID, kind delivery.Kind) uuid.UUID {
	if s.Notices == nil {
		return uuid.Nil
	}
	h, err := delivery.Notice(ctx, tx, s.Notices, user, kind)
	if err != nil && !errors.Is(err, delivery.ErrNoSender) {
		s.log().Error("auth: security notice not recorded", "request_id", httpx.RequestIDFrom(ctx), "user_id", user.String(),
			"kind", string(kind), "reason", "internal_error")
		return uuid.Nil
	}
	return h
}

// afterNotice hands a recorded notice to the sender after the commit: the
// log sender logs it is due (ids only), the notifications sender kicks the
// drainer (the outbox row is published on NOTIFY; the 30-second sweep does
// it otherwise). Nothing here can undo the change.
func (s *Service) afterNotice(ctx context.Context, hospital, user uuid.UUID, kind delivery.Kind) {
	if s.Notices == nil || hospital == uuid.Nil {
		return
	}
	if err := s.Notices.Send(context.WithoutCancel(ctx), delivery.Message{Kind: kind, HospitalID: hospital, Recipient: user,
		By: user}); err != nil {
		s.log().Warn("auth: security notice hand-over failed; the outbox sweep sends it", "request_id", httpx.RequestIDFrom(ctx),
			"user_id", user.String(), "kind", string(kind))
	}
}
