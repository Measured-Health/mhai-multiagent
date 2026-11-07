package intel

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/config"
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
			{"decision": "PASS", "attempts": 1, "egress": "anthropic", "cloud_outcome": "ok", "model_versions": versions},
			{"decision": "ESCALATE", "attempts": 1, "egress": "none", "cloud_outcome": "egress_blocked"},
			{"decision": "ESCALATE", "attempts": 1, "egress": "elsewhere", "cloud_outcome": "free text"},
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

// A16 (cloud model providers plan, Task 3): query.completed records where
// the answer's text went (egress) and the cloud outcome code, each from a
// closed set; a fact from an older worker, without either, stays valid.
// Both intel's schema and the audit registry are held to the same sets.
func TestQueryCompletedEgressAndOutcome(t *testing.T) {
	qid := uuid.New()
	schema := jobFactSchemas["query.completed"].schema
	base := func(kv ...any) map[string]any {
		p := map[string]any{"decision": "ESCALATE", "attempts": float64(1), "query_id": qid.String()}
		for i := 0; i+1 < len(kv); i += 2 {
			p[kv[i].(string)] = kv[i+1]
		}
		return p
	}
	accept := []map[string]any{base()}
	for _, e := range []string{"none", "openai", "anthropic", "xai"} {
		accept = append(accept, base("egress", e))
	}
	outcomes := []string{"ok", "egress_blocked", "budget_exhausted", "unconfigured", "unavailable", "timeout",
		"auth_failed", "refused", "truncated", "malformed", "insufficient", "retention_unconfirmed"}
	for _, o := range outcomes {
		accept = append(accept, base("cloud_outcome", o), base("egress", "anthropic", "cloud_outcome", o))
	}
	for i, p := range accept {
		if !schema.check(p, qid) {
			t.Errorf("accept %d: intel refused %v", i, p)
		}
		if err := audit.CheckActionPayload("query.completed", p); err != nil {
			t.Errorf("accept %d: audit refused %v: %v", i, p, err)
		}
	}
	refuse := []map[string]any{
		base("egress", "gemini"), base("egress", "OpenAI"), base("egress", ""), base("egress", float64(1)),
		base("egress", nil), base("egress", "sent the note to anthropic"), base("egress", []any{"openai"}),
		base("cloud_outcome", "nope"), base("cloud_outcome", "OK"), base("cloud_outcome", float64(0)),
		base("cloud_outcome", nil), base("cloud_outcome", "timeout: Adaeze Okafor"), base("cloud_outcome", map[string]any{}),
		base("cloud", "anthropic"),
	}
	for i, p := range refuse {
		if schema.check(p, qid) {
			t.Errorf("refuse %d: intel accepted %v", i, p)
		}
		if err := audit.CheckActionPayload("query.completed", p); err == nil {
			t.Errorf("refuse %d: audit accepted %v", i, p)
		}
	}
	// The Go lists are exactly these (the worker's are pinned below).
	if !slices.Equal(config.CloudOutcomes, outcomes) {
		t.Errorf("config.CloudOutcomes %v", config.CloudOutcomes)
	}
	if !slices.Equal(config.EgressValues(), []string{"none", "anthropic", "openai", "xai"}) {
		t.Errorf("config.EgressValues %v", config.EgressValues())
	}
}

// K14: Go's outcome, provider and egress lists equal the worker's
// (services/intelligence/intelligence/boundary/cloud/vocab.py, read as
// text), so the two sides can't drift.
func TestOutcomeVocabularyMatchesWorker(t *testing.T) {
	path := filepath.Join("..", "..", "..", "intelligence", "intelligence", "boundary", "cloud", "vocab.py")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for name, want := range map[string][]string{
		"OUTCOMES":  config.CloudOutcomes,
		"PROVIDERS": config.CloudProviderNames,
		"EGRESS":    config.EgressValues(),
	} {
		got := pyStrings(t, src, name)
		if !slices.Equal(sorted(got), sorted(want)) {
			t.Errorf("vocab.%s = %v, Go has %v", name, got, want)
		}
	}
}

// pyStrings returns the quoted strings of the tuple, list or set assigned
// to name at the start of a line in src. It reads literal strings only, so
// vocab.py spells each list out (no `*PROVIDERS` unpacking).
func pyStrings(t *testing.T, src, name string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + `\b[^=\n]*=\s*[(\[{](.*?)[)\]}]`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("vocab.py has no %s = (...)", name)
	}
	var out []string
	for _, s := range regexp.MustCompile(`["']([^"']*)["']`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, s[1])
	}
	return out
}

func sorted(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}
