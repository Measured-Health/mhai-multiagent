package feedback

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

// The request and response shapes: mhai-web's contract.ts (FeedbackIn,
// FeedbackOut, CorrectionOut, RejectIn, FeedbackStatsOut) and frontend
// contract §4, unchanged.

// clientClockSkew is how far ahead of core's clock client_created_at may be.
const clientClockSkew = 5 * time.Minute

// FeedbackIn is POST /feedback's body. Identity is never in it: an unknown
// field (submitted_by, hospital_id, ...) is 422.
type FeedbackIn struct {
	QueryID            string  `json:"query_id"`
	Action             string  `json:"action"`
	CorrectedDiagnosis *string `json:"corrected_diagnosis"`
	Reason             *string `json:"reason"`
	ClientCreatedAt    *string `json:"client_created_at"`
}

// submission is a FeedbackIn that passed validate: trimmed text, the
// client's time parsed.
type submission struct {
	action            string
	diagnosis, reason *string
	clientCreatedAt   *time.Time
}

// validate checks the body (spec §4.2): validate carries no text; correct
// carries a corrected diagnosis of 1-200 characters on one line and a
// reason of 3-1000 characters that may span lines (\n, \r\n), both
// trimmed, with no other control character and no Unicode format
// character (R11); client_created_at is RFC 3339 and at most 5 minutes
// ahead of now.
func (in FeedbackIn) validate(now time.Time) (submission, bool) {
	s := submission{action: in.Action}
	switch in.Action {
	case "validate":
		if in.CorrectedDiagnosis != nil || in.Reason != nil {
			return s, false
		}
	case "correct":
		d, ok1 := text(in.CorrectedDiagnosis, 1, 200, false)
		r, ok2 := text(in.Reason, 3, 1000, true)
		if !ok1 || !ok2 {
			return s, false
		}
		s.diagnosis, s.reason = &d, &r
	default:
		return s, false
	}
	if in.ClientCreatedAt != nil {
		t, err := time.Parse(time.RFC3339Nano, *in.ClientCreatedAt)
		if err != nil || t.After(now.Add(clientClockSkew)) {
			return s, false
		}
		s.clientCreatedAt = &t
	}
	return s, true
}

// text trims p and checks it is lo-hi characters of valid UTF-8 with no
// control character and no Unicode format character (category Cf: bidi
// overrides and isolates, zero-width characters, the BOM), which could
// make the text read differently from what it holds (R11), and no line or
// paragraph separator (U+2028, U+2029: a line break by another name).
// multiline allows line breaks, \n and \r\n only (a lone \r is
// refused).
func text(p *string, lo, hi int, multiline bool) (string, bool) {
	if p == nil {
		return "", false
	}
	s := strings.TrimSpace(*p)
	n := utf8.RuneCountInString(s)
	if !utf8.ValidString(s) || n < lo || n > hi {
		return "", false
	}
	for i, r := range s {
		switch {
		case multiline && r == '\n':
		case multiline && r == '\r' && i+1 < len(s) && s[i+1] == '\n':
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp):
			return "", false
		}
	}
	return s, true
}

// FeedbackOut is POST /feedback's reply.
type FeedbackOut struct {
	Status       string `json:"status"`
	CorrectionID string `json:"correction_id,omitempty"`
}

// RejectIn is POST /corrections/{id}/reject's body.
type RejectIn struct {
	ReasonCode string  `json:"reason_code"`
	Note       *string `json:"note"`
}

var rejectCodes = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range audit.RejectReasonCodes {
		m[c] = true
	}
	return m
}()

// parseReject decodes and checks a reject body: a known reason_code and a
// note of at most 500 characters that may span lines, with no other
// control character and no format character (text; trimmed; a blank note
// is no note).
func parseReject(body []byte) (code string, note *string, ok bool) {
	var in RejectIn
	if len(bytes.TrimSpace(body)) == 0 || decodeStrict(body, &in) != nil || !rejectCodes[in.ReasonCode] {
		return "", nil, false
	}
	if in.Note != nil {
		n := strings.TrimSpace(*in.Note)
		if n != "" {
			t, ok := text(&n, 1, 500, true)
			if !ok {
				return "", nil, false
			}
			note = &t
		}
	}
	return in.ReasonCode, note, true
}

// emptyBody accepts no body or an empty JSON object: approve and withdraw
// take nothing, so a body naming a decider is 422.
func emptyBody(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	var v struct{}
	return decodeStrict(body, &v) == nil
}

func decodeStrict(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

// PersonOut is a person as the API shows them: never an id.
type PersonOut struct {
	DisplayName string  `json:"display_name"`
	FullName    *string `json:"full_name"`
	Specialty   *string `json:"specialty"`
	Hospital    *string `json:"hospital"`
}

// CitationOut is what a reviewer sees of a citation.
type CitationOut struct {
	SourceTitle string `json:"source_title"`
	ClaimText   string `json:"claim_text"`
	Action      string `json:"action"`
}

// OriginalOut is what the system said.
type OriginalOut struct {
	Decision     string        `json:"decision"`
	Degraded     bool          `json:"degraded"`
	Diagnosis    *string       `json:"diagnosis"`
	ICD10Code    *string       `json:"icd10_code"`
	ResponseText string        `json:"response_text"`
	Citations    []CitationOut `json:"citations"`
}

// ConsultationOut is the consultation as it was asked, from the masked copy
// only (spec §4.4).
type ConsultationOut struct {
	Age              float64  `json:"age"`
	Sex              string   `json:"sex"`
	Symptoms         []string `json:"symptoms"`
	ClinicalQuestion string   `json:"clinical_question"`
	Urgency          string   `json:"urgency"`
	Specialty        *string  `json:"specialty"`
}

// RejectionOut is a rejection's code and note.
type RejectionOut struct {
	ReasonCode string  `json:"reason_code"`
	Note       *string `json:"note"`
}

// CorrectionOut is one correction.
type CorrectionOut struct {
	ID                 string          `json:"id"`
	QueryID            string          `json:"query_id"`
	Status             string          `json:"status"`
	CorrectedDiagnosis string          `json:"corrected_diagnosis"`
	Reason             string          `json:"reason"`
	SubmittedBy        PersonOut       `json:"submitted_by"`
	SubmittedAt        string          `json:"submitted_at"`
	Original           OriginalOut     `json:"original"`
	Consultation       ConsultationOut `json:"consultation"`
	DecidedBy          *PersonOut      `json:"decided_by"`
	DecidedAt          *string         `json:"decided_at"`
	Rejection          *RejectionOut   `json:"rejection"`
}

// FeedbackStatsOut is GET /feedback/stats/mine.
type FeedbackStatsOut struct {
	Agreed               int64 `json:"agreed"`
	CorrectionsWaiting   int64 `json:"corrections_waiting"`
	CorrectionsApproved  int64 `json:"corrections_approved"`
	CorrectionsRejected  int64 `json:"corrections_rejected"`
	CorrectionsWithdrawn int64 `json:"corrections_withdrawn"`
}

var (
	decisions = map[string]bool{"PASS": true, "REGENERATE": true, "ESCALATE": true}
	sexes     = map[string]bool{"female": true, "male": true, "other": true, "unknown": true}
	urgencies = map[string]bool{"routine": true, "urgent": true, "emergency": true}
	actions   = map[string]bool{"PASS": true, "FLAG": true, "REDACT": true}
)

// citations keeps the stored citations that have the reviewer's three
// fields; the pipeline stores chunk ids only today, so this is usually
// empty, never made up.
func citations(raw []byte) []CitationOut {
	out := []CitationOut{}
	var list []any
	if len(raw) == 0 || json.Unmarshal(raw, &list) != nil {
		return out
	}
	for _, v := range list {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		title, ok1 := m["source_title"].(string)
		claim, ok2 := m["claim_text"].(string)
		act, ok3 := m["action"].(string)
		if ok1 && ok2 && ok3 && actions[act] {
			out = append(out, CitationOut{SourceTitle: title, ClaimText: claim, Action: act})
		}
	}
	return out
}

// consultation builds the reviewer's view from the masked copy: the masked
// question and the masked patient data's age, sex and symptoms.
func consultation(maskedQuestion *string, maskedPatient []byte, urgency string, specialty *string) ConsultationOut {
	c := ConsultationOut{Sex: "unknown", Symptoms: []string{}, Urgency: urgency, Specialty: specialty}
	if maskedQuestion != nil {
		c.ClinicalQuestion = *maskedQuestion
	}
	if !urgencies[c.Urgency] {
		c.Urgency = "routine"
	}
	var p map[string]any
	if len(maskedPatient) > 0 && json.Unmarshal(maskedPatient, &p) == nil {
		if age, ok := p["age"].(float64); ok {
			c.Age = age
		}
		if sex, ok := p["sex"].(string); ok && sexes[sex] {
			c.Sex = sex
		}
		if list, ok := p["symptoms"].([]any); ok {
			for _, s := range list {
				if str, ok := s.(string); ok {
					c.Symptoms = append(c.Symptoms, str)
				}
			}
		}
	}
	return c
}

func isoTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// canonicalID parses a path id that must be a canonical uuid.
func canonicalID(raw string) (uuid.UUID, bool) {
	id, err := uuid.Parse(raw)
	return id, err == nil && id.String() == raw
}
