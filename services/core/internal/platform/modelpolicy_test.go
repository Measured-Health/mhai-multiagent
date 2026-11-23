package platform

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
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
		`{"cloud_provider":null,"monthly_token_budget":5,"own_model":true}`,
		`{"cloud_provider":null,"monthly_token_budget":5,"own_model":false}`,
		`{"cloud_provider":"openai","agreement_ref":"DPA-2026-11","monthly_token_budget":5,"own_model":false}`,
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
		"unknown key":               `{"cloud_provider":null,"monthly_token_budget":5,"model":"x"}`,
		"no provider key":           `{"agreement_ref":"DEMO-SYNTHETIC","monthly_token_budget":5}`,
		"no budget":                 `{"cloud_provider":null}`,
		"unknown provider":          `{"cloud_provider":"mistral","agreement_ref":"DEMO-SYNTHETIC","monthly_token_budget":5}`,
		"provider as none":          `{"cloud_provider":"none","monthly_token_budget":5}`,
		"empty provider":            `{"cloud_provider":"","monthly_token_budget":5}`,
		"provider without ref":      `{"cloud_provider":"anthropic","monthly_token_budget":5}`,
		"provider with null":        `{"cloud_provider":"anthropic","agreement_ref":null,"monthly_token_budget":5}`,
		"empty ref":                 `{"cloud_provider":null,"agreement_ref":"","monthly_token_budget":5}`,
		"free text ref":             `{"cloud_provider":"openai","agreement_ref":"signed by Dr Synthetic","monthly_token_budget":5}`,
		"lower-case ref":            `{"cloud_provider":"openai","agreement_ref":"demo-synthetic","monthly_token_budget":5}`,
		"short ref":                 `{"cloud_provider":"openai","agreement_ref":"AB","monthly_token_budget":5}`,
		"long ref":                  `{"cloud_provider":"openai","agreement_ref":"A` + string(bytes.Repeat([]byte("B"), 64)) + `","monthly_token_budget":5}`,
		"budget 0":                  `{"cloud_provider":null,"monthly_token_budget":0}`,
		"ref while off":             `{"cloud_provider":null,"agreement_ref":"AGR-2026-01","monthly_token_budget":5}`,
		"old free shape":            `{"cloud_provider":"openai","agreement_ref":"SYNTH-1","monthly_token_budget":5}`,
		"patient pseudonym":         `{"cloud_provider":"openai","agreement_ref":"P-AB12CD34","monthly_token_budget":5}`,
		"a person's name":           `{"cloud_provider":"openai","agreement_ref":"ADAEZE-OKAFOR","monthly_token_budget":5}`,
		"an MRN shape":              `{"cloud_provider":"openai","agreement_ref":"HAV-000123","monthly_token_budget":5}`,
		"a name and a number":       `{"cloud_provider":"openai","agreement_ref":"OKAFOR-2026-01","monthly_token_budget":5}`,
		"budget negative":           `{"cloud_provider":null,"monthly_token_budget":-1}`,
		"budget too big":            `{"cloud_provider":null,"monthly_token_budget":1000000001}`,
		"budget fraction":           `{"cloud_provider":null,"monthly_token_budget":1.5}`,
		"budget string":             `{"cloud_provider":null,"monthly_token_budget":"5"}`,
		"budget null":               `{"cloud_provider":null,"monthly_token_budget":null}`,
		"provider not a string":     `{"cloud_provider":1,"monthly_token_budget":5}`,
		"own model with a provider": `{"cloud_provider":"openai","agreement_ref":"DPA-2026-11","monthly_token_budget":5,"own_model":true}`,
		"own model as a string":     `{"cloud_provider":null,"monthly_token_budget":5,"own_model":"true"}`,
		"own model as a number":     `{"cloud_provider":null,"monthly_token_budget":5,"own_model":1}`,
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

// own_model (00055): left out is nil (keep, or cleared by a provider);
// present is carried as given; true never goes with a provider (above).
func TestModelPolicyBodyOwnModel(t *testing.T) {
	for raw, want := range map[string]*bool{
		`{"cloud_provider":null,"monthly_token_budget":5}`:                   nil,
		`{"cloud_provider":null,"monthly_token_budget":5,"own_model":null}`:  nil,
		`{"cloud_provider":null,"monthly_token_budget":5,"own_model":true}`:  ptrBool(true),
		`{"cloud_provider":null,"monthly_token_budget":5,"own_model":false}`: ptrBool(false),
	} {
		b, ok := decodeBody(raw)
		if !ok {
			t.Fatalf("decode %s", raw)
		}
		c, ok := b.check()
		if !ok {
			t.Fatalf("refused %s", raw)
		}
		if (c.OwnModel == nil) != (want == nil) || (want != nil && *c.OwnModel != *want) {
			t.Errorf("%s: own model %v, want %v", raw, c.OwnModel, want)
		}
	}
	own := true
	for name, c := range map[string]ModelPolicyChange{
		"own with a provider": {Hospital: uuid.New(), Provider: "openai", AgreementRef: "DPA-2026-11", OwnModel: &own, Via: ViaCLI},
	} {
		if c.valid() {
			t.Errorf("%s: valid", name)
		}
	}
	if c := (ModelPolicyChange{Hospital: uuid.New(), OwnModel: &own, Via: ViaCLI}); !c.valid() {
		t.Error("own model alone: invalid")
	}
}

func ptrBool(b bool) *bool { return &b }

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

// The contract says what "off" is (review of #137): provider_code null.
// cloud_provider is also null for gemini (its 1.x enum can't name it), so
// no description may define off by cloud_provider alone, or a client
// reading the contract would show a Gemini hospital as off.
func TestModelPolicyContractOffIsProviderCode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Description string `yaml:"description"`
		} `yaml:"paths"`
		Components struct {
			Schemas map[string]struct {
				Description string `yaml:"description"`
				Properties  map[string]struct {
					Description string `yaml:"description"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	mp := doc.Components.Schemas["ModelPolicy"]
	get := doc.Paths["/platform/hospitals/{id}/model-policy"]["get"].Description
	for what, text := range map[string]string{
		"ModelPolicy":                mp.Description,
		"ModelPolicy.cloud_provider": mp.Properties["cloud_provider"].Description,
		"ModelPolicy.provider_code":  mp.Properties["provider_code"].Description,
		"GET":                        get,
	} {
		flat := strings.Join(strings.Fields(text), " ")
		if regexp.MustCompile("off (when|if) `cloud_provider` is null").MatchString(flat) ||
			strings.Contains(flat, "off (local models): `cloud_provider`") {
			t.Errorf("%s defines off by cloud_provider: %q", what, flat)
		}
	}
	for what, text := range map[string]string{"ModelPolicy": mp.Description, "GET": get} {
		flat := strings.Join(strings.Fields(text), " ")
		if !strings.Contains(flat, "`provider_code` is null") {
			t.Errorf("%s doesn't define off as provider_code null: %q", what, flat)
		}
	}
}

// own_model on needs the deployment's allow-list (CORE_OWN_MODEL_ALLOWED);
// off, or left out, never does.
func TestOwnModelPermitted(t *testing.T) {
	on, off := true, false
	for _, c := range []struct {
		own     *bool
		allowed bool
		want    bool
	}{
		{&on, false, false},
		{&on, true, true},
		{&off, false, true},
		{&off, true, true},
		{nil, false, true},
		{nil, true, true},
	} {
		if got := OwnModelPermitted(ModelPolicyChange{OwnModel: c.own}, c.allowed); got != c.want {
			t.Errorf("own %v allowed %v: %v, want %v", c.own, c.allowed, got, c.want)
		}
	}
}
