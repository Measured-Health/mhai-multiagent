package platform

// The platform activity feed (mhai-web Plan 7b; platform admin design §13):
// GET /platform/activity?hospital&cursor, platform admins and auditors.
//
// What platform staff did, newest first, 50 a page: the audit rows of the
// platform actions core writes that mhai-web's ActivityAction names, with
// an actor (a platform account; the CLI's and the workers' rows have none
// and stay in the audit log only). Each row is mapped here, by action, to
// {id, at, actor, action, hospital, target, detail}; nothing else from the
// payload leaves. The target is a code, a count, a document reference, a
// source label or a platform member's name, never a patient's anything.
// hospital filters on the row's hospital, else the payload's hospital_id,
// else (a follow-up) its change's hospital. The cursor is the last event's
// id. Not recorded (as the hospitals list).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// ActivityPage is the feed's page size.
const ActivityPage = 50

// activityActions maps a core action to mhai-web's ActivityAction.
var activityActions = map[string]string{
	"hospital.model_policy_changed": "model_policy_changed",
	"emr.connection_added":          "emr_connection_added",
	"emr.connection_changed":        "emr_connection_changed",
	"emr.connection_switched_on":    "emr_connection_switched_on",
	"emr.connection_switched_off":   "emr_connection_switched_off",
	"emr.connection_tested":         "emr_connection_tested",
	"analytics.followed_up":         "change_followed_up",
	"analytics.follow_up_edited":    "change_note_edited",
	"analytics.follow_up_undone":    "change_follow_up_undone",
	"job.retried":                   "jobs_retried",
	"job.cancelled":                 "jobs_cancelled",
	"legal_hold.placed":             "hold_placed",
	"legal_hold.lift_requested":     "hold_lift_requested",
	"legal_hold.lift_cancelled":     "hold_lift_cancelled",
	"legal_hold.lifted":             "hold_lifted",
	"platform.team_role_changed":    "team_role_changed",
	"platform.team_member_removed":  "team_removed",
}

func registerActivity(m *authz.Mux, d Deps) {
	authz.Route(m, http.MethodGet, "/platform/activity",
		authz.Rule{Platform: true, NeedsMFA: true, AnyRole: TeamRoles}, http.HandlerFunc(d.activity))
}

// ActivityEvent is one line of the feed.
type ActivityEvent struct {
	ID       string  `json:"id"`
	At       string  `json:"at"`
	Actor    Person  `json:"actor"`
	Action   string  `json:"action"`
	Hospital *Person `json:"hospital"`
	Target   *string `json:"target"`
	Detail   *string `json:"detail"`
}

// activityActionList is activityActions' keys, for the query.
func activityActionList() []string {
	out := make([]string, 0, len(activityActions))
	for a := range activityActions {
		out = append(out, a)
	}
	return out
}

const uuidRE = `'^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'`

// activitySelect reads the feed's rows with what each mapping needs.
const activitySelect = `WITH e AS (
	SELECT a.id, a.ts, a.actor_id, a.action, a.entity, a.entity_id, a.payload,
		COALESCE(a.hospital_id,
			CASE WHEN a.payload->>'hospital_id' ~ ` + uuidRE + ` THEN (a.payload->>'hospital_id')::uuid END,
			(SELECT c.hospital_id FROM analytics_changes c
			 WHERE a.payload->>'change_id' ~ ` + uuidRE + ` AND c.id = (a.payload->>'change_id')::uuid)) AS hid
	FROM audit_log a
	WHERE a.action = ANY($1) AND a.actor_id IS NOT NULL AND ($3::bigint IS NULL OR a.id < $3)
	ORDER BY a.id DESC)
SELECT e.id, e.ts, e.actor_id, au.display_name, e.action, e.hid, h.name, e.payload,
	(SELECT c.source_label FROM emr_connections c WHERE e.entity = 'emr_connection' AND c.id::text = e.entity_id),
	(SELECT l.reference FROM legal_holds l WHERE e.entity = 'legal_hold' AND l.id::text = e.entity_id),
	(SELECT u.display_name FROM users u WHERE e.entity = 'user' AND u.id::text = e.entity_id)
FROM e
LEFT JOIN users au ON au.id = e.actor_id
LEFT JOIN hospitals h ON h.id = e.hid
WHERE $2::uuid IS NULL OR e.hid = $2
ORDER BY e.id DESC LIMIT $4`

func str(s string) *string { return &s }

// activityTarget is the event's target and detail, by action.
func activityTarget(action string, payload map[string]any, label, reference, member *string) (target, detail *string) {
	s := func(k string) *string {
		if v, ok := payload[k].(string); ok {
			return &v
		}
		return nil
	}
	switch action {
	case "hospital.model_policy_changed":
		return s("provider"), s("previous")
	case "emr.connection_added", "emr.connection_changed", "emr.connection_switched_on",
		"emr.connection_switched_off", "emr.connection_tested":
		return label, nil
	case "job.retried", "job.cancelled":
		n := 1
		if c, ok := payload["job_count"].(float64); ok && c >= 1 {
			n = int(c)
		}
		if n == 1 {
			return str("1 failed job"), nil
		}
		return str(fmt.Sprintf("%d failed jobs", n)), nil
	case "legal_hold.placed", "legal_hold.lift_requested", "legal_hold.lift_cancelled", "legal_hold.lifted":
		return reference, nil
	case "platform.team_role_changed":
		return member, s("role")
	case "platform.team_member_removed":
		return member, nil
	}
	return nil, nil
}

// activity is GET /platform/activity.
func (d Deps) activity(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	v, err := params(r, []string{"hospital", "cursor"})
	if err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var hospital, before any
	if h, ok := v["hospital"]; ok {
		if !canonicalUUID(h) {
			httpx.Error(w, r, "validation_failed")
			return
		}
		hospital = h
	}
	if c, ok := v["cursor"]; ok {
		n, ok := audit.EventID(c)
		if !ok {
			httpx.Error(w, r, "validation_failed")
			return
		}
		before = n
	}
	out := struct {
		Events     []ActivityEvent `json:"events"`
		NextCursor *string         `json:"next_cursor"`
	}{Events: []ActivityEvent{}}
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), activitySelect, activityActionList(), hospital, before, ActivityPage+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var at time.Time
			var actor uuid.UUID
			var actorName, hName, label, reference, member *string
			var hid *uuid.UUID
			var action string
			var raw []byte
			if err := rows.Scan(&id, &at, &actor, &actorName, &action, &hid, &hName, &raw, &label, &reference, &member); err != nil {
				return err
			}
			var payload map[string]any
			if err := json.Unmarshal(raw, &payload); err != nil {
				payload = nil
			}
			e := ActivityEvent{ID: strconv.FormatInt(id, 10), At: stamp(at), Actor: person(&actor, actorName),
				Action: activityActions[action]}
			if hid != nil {
				e.Hospital = optPerson(hid, hName)
			}
			e.Target, e.Detail = activityTarget(action, payload, label, reference, member)
			out.Events = append(out.Events, e)
		}
		return rows.Err()
	})
	if err != nil {
		d.fail(w, r, p, "activity")
		return
	}
	if len(out.Events) > ActivityPage {
		out.Events = out.Events[:ActivityPage]
		c := out.Events[ActivityPage-1].ID
		out.NextCursor = &c
	}
	httpx.JSON(w, http.StatusOK, out)
}
