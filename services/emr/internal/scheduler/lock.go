package scheduler

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lock takes the connection's session advisory lock on a connection of its
// own (design §7.1: one fetch per connection at a time, across emr
// copies). ok is false when another holder has it. unlock releases it and
// returns the connection; if the release fails the connection is closed,
// which drops the lock with it.
func lock(ctx context.Context, pool *pgxpool.Pool, conn uuid.UUID) (unlock func(), ok bool, err error) {
	c, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	key := "emr-sync:" + conn.String()
	if err := c.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, key).Scan(&ok); err != nil {
		c.Release()
		return nil, false, err
	}
	if !ok {
		c.Release()
		return nil, false, nil
	}
	return func() {
		var released bool
		err := c.QueryRow(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key).Scan(&released)
		if err != nil || !released {
			_ = c.Conn().Close(context.WithoutCancel(ctx))
		}
		c.Release()
	}, true, nil
}
