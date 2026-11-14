package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The named-access list and the letter a named entry names (design §8.8;
// contract §14.9; plan Task 25). Letters are read through 00044's
// mhai_audit_letters (mhai_platform holds no grant on clearances): the
// status, the letter's date and end date, and who verified it. Nothing of
// a letter's file, signer or notes.

// LetterPerson is the platform admin who verified a letter.
type LetterPerson struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Letter is a named entry's letter (mhai-web Letter): valid_from is the
// letter's date and valid_to its end date (departure 69); an active letter
// past its end date at its hospital is ended.
type Letter struct {
	ID         string        `json:"id"`
	Status     string        `json:"status"`
	ValidFrom  string        `json:"valid_from"`
	ValidTo    string        `json:"valid_to"`
	VerifiedBy *LetterPerson `json:"verified_by"`
}

// letterActions are the entries whose payload names a letter
// (clearance_id) that the detail shows.
var letterActions = []string{"analysis.named_viewed", "analysis.named_view_failed", "analysis.legal_resolved"}

// letterIDOf is the letter a named entry's checked payload names, or Nil.
func letterIDOf(r rawRow) uuid.UUID {
	if !slices.Contains(letterActions, r.row.Event.Action) || r.badPayload ||
		CheckActionPayload(r.row.Event.Action, r.row.Event.Payload) != nil {
		return uuid.Nil
	}
	s, _ := r.row.Event.Payload["clearance_id"].(string)
	id, err := uuid.Parse(s)
	if err != nil || id.String() != s {
		return uuid.Nil
	}
	return id
}

var letterStatuses = []string{"checking", "refused", "waiting", "active", "rejected", "ended", "revoked"}

// Letters reads the letters ids names (at most 1000), keyed by id. A
// letter whose status isn't one mhai-web knows is left out; a verifier
// whose name it wouldn't read is null.
func Letters(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) (map[uuid.UUID]Letter, error) {
	out := map[uuid.UUID]Letter{}
	if len(ids) == 0 {
		return out, nil
	}
	if len(ids) > 1000 {
		return nil, fmt.Errorf("audit: %d letters", len(ids))
	}
	rows, err := tx.Query(ctx, `SELECT l.id, l.status, l.letter_date, l.end_date, l.verified_by, u.display_name
		FROM mhai_audit_letters($1) l LEFT JOIN users u ON u.id = l.verified_by`, ids)
	if err != nil {
		return nil, fmt.Errorf("audit: letters: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var status string
		var from, to time.Time
		var by *uuid.UUID
		var name *string
		if err := rows.Scan(&id, &status, &from, &to, &by, &name); err != nil {
			return nil, fmt.Errorf("audit: letters: %w", err)
		}
		if !slices.Contains(letterStatuses, status) {
			continue
		}
		l := Letter{ID: id.String(), Status: status, ValidFrom: from.Format(time.DateOnly), ValidTo: to.Format(time.DateOnly)}
		if by != nil && name != nil && ShowableName(*name) {
			l.VerifiedBy = &LetterPerson{ID: by.String(), Name: *name}
		}
		out[id] = l
	}
	return out, rows.Err()
}

// NamedPage is the named-access list's reply (mhai-web NamedAccessList):
// each entry as the catalogue sends it, with its letter.
type NamedPage struct {
	Events     []NamedEvent `json:"events"`
	NextCursor *string      `json:"next_cursor"`
}

// NamedEvent is an entry flattened with its letter.
type NamedEvent struct {
	Entry  any
	Letter *Letter
}

// MarshalJSON flattens the entry with letter.
func (n NamedEvent) MarshalJSON() ([]byte, error) {
	raw, err := json.Marshal(n.Entry)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	m["letter"] = json.RawMessage("null")
	if n.Letter != nil {
		if m["letter"], err = json.Marshal(n.Letter); err != nil {
			return nil, err
		}
	}
	return json.Marshal(m)
}

// ListNamed is one page of the named-access list (f.Named), newest first,
// after the cursor's id (0: the first page). cur signs the next cursor.
func ListNamed(ctx context.Context, tx pgx.Tx, cur Cursors, f Filters, after int64, limit int) (NamedPage, error) {
	if len(cur.Key) < 32 || cur.Session == "" {
		return NamedPage{}, ErrCursorKey
	}
	if !f.Named || limit < 1 || limit > MaxLimit {
		return NamedPage{}, fmt.Errorf("audit: not a named-access page")
	}
	q := &query{}
	if err := q.filter(f); err != nil {
		return NamedPage{}, err
	}
	if after > 0 {
		q.where = append(q.where, "id < "+q.arg(after))
	}
	rows, err := tx.Query(ctx, `SELECT `+rowColumns+` FROM audit_log WHERE `+q.clause()+
		` ORDER BY id DESC LIMIT `+strconv.Itoa(limit+1), q.args...)
	if err != nil {
		return NamedPage{}, fmt.Errorf("audit: named list: %w", err)
	}
	raws, err := scanRows(rows)
	if err != nil {
		return NamedPage{}, fmt.Errorf("audit: named list: %w", err)
	}
	out := NamedPage{Events: []NamedEvent{}}
	if len(raws) > limit {
		raws = raws[:limit]
		c := cur.Encode(raws[len(raws)-1].row.Seq, f)
		out.NextCursor = &c
	}
	es, err := entries(ctx, tx, raws)
	if err != nil {
		return NamedPage{}, err
	}
	var ids []uuid.UUID
	for _, r := range raws {
		if id := letterIDOf(r); id != uuid.Nil && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	ls, err := Letters(ctx, tx, ids)
	if err != nil {
		return NamedPage{}, err
	}
	for i, r := range raws {
		ev := NamedEvent{Entry: es[i]}
		if l, ok := ls[letterIDOf(r)]; ok {
			ev.Letter = &l
		}
		out.Events = append(out.Events, ev)
	}
	return out, nil
}
