package ctl

// sync_now (svc:core; EMR design §3.2, plan Task 24): the connection is
// polled at the scheduler's next tick. The hospital is the verified
// token's; another hospital's connection is not found and nothing is
// answered. A connection backing off a failing source (a 429's
// Retry-After, a 5xx doubling) is not hurried: the reply is queued false.
// Repeating it is harmless.

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// SyncNow serves sync_now.
type SyncNow struct {
	Pool *pgxpool.Pool
	Now  func() time.Time
}

func (s *SyncNow) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Handle is the sync_now op.
func (s *SyncNow) Handle(ctx context.Context, c reqtoken.Claims, m proto.Message) (proto.Message, error) {
	req, ok := m.(*emrpb.SyncNowRequest)
	if !ok || s.Pool == nil {
		return nil, errRequest
	}
	hid, id, err := ids(c, req.GetConnectionId())
	if err != nil {
		return nil, err
	}
	queued := false
	err = dbx.WithHospital(ctx, s.Pool, dbx.EMR, hid, func(tx pgx.Tx) error {
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM public.emr_connections WHERE id = $1 AND hospital_id = $2`, id, hid).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return errConnection
		}
		if err != nil {
			return errDatabase
		}
		tag, err := tx.Exec(ctx, `INSERT INTO emr.sync_state (connection_id, hospital_id, next_due_at) VALUES ($1, $2, $3)
			ON CONFLICT (connection_id) DO UPDATE SET next_due_at = LEAST(emr.sync_state.next_due_at, excluded.next_due_at),
			    updated_at = excluded.next_due_at
			WHERE emr.sync_state.failures = 0`, id, hid, s.now())
		if err != nil {
			return errDatabase
		}
		queued = tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &emrpb.SyncNowReply{Queued: queued}, nil
}
