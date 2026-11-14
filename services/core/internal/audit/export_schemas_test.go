package audit_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

// The export and legal actions (analysis plan Tasks 25-26) have closed
// schemas that refuse free text, names and pids; the export actions map to
// mhai-web's export kinds with the export as the target, and the legal
// ones are sent bare (departure 68).
func TestExportActionsSchemas(t *testing.T) {
	id := uuid.NewString()
	sum := strings.Repeat("ab", 32)
	filters := map[string]any{"hospital": id, "person": id, "clearance": id, "from": "2026-11-01T00:00:00Z"}
	for action, ok := range map[string]map[string]any{
		"audit.exported":                {"rows": 12, "capped": false, "checksum": sum, "filters": map[string]any{"type": []any{"export"}}},
		"audit.named_access_exported":   {"rows": 3, "capped": false, "checksum": sum, "filters": filters},
		"audit.export_downloaded":       {},
		"audit.export_download_refused": {"reason": "gone"},
		"legal_reference.created":       {"export_id": id, "hospital_id": id, "admins": []any{id}},
		"analysis.legal_resolved":       {"export_id": id, "clearance_id": id, "rows": 7, "replays": 0},
		"analysis.legal_resolve_failed": {"export_id": id},
	} {
		if err := audit.CheckPayload(audit.Event{Action: action, Payload: ok}); err != nil {
			t.Errorf("%s: %v", action, err)
		}
		for _, extra := range []map[string]any{{"note": "free text"}, {"reference": "LR-2026-0001"}, {"pid": "P-ABCDEFGH"},
			{"name": "Synthetic Person"}} {
			bad := map[string]any{}
			for k, v := range ok {
				bad[k] = v
			}
			for k, v := range extra {
				bad[k] = v
			}
			if audit.CheckPayload(audit.Event{Action: action, Payload: bad}) == nil {
				t.Errorf("%s took %v", action, extra)
			}
		}
		if _, found := audit.Catalogue[action]; !found {
			t.Errorf("%s has no catalogue row", action)
		}
	}
	for _, bad := range []map[string]any{
		{"rows": 50001, "capped": false, "checksum": sum, "filters": map[string]any{}},
		{"rows": 1, "capped": false, "checksum": "ABC", "filters": map[string]any{}},
		{"rows": 1, "capped": false, "checksum": sum, "filters": map[string]any{"q": "P-ABCDEFGH"}},
	} {
		if audit.CheckPayload(audit.Event{Action: "audit.exported", Payload: bad}) == nil {
			t.Errorf("an export entry took %v", bad)
		}
	}
	for _, bad := range []map[string]any{
		{"export_id": id, "hospital_id": id, "admins": []any{id, id, id, id}},
		{"export_id": id, "hospital_id": id, "admins": []any{"Synthetic Admin"}},
	} {
		if audit.CheckPayload(audit.Event{Action: "legal_reference.created", Payload: bad}) == nil {
			t.Errorf("a reference entry took %v", bad)
		}
	}
	if audit.CheckPayload(audit.Event{Action: "analysis.legal_resolved",
		Payload: map[string]any{"export_id": id, "clearance_id": id, "rows": 201, "replays": 0}}) == nil {
		t.Error("a resolution of 201 rows")
	}

	m, ok := audit.Map(audit.Event{Action: "audit.named_access_exported", Entity: "audit_export", EntityID: id,
		Payload: map[string]any{"rows": 3, "capped": false, "checksum": sum, "filters": filters}})
	if !ok || m.Kind != "named_access_exported" || m.Type != "export" || m.Target == nil || m.Target.Kind != "export" ||
		m.Target.ID != id || fmt.Sprint(m.Facts["rows"]) != "3" || m.Facts["checksum"] != sum {
		t.Errorf("named export: %+v %v", m, ok)
	}
	m, ok = audit.Map(audit.Event{Action: "audit.export_download_refused", Entity: "audit_export", EntityID: id,
		Payload: map[string]any{"reason": "not_owner"}})
	if !ok || m.Kind != "export_download_refused" || m.Outcome != "refused" {
		t.Errorf("refused download: %+v %v", m, ok)
	}
	for _, a := range []string{"legal_reference.created", "analysis.legal_resolved", "analysis.legal_resolve_failed"} {
		if e := audit.Catalogue[a]; e.Kind != audit.UnknownKind {
			t.Errorf("%s is mapped (%s); it has no mhai-web kind yet", a, e.Kind)
		}
	}
}
