package analysis

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
)

// Named access (design §8.2-§8.5; plan Tasks 21-22, migration 00042): who
// is covered, and the named list under the letter that covers the person.
// The database holds every rule (the cover, the budget, the key, the
// cap); these are its calls, run in the request's transaction as the
// person (PersonTx).

// The named list's refusals, each one problem code in internal/analysis/routes.
var (
	ErrNamedOff       = errors.New("analysis: named access off")         // 403 named_access_off
	ErrNotCleared     = errors.New("analysis: not cleared")              // 403 not_cleared
	ErrClearanceEnded = errors.New("analysis: clearance ended")          // 403 clearance_ended
	ErrNotAllowed     = errors.New("analysis: role or account")          // 403 forbidden
	ErrIdemMismatch   = errors.New("analysis: idempotency key mismatch") // 422 idempotency_mismatch
	ErrExpired        = errors.New("analysis: named list expired")       // 409 expired
	ErrAnalysisOff    = errors.New("analysis: off")                      // 404 not_found
)

// Purposes are a named list's purposes (design §8.3).
var Purposes = []string{"clinical_follow_up", "quality_improvement", "audit", "other"}

// Cover is mhai_named_cover's answer for the session's person.
type Cover struct {
	Named     bool
	Until     *time.Time
	Clearance *uuid.UUID
	Version   *int
	EndDate   *time.Time
	Reason    *string
}

// NamedCover is who the session's person is covered by, now (nothing
// cached). ErrAnalysisOff while analysis is off.
func NamedCover(ctx context.Context, tx pgx.Tx, user uuid.UUID) (Cover, error) {
	var c Cover
	err := tx.QueryRow(ctx, `SELECT named, until, clearance_id, version, end_date, reason FROM mhai_named_cover($1)`, user).
		Scan(&c.Named, &c.Until, &c.Clearance, &c.Version, &c.EndDate, &c.Reason)
	if err != nil {
		return Cover{}, namedErr(err)
	}
	return c, nil
}

// CoverRefusal is the named route's refusal for a cover that doesn't name
// the person (nil when it does).
func CoverRefusal(c Cover) error {
	if c.Named {
		return nil
	}
	r := ""
	if c.Reason != nil {
		r = *c.Reason
	}
	switch r {
	case "switched_off":
		return ErrNamedOff
	case "ended":
		return ErrClearanceEnded
	case "role_or_account":
		return ErrNotAllowed
	}
	return ErrNotCleared
}

// NamedState is the hospital's effective named-access position and since.
func NamedState(ctx context.Context, tx pgx.Tx) (string, *time.Time, error) {
	var pos string
	var since *time.Time
	if err := tx.QueryRow(ctx, `SELECT "position", since FROM mhai_named_access_state()`).Scan(&pos, &since); err != nil {
		return "", nil, namedErr(err)
	}
	return pos, since, nil
}

// NamedAdmit reserves one of the person's named views for the hospital's
// day (replays count); false past limit. Run in the request's
// transaction: a refusal there rolls the slot back, and the list or
// replay opens only in the transaction that reserved it.
func NamedAdmit(ctx context.Context, tx pgx.Tx, limit int) (bool, error) {
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT mhai_analysis_named_admit($1)`, limit).Scan(&ok); err != nil {
		return false, namedErr(err)
	}
	return ok, nil
}

// KeyState is what an Idempotency-Key is: new, replay (with its list),
// mismatch or expired, under a lock on the key held to commit.
func KeyState(ctx context.Context, tx pgx.Tx, key, body []byte) (string, uuid.UUID, error) {
	var state string
	var view *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT state, view_id FROM mhai_analysis_named_key($1, $2)`, key, body).Scan(&state, &view); err != nil {
		return "", uuid.Nil, namedErr(err)
	}
	if view == nil {
		return state, uuid.Nil, nil
	}
	return state, *view, nil
}

// KeyHash is the stored form of an Idempotency-Key (never the key),
// bound to the hospital and the person: one person's key is never another
// person's, so a key is no probe of what others sent (departure 57).
func KeyHash(hospital, user uuid.UUID, key string) []byte {
	h := sha256.New()
	h.Write([]byte("analysis/named-key/v2\x00"))
	h.Write(hospital[:])
	h.Write(user[:])
	h.Write([]byte(key))
	return h.Sum(nil)
}

// BodyMAC binds a key to one request body: an HMAC, under the drill key
// with its own label, of the canonical body. A stored key holds no
// filter, note or drill (departure 57).
func (k *Keys) BodyMAC(canonical []byte) []byte {
	s := k.Drill([]byte("analysis/named-body/v1"), canonical)
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil
	}
	return b
}

// NamedList is one named list to open or replay.
type NamedList struct {
	View    uuid.UUID
	Period  Period
	F       disclose.Filters
	G       DrillGroup
	Drill   string
	Purpose string
	// Note is the purpose note sealed with the note key (nil: none).
	Note      []byte
	Key, Body []byte
}

// Named is a named list as recorded: its rows (pid first, as the
// de-identified list's), each subject's encrypted reference in the same
// order, the letter, named_until and the replay's number (0: the list).
type Named struct {
	Rows         []PatientRow
	Refs         [][]byte
	Clearance    uuid.UUID
	ClearanceEnd time.Time
	Demo         bool
	Until        time.Time
	Replays      int
}

// OpenNamed opens l under the letter that covers the session's person.
// The caller appends analysis.named_viewed in the same transaction: a
// list without it is refused at commit.
func OpenNamed(ctx context.Context, tx pgx.Tx, l NamedList) (Named, error) {
	g, err := json.Marshal(l.G.Group)
	if err != nil {
		return Named{}, ErrFailed
	}
	drill := sha256.Sum256([]byte(l.Drill))
	rows, err := tx.Query(ctx, `SELECT ord, pid, age_band, sex, setting, conditions::text, findings::text, ref_enc,
			clearance_id, clearance_end, demo, named_until, replays
		FROM mhai_analysis_open_named($1, $2, $3::jsonb, $4::jsonb, $5, $6, $7, $8, $9, $10, $11, $12)`,
		l.View, l.Period.ID, filtersJSON(l.F), string(g), string(l.G.View), drill[:], l.G.Count, disclose.PatientCap,
		l.Purpose, l.Note, l.Key, l.Body)
	if err != nil {
		return Named{}, namedErr(err)
	}
	return scanNamed(ctx, tx, rows)
}

// ReplayNamed replays the session person's named list behind l.Key: every
// check again, the stored rows. The caller appends analysis.named_viewed
// with the replay's number in the same transaction.
func ReplayNamed(ctx context.Context, tx pgx.Tx, l NamedList) (Named, error) {
	g, err := json.Marshal(l.G.Group)
	if err != nil {
		return Named{}, ErrFailed
	}
	drill := sha256.Sum256([]byte(l.Drill))
	rows, err := tx.Query(ctx, `SELECT ord, pid, age_band, sex, setting, conditions::text, findings::text, ref_enc,
			clearance_id, clearance_end, demo, named_until, replays
		FROM mhai_analysis_named_replay($1, $2, $3, $4::jsonb, $5::jsonb, $6, $7, $8)`,
		l.Key, l.Body, l.Period.ID, filtersJSON(l.F), string(g), drill[:], l.G.Count, disclose.PatientCap)
	if err != nil {
		return Named{}, namedErr(err)
	}
	return scanNamed(ctx, tx, rows)
}

func scanNamed(ctx context.Context, tx pgx.Tx, rows pgx.Rows) (Named, error) {
	var out Named
	var got []rawRow
	for rows.Next() {
		var r rawRow
		var ord int
		var ref []byte
		if err := rows.Scan(&ord, &r.row.PID, &r.row.AgeBand, &r.row.Sex, &r.row.Setting, &r.conds, &r.finds, &ref,
			&out.Clearance, &out.ClearanceEnd, &out.Demo, &out.Until, &out.Replays); err != nil {
			rows.Close()
			return Named{}, namedErr(err)
		}
		got = append(got, r)
		out.Refs = append(out.Refs, ref)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Named{}, namedErr(err)
	}
	if len(got) == 0 || len(got) > disclose.DrillMax {
		return Named{}, ErrFailed
	}
	var err error
	if out.Rows, err = buildRows(ctx, tx, got); err != nil {
		return Named{}, err
	}
	return out, nil
}

// namedErr maps the named functions' refusals to their errors, and
// anything else to ErrFailed (never the driver's text).
func namedErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "P0001" {
		switch pe.Message {
		case "too_small":
			return ErrTooSmall
		case "patient_cap":
			return ErrPatientCap
		case "named_access_off":
			return ErrNamedOff
		case "not_cleared":
			return ErrNotCleared
		case "clearance_ended":
			return ErrClearanceEnded
		case "forbidden":
			return ErrNotAllowed
		case "idempotency_mismatch":
			return ErrIdemMismatch
		case "expired":
			return ErrExpired
		case "not_found":
			return ErrAnalysisOff
		}
	}
	if errors.As(err, &pe) && pe.Code == "22023" {
		// Not a drillable group of a frozen period of this hospital.
		return ErrTooSmall
	}
	return failed(err)
}
