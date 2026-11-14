package hospital

// GET /hospital/activity?type&person&staff_id&from&to&flagged&cursor&limit
// (frontend requirements §5.3): the caller's hospital's own audit entries,
// newest first, the last 12 months, as structured events. Only the actions
// Activity maps are shown; each event carries codes, display names and
// the roles a self role change added, never a payload's ids, free text, a
// question or a patient. Row-level security scopes the chain to the
// hospital (00003's gateway_read).

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// ActivityTypes are the contract's event types.
var ActivityTypes = []string{"sign_in", "consultation", "feedback", "review", "account", "admin", "security"}

// activityKind is how one action is shown.
type activityKind struct{ typ, kind string }

// Type is the event type the action is shown under.
func (k activityKind) Type() string { return k.typ }

// Kind is the event kind the action is shown as.
func (k activityKind) Kind() string { return k.kind }

// ActivityActions maps each action Activity shows to its type and kind.
// auth.login.succeeded is signed_in, or signed_in_without_mfa without a
// second factor. Core flags no event today: self role adds are refused
// (ruling, 14 Nov), so flagged=true answers an empty page.
var ActivityActions = map[string]activityKind{
	"auth.login.succeeded":              {"sign_in", "signed_in"},
	"auth.login.locked":                 {"sign_in", "sign_in_locked"},
	"auth.login.ceiling":                {"sign_in", "sign_in_locked"},
	"query.submitted":                   {"consultation", "consultation_run"},
	"feedback.validated":                {"feedback", "result_agreed"},
	"correction.submitted":              {"feedback", "correction_suggested"},
	"correction.approved":               {"review", "correction_approved"},
	"correction.rejected":               {"review", "correction_rejected"},
	"correction.withdrawn":              {"review", "correction_withdrawn"},
	"staff.roles_changed":               {"admin", "roles_changed"},
	"staff.specialty_changed":           {"admin", "specialty_changed"},
	"staff.deactivated":                 {"admin", "deactivated"},
	"staff.reactivated":                 {"admin", "reactivated"},
	"analyst.granted":                   {"admin", "analyst_granted"},
	"analyst.removed":                   {"admin", "analyst_removed"},
	"hospital_settings.changed":         {"admin", "settings_changed"},
	"alert_settings.units_changed":      {"admin", "alert_units_changed"},
	"alert_settings.rules_changed":      {"admin", "alert_rules_changed"},
	"alert_settings.recipients_changed": {"admin", "alert_recipients_changed"},
}

// Activity bounds.
const (
	activityDefaultLimit = 50
	activityMaxLimit     = 100
	activityMaxPerson    = 64
	activityWindow       = 365 * 24 * time.Hour
)

// PersonOut names a person by display name.
type PersonOut struct {
	DisplayName string `json:"display_name"`
}

// EventOut is ActivityEventOut.
type EventOut struct {
	ID         string     `json:"id"`
	At         string     `json:"at"`
	Type       string     `json:"type"`
	Kind       string     `json:"kind"`
	Actor      *PersonOut `json:"actor"`
	Target     *PersonOut `json:"target"`
	Specialty  *string    `json:"specialty"`
	Confidence *string    `json:"confidence"`
	Device     *string    `json:"device"`
	Count      *int       `json:"count"`
	Flag       *string    `json:"flag"`
}

// ActivityPage is ActivityPageOut.
type ActivityPage struct {
	Events     []EventOut `json:"events"`
	NextCursor *string    `json:"next_cursor"`
}

// ActivityQuery is a checked query.
type ActivityQuery struct {
	Type     string
	Person   string
	StaffID  *uuid.UUID
	From, To *time.Time
	Flagged  bool
	Cursor   *int64
	Limit    int
}

// ParseActivityQuery checks the query: known keys, each once, each of its
// shape (errInvalid otherwise).
func ParseActivityQuery(raw string) (ActivityQuery, error) {
	q := ActivityQuery{Limit: activityDefaultLimit}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return q, errInvalid
	}
	for k, vs := range vals {
		if len(vs) != 1 {
			return q, errInvalid
		}
		v := vs[0]
		switch k {
		case "type":
			if !slices.Contains(ActivityTypes, v) {
				return q, errInvalid
			}
			q.Type = v
		case "person":
			v = strings.TrimSpace(v)
			if v == "" || len([]rune(v)) > activityMaxPerson {
				return q, errInvalid
			}
			q.Person = v
		case "staff_id":
			id, ok := canonicalID(v)
			if !ok {
				return q, errInvalid
			}
			q.StaffID = &id
		case "from", "to":
			t, ok := parseInstant(v)
			if !ok {
				return q, errInvalid
			}
			if k == "from" {
				q.From = &t
			} else {
				q.To = &t
			}
		case "flagged":
			switch v {
			case "true":
				q.Flagged = true
			case "false":
			default:
				return q, errInvalid
			}
		case "cursor":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 1 || strconv.FormatInt(n, 10) != v {
				return q, errInvalid
			}
			q.Cursor = &n
		case "limit":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > activityMaxLimit || strconv.Itoa(n) != v {
				return q, errInvalid
			}
			q.Limit = n
		default:
			return q, errInvalid
		}
	}
	if q.From != nil && q.To != nil && !q.From.Before(*q.To) {
		return q, errInvalid
	}
	return q, nil
}

// parseInstant takes an RFC 3339 time or a date (the day's start, UTC).
func parseInstant(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// likePattern escapes s for ILIKE ... ESCAPE '\' and wraps it in %.
func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

// actionsFor lists the actions of type typ ("" for all).
func actionsFor(typ string) []string {
	actions := []string{}
	for a, k := range ActivityActions {
		if typ == "" || k.typ == typ {
			actions = append(actions, a)
		}
	}
	slices.Sort(actions)
	return actions
}

func (d Deps) activity(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	q, err := ParseActivityQuery(r.URL.RawQuery)
	if err != nil {
		d.fail(w, r, p, "activity", err)
		return
	}
	ctx := r.Context()
	now := d.now()
	from := now.Add(-activityWindow)
	if q.From != nil && q.From.After(from) {
		from = *q.From
	}
	to := now.Add(time.Minute)
	if q.To != nil && q.To.Before(to) {
		to = *q.To
	}
	actions := actionsFor(q.Type)
	if q.Flagged {
		actions = []string{} // nothing is flagged
	}
	var person *string
	if q.Person != "" {
		s := likePattern(q.Person)
		person = &s
	}
	var staff *string
	if q.StaffID != nil {
		s := q.StaffID.String()
		staff = &s
	}
	sql := `SELECT a.id, a.ts, a.action, ua.display_name, ut.display_name, a.payload
		FROM audit_log a
		LEFT JOIN users ua ON ua.id = a.actor_id AND ua.hospital_id = a.hospital_id
		LEFT JOIN users ut ON a.entity = 'user' AND ut.id::text = a.entity_id AND ut.hospital_id = a.hospital_id
		WHERE a.hospital_id = $1 AND a.action = ANY($2::text[])
		  AND a.ts >= $3 AND a.ts < $4
		  AND ($5::bigint IS NULL OR a.id < $5)
		  AND ($6::text IS NULL OR a.actor_id::text = $6 OR (a.entity = 'user' AND a.entity_id = $6))
		  AND ($7::text IS NULL OR ua.display_name ILIKE $7 ESCAPE '\' OR ut.display_name ILIKE $7 ESCAPE '\')
		ORDER BY a.id DESC LIMIT $8`
	page := ActivityPage{Events: []EventOut{}}
	err = d.tx(ctx, func(tx pgx.Tx) error {
		// A filter that matches nothing walks the year's entries: bounded.
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '5s'`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, sql, p.HospitalID, actions, from, to, q.Cursor, staff, person, q.Limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var at time.Time
			var action string
			var actor, target *string
			var payload []byte
			if err := rows.Scan(&id, &at, &action, &actor, &target, &payload); err != nil {
				return err
			}
			if len(page.Events) == q.Limit {
				c := strconv.FormatInt(lastID(page.Events), 10)
				page.NextCursor = &c
				break
			}
			page.Events = append(page.Events, toEvent(id, at, action, actor, target, payload))
		}
		return rows.Err()
	})
	if err != nil {
		d.fail(w, r, p, "activity", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, page)
}

func lastID(es []EventOut) int64 {
	n, _ := strconv.ParseInt(es[len(es)-1].ID, 10, 64)
	return n
}

// toEvent shows one row. Only closed values leave: the kind's codes, the
// display names.
func toEvent(id int64, at time.Time, action string, actor, target *string, raw []byte) EventOut {
	k := ActivityActions[action]
	e := EventOut{ID: strconv.FormatInt(id, 10), At: stamp(at), Type: k.typ, Kind: k.kind}
	if actor != nil {
		e.Actor = &PersonOut{DisplayName: *actor}
	}
	if target != nil {
		e.Target = &PersonOut{DisplayName: *target}
	}
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	if mfa, _ := payload["mfa"].(bool); action == "auth.login.succeeded" && !mfa {
		e.Kind = "signed_in_without_mfa"
	}
	return e
}
