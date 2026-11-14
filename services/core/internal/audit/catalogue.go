package audit

import (
	"encoding/json"
	"regexp"
	"strconv"
	"time"
)

// The audit catalogue (Plan 4 spec §5.3): how each chain row becomes the
// frontend contract's event. The chain stores action, entity, entity_id and
// a payload; the contract's event wants type, kind, target, outcome, flag
// and facts, and mhai-web fails closed on any value it doesn't know.
//
// Every action core writes has one row here, and nothing reaches the app
// that a row doesn't map: facts carry only the payload keys a row names,
// each copied from a payload that still fits the action's closed schema
// (schemas.go). An action with no row, a row that is withheld, a payload
// outside its schema, or anything shaped like a patient pseudonym is sent
// as the bare entry (Unmapped): id, seq, hash, at, security/unknown, which
// mhai-web shows as "can't be displayed" and reports once.
//
// The types, kinds, target kinds and facts keys are mhai-web's, vendored in
// testdata/web-audit-vocabulary.json; the kinds mhai-web has yet to ship are
// listed there under pending_in_web (TestCatalogueUsesOnlyWebVocabulary).

// AuditTypes are the contract's event types (mhai-web AUDIT_TYPES).
var AuditTypes = []string{"sign_in", "account", "staff", "hospital_settings", "consultation", "review", "alerts",
	"analysis", "named_access", "platform", "export", "audit", "retention", "security"}

// Outcomes are the contract's event outcomes.
var Outcomes = []string{"succeeded", "refused", "failed"}

// The bare entry's type and kind; a withheld row uses them too.
const (
	UnknownType = "security"
	UnknownKind = "unknown"
)

// Target is the event's target: a kind of mhai-web's and an opaque id.
// No row emits a patient target.
type Target struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Entry is one action's catalogue row. Target, Outcome and Flag may be nil
// (no target, "succeeded", no flag). Facts maps a payload key to the facts
// key its value is sent under. When, if set, says which of the action's
// rows the kind fits; any other row of the action is sent as the bare entry.
type Entry struct {
	Type, Kind string
	When       func(Event) bool
	Target     func(Event) *Target
	Outcome    func(Event) string
	Flag       func(Event) *string
	Facts      map[string]string
}

// Mapped is the part of the contract's event the catalogue derives. The
// reader adds the row's id, seq, hash, at, actor, hospital and refs.
type Mapped struct {
	Type    string         `json:"type"`
	Kind    string         `json:"kind"`
	Target  *Target        `json:"target"`
	Outcome string         `json:"outcome"`
	Flag    *string        `json:"flag"`
	Facts   map[string]any `json:"facts"`
}

// Row is a stored chain row: its sequence number (audit_log.id), hash and
// time, and the event it holds.
type Row struct {
	Seq   int64
	Hash  string
	At    time.Time
	Event Event
}

// Bare is the entry sent for a row the catalogue doesn't map.
type Bare struct {
	ID   string `json:"id"`
	Seq  int64  `json:"seq"`
	Hash string `json:"hash"`
	At   string `json:"at"`
	Type string `json:"type"`
	Kind string `json:"kind"`
}

// Unmapped is r as the bare entry: its position and hash only, never its
// action, payload, entity or actor.
func Unmapped(r Row) Bare {
	return Bare{
		ID: strconv.FormatInt(r.Seq, 10), Seq: r.Seq, Hash: r.Hash,
		At:   r.At.UTC().Format(time.RFC3339Nano),
		Type: UnknownType, Kind: UnknownKind,
	}
}

var (
	opaqueID   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	pseudonymS = regexp.MustCompile(`(^|[^A-Za-z0-9])P-[A-Z0-9]{4,12}([^A-Za-z0-9]|$)`)
)

// Map applies e's catalogue row. ok is false when e must be sent as the
// bare entry: no row, a withheld row, a payload outside the action's
// schema, or a target or facts value of the pseudonym's shape.
func Map(e Event) (m Mapped, ok bool) {
	row, found := Catalogue[e.Action]
	if !found {
		return Mapped{}, false
	}
	return mapRow(row, e)
}

// mapRow applies row to e.
func mapRow(row Entry, e Event) (m Mapped, ok bool) {
	if row.Kind == UnknownKind || (row.When != nil && !row.When(e)) {
		return Mapped{}, false
	}
	payload := e.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	// Re-checked on the way out, so a row written before its schema, or by
	// hand, can't put an unchecked value in front of the app.
	if CheckActionPayload(e.Action, payload) != nil {
		return Mapped{}, false
	}
	// Values as JSON decodes them, as the schema checked them.
	raw, err := json.Marshal(payload)
	if err != nil {
		return Mapped{}, false
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Mapped{}, false
	}
	m = Mapped{Type: row.Type, Kind: row.Kind, Outcome: "succeeded", Facts: map[string]any{}}
	if row.Target != nil {
		m.Target = row.Target(e)
		if m.Target != nil && (!opaqueID.MatchString(m.Target.ID) || holdsPseudonym(m.Target.ID)) {
			return Mapped{}, false
		}
	}
	if row.Outcome != nil {
		m.Outcome = row.Outcome(e)
	}
	if row.Flag != nil {
		m.Flag = row.Flag(e)
	}
	for from, to := range row.Facts {
		if v, present := decoded[from]; present {
			m.Facts[to] = v
		}
	}
	if holdsPseudonym(m.Facts) {
		return Mapped{}, false
	}
	return m, true
}

// holdsPseudonym reports whether any string in v, keys included, at any
// depth, holds something of the pseudonym's shape.
func holdsPseudonym(v any) bool {
	switch x := v.(type) {
	case string:
		return pseudonymS.MatchString(x)
	case []any:
		for _, e := range x {
			if holdsPseudonym(e) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if holdsPseudonym(k) || holdsPseudonym(e) {
				return true
			}
		}
	}
	return false
}

// Row helpers.

func str(e Event, key string) string {
	s, _ := e.Payload[key].(string)
	return s
}

func fixed(s string) func(Event) string { return func(Event) string { return s } }

func flagged(f string) func(Event) *string { return func(Event) *string { return &f } }

// entityTarget names the row's entity as a kind-of target, when the entity
// is the one expected and its id is a uuid.
func entityTarget(kind, entity string) func(Event) *Target {
	return func(e Event) *Target {
		if e.Entity != entity || !UUID(e.EntityID) {
			return nil
		}
		return &Target{Kind: kind, ID: e.EntityID}
	}
}

// runTarget names the verify run in the payload.
func runTarget(e Event) *Target {
	if id := str(e, "run_id"); UUID(id) {
		return &Target{Kind: "run", ID: id}
	}
	return nil
}

// batchRow reports whether a job row is the platform's one-per-call row:
// a job count, no entity, and none of the per-job keys (job_id, msg_id,
// type), so the gate is the schema's batch shape exactly.
func batchRow(e Event) bool {
	for _, k := range []string{"job_id", "msg_id", "type"} {
		if _, ok := e.Payload[k]; ok {
			return false
		}
	}
	_, hasCount := e.Payload["job_count"]
	return hasCount && e.Entity == "" && e.EntityID == ""
}

// searchedInWeb reports whether an audit.searched row's kind is one
// mhai-web's facts.searched_kind takes.
func searchedInWeb(e Event) bool {
	k := str(e, "searched_kind")
	return k == "event" || k == "request"
}

// safetyFlag flags a letter refused for the safety check (design §8.7).
func safetyFlag(e Event) *string {
	if str(e, "reason") == "failed_safety_check" {
		f := "security"
		return &f
	}
	return nil
}

// withheld is a row for an action mhai-web has no kind for yet: it is sent
// as the bare entry until one exists (see the vocabulary's notes).
func withheld() Entry { return Entry{Type: UnknownType, Kind: UnknownKind} }

// Catalogue holds one row per action core writes. It is read-only after
// init.
var Catalogue = map[string]Entry{
	// Plan 2: sign-in and sessions.
	"auth.login.succeeded": {Type: "sign_in", Kind: "signed_in"},
	"auth.login.failed":    {Type: "sign_in", Kind: "sign_in_refused", Outcome: fixed("refused")},
	"auth.login.locked":    {Type: "sign_in", Kind: "sign_in_locked", Outcome: fixed("refused"), Flag: flagged("security")},
	"auth.login.ceiling":   {Type: "sign_in", Kind: "sign_in_locked", Outcome: fixed("refused"), Flag: flagged("security")},
	"auth.logout":          {Type: "sign_in", Kind: "signed_out", Target: entityTarget("session", "session")},
	// A session ended by its owner (another device, or every other one on a
	// password change).
	"auth.session.revoked": {Type: "account", Kind: "device_signed_out", Target: entityTarget("session", "session")},
	// Plan 2: the account.
	"auth.password.changed":      {Type: "account", Kind: "password_changed"},
	"auth.mfa.enrolled":          {Type: "account", Kind: "two_step_set_up"},
	"auth.mfa.codes_regenerated": {Type: "account", Kind: "backup_codes_made"},
	// Plan 2: two-step refusals.
	"auth.mfa.failed": {Type: "security", Kind: "step_up_refused", Outcome: fixed("refused")},
	"auth.mfa.locked": {Type: "security", Kind: "mfa_locked", Outcome: fixed("refused"), Flag: flagged("security")},
	// Plan 2: no mhai-web kind yet.
	"auth.refresh.reuse":   withheld(),
	"auth.terms.accepted":  withheld(),
	"auth.password.failed": withheld(),
	"auth.mfa.verified":    withheld(),
	"auth.mfa.backup_used": withheld(),
	"auth.device.trusted":  withheld(),

	// Plan 2: the provisioning CLI.
	"hospital.created":   {Type: "hospital_settings", Kind: "hospital_added", Target: entityTarget("hospital", "hospital")},
	"hospital.activated": withheld(),
	"user.created":       withheld(),
	// Cloud model providers (Task 2): mhai-web has no kind for a model
	// policy change yet; sent bare until it has one.
	"hospital.model_policy_changed": withheld(),
	// Patient Analysis (Task 4): the CLI's demo raise and its lowering. No
	// mhai-web kind yet; the platform's switch actions come with Plan 11b.
	"analysis.switch_raised":  withheld(),
	"analysis.switch_lowered": withheld(),
	"hospital.marked_demo":    withheld(),
	// Patient Analysis reads (plan Task 10): mhai-web's analysis_viewed
	// kind takes facts this payload doesn't carry yet (H6); sent bare until
	// the mapping is agreed (TODO.md, "Before scale").
	"analysis.viewed":           withheld(),
	"analysis.unusual_querying": withheld(),
	// The drill-down (plan Tasks 13-14): mhai-web's analysis_patients_viewed
	// and audit_patients_viewed take facts and targets not agreed yet (H6):
	// sent bare until they are (TODO.md, "Before scale"). Bare entries carry
	// no payload, so no count leaves through them either.
	"analysis.patients_viewed":     withheld(),
	"analysis.list_limit_reached":  withheld(),
	"analysis.patient_cap_reached": withheld(),
	"audit.patients_viewed":        withheld(),
	// The analyst grant (plan Tasks 15-16): mhai-web's staff_role_changed
	// takes facts not agreed for these yet (H6); sent bare until they are.
	"analyst.granted": withheld(),
	"analyst.removed": withheld(),
	// Clearance letters (plan Tasks 18-20): mhai-web's Plan 11 kinds, with
	// the letter as the target and no facts. The demo's documents outcome
	// (ready) and the CLI's switch change have no kind yet: sent bare. A
	// file refused for the safety check is flagged (design §8.7).
	"clearance.uploaded":      {Type: "named_access", Kind: "clearance_uploaded", Target: entityTarget("clearance", "clearance")},
	"clearance.refused":       {Type: "named_access", Kind: "clearance_refused", Target: entityTarget("clearance", "clearance"), Flag: safetyFlag},
	"clearance.verified":      {Type: "named_access", Kind: "clearance_verified", Target: entityTarget("clearance", "clearance")},
	"clearance.rejected":      {Type: "named_access", Kind: "clearance_rejected", Target: entityTarget("clearance", "clearance")},
	"clearance.narrowed":      {Type: "named_access", Kind: "clearance_narrowed", Target: entityTarget("clearance", "clearance")},
	"clearance.revoked":       {Type: "named_access", Kind: "clearance_revoked", Target: entityTarget("clearance", "clearance")},
	"clearance.ended":         {Type: "named_access", Kind: "clearance_ended", Target: entityTarget("clearance", "clearance")},
	"clearance.viewed":        {Type: "named_access", Kind: "clearance_viewed", Target: entityTarget("clearance", "clearance")},
	"clearance.reminder_sent": {Type: "named_access", Kind: "clearance_reminder_sent", Target: entityTarget("clearance", "clearance")},
	"clearance.ready":         withheld(),
	"named_access.changed":    withheld(),

	// EMR connections (EMR design §12.6): mhai-web's Plan 13 kinds, with
	// the connection as the target. A resume has no kind yet: sent bare.
	"emr.connection_added":        {Type: "hospital_settings", Kind: "emr_connection_added", Target: entityTarget("connection", "emr_connection")},
	"emr.connection_changed":      {Type: "hospital_settings", Kind: "emr_connection_changed", Target: entityTarget("connection", "emr_connection")},
	"emr.connection_switched_on":  {Type: "hospital_settings", Kind: "emr_connection_switched_on", Target: entityTarget("connection", "emr_connection")},
	"emr.connection_switched_off": {Type: "hospital_settings", Kind: "emr_connection_switched_off", Target: entityTarget("connection", "emr_connection")},
	"emr.connection_tested":       {Type: "hospital_settings", Kind: "emr_connection_tested", Target: entityTarget("connection", "emr_connection")},
	"emr.source_resumed":          withheld(),
	"emr.source_paused":           withheld(),
	// The rule set switch (Task 30): no mhai-web kind yet; sent bare.
	"emr.rule_set_switched": withheld(),
	// The EMR patient index (Task 34's routes): no mhai-web kind yet.
	"emr.patients_merged":        withheld(),
	"emr.patient_match_rejected": withheld(),
	"emr.patient_excluded":       withheld(),
	"emr.patient_matches_read":   withheld(),

	// Alerts (EMR plan Tasks 31-34): the settings saves and an
	// acknowledgement have mhai-web kinds (Plan 13), with no target; the
	// rest have none yet and are sent bare.
	"alert_settings.units_changed":      {Type: "alerts", Kind: "alert_units_changed"},
	"alert_settings.rules_changed":      {Type: "alerts", Kind: "alert_rules_changed"},
	"alert_settings.recipients_changed": {Type: "alerts", Kind: "alert_recipients_changed"},
	"alert.acknowledged":                {Type: "alerts", Kind: "alert_acknowledged"},
	"alert.dismissed":                   withheld(),
	"alert.withdrawn":                   withheld(),
	"alert.raised":                      withheld(),
	"alert.updated":                     withheld(),
	"alert.not_raised":                  withheld(),
	"alert.requeued":                    withheld(),
	"alerts.read":                       withheld(),
	"notification_settings.changed":     withheld(),

	// Plan 7c-3: follow-ups on What changed. The target is the change; the
	// note is never in the payload, so none is sent.
	"analytics.followed_up":      {Type: "platform", Kind: "change_followed_up", Target: entityTarget("change", "analytics_change")},
	"analytics.follow_up_edited": {Type: "platform", Kind: "change_note_edited", Target: entityTarget("change", "analytics_change")},
	"analytics.follow_up_undone": {Type: "platform", Kind: "change_follow_up_undone", Target: entityTarget("change", "analytics_change")},

	// Plan 3: the request-token boundary. No mhai-web kind yet.
	"intelligence.key_rotated":   withheld(),
	"intelligence.granted":       withheld(),
	"intelligence.grant_refused": withheld(),
	"intelligence.refused":       withheld(),
	"intelligence.echo.pinged":   withheld(),

	// Plan 3: questions and their jobs.
	"query.submitted":   {Type: "consultation", Kind: "question_asked"},
	"query.completed":   withheld(),
	"query.failed":      withheld(),
	"query.cancelled":   withheld(),
	"job.dead_lettered": withheld(),
	// The platform's "Retried/Cancelled failed jobs" is the batch row only
	// (ruling R24): one row per call, no job, no entity, with job_count
	// (spec §5.3). The per-job rows core's loops write (the dead-letter
	// re-mint, the sweeper's cancels) are system rows, withheld like R21's.
	// The batch rows are the platform's retry and cancel (Plan 4 Task 12).
	// mhai-web has no facts key for the cancel reason: the count only.
	"job.retried":   {Type: "platform", Kind: "jobs_retried", When: batchRow, Facts: map[string]string{"job_count": "job_count"}},
	"job.cancelled": {Type: "platform", Kind: "jobs_cancelled", When: batchRow, Facts: map[string]string{"job_count": "job_count"}},

	// Plan 4: feedback and corrections (spec §5.3). No target: mhai-web has
	// no query or correction target kind.
	"feedback.validated":   {Type: "consultation", Kind: "answer_agreed"},
	"correction.submitted": {Type: "consultation", Kind: "answer_sent_for_review"},
	// Spec §5.3 adds facts.state approved/rejected; mhai-web's state is a
	// check's state (running, intact, broken, error) and would refuse the
	// entry, so no state is sent until mhai-web takes one.
	"correction.approved":  {Type: "review", Kind: "review_submitted"},
	"correction.rejected":  {Type: "review", Kind: "review_submitted"},
	"correction.withdrawn": {Type: "review", Kind: "review_withdrawn"},
	"correction.decision_refused": {Type: "security", Kind: "review_refused", Outcome: fixed("refused"),
		Flag: func(e Event) *string {
			if str(e, "reason") == "self_approval" {
				f := "policy"
				return &f
			}
			return nil
		}},
	"correction.applied":      {Type: "review", Kind: "learning_recorded"},
	"correction.apply_failed": {Type: "review", Kind: "learning_recorded", Outcome: fixed("failed")},

	// Plan 4: the audit reader's own actions (spec §5.2, §5.4).
	"audit.viewed": {Type: "audit", Kind: "audit_viewed", Facts: map[string]string{"list": "list", "filters": "filters"}},
	// Only the searched kinds mhai-web knows map; a person-picker or
	// hospitals-list read (searched_kind person, hospital) is sent bare
	// until mhai-web takes those values (TODO.md).
	"audit.searched": {Type: "audit", Kind: "audit_searched", When: searchedInWeb,
		Facts: map[string]string{"searched_kind": "searched_kind"}},
	"audit.verify_started": {Type: "audit", Kind: "audit_check_started", Target: runTarget},
	"audit.verify_finished": {Type: "audit", Kind: "audit_check_finished", Target: runTarget,
		Outcome: func(e Event) string {
			if str(e, "state") == "error" {
				return "failed"
			}
			return "succeeded"
		},
		Flag: func(e Event) *string {
			if str(e, "state") == "broken" {
				f := "security"
				return &f
			}
			return nil
		},
		Facts: map[string]string{"state": "state", "first_broken_seq": "first_broken_seq"}},
	"audit.check_outcome":          {Type: "audit", Kind: "audit_check_outcome", Target: runTarget},
	"audit.undisplayable_reported": {Type: "audit", Kind: "audit_undisplayable_reported"},
}
