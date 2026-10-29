package queries

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// The frontend contract's shapes (mhai-web apps/clinical/lib/api/contract.ts,
// plan departure 8): QueryCreateIn, QueryStatusOut, ClinicalResultOut.

// Limits on a question (Plan 3 T12).
const (
	maxQuestion   = 4000
	maxSymptoms   = 50
	maxSymptom    = 200
	maxHistory    = 4000
	maxSpecialty  = 32 // clinical_queries.category is varchar(32)
	maxAge        = 130
	maxClientTime = 64
)

var (
	sexes     = map[string]bool{"female": true, "male": true, "other": true, "unknown": true}
	urgencies = map[string]bool{"routine": true, "urgent": true, "emergency": true}
	stages    = map[string]bool{"pii_masking": true, "retrieval": true, "generation": true,
		"nli_verification": true, "hga_verification": true, "decision": true}
	decisions = map[string]bool{"PASS": true, "REGENERATE": true, "ESCALATE": true}
	agents    = map[string]bool{"CCA": true, "HGA": true}
	bands     = map[string]bool{"low": true, "mid": true, "high": true}
	// A specialty is a word or two (letters, spaces, hyphens), stored as the
	// category: no digits (dates, record numbers). It is never audited.
	specialtyRe = regexp.MustCompile(`^[A-Za-z][A-Za-z -]{0,31}$`)
)

// PatientData is QueryCreateIn.patient_data.
type PatientData struct {
	Age            *int     `json:"age"`
	Sex            string   `json:"sex"`
	Symptoms       []string `json:"symptoms"`
	MedicalHistory *string  `json:"medical_history,omitempty"`
}

// QueryCreateIn is the body of POST /queries, plus the optional
// client_created_at (the app's own time, when it queued the question
// offline; checked, not stored).
type QueryCreateIn struct {
	PatientData      *PatientData `json:"patient_data"`
	ClinicalQuestion string       `json:"clinical_question"`
	Urgency          string       `json:"urgency"`
	Specialty        *string      `json:"specialty,omitempty"`
	ClientCreatedAt  *string      `json:"client_created_at,omitempty"`
}

// valid checks q. It never says which field failed: the answer is 422
// validation_failed with a fixed body either way.
func (q *QueryCreateIn) valid() bool {
	p := q.PatientData
	if p == nil || p.Age == nil || *p.Age < 0 || *p.Age > maxAge || !sexes[p.Sex] || p.Symptoms == nil ||
		len(p.Symptoms) > maxSymptoms {
		return false
	}
	for _, s := range p.Symptoms {
		if !text(s, 1, maxSymptom) {
			return false
		}
	}
	if p.MedicalHistory != nil && !text(*p.MedicalHistory, 0, maxHistory) {
		return false
	}
	if !text(q.ClinicalQuestion, 1, maxQuestion) || !urgencies[q.Urgency] {
		return false
	}
	if q.Specialty != nil && *q.Specialty != "" && !specialtyRe.MatchString(*q.Specialty) {
		return false
	}
	if q.ClientCreatedAt != nil {
		if len(*q.ClientCreatedAt) > maxClientTime {
			return false
		}
		if _, err := time.Parse(time.RFC3339, *q.ClientCreatedAt); err != nil {
			return false
		}
	}
	return true
}

// category is the stored specialty, or "" for none.
func (q *QueryCreateIn) category() string {
	if q.Specialty == nil {
		return ""
	}
	return strings.TrimSpace(*q.Specialty)
}

// text: valid UTF-8 of min to max characters, not only white space when
// min > 0, and no NUL (Postgres text refuses it).
func text(s string, min, max int) bool {
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
		return false
	}
	n := utf8.RuneCountInString(s)
	if n < min || n > max {
		return false
	}
	return min == 0 || strings.TrimSpace(s) != ""
}

// QueryCreated is the 202 answer of POST /queries.
type QueryCreated struct {
	QueryID string `json:"query_id"`
	Status  string `json:"status"`
}

// QueryStatusOut is GET /queries/{id}.
type QueryStatusOut struct {
	QueryID      string  `json:"query_id"`
	Status       string  `json:"status"`
	Progress     float64 `json:"progress"`
	CurrentStage *string `json:"current_stage"`
	ErrorCode    string  `json:"error_code,omitempty"`
}

// statusOut maps a row's status to the contract's (departure 8): running
// is processing; cancelled is failed with error_code cancelled; a failed
// row's error_code is its code.
func statusOut(id, status string, progress float64, stage, errCode *string) (QueryStatusOut, bool) {
	out := QueryStatusOut{QueryID: id, Progress: progress}
	if out.Progress < 0 || out.Progress != out.Progress {
		out.Progress = 0
	}
	if out.Progress > 1 {
		out.Progress = 1
	}
	if stage != nil && stages[*stage] {
		out.CurrentStage = stage
	}
	switch status {
	case "queued", "completed":
		out.Status = status
	case "running":
		out.Status = "processing"
	case "failed":
		out.Status = "failed"
		out.ErrorCode = "internal"
		if errCode != nil && *errCode != "" {
			out.ErrorCode = *errCode
		}
	case "cancelled":
		out.Status, out.ErrorCode = "failed", "cancelled"
	default:
		return out, false
	}
	return out, true
}

// Citation is one cited source.
type Citation struct {
	ChunkID       string  `json:"chunk_id"`
	SourceTitle   string  `json:"source_title"`
	Excerpt       string  `json:"excerpt"`
	ClaimText     string  `json:"claim_text"`
	NLILabel      string  `json:"nli_label"`
	NLIConfidence float64 `json:"nli_confidence"`
	Action        string  `json:"action"`
	Reason        string  `json:"reason"`
}

// L3NLI is guardrail_details.l3_nli.
type L3NLI struct {
	PassRate      float64 `json:"pass_rate"`
	ClaimsChecked int     `json:"claims_checked"`
	ModelLoaded   bool    `json:"model_loaded"`
}

// L4Trust is guardrail_details.l4_trust.
type L4Trust struct {
	TrustScore  float64 `json:"trust_score"`
	BaseTrust   float64 `json:"base_trust"`
	FlagPenalty float64 `json:"flag_penalty"`
}

// GuardrailDetails is ClinicalResultOut.guardrail_details.
type GuardrailDetails struct {
	L1Persona *struct{} `json:"l1_persona"`
	L2Entropy *struct {
		Stability float64 `json:"stability"`
	} `json:"l2_entropy"`
	L3NLI       L3NLI    `json:"l3_nli"`
	L4Trust     *L4Trust `json:"l4_trust"`
	CascadePass bool     `json:"cascade_pass"`
}

// ClinicalResultOut is GET /queries/{id}/result. trust_band (low, mid,
// high or null when unverified) and cited_chunk_ids are additions to the
// contract (clients ignore unknown fields, ADR 0012).
type ClinicalResultOut struct {
	QueryID           string           `json:"query_id"`
	Decision          string           `json:"decision"`
	Verified          bool             `json:"verified"`
	Degraded          bool             `json:"degraded"`
	FailedAgents      []string         `json:"failed_agents"`
	ResponseAvailable bool             `json:"response_available"`
	Diagnosis         *string          `json:"diagnosis"`
	ICD10Code         *string          `json:"icd10_code"`
	ResponseText      string           `json:"response_text"`
	TrustScore        *float64         `json:"trust_score"`
	TrustBand         *string          `json:"trust_band"`
	Citations         []Citation       `json:"citations"`
	CitedChunkIDs     []string         `json:"cited_chunk_ids"`
	GuardrailDetails  GuardrailDetails `json:"guardrail_details"`
	RetrievalMS       float64          `json:"retrieval_ms"`
	GenerationMS      float64          `json:"generation_ms"`
}
