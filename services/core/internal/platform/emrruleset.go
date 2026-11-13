package platform

// A hospital's EMR rule set (EMR plan Task 30; design §11.5):
//
//   - GET /platform/hospitals/{id}/emr-rule-set: platform admins and
//     auditors; the activated version and hash, or shadow (both null).
//   - PUT /platform/hospitals/{id}/emr-rule-set {version, sha256,
//     mfa_code | backup_code}: platform admins, with step-up (as the model
//     policy). A version with the rule set file's SHA-256 (as "emr rules
//     check" prints it) activates that exact file for the hospital; null
//     for both puts the hospital back in shadow. The write and its audit
//     row (emr.rule_set_switched) are one transaction. Off (no
//     CORE_EMR_ENABLED), the PUT answers 503 not_ready.
//
// Activation alone doesn't alert: the emr service raises non-shadow
// findings only when the deployment names the same version
// (EMR_RULE_SET_ACTIVE) and the loaded file has the same hash. Until then
// every finding is shadow: stored for the clinical review, never an alert.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

const (
	// RuleSetShadow is "no rule set activated" in the audit payload.
	RuleSetShadow = "shadow"
	// ruleSetScope is the idempotency_keys scope of the PUT's rate counter.
	ruleSetScope = "platform.emr_rule_set.write_rate"
)

var (
	ruleSetVersionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	sha256Pattern         = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func registerEMRRuleSet(m *authz.Mux, d Deps) {
	authz.Route(m, http.MethodGet, "/platform/hospitals/{id}/emr-rule-set",
		authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin, authz.RoleAuditor}},
		http.HandlerFunc(d.ruleSetGet))
	authz.Route(m, http.MethodPut, "/platform/hospitals/{id}/emr-rule-set",
		authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin}, Write: true, StepUp: true},
		http.HandlerFunc(d.ruleSetPut))
}

// RuleSetOut is a hospital's rule set as both routes answer it.
type RuleSetOut struct {
	HospitalID  string  `json:"hospital_id"`
	Version     *string `json:"version"`
	SHA256      *string `json:"sha256"`
	ActivatedAt *string `json:"activated_at"`
}

// ErrBadRuleSet: a switch outside the rules (callers check first).
var ErrBadRuleSet = errors.New("platform: invalid rule set switch")

// RuleSetSwitch is one switch: Version "" (and no hash) is shadow.
type RuleSetSwitch struct {
	Hospital  uuid.UUID
	Version   string
	SHA256    []byte
	Actor     uuid.UUID
	RequestID string
}

// ApplyRuleSet writes s in tx (as mhai_platform) and appends
// emr.rule_set_switched in the same transaction. Switches of one hospital
// are serialised by a transaction advisory lock. It returns the version it
// replaced (RuleSetShadow for none).
func ApplyRuleSet(ctx context.Context, tx pgx.Tx, s RuleSetSwitch) (string, error) {
	if s.Hospital == uuid.Nil || (s.Version == "") != (s.SHA256 == nil) || (s.Version != "" &&
		(!ruleSetVersionPattern.MatchString(s.Version) || len(s.SHA256) != 32)) {
		return "", ErrBadRuleSet
	}
	ok, err := hospitalExists(ctx, tx, s.Hospital)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrNoSuchHospital
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('mhai.emr_rule_set'), hashtext($1::text))`, s.Hospital); err != nil {
		return "", err
	}
	previous := RuleSetShadow
	var prevVersion string
	err = tx.QueryRow(ctx, `SELECT version FROM emr_rule_set_activation WHERE hospital_id = $1`, s.Hospital).Scan(&prevVersion)
	switch {
	case err == nil:
		previous = prevVersion
	case !errors.Is(err, pgx.ErrNoRows):
		return "", err
	}
	var by any
	if s.Actor != uuid.Nil {
		by = s.Actor
	}
	if s.Version == "" {
		_, err = tx.Exec(ctx, `DELETE FROM emr_rule_set_activation WHERE hospital_id = $1`, s.Hospital)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO emr_rule_set_activation (hospital_id, version, sha256, activated_by, activated_at)
			VALUES ($1, $2, $3, $4, now())
			ON CONFLICT (hospital_id) DO UPDATE SET version = EXCLUDED.version, sha256 = EXCLUDED.sha256,
				activated_by = EXCLUDED.activated_by, activated_at = EXCLUDED.activated_at`, s.Hospital, s.Version, s.SHA256, by)
	}
	if err != nil {
		return "", err
	}
	payload := map[string]any{"hospital_id": s.Hospital.String(), "version": RuleSetShadow, "previous": previous,
		"via": ViaPlatform}
	if s.Version != "" {
		payload["version"] = s.Version
		payload["sha256"] = hex.EncodeToString(s.SHA256)
	}
	actor := ""
	if s.Actor != uuid.Nil {
		actor = s.Actor.String()
	}
	if err := audit.Write(ctx, tx, audit.Event{ActorID: actor, RequestID: s.RequestID, Action: "emr.rule_set_switched",
		Entity: "hospital", EntityID: s.Hospital.String(), Payload: payload}); err != nil {
		return "", err
	}
	return previous, nil
}

func readRuleSet(ctx context.Context, tx pgx.Tx, hid uuid.UUID) (RuleSetOut, error) {
	out := RuleSetOut{HospitalID: hid.String()}
	ok, err := hospitalExists(ctx, tx, hid)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, ErrNoSuchHospital
	}
	var version string
	var sha []byte
	var at time.Time
	err = tx.QueryRow(ctx, `SELECT version, sha256, activated_at FROM emr_rule_set_activation WHERE hospital_id = $1`, hid).
		Scan(&version, &sha, &at)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return out, nil
	case err != nil:
		return out, err
	}
	h, a := hex.EncodeToString(sha), stamp(at)
	out.Version, out.SHA256, out.ActivatedAt = &version, &h, &a
	return out, nil
}

// ruleSetGet is GET /platform/hospitals/{id}/emr-rule-set. Not recorded:
// platform configuration, not clinical data.
func (d Deps) ruleSetGet(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	hid, ok := pathHospital(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var out RuleSetOut
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		out, err = readRuleSet(r.Context(), tx, hid)
		return err
	})
	switch {
	case errors.Is(err, ErrNoSuchHospital):
		httpx.Error(w, r, "not_found")
	case err != nil:
		d.fail(w, r, p, "rule set read")
	default:
		httpx.JSON(w, http.StatusOK, out)
	}
}

// ruleSetBody is the PUT's body: version and sha256 both present, both
// strings or both null.
type ruleSetBody struct {
	Version    optString `json:"version"`
	SHA256     optString `json:"sha256"`
	MFACode    string    `json:"mfa_code"`
	BackupCode string    `json:"backup_code"`
}

func (b ruleSetBody) check() (RuleSetSwitch, bool) {
	var s RuleSetSwitch
	if !b.Version.set || !b.SHA256.set || (b.Version.v == nil) != (b.SHA256.v == nil) {
		return s, false
	}
	if b.Version.v == nil {
		return s, true
	}
	if !ruleSetVersionPattern.MatchString(*b.Version.v) || !sha256Pattern.MatchString(*b.SHA256.v) {
		return s, false
	}
	sha, err := hex.DecodeString(*b.SHA256.v)
	if err != nil || len(sha) != 32 || bytes.Equal(sha, make([]byte, 32)) {
		return s, false
	}
	s.Version, s.SHA256 = *b.Version.v, sha
	return s, true
}

// ruleSetPut is PUT /platform/hospitals/{id}/emr-rule-set. Order: the path
// (404), EMR on (503), the body (422), the hospital (404), the session's
// minute (429), then the code (403); only then the write.
func (d Deps) ruleSetPut(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	hid, ok := pathHospital(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !d.EMREnabled || d.Auth == nil {
		httpx.Error(w, r, "not_ready")
		return
	}
	var body ruleSetBody
	if !httpx.Decode(w, r, &body) {
		return
	}
	s, ok := body.check()
	if !ok {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var exists bool
	if err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		exists, err = hospitalExists(r.Context(), tx, hid)
		return err
	}); err != nil {
		d.fail(w, r, p, "rule set lookup")
		return
	}
	if !exists {
		httpx.Error(w, r, "not_found")
		return
	}
	allowed, err := d.reserveRate(r, p, ruleSetScope, "platform.emr_rule_set.write.v1", ModelPolicyWritesPerMinute)
	switch {
	case err != nil:
		d.log().Error("platform: rule set switch refused: rate counter unavailable", "request_id", httpx.RequestIDFrom(r.Context()),
			"reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return
	case !allowed:
		httpx.Error(w, r, "rate_limited")
		return
	}
	if body.MFACode == "" && body.BackupCode == "" {
		httpx.Error(w, r, "mfa_required")
		return
	}
	// A refused code is audited (auth.mfa.failed or auth.mfa.locked).
	if err := d.Auth.StepUpAudited(r, body.MFACode, body.BackupCode); err != nil {
		d.Auth.WriteStepUpError(w, r, err)
		return
	}
	s.Hospital, s.Actor, s.RequestID = hid, p.UserID, httpx.RequestIDFrom(r.Context())
	var out RuleSetOut
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		if _, err := ApplyRuleSet(r.Context(), tx, s); err != nil {
			return err
		}
		var err error
		out, err = readRuleSet(r.Context(), tx, hid)
		return err
	})
	switch {
	case errors.Is(err, ErrNoSuchHospital):
		httpx.Error(w, r, "not_found")
	case err != nil:
		d.fail(w, r, p, "rule set write")
	default:
		httpx.JSON(w, http.StatusOK, out)
	}
}
