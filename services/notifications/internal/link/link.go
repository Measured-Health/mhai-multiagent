// Package link serves notify.v1.ctl.send_link (notifications design §4.4,
// §6.3; §15 PR 5): an invite or a password reset, sent at once, with its
// link held in memory only. Core asks after the action's commit with a
// request token naming the hospital; the responder has checked it (core
// only, the replay set, a strict schema, a hospital id) before Handle runs.
//
// The recipient is a user id, never an address. notify_link_recipient
// answers the address only for an active user of the token's hospital who
// holds a live token of that purpose created in the last 2 minutes (NT6,
// NT8): a stale, forged or replayed request names nobody. The link must be
// on the app's address (NT5, checked again when rendered). The send is
// tried 3 times within about 25 seconds; the answer is accepted or a
// closed code, and core turns anything but accepted into 424 not_delivered.
// Per hospital, at most 30 links a minute; per person, the hourly
// allowance (mandatory kinds: the hospital's cap doesn't hold them). Logs
// carry the kind, ids and the outcome; never the link, the address or the
// hospital's name.
package link

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/notifypb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/channel"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/ctl"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/deliver"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/templates"
)

// The outcomes (SendLinkReply.outcome).
const (
	Accepted    = "accepted"
	Failed      = "failed"
	NoRecipient = "no_recipient"
	NotReady    = "not_ready"
	RateLimited = "rate_limited"
	Invalid     = "invalid"
)

// PerHospitalPerMinute bounds send_link per hospital (design §9.4).
const PerHospitalPerMinute = 30

// waits are the pauses between the 3 tries; each try gets tryTimeout.
var waits = []time.Duration{time.Second, 3 * time.Second}

const tryTimeout = 7 * time.Second

// Store is what Handle needs of the database.
type Store interface {
	// Recipient is notify_link_recipient under hospital: the address and
	// the hospital's name; ok false for nobody.
	Recipient(ctx context.Context, hospital, user uuid.UUID, purpose string) (address, hospitalName string, ok bool, err error)
	Reserve(ctx context.Context, hospital, recipient uuid.UUID, mandatory bool, now time.Time, l deliver.Limits) (bool, time.Time, error)
}

// PG is the Store on Postgres, as mhai_notify under the token's hospital.
type PG struct {
	deliver.PG
}

// NewPG is the store on pool.
func NewPG(pool *pgxpool.Pool) PG { return PG{deliver.PG{Pool: pool}} }

// Recipient calls notify_link_recipient.
func (p PG) Recipient(ctx context.Context, hospital, user uuid.UUID, purpose string) (string, string, bool, error) {
	var addr, name string
	found := false
	err := dbx.WithHospital(ctx, p.Pool, dbx.Notify, hospital, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT address, hospital_name FROM notify_link_recipient($1, $2)`, user, purpose).
			Scan(&addr, &name)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		found = true
		return nil
	})
	return addr, name, found, err
}

// Handler serves send_link.
type Handler struct {
	Store  Store
	Email  channel.Channel // nil: email is off, nothing is sent
	Limits deliver.Limits
	Log    *slog.Logger
	Now    func() time.Time
	// Sleep waits between tries (tests); nil sleeps.
	Sleep func(context.Context, time.Duration)

	mu     sync.Mutex
	window time.Time
	counts map[uuid.UUID]int
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (h *Handler) sleep(ctx context.Context, d time.Duration) {
	if h.Sleep != nil {
		h.Sleep(ctx, d)
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// allow takes one of hospital's links this minute, under the lock, before
// anything else is done (reserve, then check).
func (h *Handler) allow(hospital uuid.UUID, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	w := now.Truncate(time.Minute)
	if !w.Equal(h.window) || h.counts == nil {
		h.window, h.counts = w, map[uuid.UUID]int{}
	}
	if h.counts[hospital] >= PerHospitalPerMinute {
		return false
	}
	h.counts[hospital]++
	return true
}

func canonical(s string) (uuid.UUID, bool) {
	u, err := uuid.Parse(s)
	return u, err == nil && u != uuid.Nil && u.String() == s
}

// Handle sends one link and answers its outcome.
func (h *Handler) Handle(ctx context.Context, c reqtoken.Claims, req proto.Message) (proto.Message, error) {
	hospital, err := ctl.Hospital(c)
	r, ok := req.(*notifypb.SendLinkRequest)
	if err != nil || !ok {
		return h.done("", uuid.Nil, uuid.Nil, Invalid), nil
	}
	now := h.now()
	user, ok := canonical(r.GetUserId())
	kind := r.GetPurpose()
	exp := time.Unix(r.GetExpiresAt(), 0)
	if !ok || (kind != "invite" && kind != "password_reset") || r.GetLink() == "" || len(r.GetLink()) > 2048 ||
		!exp.After(now) || exp.After(now.Add(7*24*time.Hour+time.Minute)) {
		return h.done(kind, hospital, user, Invalid), nil
	}
	if h.Email == nil {
		return h.done(kind, hospital, user, NotReady), nil
	}
	if !h.allow(hospital, now) {
		return h.done(kind, hospital, user, RateLimited), nil
	}
	addr, name, found, err := h.Store.Recipient(ctx, hospital, user, kind)
	switch {
	case err != nil:
		return h.done(kind, hospital, user, NotReady), nil
	case !found:
		return h.done(kind, hospital, user, NoRecipient), nil
	}
	reserved, _, err := h.Store.Reserve(ctx, hospital, user, true, now, h.Limits)
	switch {
	case err != nil:
		return h.done(kind, hospital, user, NotReady), nil
	case !reserved:
		return h.done(kind, hospital, user, RateLimited), nil
	}
	m := channel.Message{ID: uuid.New(), Hospital: hospital, Recipient: user, Kind: kind, Address: addr,
		Link: r.GetLink(), Expires: templates.FormatExpiry(exp), HospitalName: name}
	for i := 0; ; i++ {
		tctx, cancel := context.WithTimeout(ctx, tryTimeout)
		res, err := h.Email.Send(tctx, m)
		cancel()
		switch {
		case err == nil && res.Outcome == channel.Accepted:
			return h.done(kind, hospital, user, Accepted), nil
		case err == nil && res.Outcome == channel.Permanent:
			return h.done(kind, hospital, user, Failed), nil
		}
		if i >= len(waits) || ctx.Err() != nil {
			return h.done(kind, hospital, user, Failed), nil
		}
		h.sleep(ctx, waits[i])
	}
}

func (h *Handler) done(kind string, hospital, user uuid.UUID, outcome string) proto.Message {
	if kind != "invite" && kind != "password_reset" {
		kind = "other"
	}
	h.log().Info("notifications: link", "kind", kind, "hospital_id", hospital.String(), "recipient_id", user.String(),
		"outcome", outcome)
	return &notifypb.SendLinkReply{Outcome: outcome}
}
