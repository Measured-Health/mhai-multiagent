package intel

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

// Every fact intelligence may hand core has a schema in core's audit
// registry too, so a fact that passes intel's check is never refused by
// audit.Write (which would stall the drainer).
func TestFactActionsHaveAuditSchemas(t *testing.T) {
	for a := range jobFactSchemas {
		if !audit.Registered(a) {
			t.Errorf("job fact %s has no audit schema", a)
		}
	}
	for a := range replyFactSchemas {
		if !audit.Registered(a) {
			t.Errorf("reply fact %s has no audit schema", a)
		}
	}
}

// TestDrainedFactsFitAuditSchemas holds the drainer's output for every action in
// jobFactSchemas against the audit registry: whatever DrainOnce builds, fit
// or refused, audit.Write must accept, or the whole drain batch rolls back
// and stalls. Valid, edge and refused payloads, with and without a fact
// entity, for each action.
func TestDrainedFactsFitAuditSchemas(t *testing.T) {
	jobID, hid, user, subject := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	str := func(s string) *string { return &s }
	chunks := make([]any, maxChunkIDs)
	for i := range chunks {
		chunks[i] = "KB#0001"
	}
	versions := map[string]any{"model.a": "v1.2@sha+x/y"}
	manyVersions := map[string]any{}
	for i := 0; i < maxModelVersions; i++ {
		manyVersions[fmt.Sprintf("m%d", i)] = "1"
	}
	sub := subject.String()
	payloads := map[string][]map[string]any{
		"query.completed": {
			{"decision": "PASS", "attempts": 1},
			{"decision": "ESCALATE", "trust_band": nil, "chunk_ids": chunks, "model_versions": manyVersions, "attempts": maxAttempts},
			{"decision": "REGENERATE", "trust_band": "mid", "chunk_ids": []any{}, "model_versions": versions, "attempts": 0},
			{"decision": "free text"}, {"free": "text"}, {},
		},
		"query.failed": {
			{"error": "unauthorized", "attempts": 2},
			{"error": "not a code", "attempts": 2}, {"attempts": 2}, {"error": "unauthorized", "attempts": -1},
		},
		"query.cancelled": {
			{"reason": "user_deactivated"}, {"reason": "hospital_deactivated"}, {"reason": cancelUnusable},
			{"reason": "other"},
		},
		"correction.applied": {
			{"correction_id": sub, "attempts": 3, "persona_update": "applied"},
			{"correction_id": sub, "attempts": 0, "persona_update": "skipped", "model_versions": manyVersions},
			{"correction_id": sub, "attempts": 3, "persona_update": "applied", "job_id": jobID.String()},
			{"correction_id": uuid.New().String(), "attempts": 3, "persona_update": "applied"}, // another subject
			{"correction_id": sub, "attempts": 3, "persona_update": "applied", "job_id": uuid.New().String()},
			{"correction_id": sub, "persona_update": "applied"}, {"reason": "free text"},
		},
		"correction.apply_failed": {
			{"correction_id": sub, "error": "unauthorized", "attempts": 5},
			{"correction_id": sub, "error": "free text", "attempts": 5},
			{"correction_id": sub},
		},
	}
	entities := []struct{ ent, id *string }{
		{nil, nil}, {str("query"), str(jobID.String())}, {str("correction"), str(sub)}, {str("job"), str(jobID.String())},
		{str("other.thing"), str("x-1")}, {str("bad entity!"), str("y")}, {str("correction"), str(uuid.New().String())},
	}
	for action := range jobFactSchemas {
		cases, ok := payloads[action]
		if !ok {
			t.Fatalf("no payloads for %s: add the drained shapes", action)
		}
		f := jobFactSchemas[action]
		for ci, p := range cases {
			for ei, en := range entities {
				raw, _ := json.Marshal(p)
				o := outboxRow{action: action, entity: en.ent, entityID: en.id, payload: raw,
					jobID: jobID, hospital: hid, createdBy: &user, status: str(f.status)}
				if f.jobType != "" {
					o.jobType, o.subject = str(f.jobType), &subject
				}
				e := drainEvent(o, slog.New(slog.NewTextHandler(io.Discard, nil)))
				if err := audit.CheckActionPayload(action, e.Payload); err != nil {
					t.Errorf("%s case %d entity %d: drained payload %v refused by audit schema: %v", action, ci, ei, e.Payload, err)
				}
			}
		}
	}
	// The refused shape, explicitly, for a question and a job.
	for _, a := range []string{"query.failed", "correction.applied"} {
		f := jobFactSchemas[a]
		o := outboxRow{action: a, payload: []byte(`{"x":"free text"}`), jobID: jobID, hospital: hid, status: str(f.status)}
		if f.jobType != "" {
			o.jobType, o.subject = str(f.jobType), &subject
		}
		e := drainEvent(o, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if e.Payload["payload_refused"] != true {
			t.Errorf("%s: expected payload_refused, got %v", a, e.Payload)
		}
		if err := audit.CheckActionPayload(a, e.Payload); err != nil {
			t.Errorf("%s: refused payload %v not accepted by audit: %v", a, e.Payload, err)
		}
	}
}
