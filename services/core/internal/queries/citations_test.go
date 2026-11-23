package queries

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCitationChecks: the result's citations are the pipeline's stored
// per-citation NLI results (#28), checked field by field; anything that
// doesn't fit is dropped rather than shown.
func TestCitationChecks(t *testing.T) {
	good := `{"chunk_id":"KB#1000001","claim_index":0,"claim_text":"Synthetic claim [KB#1000001].",` +
		`"nli_label":"ENTAILMENT","nli_confidence":0.99,"action":"PASS","reason":"entailed"}`
	redacted := `{"chunk_id":"KB#0002","claim_index":1,"claim_text":"Another synthetic claim [KB#0002].",` +
		`"nli_label":"NEUTRAL","nli_confidence":0,"action":"REDACT","reason":"not_retrieved"}`
	sources := []CitedSource{{ChunkID: "KB#1000001", Title: "Synthetic Fever", Source: "MedlinePlus"}}
	got := citationChecks([]byte(`{"flags":[],"citation_checks":[`+good+`,`+redacted+`]}`), sources)
	want := []Citation{
		{ChunkID: "KB#1000001", SourceTitle: "Synthetic Fever", ClaimText: "Synthetic claim [KB#1000001].",
			NLILabel: "ENTAILMENT", NLIConfidence: 0.99, Action: "PASS", Reason: "entailed"},
		{ChunkID: "KB#0002", ClaimText: "Another synthetic claim [KB#0002].", NLILabel: "NEUTRAL", Action: "REDACT",
			Reason: "not_retrieved"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v", got)
	}
	b, _ := json.Marshal(got[1])
	if !strings.Contains(string(b), `"excerpt":""`) || !strings.Contains(string(b), `"source_title":""`) {
		t.Errorf("json %s", b)
	}

	field := func(k, v string) string {
		var m map[string]any
		_ = json.Unmarshal([]byte(good), &m)
		var raw any
		if err := json.Unmarshal([]byte(v), &raw); err != nil {
			delete(m, k)
		} else {
			m[k] = raw
		}
		out, _ := json.Marshal(map[string]any{"citation_checks": []any{m}})
		return string(out)
	}
	for _, c := range []struct{ name, raw string }{
		{"unknown label", field("nli_label", `"entailment"`)},
		{"unknown action", field("action", `"ALLOW"`)},
		{"unknown reason", field("reason", `"Claim entailed by source"`)},
		{"confidence above 1", field("nli_confidence", `1.5`)},
		{"negative confidence", field("nli_confidence", `-0.1`)},
		{"confidence a string", field("nli_confidence", `"0.9"`)},
		{"bad chunk id", field("chunk_id", `"KB#0001<script>"`)},
		{"no chunk id", field("chunk_id", `-`)},
		{"claim too long", field("claim_text", `"`+strings.Repeat("x", 301)+`"`)},
		{"claim with a control character", field("claim_text", `"a\u0007b"`)},
		{"empty claim", field("claim_text", `""`)},
		{"no checks", `{"flags":[]}`},
		{"checks not a list", `{"citation_checks":{"chunk_id":"KB#0001"}}`},
		{"not json", `{`},
		{"null", `null`},
		{"empty", ``},
	} {
		if got := citationChecks([]byte(c.raw), nil); got == nil || len(got) != 0 {
			t.Errorf("%s: got %+v", c.name, got)
		}
	}

	many := make([]string, 60)
	for i := range many {
		many[i] = good
	}
	if got := citationChecks([]byte(`{"citation_checks":[`+strings.Join(many, ",")+`]}`), nil); len(got) != maxCitationChecks {
		t.Errorf("cap: %d", len(got))
	}
}
