package queries

import (
	"encoding/json"
	"testing"
)

// TestGeneratedByFromDecision: generated_by comes from the decision's own
// model_versions (plan departure 15), never from the inserted row or the
// hospital's policy now, so it names the route that produced the answer.
func TestGeneratedByFromDecision(t *testing.T) {
	anthropic, openai, xai, gemini := "anthropic", "openai", "xai", "gemini"
	for _, c := range []struct {
		name     string
		versions string
		want     *GeneratedBy
	}{
		{"local", `{"cca_route":"local","cca":"qwen2.5:7b","dense":"bge"}`, &GeneratedBy{Route: "local"}},
		{"local without cca", `{"cca_route":"local"}`, &GeneratedBy{Route: "local"}},
		{"local ignores a provider-shaped cca", `{"cca_route":"local","cca":"anthropic/x@y"}`, &GeneratedBy{Route: "local"}},
		{"cloud anthropic", `{"cca_route":"cloud","cca":"anthropic/claude-opus-5-5@claude-opus-5-5"}`,
			&GeneratedBy{Route: "cloud", Provider: &anthropic, ProviderCode: &anthropic}},
		{"cloud openai", `{"cca_route":"cloud","cca":"openai/gpt-6.1-sol@gpt-6.1-sol"}`, &GeneratedBy{Route: "cloud", Provider: &openai, ProviderCode: &openai}},
		{"cloud xai", `{"cca_route":"cloud","cca":"xai/grok-4.7@grok-4.7"}`, &GeneratedBy{Route: "cloud", Provider: &xai, ProviderCode: &xai}},
		// openapi 1.x's provider enum can't name gemini: null there, and
		// provider_code says gemini.
		{"cloud gemini", `{"cca_route":"cloud","cca":"gemini/synthetic-model@synthetic-model"}`,
			&GeneratedBy{Route: "cloud", ProviderCode: &gemini}},
		{"unknown provider prefix", `{"cca_route":"cloud","cca":"mistral/m-3@m-3"}`, nil},
		{"provider in the wrong case", `{"cca_route":"cloud","cca":"Anthropic/claude@x"}`, nil},
		{"cloud without cca", `{"cca_route":"cloud"}`, nil},
		{"malformed cca: no slash", `{"cca_route":"cloud","cca":"anthropic"}`, nil},
		{"malformed cca: empty", `{"cca_route":"cloud","cca":""}`, nil},
		{"malformed cca: not a string", `{"cca_route":"cloud","cca":7}`, nil},
		{"malformed cca: leading slash", `{"cca_route":"cloud","cca":"/anthropic"}`, nil},
		{"no cca_route (older worker)", `{"cca":"qwen2.5:7b"}`, nil},
		{"no cca_route, cloud-shaped cca", `{"cca":"anthropic/claude@x"}`, nil},
		{"unknown route", `{"cca_route":"cloud:anthropic","cca":"anthropic/claude@x"}`, nil},
		{"route not a string", `{"cca_route":true}`, nil},
		{"empty", `{}`, nil},
		{"null", `null`, nil},
		{"not an object", `["cloud"]`, nil},
		{"absent column", ``, nil},
	} {
		got := generatedBy(stringVersions([]byte(c.versions)))
		if !sameGeneratedBy(got, c.want) {
			g, _ := json.Marshal(got)
			w, _ := json.Marshal(c.want)
			t.Errorf("%s: got %s, want %s", c.name, g, w)
		}
	}
	// The wire shape: provider is present, and null on a local answer.
	raw, _ := json.Marshal(generatedBy(stringVersions([]byte(`{"cca_route":"local"}`))))
	if string(raw) != `{"route":"local","provider":null,"provider_code":null}` {
		t.Errorf("local wire shape %s", raw)
	}
	raw, _ = json.Marshal(generatedBy(stringVersions([]byte(`{"cca_route":"cloud","cca":"gemini/m@m"}`))))
	if string(raw) != `{"route":"cloud","provider":null,"provider_code":"gemini"}` {
		t.Errorf("gemini wire shape %s", raw)
	}
	raw, _ = json.Marshal(generatedBy(stringVersions([]byte(`{"cca_route":"cloud","cca":"xai/m@m"}`))))
	if string(raw) != `{"route":"cloud","provider":"xai","provider_code":"xai"}` {
		t.Errorf("xai wire shape %s", raw)
	}
	raw, _ = json.Marshal(ClinicalResultOut{})
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if _, ok := m["generated_by"]; ok {
		t.Errorf("generated_by not omitted when unknown: %s", raw)
	}
}

func sameGeneratedBy(a, b *GeneratedBy) bool {
	if a == nil || b == nil {
		return a == b
	}
	same := func(x, y *string) bool { return (x == nil && y == nil) || (x != nil && y != nil && *x == *y) }
	return a.Route == b.Route && same(a.Provider, b.Provider) && same(a.ProviderCode, b.ProviderCode)
}
