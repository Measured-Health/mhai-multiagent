package platform

// Platform settings (mhai-web Plan 8c; platform operations design §7, §9;
// migration 00062). mhai-web's settings-contract.ts is the contract.
//
//   - GET /platform/settings: platform admins and auditors; the whole
//     SettingsOut: retention, triage, the learning-data buffer and the
//     waiting two-person requests (requested_by_you set per caller).
//   - GET /platform/settings/retention/{type}/preview?days: about how
//     many rows a period of days would cover (0 when it isn't shorter),
//     rounded up to a multiple of 10: counts only, every hospital with an
//     open legal hold left out.
//   - PUT /platform/settings/retention/{type} {days, reason, version,
//     code}: platform admins, step-up. Longer applies at once (200); a
//     shorter one makes a request a different platform admin approves
//     (202). Below the minimum is 422 below_minimum; the same period 422
//     validation_failed; a stale version, or a second shorter request for
//     the type, 409 conflict. A longer period ends a shorter one waiting on
//     the type (superseded, audited as cancelled).
//   - PUT /platform/settings/retention-pause {paused, reason, version,
//     code}: one platform admin, step-up, at once (pausing only keeps
//     more). version is the pause's own; a stale one, or the state asked
//     for already, is 409 conflict.
//   - PUT /platform/settings/triage {enabled, reason, clinical_signoff?,
//     version, code}: core has no triage. It is always off and never
//     ready (not_ready_reason missing_files: no validated scoring files),
//     so off is 409 conflict (already off) and on is 503 not_ready, both
//     before a code is spent.
//   - POST /platform/settings/requests/{id}/approve {version, code}: a
//     different platform admin (409 same_person), step-up, within 7 days
//     (409 expired), with the setting's current version (409 conflict),
//     while the requester is still an active platform admin (409
//     conflict). The period changes at once.
//   - POST /platform/settings/requests/{id}/cancel {version}: either
//     platform admin, no step-up.
//   - POST /platform/settings/feedback-buffer/reset {reason, code}: one
//     platform admin, step-up; 202 {job_id}. The reset runs in the request
//     (one UPDATE that marks every waiting row cleared, never a DELETE), so
//     the reply's settings show it done: running and failed_job_id are
//     always null.
//
// Nothing deletes by these periods yet: no clean-up job reads them, so
// last_run and next_run_at are null and confirmed is false (the periods
// are the placeholders awaiting the compliance lead). The database holds
// the two-person rule whatever the handler does (00062's CHECK and
// trigger). Every write takes one advisory lock (and the team lock,
// shared, as a hold's approval does), is audited by ids and codes only
// (the reason is kept in platform_setting_log), and answers the whole
// SettingsOut. A request that lapsed is ended by the next write, audited
// as lapsed by its requester (as mhai-web's sample API words it).

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

const (
	// SettingLapse is how long a settings request waits for its approval.
	SettingLapse = 7 * 24 * time.Hour
	// SettingWritesPerMinute is the settings writes one session may make a
	// minute.
	SettingWritesPerMinute = 10
	settingsScope          = "platform.settings.write_rate"
	// SettingReasonMax and SignoffMax are the reason's and the clinical
	// sign-off's longest, in characters (mhai-web's REASON_MAX,
	// SIGNOFF_MAX).
	SettingReasonMax = 500
	SignoffMax       = 200
	// maxDays is a period's longest (100 years; 00062's CHECK).
	maxDays = 36500
	// TriageNotReady is why triage can't be switched on: core has no
	// validated scoring files.
	TriageNotReady = "missing_files"
)

// RetentionTypes are the record types, in the page's order.
var RetentionTypes = []string{"records", "documents", "pending_corrections", "audit_log"}

// retentionWords are the types as Activity words them (mhai-web's
// RETENTION_WORDS).
var retentionWords = map[string]string{"records": "Records", "documents": "Documents",
	"pending_corrections": "Pending corrections", "audit_log": "Audit log"}

var (
	errSettingConflict = errors.New("platform: setting changed")
	errBelowMinimum    = errors.New("platform: below the minimum")
	errSamePeriod      = errors.New("platform: the same period")
	errNoRequest       = errors.New("platform: no such request")
	errRequestExpired  = errors.New("platform: request lapsed")
	errTriageNotReady  = errors.New("platform: triage not ready")
)

func registerSettings(m *authz.Mux, d Deps) {
	read := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: TeamRoles}
	write := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin}, Write: true}
	stepUp := write
	stepUp.StepUp = true
	authz.Route(m, http.MethodGet, "/platform/settings", read, http.HandlerFunc(d.settingsGet))
	authz.Route(m, http.MethodGet, "/platform/settings/retention/{id}/preview", read, http.HandlerFunc(d.retentionPreview))
	authz.Route(m, http.MethodPut, "/platform/settings/retention/{id}", stepUp, http.HandlerFunc(d.retentionSet))
	authz.Route(m, http.MethodPut, "/platform/settings/retention-pause", stepUp, http.HandlerFunc(d.retentionPause))
	authz.Route(m, http.MethodPut, "/platform/settings/triage", stepUp, http.HandlerFunc(d.triageSet))
	authz.Route(m, http.MethodPost, "/platform/settings/requests/{id}/approve", stepUp, http.HandlerFunc(d.settingApprove))
	authz.Route(m, http.MethodPost, "/platform/settings/requests/{id}/cancel", write, http.HandlerFunc(d.settingCancel))
	authz.Route(m, http.MethodPost, "/platform/settings/feedback-buffer/reset", stepUp, http.HandlerFunc(d.feedbackReset))
}

// --- the reply ---------------------------------------------------------------

// RetentionRun is a clean-up's last run (always null: no clean-up runs by
// these periods yet).
type RetentionRun struct {
	At           string `json:"at"`
	OK           bool   `json:"ok"`
	Deleted      int    `json:"deleted"`
	KeptForHolds int    `json:"kept_for_holds"`
}

// RetentionItem is one record type's period.
type RetentionItem struct {
	Type        string        `json:"type"`
	Days        int           `json:"days"`
	MinimumDays int           `json:"minimum_days"`
	LastRun     *RetentionRun `json:"last_run"`
	NextRunAt   *string       `json:"next_run_at"`
	ChangedBy   *Person       `json:"changed_by"`
	ChangedAt   *string       `json:"changed_at"`
	Version     int           `json:"version"`
}

// SettingsRetention is the retention section.
type SettingsRetention struct {
	Confirmed bool            `json:"confirmed"`
	Paused    bool            `json:"paused"`
	PausedBy  *Person         `json:"paused_by"`
	PausedAt  *string         `json:"paused_at"`
	Version   int             `json:"version"`
	Items     []RetentionItem `json:"items"`
}

// SettingsTriage is the triage gate.
type SettingsTriage struct {
	Enabled        bool    `json:"enabled"`
	Ready          bool    `json:"ready"`
	NotReadyReason *string `json:"not_ready_reason"`
	ChangedBy      *Person `json:"changed_by"`
	ChangedAt      *string `json:"changed_at"`
	Version        int     `json:"version"`
}

// FeedbackReset is the last learning-data reset.
type FeedbackReset struct {
	By      Person `json:"by"`
	At      string `json:"at"`
	Reason  string `json:"reason"`
	Cleared int    `json:"cleared"`
}

// FeedbackRunning is a reset under way (never: the reset runs in its
// request).
type FeedbackRunning struct {
	JobID     string `json:"job_id"`
	StartedAt string `json:"started_at"`
}

// SettingsBuffer is the learning-data buffer.
type SettingsBuffer struct {
	Waiting     int              `json:"waiting"`
	LastReset   *FeedbackReset   `json:"last_reset"`
	Running     *FeedbackRunning `json:"running"`
	FailedJobID *string          `json:"failed_job_id"`
}

// SettingRequest is a waiting two-person request: a shorter period.
type SettingRequest struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	RequestedBy    Person `json:"requested_by"`
	RequestedAt    string `json:"requested_at"`
	Reason         string `json:"reason"`
	RequestedByYou bool   `json:"requested_by_you"`
	ExpiresAt      string `json:"expires_at"`
	Version        int    `json:"version"`
	Type           string `json:"type"`
	From           int    `json:"from"`
	To             int    `json:"to"`
}

// SettingsOut is GET /platform/settings and every settings write's reply.
type SettingsOut struct {
	Retention      SettingsRetention `json:"retention"`
	Triage         SettingsTriage    `json:"triage"`
	FeedbackBuffer SettingsBuffer    `json:"feedback_buffer"`
	Pending        []SettingRequest  `json:"pending"`
}

// retentionRow is a stored period.
type retentionRow struct {
	days, minimum, version int
}

func readRetention(ctx context.Context, tx pgx.Tx, typ string) (retentionRow, error) {
	var r retentionRow
	err := tx.QueryRow(ctx, `SELECT days, minimum_days, version FROM platform_retention WHERE type = $1`, typ).
		Scan(&r.days, &r.minimum, &r.version)
	return r, err
}

func optStamp(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := stamp(*t)
	return &s
}

// readSettings reads the whole reply as you see it. A request past its
// expiry is not waiting (the next write ends it).
func readSettings(ctx context.Context, tx pgx.Tx, you uuid.UUID) (SettingsOut, error) {
	notReady := TriageNotReady
	out := SettingsOut{
		Retention: SettingsRetention{Items: []RetentionItem{}},
		Triage:    SettingsTriage{NotReadyReason: &notReady, Version: 1},
		Pending:   []SettingRequest{},
	}
	rows, err := tx.Query(ctx, `SELECT r.type, r.days, r.minimum_days, r.version, r.changed_by, u.display_name, r.changed_at
		FROM platform_retention r LEFT JOIN users u ON u.id = r.changed_by
		ORDER BY array_position(ARRAY['records', 'documents', 'pending_corrections', 'audit_log'], r.type)`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var it RetentionItem
		var by *uuid.UUID
		var name *string
		var at *time.Time
		if err := rows.Scan(&it.Type, &it.Days, &it.MinimumDays, &it.Version, &by, &name, &at); err != nil {
			rows.Close()
			return out, err
		}
		it.ChangedBy, it.ChangedAt = optPerson(by, name), optStamp(at)
		out.Retention.Items = append(out.Retention.Items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	var by *uuid.UUID
	var name *string
	var at *time.Time
	if err := tx.QueryRow(ctx, `SELECT p.paused, p.paused_by, u.display_name, p.paused_at, p.version
		FROM platform_retention_pause p LEFT JOIN users u ON u.id = p.paused_by`).
		Scan(&out.Retention.Paused, &by, &name, &at, &out.Retention.Version); err != nil {
		return out, err
	}
	out.Retention.PausedBy, out.Retention.PausedAt = optPerson(by, name), optStamp(at)

	var waiting int64
	if err := tx.QueryRow(ctx, `SELECT mhai_feedback_buffer_waiting()`).Scan(&waiting); err != nil {
		return out, err
	}
	out.FeedbackBuffer.Waiting = int(waiting)
	var l FeedbackReset
	var lby uuid.UUID
	var lname *string
	var lat time.Time
	err = tx.QueryRow(ctx, `SELECT f.by, u.display_name, f.at, f.reason, f.cleared FROM platform_feedback_resets f
		LEFT JOIN users u ON u.id = f.by ORDER BY f.at DESC, f.id LIMIT 1`).Scan(&lby, &lname, &lat, &l.Reason, &l.Cleared)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return out, err
	default:
		l.By, l.At = person(&lby, lname), stamp(lat)
		out.FeedbackBuffer.LastReset = &l
	}

	rows, err = tx.Query(ctx, `SELECT q.id, q.requested_by, u.display_name, q.requested_at, q.reason, q.expires_at, r.version,
			q.type, q.from_days, q.to_days
		FROM platform_setting_requests q JOIN platform_retention r ON r.type = q.type
		LEFT JOIN users u ON u.id = q.requested_by
		WHERE q.approved_at IS NULL AND q.ended_at IS NULL AND q.expires_at > now()
		ORDER BY q.requested_at, q.id`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var q SettingRequest
		var id, by uuid.UUID
		var name *string
		var at, exp time.Time
		if err := rows.Scan(&id, &by, &name, &at, &q.Reason, &exp, &q.Version, &q.Type, &q.From, &q.To); err != nil {
			return out, err
		}
		q.ID, q.Kind, q.RequestedBy = id.String(), "retention_shorten", person(&by, name)
		q.RequestedAt, q.ExpiresAt, q.RequestedByYou = stamp(at), stamp(exp), by == you
		out.Pending = append(out.Pending, q)
	}
	return out, rows.Err()
}

// --- the reads -----------------------------------------------------------------

func (d Deps) settingsGet(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var out SettingsOut
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		out, err = readSettings(r.Context(), tx, p.UserID)
		return err
	})
	if err != nil {
		d.fail(w, r, p, "settings read")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

// pathRetention is the path's record type, or false (404).
func pathRetention(r *http.Request) (string, bool) {
	t := r.PathValue("id")
	return t, slices.Contains(RetentionTypes, t)
}

func (d Deps) retentionPreview(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	typ, ok := pathRetention(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	v, err := params(r, []string{"days"})
	if err != nil || v["days"] == "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	days, err := strconv.Atoi(v["days"])
	if err != nil || days < 1 || days > maxDays || strconv.Itoa(days) != v["days"] {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var n int64
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		cur, err := readRetention(r.Context(), tx, typ)
		if err != nil || days >= cur.days {
			return err
		}
		return tx.QueryRow(r.Context(), `SELECT mhai_retention_preview($1, $2)`, typ, days).Scan(&n)
	})
	n = PreviewRound(n)
	if err != nil {
		d.fail(w, r, p, "retention preview")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, map[string]int64{"would_delete": n})
}

// PreviewRound rounds a preview's count up to the next multiple of 10
// (mhai-web says "about"): any period may be asked for, so exact counts a
// day apart would give one day's rows, of one hospital when only one has
// rows that old. Up, never down: it never says less would go.
func PreviewRound(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n + 9) / 10 * 10
}

// --- the writes ----------------------------------------------------------------

// settingReason is a trimmed reason or sign-off of 1 to max characters with
// no control, format or separator character but a newline (mhai-web's
// NOTE rule); ok false otherwise.
func settingReason(s string, max int) (string, bool) {
	t := strings.TrimSpace(s)
	n := 0
	for _, c := range t {
		n++
		if c != '\n' && (unicode.IsControl(c) || unicode.Is(unicode.Cf, c) || unicode.Is(unicode.Zl, c) || unicode.Is(unicode.Zp, c)) {
			return "", false
		}
	}
	return t, n >= 1 && n <= max
}

func (d Deps) settingsAnswer(w http.ResponseWriter, r *http.Request, p authz.Principal, what string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errSettingConflict):
		httpx.Error(w, r, "conflict")
	case errors.Is(err, errBelowMinimum):
		httpx.Error(w, r, "below_minimum")
	case errors.Is(err, errSamePeriod):
		httpx.Error(w, r, "validation_failed")
	case errors.Is(err, errNoRequest):
		httpx.Error(w, r, "not_found")
	case errors.Is(err, errRequestExpired):
		httpx.Error(w, r, "expired")
	case errors.Is(err, errSamePerson):
		httpx.Error(w, r, "same_person")
	case errors.Is(err, errTriageNotReady):
		httpx.Error(w, r, "not_ready")
	default:
		d.fail(w, r, p, what)
	}
	return true
}

// settingsLock serialises the settings writes, and holds the team lock
// shared: a removal or demotion of a requester (which ends their
// requests) either committed before this reads, or waits for it.
func settingsLock(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtext('mhai.platform_team'))`); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('mhai.platform_settings'))`)
	return err
}

func settingAudit(ctx context.Context, tx pgx.Tx, actor uuid.UUID, rid, action, entity, entityID string, payload map[string]any) error {
	return audit.Write(ctx, tx, audit.Event{ActorID: actor.String(), RequestID: rid, Action: action,
		Entity: entity, EntityID: entityID, Payload: payload})
}

func settingLog(ctx context.Context, tx pgx.Tx, by uuid.UUID, action, reason string) error {
	_, err := tx.Exec(ctx, `INSERT INTO platform_setting_log (action, by, reason) VALUES ($1, $2, $3)`, action, by, reason)
	return err
}

// endRequests ends the open requests the WHERE picks (on top of being
// open; its arguments start at $3) with reason, by `by` (nil: the requester, a lapse), bumps each
// one's setting and audits each as action.
func endRequests(ctx context.Context, tx pgx.Tx, by *uuid.UUID, rid, reason, action, where string, args ...any) error {
	rows, err := tx.Query(ctx, `UPDATE platform_setting_requests q SET ended_at = now(), end_reason = $2,
			ended_by = COALESCE($1::uuid, q.requested_by)
		WHERE q.approved_at IS NULL AND q.ended_at IS NULL AND `+where+`
		RETURNING q.id, q.type, q.from_days, q.to_days, q.requested_by`, append([]any{by, reason}, args...)...)
	if err != nil {
		return err
	}
	type ended struct {
		id, by   uuid.UUID
		typ      string
		from, to int
	}
	var all []ended
	for rows.Next() {
		var e ended
		if err := rows.Scan(&e.id, &e.typ, &e.from, &e.to, &e.by); err != nil {
			rows.Close()
			return err
		}
		all = append(all, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range all {
		// A lapse leaves the setting's version (the person saw nothing
		// change); an act on a request bumps it.
		if reason != "lapsed" {
			if _, err := tx.Exec(ctx, `UPDATE platform_retention SET version = version + 1 WHERE type = $1`, e.typ); err != nil {
				return err
			}
		}
		actor := e.by
		if by != nil {
			actor = *by
		}
		if err := settingAudit(ctx, tx, actor, rid, action, "setting_request", e.id.String(),
			map[string]any{"request_id": e.id.String(), "type": e.typ, "from": e.from, "to": e.to}); err != nil {
			return err
		}
	}
	return nil
}

// lapseRequests ends every request past its expiry.
func lapseRequests(ctx context.Context, tx pgx.Tx, rid string) error {
	return endRequests(ctx, tx, nil, rid, "lapsed", "platform.setting_change_lapsed", "q.expires_at <= now()")
}

// cancelMemberRequests ends the open settings requests of member (removed
// or demoted: no longer one of the two people), audited as cancelled by
// the admin who changed the team. It runs in the team write's transaction.
func cancelMemberRequests(ctx context.Context, tx pgx.Tx, p authz.Principal, rid string, member uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('mhai.platform_settings'))`); err != nil {
		return err
	}
	by := p.UserID
	return endRequests(ctx, tx, &by, rid, "requester_left", "platform.setting_change_cancelled", "q.requested_by = $3", member)
}

// settingsWrite runs one settings write: check (read-only, before a code
// is spent), the session's minute, the code when code is set, then under
// the lock the lapses, check again and apply, and the whole reply.
func (d Deps) settingsWrite(w http.ResponseWriter, r *http.Request, p authz.Principal, what string, code *stepUpBody,
	check func(tx pgx.Tx) error, apply func(tx pgx.Tx, rid string) (int, error)) {
	ctx := r.Context()
	if err := d.tx(ctx, check); d.settingsAnswer(w, r, p, what+" check", err) {
		return
	}
	allowed, err := d.reserveRate(r, p, settingsScope, "platform.settings.write.v1", SettingWritesPerMinute)
	switch {
	case err != nil:
		d.log().Error("platform: settings write refused: rate counter unavailable", "request_id", httpx.RequestIDFrom(ctx),
			"reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return
	case !allowed:
		httpx.Error(w, r, "rate_limited")
		return
	}
	if code != nil && !d.stepUp(w, r, code.MFACode, code.BackupCode) {
		return
	}
	rid := httpx.RequestIDFrom(ctx)
	status := http.StatusOK
	var out SettingsOut
	err = d.tx(ctx, func(tx pgx.Tx) error {
		if err := settingsLock(ctx, tx); err != nil {
			return err
		}
		if err := lapseRequests(ctx, tx, rid); err != nil {
			return err
		}
		if err := check(tx); err != nil {
			return err
		}
		var err error
		if status, err = apply(tx, rid); err != nil {
			return err
		}
		out, err = readSettings(ctx, tx, p.UserID)
		return err
	})
	if d.settingsAnswer(w, r, p, what, err) {
		return
	}
	httpx.JSON(w, status, out)
}

// stepUpReady answers 503 not_ready when step-up can't be checked.
func (d Deps) stepUpReady(w http.ResponseWriter, r *http.Request) bool {
	if d.Auth == nil {
		httpx.Error(w, r, "not_ready")
		return false
	}
	return true
}

type retentionBody struct {
	Days    *int   `json:"days"`
	Reason  string `json:"reason"`
	Version *int   `json:"version"`
	stepUpBody
}

// retentionSet is PUT /platform/settings/retention/{type}.
func (d Deps) retentionSet(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	typ, ok := pathRetention(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var b retentionBody
	if !httpx.Decode(w, r, &b) {
		return
	}
	reason, okReason := settingReason(b.Reason, SettingReasonMax)
	if b.Days == nil || *b.Days < 1 || *b.Days > maxDays || !okReason || b.Version == nil || *b.Version < 1 {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !d.stepUpReady(w, r) {
		return
	}
	days := *b.Days
	var cur retentionRow
	check := func(tx pgx.Tx) error {
		var err error
		if cur, err = readRetention(r.Context(), tx, typ); err != nil {
			return err
		}
		switch {
		case cur.version != *b.Version:
			return errSettingConflict
		case days < cur.minimum:
			return errBelowMinimum
		case days == cur.days:
			return errSamePeriod
		}
		if days < cur.days {
			var open bool
			if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM platform_setting_requests
				WHERE type = $1 AND approved_at IS NULL AND ended_at IS NULL AND expires_at > now())`, typ).Scan(&open); err != nil {
				return err
			}
			if open {
				return errSettingConflict
			}
		}
		return nil
	}
	apply := func(tx pgx.Tx, rid string) (int, error) {
		ctx := r.Context()
		if days > cur.days {
			// Keeping more is safe: at once, and it ends a shorter one
			// waiting on the type.
			by := p.UserID
			if err := endRequests(ctx, tx, &by, rid, "superseded", "platform.setting_change_cancelled", "q.type = $3", typ); err != nil {
				return 0, err
			}
			if _, err := tx.Exec(ctx, `UPDATE platform_retention SET days = $2, version = version + 1, changed_by = $3,
				changed_at = now() WHERE type = $1`, typ, days, p.UserID); err != nil {
				return 0, err
			}
			if err := settingLog(ctx, tx, p.UserID, "retention_changed", reason); err != nil {
				return 0, err
			}
			return http.StatusOK, settingAudit(ctx, tx, p.UserID, rid, "platform.retention_changed", "retention", typ,
				map[string]any{"type": typ, "from": cur.days, "to": days})
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO platform_setting_requests (kind, type, from_days, to_days, reason, requested_by, expires_at)
			VALUES ('retention_shorten', $1, $2, $3, $4, $5, now() + make_interval(secs => $6)) RETURNING id`,
			typ, cur.days, days, reason, p.UserID, SettingLapse.Seconds()).Scan(&id); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE platform_retention SET version = version + 1 WHERE type = $1`, typ); err != nil {
			return 0, err
		}
		if err := settingLog(ctx, tx, p.UserID, "setting_change_requested", reason); err != nil {
			return 0, err
		}
		return http.StatusAccepted, settingAudit(ctx, tx, p.UserID, rid, "platform.setting_change_requested", "setting_request",
			id.String(), map[string]any{"request_id": id.String(), "type": typ, "from": cur.days, "to": days})
	}
	d.settingsWrite(w, r, p, "retention change", &b.stepUpBody, check, apply)
}

type pauseBody struct {
	Paused  *bool  `json:"paused"`
	Reason  string `json:"reason"`
	Version *int   `json:"version"`
	stepUpBody
}

// retentionPause is PUT /platform/settings/retention-pause.
func (d Deps) retentionPause(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var b pauseBody
	if !httpx.Decode(w, r, &b) {
		return
	}
	reason, okReason := settingReason(b.Reason, SettingReasonMax)
	if b.Paused == nil || !okReason || b.Version == nil || *b.Version < 1 {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !d.stepUpReady(w, r) {
		return
	}
	check := func(tx pgx.Tx) error {
		var paused bool
		var version int
		if err := tx.QueryRow(r.Context(), `SELECT paused, version FROM platform_retention_pause`).Scan(&paused, &version); err != nil {
			return err
		}
		if version != *b.Version || paused == *b.Paused {
			return errSettingConflict
		}
		return nil
	}
	apply := func(tx pgx.Tx, rid string) (int, error) {
		ctx := r.Context()
		logKind, audited := "clean_up_resumed", "platform.clean_up_resumed"
		var err error
		if *b.Paused {
			logKind, audited = "clean_up_paused", "platform.clean_up_paused"
			_, err = tx.Exec(ctx, `UPDATE platform_retention_pause SET paused = true, paused_by = $1, paused_at = now(),
				version = version + 1`, p.UserID)
		} else {
			_, err = tx.Exec(ctx, `UPDATE platform_retention_pause SET paused = false, paused_by = NULL, paused_at = NULL,
				version = version + 1`)
		}
		if err != nil {
			return 0, err
		}
		if err := settingLog(ctx, tx, p.UserID, logKind, reason); err != nil {
			return 0, err
		}
		return http.StatusOK, settingAudit(ctx, tx, p.UserID, rid, audited, "retention_pause", "", map[string]any{})
	}
	d.settingsWrite(w, r, p, "clean-up pause", &b.stepUpBody, check, apply)
}

type triageBody struct {
	Enabled         *bool   `json:"enabled"`
	Reason          string  `json:"reason"`
	ClinicalSignoff *string `json:"clinical_signoff"`
	Version         *int    `json:"version"`
	stepUpBody
}

// triageSet is PUT /platform/settings/triage: core has no triage, so
// every call is refused before a code is spent (off: already off, 409
// conflict; on: never ready, 503 not_ready; a stale version: 409).
func (d Deps) triageSet(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var b triageBody
	if !httpx.Decode(w, r, &b) {
		return
	}
	_, okReason := settingReason(b.Reason, SettingReasonMax)
	if b.Enabled == nil || !okReason || b.Version == nil || *b.Version < 1 {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if *b.Enabled {
		if b.ClinicalSignoff == nil {
			httpx.Error(w, r, "validation_failed")
			return
		}
		if _, ok := settingReason(*b.ClinicalSignoff, SignoffMax); !ok {
			httpx.Error(w, r, "validation_failed")
			return
		}
	}
	var err error
	switch {
	case *b.Version != 1, !*b.Enabled:
		err = errSettingConflict
	default:
		err = errTriageNotReady
	}
	d.settingsAnswer(w, r, p, "triage", err)
}

// pathRequest is the path's request id (a canonical uuid), or false (404).
func pathRequest(r *http.Request) (uuid.UUID, bool) { return pathMember(r) }

// openRequest reads request id for an approval or cancel: 404 for none;
// expired for one that lapsed (ended or not), when lapsed is set, else
// conflict; conflict for one approved or ended otherwise, or when version
// isn't its setting's. It returns the request's type, periods and
// requester.
func openRequest(ctx context.Context, tx pgx.Tx, id uuid.UUID, version int, lapsedIsExpired bool) (string, int, int, uuid.UUID, error) {
	var typ, end string
	var from, to, cur int
	var by uuid.UUID
	var approved, live bool
	err := tx.QueryRow(ctx, `SELECT q.type, q.from_days, q.to_days, q.requested_by, q.approved_at IS NOT NULL,
			COALESCE(q.end_reason, ''), q.expires_at > now(), r.version
		FROM platform_setting_requests q JOIN platform_retention r ON r.type = q.type WHERE q.id = $1`, id).
		Scan(&typ, &from, &to, &by, &approved, &end, &live, &cur)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", 0, 0, by, errNoRequest
	case err != nil:
		return "", 0, 0, by, err
	case lapsedIsExpired && !approved && (end == "lapsed" || (end == "" && !live)):
		return typ, from, to, by, errRequestExpired
	case approved || end != "" || !live:
		return typ, from, to, by, errSettingConflict
	case version != cur:
		return typ, from, to, by, errSettingConflict
	}
	return typ, from, to, by, nil
}

// settingApprove is POST /platform/settings/requests/{id}/approve.
func (d Deps) settingApprove(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathRequest(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var b versionBody
	if !httpx.Decode(w, r, &b) {
		return
	}
	if b.Version == nil || *b.Version < 1 {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !d.stepUpReady(w, r) {
		return
	}
	var typ string
	var from, to int
	check := func(tx pgx.Tx) error {
		var by uuid.UUID
		var err error
		if typ, from, to, by, err = openRequest(r.Context(), tx, id, *b.Version, true); err != nil {
			return err
		}
		// Only the server's record of who asked decides, never a name.
		if by == p.UserID {
			return errSamePerson
		}
		// The requester must still be an active platform admin, and the
		// period still the one the request shortens.
		var still bool
		if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM users u WHERE u.id = $1 AND u.is_active
			AND u.hospital_id IS NULL AND EXISTS (SELECT 1 FROM user_roles x WHERE x.user_id = u.id AND x.role = 'platform_admin'))
			AND (SELECT days FROM platform_retention WHERE type = $2) = $3`, by, typ, from).Scan(&still); err != nil {
			return err
		}
		if !still {
			return errSettingConflict
		}
		return nil
	}
	apply := func(tx pgx.Tx, rid string) (int, error) {
		ctx := r.Context()
		if _, err := tx.Exec(ctx, `UPDATE platform_setting_requests SET approved_by = $2, approved_at = now() WHERE id = $1`,
			id, p.UserID); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE platform_retention SET days = $2, version = version + 1, changed_by = $3, changed_at = now()
			WHERE type = $1`, typ, to, p.UserID); err != nil {
			return 0, err
		}
		return http.StatusOK, settingAudit(ctx, tx, p.UserID, rid, "platform.setting_change_approved", "setting_request", id.String(),
			map[string]any{"request_id": id.String(), "type": typ, "from": from, "to": to})
	}
	d.settingsWrite(w, r, p, "settings approval", &stepUpBody{MFACode: b.MFACode, BackupCode: b.BackupCode}, check, apply)
}

// settingCancel is POST /platform/settings/requests/{id}/cancel: either
// platform admin, no step-up.
func (d Deps) settingCancel(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathRequest(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var b struct {
		Version *int `json:"version"`
	}
	if !httpx.Decode(w, r, &b) {
		return
	}
	if b.Version == nil || *b.Version < 1 {
		httpx.Error(w, r, "validation_failed")
		return
	}
	check := func(tx pgx.Tx) error {
		_, _, _, _, err := openRequest(r.Context(), tx, id, *b.Version, false)
		return err
	}
	apply := func(tx pgx.Tx, rid string) (int, error) {
		by := p.UserID
		return http.StatusOK, endRequests(r.Context(), tx, &by, rid, "cancelled", "platform.setting_change_cancelled", "q.id = $3", id)
	}
	d.settingsWrite(w, r, p, "settings cancel", nil, check, apply)
}

// feedbackReset is POST /platform/settings/feedback-buffer/reset.
func (d Deps) feedbackReset(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var b struct {
		Reason string `json:"reason"`
		stepUpBody
	}
	if !httpx.Decode(w, r, &b) {
		return
	}
	reason, okReason := settingReason(b.Reason, SettingReasonMax)
	if !okReason {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !d.stepUpReady(w, r) {
		return
	}
	ctx := r.Context()
	allowed, err := d.reserveRate(r, p, settingsScope, "platform.settings.write.v1", SettingWritesPerMinute)
	switch {
	case err != nil:
		httpx.Error(w, r, "not_ready")
		return
	case !allowed:
		httpx.Error(w, r, "rate_limited")
		return
	}
	if !d.stepUp(w, r, b.MFACode, b.BackupCode) {
		return
	}
	rid := httpx.RequestIDFrom(ctx)
	var reset uuid.UUID
	err = d.tx(ctx, func(tx pgx.Tx) error {
		if err := settingsLock(ctx, tx); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT reset_id, cleared FROM mhai_feedback_buffer_reset($1, $2)`, p.UserID, reason).
			Scan(&reset, &n); err != nil {
			return err
		}
		return settingAudit(ctx, tx, p.UserID, rid, "platform.feedback_buffer_cleared", "feedback_reset", reset.String(),
			map[string]any{"reset_id": reset.String(), "cleared": n})
	})
	if err != nil {
		d.fail(w, r, p, "learning-data reset")
		return
	}
	httpx.JSON(w, http.StatusAccepted, map[string]string{"job_id": "j_" + reset.String()})
}
