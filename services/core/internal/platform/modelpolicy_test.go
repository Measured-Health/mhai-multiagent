package platform

import (
	"bytes"
	"encoding/json"
	"testing"
)

// decodeBody decodes as httpx.Decode does: unknown keys refused.
func decodeBody(raw string) (policyBody, bool) {
	var b policyBody
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return b, false
	}
	return b, true
}

// The PUT body's rules (Task 2): cloud_provider and monthly_token_budget
// always present; a provider from the closed list or null; a provider needs
// an agreement reference of the id shape; the budget is a whole number of
// 1 to 1000000000. Anything else is 422 before the code is checked.
func TestModelPolicyBodyRules(t *testing.T) {
	for _, raw := range []string{
		`{"cloud_provider":"anthropic","agreement_ref":"DEMO-SYNTHETIC","monthly_token_budget":2000000}`,
		`{"cloud_provider":"openai","agreement_ref":"DPA-2026-11","monthly_token_budget":1}`,
		`{"cloud_provider":"xai","agreement_ref":"AGR-2026-12345678","monthly_token_budget":1000000000,"mfa_code":"123456"}`,
		`{"cloud_provider":"gemini","agreement_ref":"DPA-2026-0007","monthly_token_budget":5}`,
		`{"cloud_provider":null,"agreement_ref":null,"monthly_token_budget":5}`,
		`{"cloud_provider":null,"monthly_token_budget":5,"backup_code":"synthetic"}`,
	} {
		b, ok := decodeBody(raw)
		if !ok {
			t.Errorf("decode %s", raw)
			continue
		}
		if _, ok := b.check(); !ok {
			t.Errorf("refused %s", raw)
		}
	}
	for name, raw := range map[string]string{
		"unknown key":           `{"cloud_provider":null,"monthly_token_budget":5,"model":"x"}`,
		"no provider key":       `{"agreement_ref":"DEMO-SYNTHETIC","monthly_token_budget":5}`,
		"no budget":             `{"cloud_provider":null}`,
		"unknown provider":      `{"cloud_provider":"mistral","agreement_ref":"DEMO-SYNTHETIC","monthly_token_budget":5}`,
		"provider as none":      `{"cloud_provider":"none","monthly_token_budget":5}`,
		"empty provider":        `{"cloud_provider":"","monthly_token_budget":5}`,
		"provider without ref":  `{"cloud_provider":"anthropic","monthly_token_budget":5}`,
		"provider with null":    `{"cloud_provider":"anthropic","agreement_ref":null,"monthly_token_budget":5}`,
		"empty ref":             `{"cloud_provider":null,"agreement_ref":"","monthly_token_budget":5}`,
		"free text ref":         `{"cloud_provider":"openai","agreement_ref":"signed by Dr Synthetic","monthly_token_budget":5}`,
		"lower-case ref":        `{"cloud_provider":"openai","agreement_ref":"demo-synthetic","monthly_token_budget":5}`,
		"short ref":             `{"cloud_provider":"openai","agreement_ref":"AB","monthly_token_budget":5}`,
		"long ref":              `{"cloud_provider":"openai","agreement_ref":"A` + string(bytes.Repeat([]byte("B"), 64)) + `","monthly_token_budget":5}`,
		"budget 0":              `{"cloud_provider":null,"monthly_token_budget":0}`,
		"ref while off":         `{"cloud_provider":null,"agreement_ref":"AGR-2026-01","monthly_token_budget":5}`,
		"old free shape":        `{"cloud_provider":"openai","agreement_ref":"SYNTH-1","monthly_token_budget":5}`,
		"patient pseudonym":     `{"cloud_provider":"openai","agreement_ref":"P-AB12CD34","monthly_token_budget":5}`,
		"a person's name":       `{"cloud_provider":"openai","agreement_ref":"ADAEZE-OKAFOR","monthly_token_budget":5}`,
		"an MRN shape":          `{"cloud_provider":"openai","agreement_ref":"HAV-000123","monthly_token_budget":5}`,
		"a name and a number":   `{"cloud_provider":"openai","agreement_ref":"OKAFOR-2026-01","monthly_token_budget":5}`,
		"budget negative":       `{"cloud_provider":null,"monthly_token_budget":-1}`,
		"budget too big":        `{"cloud_provider":null,"monthly_token_budget":1000000001}`,
		"budget fraction":       `{"cloud_provider":null,"monthly_token_budget":1.5}`,
		"budget string":         `{"cloud_provider":null,"monthly_token_budget":"5"}`,
		"budget null":           `{"cloud_provider":null,"monthly_token_budget":null}`,
		"provider not a string": `{"cloud_provider":1,"monthly_token_budget":5}`,
	} {
		b, ok := decodeBody(raw)
		if !ok {
			continue // httpx.Decode answers 422
		}
		if _, ok := b.check(); ok {
			t.Errorf("%s: accepted %s", name, raw)
		}
	}
}

// AgreementRefOK (review I1): an agreement code, never a word, a name, a
// patient pseudonym or an identifier from a record.
func TestAgreementRefShape(t *testing.T) {
	for _, ok := range []string{"DEMO-SYNTHETIC", "AGR-2026-01", "DPA-2026-0042", "DPA-2026-12345678"} {
		if !AgreementRefOK(ok) {
			t.Errorf("refused %q", ok)
		}
	}
	for _, bad := range []string{"", "P-AB12CD34", "AGR-P-AB12CD34", "ADAEZE-OKAFOR", "HAV-000123", "OKAFOR-2026-01",
		"AGR-2026-1", "AGR-26-01", "AGR-2026-123456789", "agr-2026-01", "AGR-2026-01 ", "DEMO-SYNTHETIC-2", "SYNTH-1",
		"AGR-2026-01\n", "\uff21GR-2026-01"} {
		if AgreementRefOK(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}
