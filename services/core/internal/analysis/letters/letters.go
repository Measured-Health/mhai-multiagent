// Package letters serves clearance letters on the hospital side (analysis
// backend design §8.1, §9; plan Tasks 18-20, migration 00034): the list,
// one letter, its pages, upload, revoke and narrow, for hospital admins
// with two-step, while the hospital's analysis switch is on (404
// otherwise) and its named-access position is letters_only or on (403
// named_access_off otherwise). Off by default: no hospital has a
// named-access row until the CLI raises the demo hospital.
//
// The rules live in the database (00034's triggers and functions, for
// every writer): this package only calls the functions and appends the
// audit entries the database demands before it lets the transaction
// commit. A letter is never parsed here: a live upload is 503 until the
// documents service exists, and the demo keeps only the file's SHA-256 and
// serves fixture pages drawn in code. Nothing a letter holds (a signer, a
// note, an email, a file name) is logged or audited: ids and codes only.
package letters

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
)

// Deps are what the routes need. A nil Pool registers the routes for the
// permission table only.
type Deps struct {
	Pool *pgxpool.Pool
	Auth *auth.Service
	Log  *slog.Logger
	// stepUp replaces Auth's step-up check (tests only).
	stepUpCheck func(r *http.Request, mfa, backup string) error
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// refusal is a rule the database refused with: its problem code.
type refusal struct{ code string }

func (r refusal) Error() string { return "letters: refused: " + r.code }

// refusals maps 00034's P0001 messages to problem codes.
var refusals = map[string]string{
	"not_found":             "not_found",
	"named_access_off":      "named_access_off",
	"forbidden":             "forbidden",
	"conflict":              "conflict",
	"would_empty":           "would_empty",
	"end_date_too_late":     "end_date_too_late",
	"not_eligible":          "not_eligible",
	"not_eligible_platform": "not_eligible_platform",
	"invalid":               "validation_failed",
	"unavailable":           "unavailable",
	"rate_limited":          "rate_limited",
	// The platform side's checks (Plan 11b); no route here reaches them.
	"same_person": "forbidden",
	"same_side":   "forbidden",
}

// codeOf is the problem code for err, and whether it is a refusal (an
// expected outcome) rather than a failure.
func codeOf(err error) (string, bool) {
	var rf refusal
	if errors.As(err, &rf) {
		return rf.code, true
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		if pe.Code == "P0001" {
			if c, ok := refusals[pe.Message]; ok {
				return c, true
			}
		}
		if pe.Code == "23505" { // a person named twice
			return "validation_failed", true
		}
	}
	if errors.Is(err, auth.ErrMFAInvalid) {
		return "mfa_invalid", true
	}
	return "not_ready", false
}

// Who is a person, by display name only.
type Who struct {
	DisplayName string `json:"display_name"`
}

// CoveredOut is a person a letter covers.
type CoveredOut struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// EventOut is one line of a letter's history.
type EventOut struct {
	At      string       `json:"at"`
	Kind    string       `json:"kind"`
	By      *Who         `json:"by"`
	Side    *string      `json:"side"`
	Reason  *string      `json:"reason"`
	Note    *string      `json:"note"`
	Removed []CoveredOut `json:"removed,omitempty"`
}

// RejectionOut is a platform rejection.
type RejectionOut struct {
	Reason string  `json:"reason"`
	Note   *string `json:"note"`
	At     string  `json:"at"`
}

// RevocationOut is a revocation; Day is At as the hospital's day.
type RevocationOut struct {
	By     Who     `json:"by"`
	Side   string  `json:"side"`
	At     string  `json:"at"`
	Day    string  `json:"day"`
	Reason string  `json:"reason"`
	Note   *string `json:"note"`
}

// ClearanceOut is one letter (contract §10, ClearanceOut).
type ClearanceOut struct {
	ID              string         `json:"id"`
	Status          string         `json:"status"`
	RefusedReason   *string        `json:"refused_reason"`
	SignedBy        string         `json:"signed_by"`
	SignerTitle     string         `json:"signer_title"`
	LetterDate      string         `json:"letter_date"`
	Covered         []CoveredOut   `json:"covered"`
	CoveredPlatform []CoveredOut   `json:"covered_platform"`
	EndDate         string         `json:"end_date"`
	UploadedBy      Who            `json:"uploaded_by"`
	UploadedAt      string         `json:"uploaded_at"`
	VerifiedBy      *Who           `json:"verified_by"`
	VerifiedAt      *string        `json:"verified_at"`
	Rejection       *RejectionOut  `json:"rejection"`
	Revocation      *RevocationOut `json:"revocation"`
	Renews          *string        `json:"renews"`
	Pages           int            `json:"pages"`
	History         []EventOut     `json:"history"`
	Version         int            `json:"version"`
}

// ClearancesOut is the list.
type ClearancesOut struct {
	Clearances []ClearanceOut `json:"clearances"`
	Today      string         `json:"today"`
	MaxEndDate string         `json:"max_end_date"`
}

// MaxList bounds the list.
const MaxList = 200

// unknownName stands for an account that no longer exists.
const unknownName = "Former account"

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func day(t time.Time) string { return t.Format(time.DateOnly) }

// today is the hospital's day and the latest end date a letter may have.
type today struct {
	Today, MaxEndDate time.Time
	Demo              bool
}

func readToday(ctx context.Context, tx pgx.Tx) (today, error) {
	var t today
	var zone string
	err := tx.QueryRow(ctx, `SELECT today, max_end_date, time_zone, demo FROM mhai_clearance_today()`).
		Scan(&t.Today, &t.MaxEndDate, &zone, &t.Demo)
	return t, err
}

// load reads the hospital's letters (all, newest first, at most MaxList,
// or the one with id) under RLS as the caller, with display names.
func load(ctx context.Context, tx pgx.Tx, id *uuid.UUID) ([]ClearanceOut, error) {
	rows, err := tx.Query(ctx, `SELECT c.id,
			CASE WHEN c.status = 'active' AND c.end_date < t.today THEN 'ended' ELSE c.status END,
			c.refused_reason, c.signed_by, c.signer_title, c.letter_date, c.end_date, c.renews, c.uploaded_by, c.uploaded_at,
			c.verified_by, c.verified_at, c.rejected_at, c.reject_reason, c.reject_note,
			c.revoked_by, c.revoked_at, (c.revoked_at AT TIME ZONE t.time_zone)::date, c.revoked_side, c.revoke_reason, c.revoke_note,
			c.pages, c.version
		FROM clearances c, mhai_clearance_today() t
		WHERE ($1::uuid IS NULL OR c.id = $1)
		ORDER BY c.uploaded_at DESC, c.id LIMIT $2`, id, MaxList)
	if err != nil {
		return nil, err
	}
	type raw struct {
		out                     ClearanceOut
		uploadedBy              uuid.UUID
		verifiedBy, revokedBy   *uuid.UUID
		verifiedAt, rejectedAt  *time.Time
		revokedAt, revokedDay   *time.Time
		rejectReason, revSide   *string
		rejectNote, revReason   *string
		revokeNote              *string
		renews                  *uuid.UUID
		letterDate, endDate, up time.Time
	}
	var all []raw
	var ids []uuid.UUID
	for rows.Next() {
		var x raw
		var cid uuid.UUID
		if err := rows.Scan(&cid, &x.out.Status, &x.out.RefusedReason, &x.out.SignedBy, &x.out.SignerTitle, &x.letterDate,
			&x.endDate, &x.renews, &x.uploadedBy, &x.up, &x.verifiedBy, &x.verifiedAt, &x.rejectedAt, &x.rejectReason,
			&x.rejectNote, &x.revokedBy, &x.revokedAt, &x.revokedDay, &x.revSide, &x.revReason, &x.revokeNote,
			&x.out.Pages, &x.out.Version); err != nil {
			rows.Close()
			return nil, err
		}
		x.out.ID = cid.String()
		all = append(all, x)
		ids = append(ids, cid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return []ClearanceOut{}, nil
	}

	type person struct {
		id   uuid.UUID
		side string
	}
	covered := map[uuid.UUID][]person{}
	rows, err = tx.Query(ctx, `SELECT clearance_id, user_id, side FROM clearance_covered
		WHERE clearance_id = ANY ($1) AND removed_at IS NULL ORDER BY side, user_id`, ids)
	if err != nil {
		return nil, err
	}
	var people []uuid.UUID
	for rows.Next() {
		var c uuid.UUID
		var p person
		if err := rows.Scan(&c, &p.id, &p.side); err != nil {
			rows.Close()
			return nil, err
		}
		covered[c] = append(covered[c], p)
		people = append(people, p.id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	type event struct {
		at                 time.Time
		kind               string
		by, person         *uuid.UUID
		side, reason, note *string
	}
	events := map[uuid.UUID][]event{}
	rows, err = tx.Query(ctx, `SELECT clearance_id, at, kind, by_user, side, reason, note, person FROM clearance_events
		WHERE clearance_id = ANY ($1) ORDER BY seq`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c uuid.UUID
		var e event
		if err := rows.Scan(&c, &e.at, &e.kind, &e.by, &e.side, &e.reason, &e.note, &e.person); err != nil {
			rows.Close()
			return nil, err
		}
		events[c] = append(events[c], e)
		for _, u := range []*uuid.UUID{e.by, e.person} {
			if u != nil {
				people = append(people, *u)
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, x := range all {
		people = append(people, x.uploadedBy)
		for _, u := range []*uuid.UUID{x.verifiedBy, x.revokedBy} {
			if u != nil {
				people = append(people, *u)
			}
		}
	}

	names := map[uuid.UUID]string{}
	rows, err = tx.Query(ctx, `SELECT id, display_name FROM mhai_clearance_names($1)`, people)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var u uuid.UUID
		var n string
		if err := rows.Scan(&u, &n); err != nil {
			rows.Close()
			return nil, err
		}
		names[u] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	name := func(u uuid.UUID) string {
		if n, ok := names[u]; ok {
			return n
		}
		return unknownName
	}

	out := make([]ClearanceOut, 0, len(all))
	for i, x := range all {
		c := x.out
		cid := ids[i]
		c.LetterDate, c.EndDate, c.UploadedAt = day(x.letterDate), day(x.endDate), stamp(x.up)
		c.UploadedBy = Who{name(x.uploadedBy)}
		c.Covered, c.CoveredPlatform = []CoveredOut{}, []CoveredOut{}
		for _, p := range covered[cid] {
			o := CoveredOut{ID: p.id.String(), DisplayName: name(p.id)}
			if p.side == "platform" {
				// A platform admin is shown only once Measured Health has
				// verified a letter of this hospital naming them (security
				// review: the upload is not a lookup of platform accounts).
				if _, known := names[p.id]; known && x.verifiedAt != nil {
					c.CoveredPlatform = append(c.CoveredPlatform, o)
				}
			} else {
				c.Covered = append(c.Covered, o)
			}
		}
		if x.verifiedBy != nil && x.verifiedAt != nil {
			c.VerifiedBy = &Who{name(*x.verifiedBy)}
			s := stamp(*x.verifiedAt)
			c.VerifiedAt = &s
		}
		if x.rejectedAt != nil && x.rejectReason != nil {
			c.Rejection = &RejectionOut{Reason: *x.rejectReason, Note: x.rejectNote, At: stamp(*x.rejectedAt)}
		}
		if x.revokedAt != nil && x.revokedDay != nil && x.revReason != nil && x.revSide != nil {
			by := unknownName
			if x.revokedBy != nil {
				by = name(*x.revokedBy)
			}
			c.Revocation = &RevocationOut{By: Who{by}, Side: *x.revSide, At: stamp(*x.revokedAt), Day: day(*x.revokedDay),
				Reason: *x.revReason, Note: x.revokeNote}
		}
		if x.renews != nil {
			s := x.renews.String()
			c.Renews = &s
		}
		c.History = []EventOut{}
		for _, e := range events[cid] {
			o := EventOut{At: stamp(e.at), Kind: e.kind, Side: e.side, Reason: e.reason, Note: e.note}
			if e.by != nil {
				o.By = &Who{name(*e.by)}
			}
			if e.person != nil {
				// Only a person this hospital may see (a platform admin
				// removed before verification is not named).
				if n, known := names[*e.person]; known {
					o.Removed = []CoveredOut{{ID: e.person.String(), DisplayName: n}}
				} else {
					o.Removed = []CoveredOut{}
				}
			}
			c.History = append(c.History, o)
		}
		out = append(out, c)
	}
	return out, nil
}

// AuditPending appends one entry per letter event of this transaction
// still without one (mhai_clearance_pending), in order: 00034 refuses to
// commit an event without its entry. The payload is the event's id and
// codes only. It returns how many it appended.
func AuditPending(ctx context.Context, tx pgx.Tx, requestID string) (int, error) {
	type pend struct {
		event, hospital, letter uuid.UUID
		kind                    string
		by, person              *uuid.UUID
		side, reason            *string
		days                    *int
		pages                   int
	}
	rows, err := tx.Query(ctx, `SELECT event_id, hospital_id, clearance_id, kind, by_user, side, reason, person, days, pages
		FROM mhai_clearance_pending()`)
	if err != nil {
		return 0, err
	}
	var all []pend
	for rows.Next() {
		var p pend
		if err := rows.Scan(&p.event, &p.hospital, &p.letter, &p.kind, &p.by, &p.side, &p.reason, &p.person, &p.days,
			&p.pages); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, p := range all {
		payload := map[string]any{"event_id": p.event.String()}
		action := "clearance." + p.kind
		str := func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		}
		switch p.kind {
		case "ready":
			payload["pages"] = p.pages
		case "refused", "rejected":
			payload["reason"] = str(p.reason)
		case "narrowed":
			payload["reason"] = str(p.reason)
			if p.person != nil {
				payload["person"] = p.person.String()
			}
		case "revoked":
			payload["side"], payload["reason"] = str(p.side), str(p.reason)
		case "reminded":
			action = "clearance.reminder_sent"
			if p.days != nil {
				payload["days"] = *p.days
			}
			payload["delivery"] = "pending"
		}
		actor := ""
		if p.by != nil {
			actor = p.by.String()
		}
		if err := audit.Write(ctx, tx, audit.Event{ActorID: actor, HospitalID: p.hospital.String(), RequestID: requestID,
			Action: action, Entity: "clearance", EntityID: p.letter.String(), Payload: payload}); err != nil {
			return 0, err
		}
	}
	return len(all), nil
}
