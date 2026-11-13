package analysis

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
)

// names are the hospital's dictionary labels, read once per stored body.
// Dictionaries are insert only, so a label never changes under a stored
// body (and the body never changes anyway: it is stored rendered).
type names struct {
	conds map[string]condName
	locs  map[string]locName
}

type condName struct{ name, icd10 *string }

type locName struct {
	label   string
	setting *string
}

func loadNames(ctx context.Context, tx pgx.Tx) (names, error) {
	n := names{conds: map[string]condName{}, locs: map[string]locName{}}
	rows, err := tx.Query(ctx, `SELECT id::text, name, icd10 FROM analysis_conditions`)
	if err != nil {
		return n, err
	}
	for rows.Next() {
		var id string
		var c condName
		if err := rows.Scan(&id, &c.name, &c.icd10); err != nil {
			rows.Close()
			return n, err
		}
		n.conds[id] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return n, err
	}
	rows, err = tx.Query(ctx, `SELECT id::text, label, setting FROM analysis_locations`)
	if err != nil {
		return n, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var l locName
		if err := rows.Scan(&id, &l.label, &l.setting); err != nil {
			return n, err
		}
		n.locs[id] = l
	}
	return n, rows.Err()
}

// condRef is the contract's ConditionRef: {id, name, icd10}; name null
// reads "Not standardised".
func (n names) condRef(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	id, _ := m["id"].(string)
	c := n.conds[id]
	return map[string]any{"id": id, "name": c.name, "icd10": c.icd10}
}

// drillable adds the contract's drill to a Drillable count. Drill keys are
// derived at render with the drill-down route (plan departure 13, PR 5);
// until then every drill is null.
func drillable(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := map[string]any{"drill": nil}
	for k, x := range m {
		out[k] = x
	}
	return out
}

// renderView is one view as sent, without updated_at and history (added
// when served): disclose.ViewJSON, with condition and location names and
// the drill fields.
func renderView(b disclose.Body, v disclose.View, n names) (json.RawMessage, error) {
	raw, err := disclose.ViewJSON(b, v)
	if err != nil {
		return nil, err
	}
	var o map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&o); err != nil {
		return nil, err
	}
	rows := func(key string) []any { r, _ := o[key].([]any); return r }
	each := func(key string, fn func(map[string]any)) {
		for _, r := range rows(key) {
			if m, ok := r.(map[string]any); ok {
				fn(m)
			}
		}
	}
	switch v {
	case disclose.Prevalence:
		each("by_condition", func(m map[string]any) {
			m["condition"] = n.condRef(m["condition"])
			m["cases"] = drillable(m["cases"])
		})
	case disclose.Affected:
		for _, k := range []string{"by_age", "by_sex", "by_setting", "by_specialty"} {
			each(k, func(m map[string]any) { m["patients"] = drillable(m["patients"]) })
		}
		each("by_location", func(m map[string]any) {
			m["patients"] = drillable(m["patients"])
			if l, ok := m["location"].(map[string]any); ok {
				id, _ := l["id"].(string)
				m["location"] = map[string]any{"id": id, "label": n.locs[id].label, "setting": n.locs[id].setting}
			}
		})
	case disclose.CoOccurring:
		each("pairs", func(m map[string]any) {
			m["a"], m["b"] = n.condRef(m["a"]), n.condRef(m["b"])
			m["patients"] = drillable(m["patients"])
		})
	case disclose.Findings:
		// A finding's name is its condition's (the dictionary's): null when
		// it has none.
		each("findings", func(m map[string]any) {
			if id, ok := m["name"].(string); ok {
				m["name"] = n.conds[id].name
			}
			m["patients"] = drillable(m["patients"])
		})
	case disclose.Quality:
		each("by_condition", func(m map[string]any) { m["condition"] = n.condRef(m["condition"]) })
	}
	return json.Marshal(o)
}

// Served is a stored view as sent: Meta's updated_at (when the body was
// stored) and the history are added; nothing else changes.
func Served(view json.RawMessage, s Stored, history []disclose.HistoryPoint) ([]byte, error) {
	var o map[string]any
	dec := json.NewDecoder(bytes.NewReader(view))
	dec.UseNumber()
	if err := dec.Decode(&o); err != nil {
		return nil, ErrFailed
	}
	o["updated_at"] = s.StoredAt.UTC().Format("2006-01-02T15:04:05Z")
	pts := make([]any, 0, len(history))
	for _, h := range history {
		pts = append(pts, map[string]any{"start": h.Start, "cases": h.Cases, "new": h.New, "patients": h.Patients})
	}
	o["history"] = pts
	return json.Marshal(o)
}
