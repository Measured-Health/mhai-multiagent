package audit_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

// Every letter action (analysis plan Tasks 18-20) has a closed schema
// that refuses free text, and is mapped or withheld in the catalogue.
func TestLetterActionsSchemas(t *testing.T) {
	id := uuid.NewString()
	for action, ok := range map[string]map[string]any{
		"clearance.uploaded":      {"event_id": id},
		"clearance.ready":         {"event_id": id, "pages": 2},
		"clearance.refused":       {"event_id": id, "reason": "failed_safety_check"},
		"clearance.verified":      {"event_id": id},
		"clearance.rejected":      {"event_id": id, "reason": "wrong_people"},
		"clearance.narrowed":      {"event_id": id, "person": id, "reason": "person_left"},
		"clearance.revoked":       {"event_id": id, "side": "hospital", "reason": "other"},
		"clearance.ended":         {"event_id": id},
		"clearance.reminder_sent": {"event_id": id, "days": 7, "delivery": "pending"},
		"clearance.viewed":        {"page": 1},
		"named_access.changed":    {"via": "cli", "position": "letters_only", "refs": "demo"},
	} {
		if err := audit.CheckPayload(audit.Event{Action: action, Payload: ok}); err != nil {
			t.Errorf("%s: %v", action, err)
		}
		bad := map[string]any{}
		for k, v := range ok {
			bad[k] = v
		}
		bad["note"] = "free text"
		if audit.CheckPayload(audit.Event{Action: action, Payload: bad}) == nil {
			t.Errorf("%s took a note", action)
		}
		if _, found := audit.Catalogue[action]; !found {
			t.Errorf("%s has no catalogue row", action)
		}
	}
	if audit.CheckPayload(audit.Event{Action: "clearance.reminder_sent",
		Payload: map[string]any{"event_id": id, "days": 5, "delivery": "pending"}}) == nil {
		t.Error("a reminder of 5 days")
	}
	// A file refused for the safety check is flagged; other letter rows not.
	m, ok := audit.Map(audit.Event{Action: "clearance.refused", Entity: "clearance", EntityID: id,
		Payload: map[string]any{"event_id": id, "reason": "failed_safety_check"}})
	if !ok || m.Flag == nil || *m.Flag != "security" || m.Target == nil || m.Target.Kind != "clearance" {
		t.Errorf("refused for safety: %+v %v", m, ok)
	}
	m, ok = audit.Map(audit.Event{Action: "clearance.revoked", Entity: "clearance", EntityID: id,
		Payload: map[string]any{"event_id": id, "side": "hospital", "reason": "other"}})
	if !ok || m.Flag != nil || m.Kind != "clearance_revoked" {
		t.Errorf("revoked: %+v %v", m, ok)
	}
}
