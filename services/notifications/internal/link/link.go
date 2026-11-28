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
	Store Store
	// Platform reads a platform invite's recipient (HandlePlatform); nil
	// refuses every platform link as invalid.
	Platform PlatformStore
	Email    channel.Channel // nil: email is off, nothing is sent
	Limits   deliver.Limits
	Log      *slog.Logger
	Now      func() time.Time
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
	return h.send(ctx, m), nil
}

// PlatformSender is the name a platform team member's invite comes from:
// the account belongs to no hospital.
const PlatformSender = "Measured Health"

// PlatformStore is what HandlePlatform needs of the database.
type PlatformStore interface {
	// PlatformRecipient is notify_platform_link_recipient: the address of
	// a platform account with a live invite made in the last 2 minutes;
	// ok false for nobody.
	PlatformRecipient(ctx context.Context, user uuid.UUID, purpose string) (address string, ok bool, err error)
}

// PlatformRecipient calls notify_platform_link_recipient as mhai_notify
// with no hospital set: the definer reads only platform accounts.
func (p PG) PlatformRecipient(ctx context.Context, user uuid.UUID, purpose string) (string, bool, error) {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+string(dbx.Notify)); err != nil {
		return "", false, err
	}
	var addr string
	err = tx.QueryRow(ctx, `SELECT address FROM notify_platform_link_recipient($1, $2)`, user, purpose).Scan(&addr)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return addr, true, tx.Commit(ctx)
}

// HandlePlatform serves send_platform_link: a platform team member's
// invite, for an account with no hospital. The token names no hospital
// (the responder checked it is core's, for this subject); one that names
// one is refused as invalid. Only an invite; the platform's links share
// one bucket of PerHospitalPerMinute a minute (the nil hospital). The
// per-person hourly allowance (message_send_counts) is per hospital, so it
// doesn't apply: core makes a new account per invite (an email is never
// invited twice) and holds every invite to its step-up and a minute's
// limit per session.
func (h *Handler) HandlePlatform(ctx context.Context, c reqtoken.Claims, req proto.Message) (proto.Message, error) {
	r, ok := req.(*notifypb.SendLinkRequest)
	if c.Hid != "" || !ok || h.Platform == nil {
		return h.done("", uuid.Nil, uuid.Nil, Invalid), nil
	}
	now := h.now()
	user, ok := canonical(r.GetUserId())
	kind := r.GetPurpose()
	exp := time.Unix(r.GetExpiresAt(), 0)
	if !ok || kind != "invite" || r.GetLink() == "" || len(r.GetLink()) > 2048 ||
		!exp.After(now) || exp.After(now.Add(7*24*time.Hour+time.Minute)) {
		return h.done(kind, uuid.Nil, user, Invalid), nil
	}
	if h.Email == nil {
		return h.done(kind, uuid.Nil, user, NotReady), nil
	}
	if !h.allow(uuid.Nil, now) {
		return h.done(kind, uuid.Nil, user, RateLimited), nil
	}
	addr, found, err := h.Platform.PlatformRecipient(ctx, user, kind)
	switch {
	case err != nil:
		return h.done(kind, uuid.Nil, user, NotReady), nil
	case !found:
		return h.done(kind, uuid.Nil, user, NoRecipient), nil
	}
	m := channel.Message{ID: uuid.New(), Recipient: user, Kind: kind, Address: addr,
		Link: r.GetLink(), Expires: templates.FormatExpiry(exp), HospitalName: PlatformSender}
	return h.send(ctx, m), nil
}

// send tries m up to 3 times and answers the outcome.
func (h *Handler) send(ctx context.Context, m channel.Message) proto.Message {
	kind, hospital, user := m.Kind, m.Hospital, m.Recipient
	for i := 0; ; i++ {
		tctx, cancel := context.WithTimeout(ctx, tryTimeout)
		res, err := h.Email.Send(tctx, m)
		cancel()
		switch {
		case err == nil && res.Outcome == channel.Accepted:
			return h.done(kind, hospital, user, Accepted)
		case err == nil && res.Outcome == channel.Permanent:
			return h.done(kind, hospital, user, Failed)
		}
		if i >= len(waits) || ctx.Err() != nil {
			return h.done(kind, hospital, user, Failed)
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
