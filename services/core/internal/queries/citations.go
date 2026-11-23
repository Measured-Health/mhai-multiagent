package queries

import (
	"encoding/json"
	"math"
	"regexp"
)

// How an answer was checked (#28): the pipeline stores each citation's NLI
// result in the decision's guardrail_report_json.citation_checks
// (services/intelligence/intelligence/agents/hga_nli.py, citation_checks).
// citationChecks turns them into ClinicalResultOut.citations.

// Limits on a stored citation check (the pipeline's MAX_CITATION_CHECKS,
// MAX_CLAIM_TEXT and its chunk id pattern).
const (
	maxCitationChecks = 50
	maxClaimText      = 300
)

var (
	checkChunkID = regexp.MustCompile(`^[A-Za-z0-9#_.:\-]{1,16}$`)
	nliLabels    = map[string]bool{"ENTAILMENT": true, "NEUTRAL": true, "CONTRADICTION": true}
	checkActions = map[string]bool{"PASS": true, "FLAG": true, "REDACT": true}
	// The pipeline's CITATION_REASONS (hga_nli.py).
	checkReasons = map[string]bool{
		"entailed": true, "not_entailed": true, "contradicted": true, "number_not_in_source": true,
		"not_retrieved": true, "not_found": true, "other_patient": true, "uncited": true,
	}
)

// citationChecks decodes a decision's guardrail_report_json and keeps, in
// order, the citation checks whose fields pass the checks; anything else is
// dropped rather than shown. The source title is the cited source's, when
// the answer has one for that chunk; the excerpt is never stored, so it is
// empty. Never nil.
func citationChecks(report []byte, sources []CitedSource) []Citation {
	out := []Citation{}
	var r struct {
		Checks []map[string]any `json:"citation_checks"`
	}
	if len(report) == 0 || json.Unmarshal(report, &r) != nil {
		return out
	}
	titles := map[string]string{}
	for _, s := range sources {
		titles[s.ChunkID] = s.Title
	}
	for _, c := range r.Checks {
		id, _ := c["chunk_id"].(string)
		label, _ := c["nli_label"].(string)
		action, _ := c["action"].(string)
		reason, _ := c["reason"].(string)
		conf, okC := c["nli_confidence"].(float64)
		if !checkChunkID.MatchString(id) || !nliLabels[label] || !checkActions[action] || !checkReasons[reason] ||
			!okC || math.IsNaN(conf) || conf < 0 || conf > 1 {
			continue
		}
		claim, okT := sourceText(c["claim_text"], maxClaimText)
		if !okT {
			continue
		}
		out = append(out, Citation{
			ChunkID: id, SourceTitle: titles[id], ClaimText: claim,
			NLILabel: label, NLIConfidence: conf, Action: action, Reason: reason,
		})
		if len(out) == maxCitationChecks {
			break
		}
	}
	return out
}
