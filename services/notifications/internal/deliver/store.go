package deliver

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
)

// The claim's outcomes (notify_claim, migration 00053).
const (
	Claimed  = "claimed"   // this caller holds the lease
	Inactive = "inactive"  // claimed, but the recipient can't be told: suppress
	Busy     = "busy"      // another holds the lease
	Held     = "held"      // waiting for its next attempt
	Done     = "done"      // already sent, failed or suppressed
	NotFound = "not_found" // no such message in the subject's hospital
)

// Claim is notify_claim's answer.
type Claim struct {
	Outcome   string
	Lease     uuid.UUID
	Kind      string
	Recipient uuid.UUID
	Subject   uuid.UUID
	Channel   string
	Attempts  int
	// FirstAttempt is when the first send was tried (zero: never).
	FirstAttempt time.Time
	// NextAttempt is when a held or busy delivery may be tried again.
	NextAttempt time.Time
	// Address is set only for the email channel and an active recipient.
	Address string
}

// Finish is how a claimed delivery ends or waits.
type Finish struct {
	Status     string // claimed (it waits), sent, failed or suppressed
	Outcome    string // a closed outcome code
	Attempts   int
	Next       time.Time // when it waits: its next attempt
	Tried      time.Time // the time of a send attempt (sets first_attempt_at once)
	ProviderID string    // sent: hashed before it is stored
}

// Store is the service's database access.
type Store interface {
	Claim(ctx context.Context, hospital, message uuid.UUID, lease time.Duration) (Claim, error)
	Finish(ctx context.Context, hospital, message uuid.UUID, channel string, lease uuid.UUID, f Finish) error
	// Reserve takes one send from the recipient's hourly allowance and,
	// unless mandatory, the hospital's. ok false: a cap is reached and
	// nothing was taken; retry at the next hour.
	Reserve(ctx context.Context, hospital, recipient uuid.UUID, mandatory bool, now time.Time, l Limits) (ok bool, retry time.Time, err error)
	Retention(ctx context.Context) (int, error)
}

// ErrLeaseLost: the delivery isn't this caller's to finish any more.
var ErrLeaseLost = errors.New("deliver: the lease is lost")

// PG is the Store on Postgres, as mhai_notify (dbx.WithHospital): every
// read and write runs under the hospital the due subject names.
type PG struct{ Pool *pgxpool.Pool }

func ts(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

// Claim calls notify_claim under hospital.
func (p PG) Claim(ctx context.Context, hospital, message uuid.UUID, lease time.Duration) (Claim, error) {
	var c Claim
	err := dbx.WithHospital(ctx, p.Pool, dbx.Notify, hospital, func(tx pgx.Tx) error {
		var (
			lid, rec, subj pgtype.UUID
			kind, ch, addr pgtype.Text
			attempts       pgtype.Int4
			first, next    pgtype.Timestamptz
		)
		if err := tx.QueryRow(ctx, `SELECT outcome, lease_id, kind, recipient_id, subject_id, channel, attempts,
			first_attempt_at, next_attempt_at, address FROM notify_claim($1, make_interval(secs => $2))`,
			message, lease.Seconds()).Scan(&c.Outcome, &lid, &kind, &rec, &subj, &ch, &attempts, &first, &next, &addr); err != nil {
			return err
		}
		if lid.Valid {
			c.Lease = lid.Bytes
		}
		if rec.Valid {
			c.Recipient = rec.Bytes
		}
		if subj.Valid {
			c.Subject = subj.Bytes
		}
		c.Kind, c.Channel, c.Address = kind.String, ch.String, addr.String
		c.Attempts = int(attempts.Int32)
		c.FirstAttempt, c.NextAttempt = ts(first), ts(next)
		return nil
	})
	return c, err
}

func nullTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}

// Finish records f on a delivery this caller holds the lease of.
func (p PG) Finish(ctx context.Context, hospital, message uuid.UUID, channel string, lease uuid.UUID, f Finish) error {
	var hash []byte
	if f.ProviderID != "" {
		h := sha256.Sum256([]byte(f.ProviderID))
		hash = h[:]
	}
	var sent pgtype.Timestamptz
	if f.Status == "sent" {
		sent = pgtype.Timestamptz{Time: f.Tried, Valid: true}
		if f.Tried.IsZero() {
			sent.Time = time.Now()
		}
	}
	var outcome pgtype.Text
	if f.Outcome != "" {
		outcome = pgtype.Text{String: f.Outcome, Valid: true}
	}
	return dbx.WithHospital(ctx, p.Pool, dbx.Notify, hospital, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE message_deliveries SET status = $4, outcome_code = $5, attempts = $6,
			next_attempt_at = $7, first_attempt_at = coalesce(first_attempt_at, $8), sent_at = $9, provider_ref_hash = $10,
			lease_until = NULL, lease_id = NULL, updated_at = now()
			WHERE message_id = $1 AND channel = $2 AND lease_id = $3 AND status = 'claimed'`,
			message, channel, lease, f.Status, outcome, f.Attempts, nullTime(f.Next), nullTime(f.Tried), sent, hash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		return nil
	})
}

// reserveSQL takes one from a counter below its cap: one upsert that
// serialises on the counter's row, so parallel senders can't both take
// the last one (reserve, then check).
const reserveSQL = `INSERT INTO message_send_counts AS c (hospital_id, scope, key, window_start, n) VALUES ($1, $2, $3, $4, 1)
	ON CONFLICT (hospital_id, scope, key, window_start) DO UPDATE SET n = c.n + 1 WHERE c.n < $5
	RETURNING n`

var errCapped = errors.New("deliver: capped")

// Reserve takes the recipient's and (unless mandatory) the hospital's
// allowance for this hour in one transaction: if either is spent, neither
// is taken.
func (p PG) Reserve(ctx context.Context, hospital, recipient uuid.UUID, mandatory bool, now time.Time, l Limits) (bool, time.Time, error) {
	hour := now.UTC().Truncate(time.Hour)
	err := dbx.WithHospital(ctx, p.Pool, dbx.Notify, hospital, func(tx pgx.Tx) error {
		take := func(scope string, key uuid.UUID, cap int) error {
			var n int
			err := tx.QueryRow(ctx, reserveSQL, hospital, scope, key, hour, cap).Scan(&n)
			if errors.Is(err, pgx.ErrNoRows) {
				return errCapped
			}
			return err
		}
		if err := take("recipient", recipient, l.recipient()); err != nil {
			return err
		}
		if !mandatory {
			return take("hospital", hospital, l.hospital())
		}
		return nil
	})
	switch {
	case errors.Is(err, errCapped):
		return false, hour.Add(time.Hour), nil
	case err != nil:
		return false, time.Time{}, err
	}
	return true, time.Time{}, nil
}

// Retention deletes deliveries older than a year and counters older than
// two days (notify_retention), as mhai_notify.
func (p PG) Retention(ctx context.Context) (int, error) {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+string(dbx.Notify)); err != nil {
		return 0, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT notify_retention()`).Scan(&n); err != nil {
		return 0, err
	}
	return n, tx.Commit(ctx)
}
