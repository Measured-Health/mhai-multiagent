//go:build integration

package platform_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/platform"
)

// Cloud model providers plan, Task 2: GET and PUT
// /platform/hospitals/{id}/model-policy through core's whole chain. The
// test server enables anthropic, gemini and openai (testCloudProviders), not xai.
// Every id, name and reference here is synthetic.

func policyPath(hid uuid.UUID) string { return "/platform/hospitals/" + hid.String() + "/model-policy" }

// policyBody is a PUT body; provider "" is null, ref "" leaves the key out.
func policyBody(provider, ref string, budget int64, backup string) string {
	m := map[string]any{"cloud_provider": nil, "monthly_token_budget": budget}
	if provider != "" {
		m["cloud_provider"] = provider
	}
	if ref != "" {
		m["agreement_ref"] = ref
	}
	if backup != "" {
		m["backup_code"] = backup
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func putPolicy(t *testing.T, tok string, hid uuid.UUID, body string) resp {
	t.Helper()
	return send(t, handler, http.MethodPut, policyPath(hid), tok, body)
}

// putClinical is a PUT on the clinical audience (no X-MHAI-Audience).
func putClinical(t *testing.T, tok string, hid uuid.UUID, body string) resp {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, httpx.BasePath+policyPath(hid), strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-MHAI-Service", serviceKey)
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Idempotency-Key", "k-"+uniq())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	out := resp{code: w.Code, raw: w.Body.String()}
	_ = json.Unmarshal(w.Body.Bytes(), &out.body)
	return out
}

// policyRow is the stored policy, or ok false for none.
func policyRow(t *testing.T, hid uuid.UUID) (provider, ref *string, budget int64, by *uuid.UUID, ok bool) {
	t.Helper()
	err := super.QueryRow(context.Background(), `SELECT cloud_provider, agreement_ref, monthly_token_budget, updated_by
		FROM hospital_model_policy WHERE hospital_id = $1`, hid).Scan(&provider, &ref, &budget, &by)
	if err != nil {
		return nil, nil, 0, nil, false
	}
	return provider, ref, budget, by, true
}

type policyAudit struct {
	actor, hospital, entity, entityID string
	payload                           map[string]any
}

// policyAudits are the hospital.model_policy_changed rows for hid, in order.
func policyAudits(t *testing.T, hid uuid.UUID) []policyAudit {
	t.Helper()
	rows, err := super.Query(context.Background(), `SELECT COALESCE(actor_id::text, ''), COALESCE(hospital_id::text, ''),
		COALESCE(entity, ''), COALESCE(entity_id, ''), payload FROM audit_log
		WHERE action = 'hospital.model_policy_changed' AND payload->>'hospital_id' = $1 ORDER BY id`, hid.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []policyAudit
	for rows.Next() {
		var a policyAudit
		var raw []byte
		if err := rows.Scan(&a.actor, &a.hospital, &a.entity, &a.entityID, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &a.payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

var policyKeys = []string{"hospital_id", "cloud_provider", "provider_code", "agreement_ref", "monthly_token_budget", "updated_at", "usage"}

func thisMonth() string { return time.Now().UTC().Format("2006-01") }

// The reply's shape, both ways: exactly the contract's keys.
func checkPolicyShape(t *testing.T, what string, r resp, hid uuid.UUID) {
	t.Helper()
	sameKeys(t, what, r.body, policyKeys...)
	if r.body["hospital_id"] != hid.String() {
		t.Errorf("%s: hospital_id %v", what, r.body["hospital_id"])
	}
	u, _ := r.body["usage"].(map[string]any)
	sameKeys(t, what+" usage", u, "month", "used", "reserved")
	if u["month"] != thisMonth() {
		t.Errorf("%s: month %v, want %s", what, u["month"], thisMonth())
	}
}

// A13: only a platform admin on the platform audience may PUT; an auditor
// reads (200) and is refused the write (403).
func TestModelPolicyRoutesRoles(t *testing.T) {
	h := seedHospital(t, "active")
	body := policyBody("anthropic", "DEMO-SYNTHETIC", 1000, "")
	for _, roles := range [][]string{{authz.RoleClinician}, {authz.RoleHospitalAdmin}, {authz.RoleClinician, authz.RoleHospitalAdmin}} {
		tok := mint(t, seedUser(t, h, "", roles...))
		problem(t, fmt.Sprint(roles, " platform PUT"), putPolicy(t, tok, h, body), 403, "not_platform")
		problem(t, fmt.Sprint(roles, " clinical PUT"), putClinical(t, tok, h, body), 403, "not_platform")
		problem(t, fmt.Sprint(roles, " GET"), get(t, policyPath(h), tok), 403, "not_platform")
	}
	// A platform admin's token on the clinical audience.
	admin := platformAdmin(t)
	problem(t, "admin clinical PUT", putClinical(t, mint(t, admin), h, policyBody("anthropic", "DEMO-SYNTHETIC", 1000, backupCode(t, admin))),
		403, "not_platform")
	aud := auditor(t)
	atok := mint(t, aud)
	problem(t, "auditor PUT", putPolicy(t, atok, h, policyBody("anthropic", "DEMO-SYNTHETIC", 1000, backupCode(t, aud))), 403, "forbidden")
	r := get(t, policyPath(h), atok)
	ok(t, "auditor GET", r)
	checkPolicyShape(t, "auditor GET", r, h)
	problem(t, "anonymous GET", get(t, policyPath(h), ""), 401, "unauthenticated")
	problem(t, "no two-step GET", get(t, policyPath(h), signIn(t, admin, false).tok), 403, "mfa_required")
	if _, _, _, _, found := policyRow(t, h); found {
		t.Fatal("a refused PUT wrote a policy")
	}
	if n := len(policyAudits(t, h)); n != 0 {
		t.Fatalf("%d audit rows after refused PUTs", n)
	}
}

// With no policy row the GET answers the defaults (off), and this month's
// usage from the ledger.
func TestModelPolicyGetDefaultsAndUsage(t *testing.T) {
	h := seedHospital(t, "active")
	tok := mint(t, platformAdmin(t))
	r := get(t, policyPath(h), tok)
	ok(t, "GET", r)
	checkPolicyShape(t, "GET", r, h)
	if r.body["cloud_provider"] != nil || r.body["provider_code"] != nil || r.body["agreement_ref"] != nil || r.body["updated_at"] != nil ||
		r.body["monthly_token_budget"] != float64(platform.DefaultMonthlyTokenBudget) {
		t.Errorf("defaults: %s", r.raw)
	}
	if u := r.body["usage"].(map[string]any); u["used"] != float64(0) || u["reserved"] != float64(0) {
		t.Errorf("usage: %v", u)
	}
	if _, err := super.Exec(context.Background(), `INSERT INTO cloud_usage (hospital_id, month, used, reserved)
		VALUES ($1, date_trunc('month', now() AT TIME ZONE 'UTC')::date, 1234, 56)`, h); err != nil {
		t.Fatal(err)
	}
	r = get(t, policyPath(h), tok)
	ok(t, "GET with usage", r)
	if u := r.body["usage"].(map[string]any); u["used"] != float64(1234) || u["reserved"] != float64(56) {
		t.Errorf("usage: %v", u)
	}
	if r := get(t, policyPath(h)+"?x=1", tok); r.code != 422 {
		t.Errorf("query string: %d %s", r.code, r.raw)
	}
}

// Step-up as jobs/cancel: no code is 403 mfa_required (not audited), a
// wrong code is 403 mfa_invalid and audited, and nothing is written; the
// right one writes.
func TestModelPolicyPutNeedsStepUp(t *testing.T) {
	h := seedHospital(t, "active")
	admin := platformAdmin(t)
	tok := mint(t, admin)
	problem(t, "no code", putPolicy(t, tok, h, policyBody("anthropic", "DEMO-SYNTHETIC", 1000, "")), 403, "mfa_required")
	if n := mfaRows(t, admin, "auth.mfa.failed", "method", "step_up"); n != 0 {
		t.Errorf("no code audited %d rows", n)
	}
	problem(t, "wrong code", putPolicy(t, tok, h, policyBody("anthropic", "DEMO-SYNTHETIC", 1000, "wrong"+uniq())), 403, "mfa_invalid")
	if n := mfaRows(t, admin, "auth.mfa.failed", "method", "step_up"); n != 1 {
		t.Errorf("auth.mfa.failed rows after a wrong code: %d", n)
	}
	if _, _, _, _, found := policyRow(t, h); found {
		t.Fatal("written without a code")
	}
	if n := len(policyAudits(t, h)); n != 0 {
		t.Fatalf("%d audit rows without a code", n)
	}
	r := putPolicy(t, tok, h, policyBody("anthropic", "DEMO-SYNTHETIC", 1000, backupCode(t, admin)))
	ok(t, "with a code", r)
	if p, _, _, _, found := policyRow(t, h); !found || p == nil || *p != "anthropic" {
		t.Fatalf("policy: %v %v", p, found)
	}
}

// K7: a provider the deployment hasn't enabled is 409 conflict, before the
// code is checked; nothing is written.
func TestModelPolicyPutRefusesDisabledProvider409(t *testing.T) {
	h := seedHospital(t, "active")
	admin := platformAdmin(t)
	tok := mint(t, admin)
	problem(t, "xai", putPolicy(t, tok, h, policyBody("xai", "DEMO-SYNTHETIC", 1000, backupCode(t, admin))), 409, "conflict")
	problem(t, "xai, wrong code", putPolicy(t, tok, h, policyBody("xai", "DEMO-SYNTHETIC", 1000, "wrong"+uniq())), 409, "conflict")
	if n := mfaRows(t, admin, "auth.mfa.failed", "method", "step_up"); n != 0 {
		t.Errorf("a refused provider checked the code: %d rows", n)
	}
	if _, _, _, _, found := policyRow(t, h); found {
		t.Fatal("a disabled provider was written")
	}
	if n := len(policyAudits(t, h)); n != 0 {
		t.Fatalf("%d audit rows", n)
	}
}

// The body is strict (422 validation_failed): an unknown key, a provider
// without a reference, budget 0, a reference that isn't the id shape.
func TestModelPolicyPutStrictBody(t *testing.T) {
	h := seedHospital(t, "active")
	admin := platformAdmin(t)
	tok := mint(t, admin)
	code := backupCode(t, admin)
	for name, body := range map[string]string{
		"unknown key":          `{"cloud_provider":null,"monthly_token_budget":5,"model":"synthetic","backup_code":"` + code + `"}`,
		"provider without ref": `{"cloud_provider":"anthropic","monthly_token_budget":5,"backup_code":"` + code + `"}`,
		"budget 0":             `{"cloud_provider":null,"monthly_token_budget":0,"backup_code":"` + code + `"}`,
		"bad ref shape":        `{"cloud_provider":"openai","agreement_ref":"signed by Dr Synthetic","monthly_token_budget":5,"backup_code":"` + code + `"}`,
		"pseudonym ref":        `{"cloud_provider":"openai","agreement_ref":"P-AB12CD34","monthly_token_budget":5,"backup_code":"` + code + `"}`,
		"name ref":             `{"cloud_provider":"openai","agreement_ref":"ADAEZE-OKAFOR","monthly_token_budget":5,"backup_code":"` + code + `"}`,
		"ref while off":        `{"cloud_provider":null,"agreement_ref":"AGR-2026-01","monthly_token_budget":5,"backup_code":"` + code + `"}`,
		"unknown provider":     `{"cloud_provider":"mistral","agreement_ref":"DEMO-SYNTHETIC","monthly_token_budget":5,"backup_code":"` + code + `"}`,
		"no provider key":      `{"monthly_token_budget":5,"backup_code":"` + code + `"}`,
		"not json":             `cloud_provider=anthropic`,
	} {
		problem(t, name, putPolicy(t, tok, h, body), 422, "validation_failed")
	}
	if _, _, _, _, found := policyRow(t, h); found {
		t.Fatal("a refused body was written")
	}
	// The code was never checked, so it still works.
	ok(t, "good body", putPolicy(t, tok, h, policyBody("openai", "DEMO-SYNTHETIC", 5, code)))
}

// The audit row: the admin's, with no hospital (a platform action), the
// hospital as the entity and in the payload, exactly the schema's keys,
// previous as it was, and switching off is provider none. The reply is the
// GET's.
func TestModelPolicyPutAuditsClosedPayload(t *testing.T) {
	h := seedHospital(t, "active")
	admin := platformAdmin(t)
	tok := mint(t, admin)

	r := putPolicy(t, tok, h, policyBody("anthropic", "DEMO-SYNTHETIC", 3000000, backupCode(t, admin)))
	ok(t, "anthropic", r)
	checkPolicyShape(t, "anthropic", r, h)
	if r.body["cloud_provider"] != "anthropic" || r.body["provider_code"] != "anthropic" || r.body["agreement_ref"] != "DEMO-SYNTHETIC" ||
		r.body["monthly_token_budget"] != float64(3000000) || r.body["updated_at"] == nil {
		t.Errorf("reply: %s", r.raw)
	}
	g := get(t, policyPath(h), tok)
	ok(t, "GET", g)
	if g.raw != r.raw {
		t.Errorf("PUT answered %s, GET %s", r.raw, g.raw)
	}
	if _, _, _, by, _ := policyRow(t, h); by == nil || *by != admin.id {
		t.Errorf("updated_by %v, want the admin", by)
	}

	ok(t, "openai", putPolicy(t, tok, h, policyBody("openai", "DPA-2026-0042", 3000000, backupCode(t, admin))))
	r = putPolicy(t, tok, h, policyBody("", "", 3000000, backupCode(t, admin)))
	ok(t, "off", r)
	if r.body["cloud_provider"] != nil || r.body["provider_code"] != nil || r.body["agreement_ref"] != nil {
		t.Errorf("off: %s", r.raw)
	}
	if p, ref, _, _, found := policyRow(t, h); !found || p != nil || ref != nil {
		t.Errorf("off row: %v %v %v", p, ref, found)
	}

	a := policyAudits(t, h)
	if len(a) != 3 {
		t.Fatalf("%d audit rows, want 3", len(a))
	}
	want := []map[string]any{
		{"hospital_id": h.String(), "provider": "anthropic", "previous": "none", "agreement_ref": "DEMO-SYNTHETIC",
			"monthly_token_budget": float64(3000000), "via": "platform"},
		{"hospital_id": h.String(), "provider": "openai", "previous": "anthropic", "agreement_ref": "DPA-2026-0042",
			"monthly_token_budget": float64(3000000), "via": "platform"},
		{"hospital_id": h.String(), "provider": "none", "previous": "openai",
			"monthly_token_budget": float64(3000000), "via": "platform"},
	}
	for i, row := range a {
		if row.actor != admin.id.String() || row.hospital != "" || row.entity != "hospital" || row.entityID != h.String() {
			t.Errorf("row %d: actor %q hospital %q entity %q %q", i, row.actor, row.hospital, row.entity, row.entityID)
		}
		got, wantKeys := keysOf(row.payload), keysOf(want[i])
		sort.Strings(got)
		sort.Strings(wantKeys)
		if !slices.Equal(got, wantKeys) {
			t.Errorf("row %d keys %v, want %v", i, got, wantKeys)
		}
		for k, v := range want[i] {
			if row.payload[k] != v {
				t.Errorf("row %d %s = %v, want %v", i, k, row.payload[k], v)
			}
		}
	}
}

// An unknown hospital is 404 on both routes, even though the ledger answers
// zeros for any id; nothing is written or audited. A path id that isn't a
// canonical uuid is 404 too.
func TestModelPolicyUnknownHospital404(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	nobody := uuid.New()
	problem(t, "GET", get(t, policyPath(nobody), tok), 404, "not_found")
	problem(t, "PUT", putPolicy(t, tok, nobody, policyBody("anthropic", "DEMO-SYNTHETIC", 1000, backupCode(t, admin))), 404, "not_found")
	problem(t, "PUT off", putPolicy(t, tok, nobody, policyBody("", "", 1000, backupCode(t, admin))), 404, "not_found")
	for _, p := range []string{"/platform/hospitals/HAV/model-policy", "/platform/hospitals/" + strings.ToUpper(nobody.String()) + "/model-policy"} {
		problem(t, "GET "+p, get(t, p, tok), 404, "not_found")
		problem(t, "PUT "+p, send(t, handler, http.MethodPut, p, tok, policyBody("", "", 1000, backupCode(t, admin))), 404, "not_found")
	}
	if _, _, _, _, found := policyRow(t, nobody); found {
		t.Fatal("a policy for an unknown hospital")
	}
	if n := len(policyAudits(t, nobody)); n != 0 {
		t.Fatalf("%d audit rows for an unknown hospital", n)
	}
	// The hospital is checked before the code: a wrong code on an unknown
	// hospital is 404, and no code check is spent or audited.
	problem(t, "PUT wrong code", putPolicy(t, tok, nobody, policyBody("", "", 1000, "wrong"+uniq())), 404, "not_found")
	if n := mfaRows(t, admin, "auth.mfa.failed", "method", "step_up"); n != 0 {
		t.Errorf("code checked for an unknown hospital: %d rows", n)
	}
}

// 10 writes a minute per session, as jobs/retry; another session has its
// own minute.
func TestModelPolicyRateLimited(t *testing.T) {
	h := seedHospital(t, "active")
	admin := platformAdmin(t)
	tok := mint(t, admin)
	for i := 0; i < platform.ModelPolicyWritesPerMinute; i++ {
		ok(t, fmt.Sprintf("write %d", i+1), putPolicy(t, tok, h, policyBody("", "", int64(1000+i), backupCode(t, admin))))
	}
	problem(t, "11th write", putPolicy(t, tok, h, policyBody("", "", 5000, backupCode(t, admin))), 429, "rate_limited")
	if _, _, b, _, _ := policyRow(t, h); b != int64(1000+platform.ModelPolicyWritesPerMinute-1) {
		t.Errorf("budget %d after the refused write", b)
	}
	ok(t, "another session", putPolicy(t, mint(t, admin), h, policyBody("", "", 5000, backupCode(t, admin))))
}

// Review M3: two PUTs racing on one hospital each audit the policy they
// replaced: one row has previous none, the other the first's provider.
func TestModelPolicyConcurrentPutsChainPrevious(t *testing.T) {
	for round := 0; round < 5; round++ {
		h := seedHospital(t, "active")
		admin := platformAdmin(t)
		bodies := []string{policyBody("anthropic", "DEMO-SYNTHETIC", 1000, backupCode(t, admin)),
			policyBody("openai", "AGR-2026-01", 1000, backupCode(t, admin))}
		toks := []string{mint(t, admin), mint(t, admin)}
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i := range bodies {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				codes[i] = putPolicy(t, toks[i], h, bodies[i]).code
			}(i)
		}
		wg.Wait()
		if codes[0] != 200 || codes[1] != 200 {
			t.Fatalf("round %d: %v", round, codes)
		}
		a := policyAudits(t, h)
		if len(a) != 2 {
			t.Fatalf("round %d: %d rows", round, len(a))
		}
		first, second := a[0].payload, a[1].payload
		if first["previous"] != "none" || second["previous"] != first["provider"] || second["provider"] == first["provider"] {
			t.Errorf("round %d: %v then %v", round, first, second)
		}
		if p, _, _, _, _ := policyRow(t, h); p == nil || *p != second["provider"] {
			t.Errorf("round %d: stored %v, last audited %v", round, p, second["provider"])
		}
	}
}

// Gemini: the policy is written and audited like any provider, and the
// reply reports it in provider_code. cloud_provider, whose openapi 1.x
// enum can't name it (ADR 0012: a response enum doesn't widen in /api/v1),
// is null; the GET answers the same.
func TestModelPolicyPutGeminiReportsProviderCode(t *testing.T) {
	h := seedHospital(t, "active")
	admin := platformAdmin(t)
	tok := mint(t, admin)
	r := putPolicy(t, tok, h, policyBody("gemini", "DPA-2026-0007", 4000, backupCode(t, admin)))
	ok(t, "gemini", r)
	checkPolicyShape(t, "gemini", r, h)
	if r.body["provider_code"] != "gemini" || r.body["cloud_provider"] != nil || r.body["agreement_ref"] != "DPA-2026-0007" {
		t.Errorf("reply: %s", r.raw)
	}
	g := get(t, policyPath(h), tok)
	ok(t, "GET", g)
	if g.raw != r.raw {
		t.Errorf("PUT answered %s, GET %s", r.raw, g.raw)
	}
	if p, _, _, _, found := policyRow(t, h); !found || p == nil || *p != "gemini" {
		t.Errorf("row provider %v", p)
	}
	a := policyAudits(t, h)
	if len(a) != 1 || a[0].payload["provider"] != "gemini" || a[0].payload["previous"] != "none" {
		t.Errorf("audit rows %+v", a)
	}
}
