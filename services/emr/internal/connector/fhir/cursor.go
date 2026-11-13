package fhir

// The cursor (design §5.1; Review Focus 2): per searched resource type, the
// greatest meta.lastUpdated of the completed pages minus the 2-minute
// overlap, never moving backward and never past a failed page. It names
// the base address it belongs to: a cursor for another address is not
// this server's, and the next fetch is a first sync again. Per type,
// because the three searches finish at different points when a run hits
// its limit (plan departure, PR 6).

import (
	"encoding/json"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
)

const cursorVersion = 1

type cursor struct {
	V    int               `json:"v"`
	Base string            `json:"b"`
	LU   map[string]string `json:"lu,omitempty"`
	// Drained: the last fetch read every search to its end.
	Drained bool `json:"d,omitempty"`
	// From: re-read from here once (the daily reconciliation).
	From string `json:"r,omitempty"`
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// at is the type's position, zero when none.
func (c cursor) at(typ string) time.Time { return parseTime(c.LU[typ]) }

func decodeCursor(raw connector.Cursor) (cursor, bool) {
	var c cursor
	if len(raw) == 0 || len(raw) > 2048 || json.Unmarshal(raw, &c) != nil || c.V != cursorVersion || len(c.LU) > len(SearchTypes) {
		return cursor{}, false
	}
	return c, true
}

// cursorFor is raw for cfg, or an empty cursor (a first sync) when raw is
// missing, unreadable or another base address's.
func (c *FHIR) cursorFor(raw connector.Cursor, cfg connector.Config) cursor {
	cur, ok := decodeCursor(raw)
	if !ok || cur.Base != cfg.BaseURL {
		return cursor{V: cursorVersion, Base: cfg.BaseURL, LU: map[string]string{}}
	}
	if cur.LU == nil {
		cur.LU = map[string]string{}
	}
	return cur
}

func (cur cursor) encode() connector.Cursor {
	b, err := json.Marshal(cur)
	if err != nil {
		return nil
	}
	return b
}

// CaughtUp reports whether the fetch that wrote cur read everything.
func (c *FHIR) CaughtUp(raw connector.Cursor) bool {
	cur, ok := decodeCursor(raw)
	return ok && cur.Drained
}

// Rewind sets cur to read from since once more, keeping its positions.
func (c *FHIR) Rewind(raw connector.Cursor, since time.Time) connector.Cursor {
	cur, ok := decodeCursor(raw)
	if !ok {
		return raw
	}
	cur.From = since.UTC().Format(time.RFC3339Nano)
	return cur.encode()
}
