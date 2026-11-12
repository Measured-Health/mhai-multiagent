//go:build integration

package platform_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
)

// EMR plan Task 30: GET and PUT /platform/hospitals/{id}/emr-rule-set
// through core's whole chain. A hospital runs its rule set in shadow until
// a platform admin activates a version and hash for it; the switch is a
// step-up write, audited emr.rule_set_switched. Synthetic ids and hashes.

func ruleSetPath(hid uuid.UUID) string {
	return "/platform/hospitals/" + hid.String() + "/emr-rule-set"
}

var ruleSetSHA = strings.Repeat("3c", 32)

func ruleSetBody(version, sha, backup string) string {
	m := map[string]any{"version": nil, "sha256": nil}
	if version != "" {
		m["version"] = version
	}
	if sha != "" {
		m["sha256"] = sha
	}
	if backup != "" {
		m["backup_code"] = backup
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func putRuleSet(t *testing.T, tok string, hid uuid.UUID, body string) resp {
	t.Helper()
	return send(t, handler, http.MethodPut, ruleSetPath(hid), tok, body)
}

func ruleSetAudits(t *testing.T, hid uuid.UUID) []map[string]any {
	t.Helper()
	rows, err := super.Query(context.Background(), `SELECT payload FROM audit_log
		WHERE action = 'emr.rule_set_switched' AND payload->>'hospital_id' = $1 AND hospital_id IS NULL ORDER BY id`, hid.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func activation(t *testing.T, hid uuid.UUID) (version, sha string, found bool) {
	t.Helper()
	var b []byte
	if err := super.QueryRow(context.Background(), `SELECT version, sha256 FROM emr_rule_set_activation WHERE hospital_id = $1`, hid).
		Scan(&version, &b); err != nil {
		return "", "", false
	}
	return version, hex.EncodeToString(b), true
}

// Only a platform admin, with step-up, activates; an auditor reads; every
// switch is audited with the version, the one replaced and the hash; null
// puts the hospital back in shadow.
func TestRuleSetActivateAuditedAndBackToShadow(t *testing.T) {
	h := seedHospital(t, "active")
	for _, roles := range [][]string{{authz.RoleClinician}, {authz.RoleHospitalAdmin}} {
		tok := mint(t, seedUser(t, h, "", roles...))
		problem(t, fmt.Sprint(roles, " PUT"), putRuleSet(t, tok, h, ruleSetBody("1", ruleSetSHA, "")), 403, "not_platform")
		problem(t, fmt.Sprint(roles, " GET"), get(t, ruleSetPath(h), tok), 403, "not_platform")
	}
	aud := auditor(t)
	problem(t, "auditor PUT", putRuleSet(t, mint(t, aud), h, ruleSetBody("1", ruleSetSHA, backupCode(t, aud))), 403, "forbidden")
	r := get(t, ruleSetPath(h), mint(t, aud))
	ok(t, "auditor GET", r)
	sameKeys(t, "GET", r.body, "hospital_id", "version", "sha256", "activated_at")
	if r.body["version"] != nil || r.body["sha256"] != nil {
		t.Fatalf("shadow by default: %s", r.raw)
	}

	admin := platformAdmin(t)
	tok := mint(t, admin)
	problem(t, "no code", putRuleSet(t, tok, h, ruleSetBody("1", ruleSetSHA, "")), 403, "mfa_required")
	problem(t, "wrong code", putRuleSet(t, tok, h, ruleSetBody("1", ruleSetSHA, "wrong"+uniq())), 403, "mfa_invalid")
	if _, _, found := activation(t, h); found || len(ruleSetAudits(t, h)) != 0 {
		t.Fatal("a refused switch was written or audited")
	}
	r = putRuleSet(t, tok, h, ruleSetBody("1", ruleSetSHA, backupCode(t, admin)))
	ok(t, "activate", r)
	if v, s, found := activation(t, h); !found || v != "1" || s != ruleSetSHA {
		t.Fatalf("activation %q %q %v", v, s, found)
	}
	if r.body["version"] != "1" || r.body["sha256"] != ruleSetSHA || r.body["activated_at"] == nil {
		t.Fatalf("reply %s", r.raw)
	}
	r = putRuleSet(t, tok, h, ruleSetBody("", "", backupCode(t, admin)))
	ok(t, "back to shadow", r)
	if _, _, found := activation(t, h); found {
		t.Fatal("still activated after the switch back")
	}
	a := ruleSetAudits(t, h)
	if len(a) != 2 || a[0]["version"] != "1" || a[0]["previous"] != "shadow" || a[0]["sha256"] != ruleSetSHA ||
		a[1]["version"] != "shadow" || a[1]["previous"] != "1" || a[1]["sha256"] != nil || a[0]["via"] != "platform" {
		t.Fatalf("audits %v", a)
	}
}

// The body is strict: both keys, both strings or both null, a version name
// and a 64-hex hash (not all zeros); an unknown hospital is 404 before any
// code is spent.
func TestRuleSetPutStrictBody(t *testing.T) {
	h := seedHospital(t, "active")
	admin := platformAdmin(t)
	tok := mint(t, admin)
	code := backupCode(t, admin)
	for name, body := range map[string]string{
		"version without hash": `{"version":"1","sha256":null,"backup_code":"` + code + `"}`,
		"hash without version": `{"version":null,"sha256":"` + ruleSetSHA + `","backup_code":"` + code + `"}`,
		"no keys":              `{"backup_code":"` + code + `"}`,
		"short hash":           `{"version":"1","sha256":"abcd","backup_code":"` + code + `"}`,
		"upper-case hash":      `{"version":"1","sha256":"` + strings.ToUpper(ruleSetSHA) + `","backup_code":"` + code + `"}`,
		"zero hash":            `{"version":"1","sha256":"` + strings.Repeat("0", 64) + `","backup_code":"` + code + `"}`,
		"free-text version":    `{"version":"v1 final","sha256":"` + ruleSetSHA + `","backup_code":"` + code + `"}`,
		"unknown key":          `{"version":"1","sha256":"` + ruleSetSHA + `","active":true,"backup_code":"` + code + `"}`,
	} {
		problem(t, name, putRuleSet(t, tok, h, body), 422, "validation_failed")
	}
	problem(t, "unknown hospital", putRuleSet(t, tok, uuid.New(), ruleSetBody("1", ruleSetSHA, "wrong"+uniq())), 404, "not_found")
	if n := mfaRows(t, admin, "auth.mfa.failed", "method", "step_up"); n != 0 {
		t.Errorf("a refused request checked the code: %d rows", n)
	}
	if _, _, found := activation(t, h); found {
		t.Fatal("a refused body was written")
	}
	problem(t, "GET unknown hospital", get(t, ruleSetPath(uuid.New()), tok), 404, "not_found")
}
