package analysis

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// SourceRow is one coded row of mhai_analysis_ingest_source: codes only,
// plus the patient reference, which never leaves Go in clear.
type SourceRow struct {
	Ref        string
	Day        time.Time // a hospital day (the date part only)
	Kind       string
	Condition  *uuid.UUID
	AgeBand    *string
	Sex        string
	Setting    *string
	Location   *uuid.UUID
	Specialty  *string
	Source     *uuid.UUID
	Urgency    *string
	Confidence *string
	Rating     *string
	Correction *string
	// PR 10: a finding's flag; an alert's severity, minutes to the
	// response and dismissal reason.
	Flag       *string
	Severity   *string
	AckMinutes *int
	Dismiss    *string
}

var errNoKeys = errors.New("analysis: rows to ingest but no analysis keys")

// Fact is one fact as mhai_analysis_put_facts takes it (codes only).
type Fact struct {
	Day        string     `json:"day"`
	Kind       string     `json:"kind"`
	Condition  *uuid.UUID `json:"condition,omitempty"`
	AgeBand    *string    `json:"age_band,omitempty"`
	Sex        string     `json:"sex"`
	Setting    *string    `json:"setting,omitempty"`
	Location   *uuid.UUID `json:"location,omitempty"`
	Specialty  *string    `json:"specialty,omitempty"`
	Source     *uuid.UUID `json:"source,omitempty"`
	Urgency    *string    `json:"urgency,omitempty"`
	Confidence *string    `json:"confidence,omitempty"`
	Rating     *string    `json:"rating,omitempty"`
	Correction *string    `json:"correction,omitempty"`
	Flag       *string    `json:"flag,omitempty"`
	Severity   *string    `json:"severity,omitempty"`
	AckMinutes *int       `json:"ack_minutes,omitempty"`
	Dismiss    *string    `json:"dismiss_reason,omitempty"`
}

type ingestSubject struct {
	RefHMAC string `json:"ref_hmac"`
	RefEnc  []byte `json:"ref_enc"` // base64 in JSON
	Facts   []Fact `json:"facts"`
}

// buildIngest groups rows by reference and replaces each reference with
// its HMAC and its sealed form: {"subjects": [...]} for mhai_analysis_ingest.
func buildIngest(keys *Keys, hospital uuid.UUID, rows []SourceRow) ([]byte, error) {
	if len(rows) > 0 && keys == nil {
		return nil, errNoKeys
	}
	order := []string{}
	by := map[string]*ingestSubject{}
	for _, r := range rows {
		s, ok := by[r.Ref]
		if !ok {
			ct, err := keys.SealRef(hospital, r.Ref)
			if err != nil {
				return nil, err
			}
			s = &ingestSubject{RefHMAC: hex.EncodeToString(keys.RefHMAC(hospital, r.Ref)), RefEnc: ct, Facts: []Fact{}}
			by[r.Ref] = s
			order = append(order, r.Ref)
		}
		s.Facts = append(s.Facts, Fact{Day: r.Day.Format(time.DateOnly), Kind: r.Kind, Condition: r.Condition, AgeBand: r.AgeBand,
			Sex: r.Sex, Setting: r.Setting, Location: r.Location, Specialty: r.Specialty, Source: r.Source,
			Urgency: r.Urgency, Confidence: r.Confidence, Rating: r.Rating, Correction: r.Correction,
			Flag: r.Flag, Severity: r.Severity, AckMinutes: r.AckMinutes, Dismiss: r.Dismiss})
	}
	subjects := make([]*ingestSubject, 0, len(order))
	for _, ref := range order {
		subjects = append(subjects, by[ref])
	}
	return json.Marshal(map[string]any{"subjects": subjects})
}

// ChangeRow is one row of mhai_analysis_patient_changes: an EMR patient
// merged into Survivor ("merged"), or excluded and purged ("excluded"). The
// references never leave Go in clear.
type ChangeRow struct {
	Kind     string
	Ref      string
	Survivor *string
}

type change struct {
	Kind    string `json:"kind"`
	RefHMAC string `json:"ref_hmac"`
	ToHMAC  string `json:"to_hmac,omitempty"`
	ToEnc   []byte `json:"to_enc,omitempty"` // base64 in JSON
}

var errBadChange = errors.New("analysis: a patient change is malformed")

// buildChanges turns the rows into {"changes": [...]} for
// mhai_analysis_apply_changes: a forget per exclusion, a merge (with the
// survivor's sealed reference, for a subject the database may have to
// make) per merge, in order.
func buildChanges(keys *Keys, hospital uuid.UUID, rows []ChangeRow) ([]byte, error) {
	if len(rows) > 0 && keys == nil {
		return nil, errNoKeys
	}
	out := make([]change, 0, len(rows))
	for _, r := range rows {
		if r.Ref == "" {
			return nil, errBadChange
		}
		c := change{RefHMAC: hex.EncodeToString(keys.RefHMAC(hospital, r.Ref))}
		switch {
		case r.Kind == "excluded":
			c.Kind = "forget"
		case r.Kind == "merged" && r.Survivor != nil && *r.Survivor != "" && *r.Survivor != r.Ref:
			ct, err := keys.SealRef(hospital, *r.Survivor)
			if err != nil {
				return nil, err
			}
			c.Kind, c.ToHMAC, c.ToEnc = "merge", hex.EncodeToString(keys.RefHMAC(hospital, *r.Survivor)), ct
		default:
			return nil, errBadChange
		}
		out = append(out, c)
	}
	return json.Marshal(map[string]any{"changes": out})
}
