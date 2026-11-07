package platform

// A hospital's cloud model policy (cloud model providers plan 2026-11-06,
// Task 2; design §5.1, §5.3, §10):
//
//   - GET /platform/hospitals/{id}/model-policy: platform admins and
//     auditors; the policy (the defaults, off, when there is no row) and
//     this month's token usage (mhai_cloud_usage, 00018).
//   - PUT /platform/hospitals/{id}/model-policy {cloud_provider,
//     agreement_ref?, monthly_token_budget, mfa_code | backup_code}:
//     platform admins, with step-up (auth.Service.StepUpAudited, as
//     jobs/cancel); 10 writes a minute per session (as jobs/retry). A
//     provider this deployment hasn't enabled (CORE_CLOUD_PROVIDERS) is 409
//     conflict. The upsert and the audit row (hospital.model_policy_changed)
//     are one transaction, as mhai_platform.
//
// An unknown hospital is 404 on both, checked first: the ledger answers
// zeros for any id. ApplyModelPolicy is shared with the CLI twin ("core
// hospitals model-policy set", as mhai_provision).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/config"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

const (
	// DefaultMonthlyTokenBudget is 00018's column default: the budget of a
	// hospital with no policy row.
	DefaultMonthlyTokenBudget = 2000000
	// MaxMonthlyTokenBudget bounds a budget (the audit schema's too).
	MaxMonthlyTokenBudget = 1000000000
	// ModelPolicyWritesPerMinute is the PUTs one session may make a minute.
	ModelPolicyWritesPerMinute = 10
	// modelPolicyScope is the idempotency_keys scope of the PUT's rate
	// counter (as retryScope).
	modelPolicyScope = "platform.model_policy.write_rate"
	// ProviderNone is "no provider" in the audit payload and the CLI.
	ProviderNone = "none"
	// ViaPlatform and ViaCLI are the audit payload's via.
	ViaPlatform = "platform"
	ViaCLI      = "cli"
)

// AgreementRefPattern is an agreement reference's shape (00018's CHECK):
// an agreement code, AGR-YYYY-NNNN or DPA-YYYY-NNNN (a fixed prefix, a
// four-digit year, then 2 to 8 digits), or the literal DEMO-SYNTHETIC for the
// synthetic demo. Letters appear only in the fixed prefixes, so a reference
// can't hold a word, a person's name, a patient pseudonym (P-XXXX...) or a
// record's identifier with letters (review I1); it is never checked against
// anything but its shape. H3 (the legal lead) may change the format.
var AgreementRefPattern = regexp.MustCompile(`^(DEMO-SYNTHETIC|(AGR|DPA)-[0-9]{4}-[0-9]{2,8})$`)

// AgreementRefOK reports whether s is an agreement reference: the pattern,
// and nothing of the pseudonym's shape (pseudonymIn, as the cancel note;
// the pattern already excludes it, this keeps the rule if the pattern is
// ever widened).
func AgreementRefOK(s string) bool {
	return AgreementRefPattern.MatchString(s) && !pseudonymIn.MatchString(s)
}

var (
	// ErrNoSuchHospital: no hospital has the id.
	ErrNoSuchHospital = errors.New("platform: no such hospital")
	// ErrBadModelPolicy: a change outside the policy's rules (callers check
	// first; this is the last guard).
	ErrBadModelPolicy = errors.New("platform: invalid model policy")
)

func registerModelPolicy(m *authz.Mux, d Deps) {
	authz.Route(m, http.MethodGet, "/platform/hospitals/{id}/model-policy",
		authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin, authz.RoleAuditor}},
		http.HandlerFunc(d.modelPolicyGet))
	authz.Route(m, http.MethodPut, "/platform/hospitals/{id}/model-policy",
		authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin}, Write: true, StepUp: true},
		http.HandlerFunc(d.modelPolicyPut))
}

// ModelPolicyOut is the policy as both routes answer it: exactly these
// keys.
type ModelPolicyOut struct {
	HospitalID         string        `json:"hospital_id"`
	CloudProvider      *string       `json:"cloud_provider"`
	AgreementRef       *string       `json:"agreement_ref"`
	MonthlyTokenBudget int64         `json:"monthly_token_budget"`
	UpdatedAt          *string       `json:"updated_at"`
	Usage              ModelUsageOut `json:"usage"`
}

// ModelUsageOut is this month's (UTC) usage: month as YYYY-MM.
type ModelUsageOut struct {
	Month    string `json:"month"`
	Used     int64  `json:"used"`
	Reserved int64  `json:"reserved"`
}

// ModelPolicyChange is one policy write.
type ModelPolicyChange struct {
	Hospital uuid.UUID
	// Provider is one of config.CloudProviderNames, or "" for none.
	Provider string
	// AgreementRef is "" for none; it is required with a provider.
	AgreementRef string
	// Budget nil keeps the current budget (the default with no row).
	Budget *int64
	// Actor is the platform admin; uuid.Nil for the CLI (no actor).
	Actor     uuid.UUID
	RequestID string
	Via       string
}

func (c ModelPolicyChange) valid() bool {
	if c.Provider != "" && !slices.Contains(config.CloudProviderNames, c.Provider) {
		return false
	}
	if c.AgreementRef != "" && !AgreementRefOK(c.AgreementRef) {
		return false
	}
	// A reference with a provider only: switching off clears it.
	if (c.Provider == "") != (c.AgreementRef == "") {
		return false
	}
	if c.Budget != nil && (*c.Budget < 1 || *c.Budget > MaxMonthlyTokenBudget) {
		return false
	}
	return c.Hospital != uuid.Nil && (c.Via == ViaPlatform || c.Via == ViaCLI)
}

// hospitalExists reports whether the hospital has a row (either role may
// read hospitals.id).
func hospitalExists(ctx context.Context, tx pgx.Tx, id uuid.UUID) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM hospitals WHERE id = $1)`, id).Scan(&ok)
	return ok, err
}

// ApplyModelPolicy writes c in tx (READ COMMITTED, as mhai_platform or
// mhai_provision) and appends hospital.model_policy_changed in the same
// transaction: the upsert sets only the columns the roles may update. The
// hospital's policy writes are serialised by a transaction advisory lock,
// so previous is the policy this write replaced. It returns previous
// (ProviderNone for none).
func ApplyModelPolicy(ctx context.Context, tx pgx.Tx, c ModelPolicyChange) (previous string, err error) {
	if !c.valid() {
		return "", ErrBadModelPolicy
	}
	ok, err := hospitalExists(ctx, tx, c.Hospital)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrNoSuchHospital
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('mhai.model_policy'), hashtext($1::text))`, c.Hospital); err != nil {
		return "", err
	}
	var prev *string
	budget := int64(DefaultMonthlyTokenBudget)
	err = tx.QueryRow(ctx, `SELECT cloud_provider, monthly_token_budget FROM hospital_model_policy WHERE hospital_id = $1`,
		c.Hospital).Scan(&prev, &budget)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	previous = ProviderNone
	if prev != nil {
		previous = *prev
	}
	if c.Budget != nil {
		budget = *c.Budget
	}
	var provider, ref, by any
	if c.Provider != "" {
		provider = c.Provider
	}
	if c.AgreementRef != "" {
		ref = c.AgreementRef
	}
	if c.Actor != uuid.Nil {
		by = c.Actor
	}
	if _, err := tx.Exec(ctx, `INSERT INTO hospital_model_policy
			(hospital_id, cloud_provider, agreement_ref, monthly_token_budget, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (hospital_id) DO UPDATE SET cloud_provider = EXCLUDED.cloud_provider,
			agreement_ref = EXCLUDED.agreement_ref, monthly_token_budget = EXCLUDED.monthly_token_budget,
			updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`,
		c.Hospital, provider, ref, budget, by); err != nil {
		return "", err
	}
	payload := map[string]any{"hospital_id": c.Hospital.String(), "provider": ProviderNone, "previous": previous,
		"monthly_token_budget": budget, "via": c.Via}
	if c.Provider != "" {
		payload["provider"] = c.Provider
	}
	if c.AgreementRef != "" {
		payload["agreement_ref"] = c.AgreementRef
	}
	actor := ""
	if c.Actor != uuid.Nil {
		actor = c.Actor.String()
	}
	// The chain lock is held from here until commit.
	if err := audit.Write(ctx, tx, audit.Event{ActorID: actor, RequestID: c.RequestID,
		Action: "hospital.model_policy_changed", Entity: "hospital", EntityID: c.Hospital.String(), Payload: payload}); err != nil {
		return "", err
	}
	return previous, nil
}

// readModelPolicy is the hospital's policy and this month's usage, or
// ErrNoSuchHospital.
func readModelPolicy(ctx context.Context, tx pgx.Tx, hid uuid.UUID) (ModelPolicyOut, error) {
	out := ModelPolicyOut{HospitalID: hid.String(), MonthlyTokenBudget: DefaultMonthlyTokenBudget}
	ok, err := hospitalExists(ctx, tx, hid)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, ErrNoSuchHospital
	}
	var at time.Time
	err = tx.QueryRow(ctx, `SELECT cloud_provider, agreement_ref, monthly_token_budget, updated_at
		FROM hospital_model_policy WHERE hospital_id = $1`, hid).Scan(&out.CloudProvider, &out.AgreementRef, &out.MonthlyTokenBudget, &at)
	switch {
	case err == nil:
		s := stamp(at)
		out.UpdatedAt = &s
	case !errors.Is(err, pgx.ErrNoRows):
		return out, err
	}
	var month time.Time
	if err := tx.QueryRow(ctx, `SELECT month, used, reserved FROM mhai_cloud_usage($1)`, hid).
		Scan(&month, &out.Usage.Used, &out.Usage.Reserved); err != nil {
		return out, err
	}
	out.Usage.Month = month.Format("2006-01")
	return out, nil
}

// pathHospital is the path's hospital id: a canonical uuid, or false (404).
func pathHospital(r *http.Request) (uuid.UUID, bool) {
	s := r.PathValue("id")
	if !canonicalUUID(s) {
		return uuid.Nil, false
	}
	return uuid.MustParse(s), true
}

// modelPolicyGet is GET /platform/hospitals/{id}/model-policy. Not
// recorded: platform configuration, not clinical data (as health).
func (d Deps) modelPolicyGet(w http.ResponseWriter, r *http.Request) {
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
	var out ModelPolicyOut
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		out, err = readModelPolicy(r.Context(), tx, hid)
		return err
	})
	switch {
	case errors.Is(err, ErrNoSuchHospital):
		httpx.Error(w, r, "not_found")
	case err != nil:
		d.fail(w, r, p, "model policy read")
	default:
		httpx.JSON(w, http.StatusOK, out)
	}
}

// optString is a JSON string or null that records whether its key was
// present at all.
type optString struct {
	set bool
	v   *string
}

func (o *optString) UnmarshalJSON(b []byte) error {
	o.set = true
	if string(b) == "null" {
		o.v = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	o.v = &s
	return nil
}

// policyBody is the PUT's body.
type policyBody struct {
	CloudProvider      optString `json:"cloud_provider"`
	AgreementRef       *string   `json:"agreement_ref"`
	MonthlyTokenBudget *int64    `json:"monthly_token_budget"`
	MFACode            string    `json:"mfa_code"`
	BackupCode         string    `json:"backup_code"`
}

// check applies the body's rules: cloud_provider present (a known provider
// or null), a provider with an agreement reference (AgreementRefOK), null
// with none, and a budget of 1 to 1000000000.
// The change it returns has no hospital, actor or via yet.
func (b policyBody) check() (ModelPolicyChange, bool) {
	var c ModelPolicyChange
	if !b.CloudProvider.set || b.MonthlyTokenBudget == nil {
		return c, false
	}
	if b.CloudProvider.v != nil {
		if !slices.Contains(config.CloudProviderNames, *b.CloudProvider.v) {
			return c, false
		}
		c.Provider = *b.CloudProvider.v
	}
	if b.AgreementRef != nil {
		if !AgreementRefOK(*b.AgreementRef) {
			return c, false
		}
		c.AgreementRef = *b.AgreementRef
	}
	// A provider needs a reference, and off has none (it is cleared).
	if (c.Provider == "") != (c.AgreementRef == "") {
		return c, false
	}
	n := *b.MonthlyTokenBudget
	if n < 1 || n > MaxMonthlyTokenBudget {
		return c, false
	}
	c.Budget = &n
	return c, true
}

// modelPolicyPut is PUT /platform/hospitals/{id}/model-policy. Order: the
// path (404), the body (422), the provider enabled (409), the hospital
// (404), the session's minute (429), then the code (403 mfa_required with
// none, mfa_invalid when wrong, audited); only then the write.
func (d Deps) modelPolicyPut(w http.ResponseWriter, r *http.Request) {
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
	var body policyBody
	if !httpx.Decode(w, r, &body) {
		return
	}
	c, ok := body.check()
	if !ok {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if c.Provider != "" && !slices.Contains(d.CloudProviders, c.Provider) {
		httpx.Error(w, r, "conflict")
		return
	}
	if d.Auth == nil {
		d.log().Error("platform: model policy refused: no step-up", "request_id", httpx.RequestIDFrom(r.Context()),
			"reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return
	}
	// The hospital before the code, so a code is never spent on one that
	// doesn't exist (hospitals are never deleted; the write checks again).
	var exists bool
	if err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		exists, err = hospitalExists(r.Context(), tx, hid)
		return err
	}); err != nil {
		d.fail(w, r, p, "model policy lookup")
		return
	}
	if !exists {
		httpx.Error(w, r, "not_found")
		return
	}
	allowed, err := d.reserveRate(r, p, modelPolicyScope, "platform.model_policy.write.v1", ModelPolicyWritesPerMinute)
	switch {
	case err != nil:
		d.log().Error("platform: model policy refused: rate counter unavailable", "request_id", httpx.RequestIDFrom(r.Context()),
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
	c.Hospital, c.Actor, c.Via, c.RequestID = hid, p.UserID, ViaPlatform, httpx.RequestIDFrom(r.Context())
	var out ModelPolicyOut
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		if _, err := ApplyModelPolicy(r.Context(), tx, c); err != nil {
			return err
		}
		var err error
		out, err = readModelPolicy(r.Context(), tx, hid)
		return err
	})
	switch {
	case errors.Is(err, ErrNoSuchHospital):
		httpx.Error(w, r, "not_found")
	case err != nil:
		d.fail(w, r, p, "model policy write")
	default:
		httpx.JSON(w, http.StatusOK, out)
	}
}
