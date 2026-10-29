package intel

import (
	"math"
	"regexp"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
)

// Closed payload schemas for the facts intelligence hands core (spec §10.2:
// ids and codes only). A key check alone lets free text reach the
// append-only chain under an innocent key ("decision": "<a note>"), so each
// action names its keys, and every value is a closed-set code, an id of a
// fixed shape or a bounded integer. A payload that doesn't fit is refused
// whole: the drainer records {query_id, payload_refused}, AppendFacts
// refuses the reply's facts (ErrBadFact).

const (
	// maxChunkIDs is the most cited chunk ids a query.completed fact may
	// list: the worker's MAX_CITED (boundary/models.py).
	maxChunkIDs = 50
	// maxModelVersions is the most model_versions entries.
	maxModelVersions = 16
	// maxAttempts bounds attempts (JetStream's max_deliver is 5).
	maxAttempts = 1 << 16
)

var (
	// The worker's chunk ids are "KB#0001" and the like (its _CHUNK_ID
	// allows '#'); nothing else outside [A-Za-z0-9_.:-].
	chunkIDPattern      = regexp.MustCompile(`^[A-Za-z0-9#_.:-]{1,128}$`)
	modelNamePattern    = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,64}$`)
	modelVersionPattern = regexp.MustCompile(`^[A-Za-z0-9_.:/@+-]{1,128}$`) // the worker's _SAFE_VERSION
	factEntityIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

	decisions     = map[string]bool{"PASS": true, "REGENERATE": true, "ESCALATE": true}
	trustBands    = map[string]bool{"low": true, "mid": true, "high": true}
	cancelReasons = map[string]bool{"user_deactivated": true, "hospital_deactivated": true, cancelUnusable: true}
	errorCodes    = func() map[string]bool {
		m := map[string]bool{}
		for _, c := range authz.ErrorCodes() {
			m[c.Code] = true
		}
		return m
	}()
)

// fieldCheck checks one payload value.
type fieldCheck func(v any) bool

// factSchema is one action's payload: the keys it may carry, which of them
// it must, and the check for each value.
type factSchema struct {
	fields   map[string]fieldCheck
	required []string
}

// jobFactSchemas are the outbox's actions (00008's CHECK allows these three),
// each tied to the job status it reports.
var jobFactSchemas = map[string]struct {
	schema factSchema
	status string
}{
	"query.completed": {factSchema{fields: map[string]fieldCheck{
		"decision":       inSet(decisions),
		"trust_band":     nullOr(inSet(trustBands)), // null when unverified
		"chunk_ids":      stringList(maxChunkIDs, chunkIDPattern),
		"model_versions": stringMap(maxModelVersions, modelNamePattern, modelVersionPattern),
		"attempts":       integer,
	}, required: []string{"decision", "attempts"}}, "completed"},
	"query.failed": {factSchema{fields: map[string]fieldCheck{
		"error":    inSet(errorCodes),
		"attempts": integer,
	}, required: []string{"error", "attempts"}}, "failed"},
	"query.cancelled": {factSchema{fields: map[string]fieldCheck{
		"reason": inSet(cancelReasons),
	}, required: []string{"reason"}}, "cancelled"},
}

// replyFactSchemas are the request/reply facts core records
// (ResponseMeta.audit_facts): an action not here is refused.
var replyFactSchemas = map[string]factSchema{
	"intelligence.echo.pinged": {fields: map[string]fieldCheck{}},
}

// check reports whether payload fits s. query_id, if present, must be
// queryID (uuid.Nil: no query_id allowed).
func (s factSchema) check(payload map[string]any, queryID uuid.UUID) bool {
	for k, v := range payload {
		if k == "query_id" && queryID != uuid.Nil {
			if str, ok := v.(string); !ok || str != queryID.String() {
				return false
			}
			continue
		}
		f, ok := s.fields[k]
		if !ok || !f(v) {
			return false
		}
	}
	for _, k := range s.required {
		if _, ok := payload[k]; !ok {
			return false
		}
	}
	return true
}

func inSet(set map[string]bool) fieldCheck {
	return func(v any) bool {
		s, ok := v.(string)
		return ok && set[s]
	}
}

func nullOr(f fieldCheck) fieldCheck {
	return func(v any) bool { return v == nil || f(v) }
}

// integer: a JSON number (float64 from encoding/json and structpb) that is
// a whole number from 0 to maxAttempts.
func integer(v any) bool {
	n, ok := v.(float64)
	return ok && n >= 0 && n <= maxAttempts && n == math.Trunc(n)
}

func stringList(max int, re *regexp.Regexp) fieldCheck {
	return func(v any) bool {
		list, ok := v.([]any)
		if !ok || len(list) > max {
			return false
		}
		for _, e := range list {
			s, ok := e.(string)
			if !ok || !re.MatchString(s) {
				return false
			}
		}
		return true
	}
}

func stringMap(max int, key, val *regexp.Regexp) fieldCheck {
	return func(v any) bool {
		m, ok := v.(map[string]any)
		if !ok || len(m) > max {
			return false
		}
		for k, e := range m {
			s, ok := e.(string)
			if !key.MatchString(k) || !ok || !val.MatchString(s) {
				return false
			}
		}
		return true
	}
}
