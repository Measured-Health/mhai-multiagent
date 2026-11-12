package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/config"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrtypes"
)

// The closed payload schemas of every action core writes (Plan 4 spec §9).
// Plan 3 closed intelligence's facts; this closes core's own actions too:
// each action names its keys, and every value is a closed-set code, an id
// of a fixed shape, a bool or a bounded integer. Write refuses an action
// with no schema, or a payload that doesn't fit (ErrSchema), before it
// touches the chain, so the caller's transaction rolls back and nothing
// half-checked reaches the append-only log. A payload that doesn't fit is a
// programming error: the request answers 500.
//
// The Plan 2 and Plan 3 actions are registered with the payloads they
// already write, frozen. No action carries free text, a name, an email, a
// diagnosis or a note.

// Check reports whether one payload value fits. Values are as
// encoding/json decodes them: string, float64, bool, nil, []any,
// map[string]any.
type Check func(v any) bool

// Schema is one action's payload: the keys it may carry, the check for
// each, and the keys it must carry.
type Schema struct {
	Fields   map[string]Check
	Required []string
	// Shapes, when set, are the payload's alternatives: it must fit one of
	// them, carrying all of that shape's required keys and no key outside
	// its keys (an action two writers use with two shapes: job.retried and
	// job.cancelled, per job and per platform call).
	Shapes []Shape
	// Holds, when set, is a rule across the payload's keys that it must
	// also meet (checked after the keys and values fit).
	Holds func(payload map[string]any) bool
}

// Shape is one alternative payload of an action: the keys it may carry and
// those it must.
type Shape struct {
	Keys, Required []string
}

// fits reports whether payload's keys fit shape.
func (sh Shape) fits(payload map[string]any) bool {
	for k := range payload {
		if !slices.Contains(sh.Keys, k) {
			return false
		}
	}
	for _, k := range sh.Required {
		if _, ok := payload[k]; !ok {
			return false
		}
	}
	return true
}

// ErrSchema is returned by Write for an action with no schema, or a
// payload with a key, a value or a missing key outside it. The error never
// names a value.
var ErrSchema = errors.New("audit: payload does not fit the action's schema")

var (
	registryMu sync.RWMutex
	registry   = map[string]Schema{}
)

var actionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*){1,4}$`)

// Register adds action's schema. It panics on a malformed action name, a
// second schema for the same action, a nil check, or a required key with no
// check: the registry is built at init, so a mistake stops the binary.
func Register(action string, s Schema) {
	if !actionPattern.MatchString(action) {
		panic(fmt.Sprintf("audit: malformed action %q", action))
	}
	for k, c := range s.Fields {
		if c == nil {
			panic(fmt.Sprintf("audit: %s: nil check for %q", action, k))
		}
	}
	for _, k := range s.Required {
		if s.Fields[k] == nil {
			panic(fmt.Sprintf("audit: %s: required key %q has no check", action, k))
		}
	}
	for _, sh := range s.Shapes {
		for _, k := range append(slices.Clone(sh.Keys), sh.Required...) {
			if s.Fields[k] == nil {
				panic(fmt.Sprintf("audit: %s: shape key %q has no check", action, k))
			}
		}
		for _, k := range sh.Required {
			if !slices.Contains(sh.Keys, k) {
				panic(fmt.Sprintf("audit: %s: shape requires %q outside its keys", action, k))
			}
		}
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[action]; dup {
		panic(fmt.Sprintf("audit: second schema for %s", action))
	}
	registry[action] = s
}

// Registered reports whether action has a schema.
func Registered(action string) bool {
	registryMu.RLock()
	defer registryMu.RUnlock()
	_, ok := registry[action]
	return ok
}

// checkPayload checks raw (the payload as Write marshalled it) against
// action's schema.
func checkPayload(action string, raw []byte) error {
	registryMu.RLock()
	s, ok := registry[action]
	registryMu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: no schema for %q", ErrSchema, action)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		return ErrSchema
	}
	for k, v := range payload {
		c, ok := s.Fields[k]
		if !ok || !c(v) {
			// The key is code's own (the schema names it, or it is the
			// caller's mistake); its value is never reported.
			return fmt.Errorf("%w: %s key %q", ErrSchema, action, k)
		}
	}
	for _, k := range s.Required {
		if _, ok := payload[k]; !ok {
			return fmt.Errorf("%w: %s lacks %q", ErrSchema, action, k)
		}
	}
	if len(s.Shapes) > 0 && !slices.ContainsFunc(s.Shapes, func(sh Shape) bool { return sh.fits(payload) }) {
		return fmt.Errorf("%w: %s fits none of its shapes", ErrSchema, action)
	}
	if s.Holds != nil && !s.Holds(payload) {
		return fmt.Errorf("%w: %s breaks its rule across keys", ErrSchema, action)
	}
	return nil
}

// CheckActionPayload reports whether payload fits action's registered schema, as
// Write checks it. Tests in other packages use it to hold a second copy of a
// schema (intel's drained facts) against this registry.
func CheckActionPayload(action string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return ErrSchema
	}
	return checkPayload(action, raw)
}

// UUID: a canonical (lower-case, hyphenated) uuid string.
func UUID(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s
}

// Bool: true or false.
func Bool(v any) bool { _, ok := v.(bool); return ok }

// OneOf: a string from the set.
func OneOf(set ...string) Check {
	m := make(map[string]bool, len(set))
	for _, s := range set {
		m[s] = true
	}
	return func(v any) bool {
		s, ok := v.(string)
		return ok && m[s]
	}
}

// Pattern: a string matching re (which must be anchored and bounded).
func Pattern(re *regexp.Regexp) Check {
	return func(v any) bool {
		s, ok := v.(string)
		return ok && re.MatchString(s)
	}
}

// Int: a whole number from lo to hi.
func Int(lo, hi float64) Check {
	return func(v any) bool {
		n, ok := v.(float64)
		return ok && n >= lo && n <= hi && n == math.Trunc(n)
	}
}

// Nullable: null, or a value c accepts.
func Nullable(c Check) Check { return func(v any) bool { return v == nil || c(v) } }

// List: an array of at most max values, each accepted by c.
func List(max int, c Check) Check {
	return func(v any) bool {
		l, ok := v.([]any)
		if !ok || len(l) > max {
			return false
		}
		for _, e := range l {
			if !c(e) {
				return false
			}
		}
		return true
	}
}

// StringMap: an object of at most max entries, keys matching key and
// values accepted by c.
func StringMap(max int, key *regexp.Regexp, c Check) Check {
	return func(v any) bool {
		m, ok := v.(map[string]any)
		if !ok || len(m) > max {
			return false
		}
		for k, e := range m {
			if !key.MatchString(k) || !c(e) {
				return false
			}
		}
		return true
	}
}

// Object: an object whose keys are all in fields, each value accepted by
// its check. None is required.
func Object(fields map[string]Check) Check {
	return func(v any) bool {
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		for k, e := range m {
			c, known := fields[k]
			if !known || !c(e) {
				return false
			}
		}
		return true
	}
}

// Instant: an RFC 3339 time with its zone, as the audit filters carry it.
func Instant(v any) bool {
	s, ok := v.(string)
	if !ok || !instantPattern.MatchString(s) {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, s)
	return err == nil
}

// RejectReasonCodes are the codes a correction is rejected with (spec §4.2).
var RejectReasonCodes = []string{"not_supported", "needs_rewording", "unsafe", "out_of_scope"}

const maxCount = 1 << 16

var (
	// Ids and words of a fixed alphabet: request keys, jtis, body hashes,
	// message ids, services and operations as core names them.
	tokenPattern     = regexp.MustCompile(`^[A-Za-z0-9_.:+/=@-]{1,128}$`)
	operationPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}\.[a-z][a-z0-9_]{0,63}$`)
	servicePattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	codePattern      = regexp.MustCompile(`^[a-z][a-z_]{0,47}$`)
	// A terms version as POST /auth/terms accepts it (Plan 2): at most 64
	// characters with no control characters.
	termsVersion = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,64}$`)
	// An instant with its zone, never a local time (the audit filters).
	instantPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T([01]\d|2[0-3]):[0-5]\d:[0-5]\d(\.\d{1,9})?(Z|[+-]\d{2}:\d{2})$`)
	// An audit event id as core sends it: the entry's sequence number.
	eventIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	// intelligence's facts, as intel/factschema.go checks them first.
	chunkIDPattern   = regexp.MustCompile(`^[A-Za-z0-9#_.:-]{1,128}$`)
	modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,64}$`)
	// A version with no '#', or a pinned model's
	// "<name>@<version>#<12 hex>" (intel's pinnedModelVersion, the same form).
	modelVersionPattern = regexp.MustCompile(`^(?:[A-Za-z0-9_.:/@+-]{1,128}|[a-z0-9][a-z0-9-]{1,62}@(?:0|[1-9][0-9]{0,8})\.(?:0|[1-9][0-9]{0,8})\.(?:0|[1-9][0-9]{0,8})#[0-9a-f]{12})$`)
	factEntityPattern   = regexp.MustCompile(`^[a-z0-9_.]{0,64}$`)
	factEntityIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	// A model policy's agreement reference (00018's CHECK): AGR-YYYY-NNNN,
	// DPA-YYYY-NNNN or DEMO-SYNTHETIC, never a word, a name or a pseudonym.
	agreementRefPattern = regexp.MustCompile(`^(DEMO-SYNTHETIC|(AGR|DPA)-[0-9]{4}-[0-9]{2,8})$`)
)

func fields(kv ...any) map[string]Check {
	m := map[string]Check{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1].(Check)
	}
	return m
}

func init() {
	count := Check(Int(0, maxCount))
	errorCode := func() Check {
		var codes []string
		for _, c := range authz.ErrorCodes() {
			codes = append(codes, c.Code)
		}
		return OneOf(codes...)
	}()
	token, operation, service, code := Pattern(tokenPattern), Pattern(operationPattern), Pattern(servicePattern), Pattern(codePattern)
	id := Check(UUID)
	b := Check(Bool)

	// Plan 2: sign-in, sessions, two-step, password, terms (internal/auth).
	Register("auth.logout", Schema{})
	Register("auth.session.revoked", Schema{Fields: fields("reason", OneOf("password_changed"))})
	Register("auth.login.locked", Schema{Fields: fields("reason", OneOf("locked")), Required: []string{"reason"}})
	Register("auth.login.ceiling", Schema{Fields: fields("outcome", OneOf("device", "locked")), Required: []string{"outcome"}})
	Register("auth.login.failed", Schema{Fields: fields("reason", OneOf("bad_credentials", "not_platform", "platform_account")),
		Required: []string{"reason"}})
	Register("auth.login.succeeded", Schema{Fields: fields(
		"mfa_required", b, "mfa", b, "amr", List(4, OneOf("pwd", "otp", "backup", "device")))})
	Register("auth.refresh.reuse", Schema{Fields: fields("revoked", b), Required: []string{"revoked"}})
	Register("auth.terms.accepted", Schema{Fields: fields("version", Pattern(termsVersion)), Required: []string{"version"}})
	Register("auth.password.failed", Schema{Fields: fields("reason", OneOf("locked", "bad_current", "weak")), Required: []string{"reason"}})
	Register("auth.password.changed", Schema{Fields: fields("sessions_revoked", count), Required: []string{"sessions_revoked"}})
	Register("auth.mfa.locked", Schema{Fields: fields("reason", OneOf("token_attempts", "account_attempts")), Required: []string{"reason"}})
	Register("auth.mfa.failed", Schema{Fields: fields("method", OneOf("token_mismatch", "otp", "backup", "none", "step_up")),
		Required: []string{"method"}})
	Register("auth.mfa.verified", Schema{Fields: fields("method", OneOf("otp"), "confirmed", b)})
	Register("auth.mfa.backup_used", Schema{Fields: fields("left", Int(0, 100)), Required: []string{"left"}})
	Register("auth.mfa.enrolled", Schema{Fields: fields("confirmed", b), Required: []string{"confirmed"}})
	Register("auth.mfa.codes_regenerated", Schema{Fields: fields("count", Int(0, 100)), Required: []string{"count"}})
	Register("auth.device.trusted", Schema{Fields: fields("days", Int(0, 366)), Required: []string{"days"}})

	// Plan 2: the provisioning CLI (internal/account).
	Register("hospital.created", Schema{Fields: fields("via", OneOf("cli")), Required: []string{"via"}})
	Register("hospital.activated", Schema{Fields: fields("via", OneOf("cli"), "from", OneOf("setup_incomplete")),
		Required: []string{"via", "from"}})
	Register("user.created", Schema{Fields: fields("via", OneOf("cli")), Required: []string{"via"}})

	// Cloud model providers (plan 2026-11-06 Task 2, design §5.3): a
	// hospital's model policy set by a platform admin (as mhai_platform)
	// or the CLI twin (as mhai_provision). The row has no hospital (a
	// platform action); the hospital is in the payload. The agreement
	// reference has the id shape and is absent when none was given.
	// The reference is present exactly when a provider is (off clears it).
	provider := OneOf("none", "openai", "anthropic", "xai")
	agreementRef := func(v any) bool {
		s, ok := v.(string)
		return ok && agreementRefPattern.MatchString(s) && !pseudonymS.MatchString(s)
	}
	Register("hospital.model_policy_changed", Schema{Fields: fields(
		"hospital_id", id, "provider", provider, "previous", provider,
		"agreement_ref", Check(agreementRef),
		"monthly_token_budget", Int(1, 1000000000),
		"via", OneOf("platform", "cli")),
		Required: []string{"hospital_id", "provider", "previous", "monthly_token_budget", "via"},
		// A reference exactly when a provider is set.
		Holds: func(p map[string]any) bool {
			_, hasRef := p["agreement_ref"]
			return hasRef == (p["provider"] != "none")
		}})

	// EMR connections (EMR design §12.6, plan Task 11): ids, the type, the
	// keys that changed (never a value), closed outcome codes and bools.
	// The platform writes them with no hospital column (00020's
	// platform_append); the hospital is the payload's id.
	emrConn := fields("hospital_id", id, "connection_id", id)
	withConn := func(kv ...any) map[string]Check {
		m := fields(kv...)
		for k, c := range emrConn {
			m[k] = c
		}
		return m
	}
	connReq := []string{"hospital_id", "connection_id"}
	Register("emr.connection_added", Schema{Fields: withConn("type", OneOf(emrTypeIDs()...)),
		Required: append(slices.Clone(connReq), "type")})
	Register("emr.connection_changed", Schema{Fields: withConn("fields_changed", List(40, OneOf(EMRConnectionKeys()...)),
		"credentials_replaced", Check(Bool), "credentials_wiped", Check(Bool)),
		Required: append(slices.Clone(connReq), "fields_changed", "credentials_replaced", "credentials_wiped")})
	Register("emr.connection_switched_on", Schema{Fields: withConn(), Required: connReq})
	Register("emr.connection_switched_off", Schema{Fields: withConn(), Required: connReq})
	Register("emr.connection_tested", Schema{Fields: withConn("outcome", OneOf(emrtypes.Codes...)),
		Required: append(slices.Clone(connReq), "outcome")})
	Register("emr.source_resumed", Schema{Fields: withConn("reason", OneOf("platform_resume")),
		Required: append(slices.Clone(connReq), "reason")})

	// A hospital's EMR rule set switched by a platform admin (EMR plan Task
	// 30; design §11.5): the version activated or "shadow", the one it
	// replaced, and the rule set file's SHA-256 (hex) when one is
	// activated (ApplyRuleSet writes it exactly then). No hospital column
	// (a platform action).
	ruleSetVersion := Pattern(regexp.MustCompile(`^(shadow|[a-z0-9][a-z0-9-]{0,62})$`))
	Register("emr.rule_set_switched", Schema{Fields: fields(
		"hospital_id", id, "version", ruleSetVersion, "previous", ruleSetVersion,
		"sha256", Pattern(regexp.MustCompile(`^[0-9a-f]{64}$`)), "via", OneOf("platform")),
		Required: []string{"hospital_id", "version", "previous", "via"}})

	// The EMR patient index (EMR design §10.2, §10.6, §12.6; plan Task 16,
	// written by Task 34's routes): ids, closed reason codes and counts;
	// never a name, an MRN or a source id. The gateway writes them with its
	// hospital.
	Register("emr.patients_merged", Schema{Fields: fields("match_id", id, "merge_id", id, "kept_patient_id", id,
		"merged_patient_id", id), Required: []string{"match_id", "merge_id", "kept_patient_id", "merged_patient_id"}})
	Register("emr.patient_match_rejected", Schema{Fields: fields("match_id", id), Required: []string{"match_id"}})
	Register("emr.patient_excluded", Schema{Fields: fields("patient_id", id,
		"reason", OneOf("objection", "hospital_policy", "other"), "via", OneOf("route", "cli"),
		"observations", count, "medications", count, "encounters", count, "raw_records", count, "matches", count),
		Required: []string{"patient_id", "reason", "via", "observations", "medications", "encounters", "raw_records", "matches"}})

	// Plan 7c-3: a platform admin follows a change up, edits the note or
	// undoes it (spec §7.4). Ids and whether there is a note only: the note
	// itself never reaches the log (A15).
	Register("analytics.followed_up", Schema{Fields: fields("change_id", id, "has_note", Check(Bool)),
		Required: []string{"change_id", "has_note"}})
	Register("analytics.follow_up_edited", Schema{Fields: fields("change_id", id), Required: []string{"change_id"}})
	Register("analytics.follow_up_undone", Schema{Fields: fields("change_id", id), Required: []string{"change_id"}})

	// Patient Analysis (analysis plan Task 4): the provisioning CLI raises
	// a switch for the synthetic demo only, and lowers one. The row has no
	// hospital and no actor; the payload names the hospital for a
	// hospital's switch and nothing for the global one.
	Register("analysis.switch_raised", Schema{Fields: fields("hospital_id", id, "via", OneOf("cli"), "refs", OneOf("demo")),
		Required: []string{"via", "refs"}})
	// A hospital made before hospitals.is_demo, marked as the demo once
	// (core hospitals mark-demo); the hospital is the row's entity.
	Register("hospital.marked_demo", Schema{Fields: fields("via", OneOf("cli")), Required: []string{"via"}})
	Register("analysis.switch_lowered", Schema{Fields: fields("hospital_id", id, "via", OneOf("cli")),
		Required: []string{"via"}})

	// Plan 3: the request-token boundary (internal/account, grant, intel).
	Register("intelligence.key_rotated", Schema{Fields: fields(
		"kid", token, "rotation", OneOf("planned", "emergency"), "change", OneOf("added", "removed")),
		Required: []string{"kid", "rotation", "change"}})
	Register("intelligence.granted", Schema{Fields: fields(
		"operation", operation, "jti", token, "bh", token, "patient_ids", List(50, id), "service", service),
		Required: []string{"operation", "jti", "service"}})
	Register("intelligence.grant_refused", Schema{Fields: fields("reason", code, "service", service, "operation", operation),
		Required: []string{"reason", "service"}})
	Register("intelligence.refused", Schema{Fields: fields(
		"operation", operation, "reason", OneOf("unauthorized", "forbidden", "invalid_request"), "kid", token),
		Required: []string{"operation", "reason"}})
	// Request/reply facts core records (intel replyFactSchemas).
	Register("intelligence.echo.pinged", Schema{})

	// Plan 3: questions (internal/queries) and their job facts (the
	// drainer, the sweeper, the dead-letter consumer). The drainer may add
	// fact_entity and fact_entity_id, or replace a payload that doesn't fit
	// intel's schema with {query_id, payload_refused}.
	Register("query.submitted", Schema{Fields: fields("query_id", id, "urgency", OneOf("routine", "urgent", "emergency")),
		Required: []string{"query_id", "urgency"}})
	drained := func(kv ...any) map[string]Check {
		m := fields(kv...)
		m["query_id"] = id
		m["fact_entity"] = Pattern(factEntityPattern)
		m["fact_entity_id"] = Pattern(factEntityIDPattern)
		m["payload_refused"] = b
		return m
	}
	Register("query.completed", Schema{Fields: drained(
		"decision", OneOf("PASS", "REGENERATE", "ESCALATE"),
		"trust_band", Nullable(OneOf("low", "mid", "high")),
		"chunk_ids", List(50, Pattern(chunkIDPattern)),
		"model_versions", StringMap(16, modelNamePattern, Pattern(modelVersionPattern)),
		"attempts", count,
		// Cloud model providers (plan Task 3): where the answer's text went
		// and the cloud outcome code; optional (older workers send neither).
		"egress", OneOf(config.EgressValues()...),
		"cloud_outcome", OneOf(config.CloudOutcomes...))})
	Register("query.failed", Schema{Fields: drained("error", errorCode, "attempts", count)})
	Register("query.cancelled", Schema{Fields: drained("reason", OneOf("user_deactivated", "hospital_deactivated", "account_unusable"))})
	Register("job.dead_lettered", Schema{Fields: fields(
		"job_id", id, "type", OneOf("run_query", "approve_correction"), "error", errorCode, "attempts", count, "kid", token),
		Required: []string{"job_id", "type", "error"}})
	// Two shapes (spec §9, Plan 4 Task 12): core's per-job row (the
	// dead-letter re-mint: job_id, msg_id) and the platform's one row per
	// retry call (job_count, 1-50, no job), whose actor is the admin. The
	// catalogue shows only the batch row (batchRow; ruling R24).
	Register("job.retried", Schema{Fields: fields("job_id", id, "msg_id", token, "job_count", Int(1, 50)),
		Shapes: []Shape{
			{Keys: []string{"job_id", "msg_id"}, Required: []string{"job_id", "msg_id"}},
			{Keys: []string{"job_count"}, Required: []string{"job_count"}},
		}})

	// Plan 4 (spec §9 rows 1-6): feedback and corrections. Ids and codes
	// only: never the reason, the corrected diagnosis or a rejection note.
	Register("feedback.validated", Schema{Fields: fields("query_id", id), Required: []string{"query_id"}})
	Register("correction.submitted", Schema{Fields: fields("correction_id", id, "query_id", id),
		Required: []string{"correction_id", "query_id"}})
	Register("correction.approved", Schema{Fields: fields("correction_id", id, "job_id", id),
		Required: []string{"correction_id", "job_id"}})
	Register("correction.rejected", Schema{Fields: fields("correction_id", id, "reason_code", OneOf(RejectReasonCodes...)),
		Required: []string{"correction_id", "reason_code"}})
	Register("correction.withdrawn", Schema{Fields: fields("correction_id", id), Required: []string{"correction_id"}})
	// Spec §9 names the refusal's key "code", but "code" is on the
	// sensitive-key list (an MFA code) that Write refuses at any depth;
	// "reason" is the key core's other refusals use for their code
	// (intelligence.refused, intelligence.grant_refused).
	Register("correction.decision_refused", Schema{Fields: fields(
		"correction_id", id,
		"reason", OneOf("self_approval", "forbidden", "already_decided"),
		"attempted", OneOf("approve", "reject", "withdraw")),
		Required: []string{"correction_id", "reason", "attempted"}})

	// Plan 4 (spec §9): the approve_correction job's facts, through the
	// outbox and the drainer, which checks intel's schema first (keys
	// required there, and correction_id bound to the job's subject) and
	// may replace a payload that doesn't fit with {job_id,
	// payload_refused}. The row's entity is the correction (the jobs row's
	// subject); job_id is the job's id, which the drainer sets.
	jobFact := func(kv ...any) map[string]Check {
		m := fields(kv...)
		m["job_id"] = id
		m["payload_refused"] = b
		return m
	}
	Register("correction.applied", Schema{Fields: jobFact(
		"correction_id", id, "attempts", count,
		"model_versions", StringMap(16, modelNamePattern, Pattern(modelVersionPattern)),
		"persona_update", OneOf("skipped", "applied")), Required: []string{"job_id"}})
	Register("correction.apply_failed", Schema{Fields: jobFact("correction_id", id, "error", errorCode, "attempts", count),
		Required: []string{"job_id"}})
	// A jobs row core cancelled (the sweeper: its creator can't use the
	// service any more), against the job. Keys as job.dead_lettered's
	// (type, not spec §9's job_type: Plan 3's frozen key) and as
	// query.cancelled's reason codes.
	// Widened for the platform's one row per cancel call (Plan 4 Task 12):
	// job_count (1-50), no job, and the reason as one of the platform's
	// three codes. Never the note (C16).
	Register("job.cancelled", Schema{Fields: fields(
		"job_id", id, "type", OneOf("run_query", "approve_correction"),
		"reason", OneOf("user_deactivated", "hospital_deactivated", "account_unusable",
			"no_longer_needed", "cant_succeed", "other"),
		"job_count", Int(1, 50)),
		Required: []string{"reason"},
		Shapes: []Shape{
			{Keys: []string{"job_id", "type", "reason"}, Required: []string{"job_id", "type", "reason"}},
			{Keys: []string{"job_count", "reason"}, Required: []string{"job_count", "reason"}},
		}})

	// Plan 4 (spec §9): the audit reader's own actions, written by core as
	// mhai_platform (00014's platform_append). Never the searched text, the
	// outcome note, or anything of the entries read.
	seq := Int(1, 1<<53-1)
	Register("audit.viewed", Schema{Fields: fields(
		"list", OneOf("events", "named_access"),
		"filters", Object(map[string]Check{
			"hospital": func(v any) bool { return v == "platform" || UUID(v) },
			"person":   id,
			"type":     List(len(AuditTypes), OneOf(AuditTypes...)),
			"outcome":  OneOf(Outcomes...),
			"flagged":  b,
			"from":     Instant,
			"to":       Instant,
		})),
		Required: []string{"list"}})
	// person and hospital: the person picker and the hospitals list (Task 9
	// review); run: the chain checks' reads (runs, a run, the status).
	// mhai-web has no such searched_kind yet, so those rows are gated out
	// of the catalogue and sent bare (TODO.md).
	Register("audit.searched", Schema{Fields: fields("searched_kind", OneOf("event", "request", "person", "hospital", "run")),
		Required: []string{"searched_kind"}})
	Register("audit.verify_started", Schema{Fields: fields("run_id", id, "by", OneOf("person", "schedule")),
		Required: []string{"run_id", "by"}})
	Register("audit.verify_finished", Schema{Fields: fields(
		"run_id", id, "state", OneOf("intact", "broken", "error"),
		"entries_checked", Int(0, 1<<53-1), "first_broken_seq", seq,
		"error_code", OneOf("timed_out", "unreachable", "unexpected_reply")),
		Required: []string{"run_id", "state"}})
	Register("audit.check_outcome", Schema{Fields: fields("run_id", id), Required: []string{"run_id"}})
	Register("audit.undisplayable_reported", Schema{Fields: fields(
		"count", Int(1, 50), "first_id", Pattern(eventIDPattern), "last_id", Pattern(eventIDPattern)),
		Required: []string{"count", "first_id", "last_id"}})
}

// emrTypeIDs are the connector type ids (gokit/emrtypes).
func emrTypeIDs() []string {
	var out []string
	for _, t := range emrtypes.Types {
		out = append(out, t.ID)
	}
	return out
}

// EMRConnectionKeys are the keys an emr.connection_changed payload may name
// in fields_changed: the connection's own settings and every field a
// connector type declares (gokit/emrtypes). Keys, never values.
func EMRConnectionKeys() []string {
	out := []string{"source_label", "base_url", "poll_seconds", "private_network", "private_cidrs"}
	for _, t := range emrtypes.Types {
		for _, f := range t.Fields {
			if !slices.Contains(out, f.Key) {
				out = append(out, f.Key)
			}
		}
	}
	return out
}
