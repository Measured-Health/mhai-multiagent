package queries

import (
	"encoding/json"
	"strings"
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

// TestCitedSources: a stored cited source is shown only for a cited id, in
// order and once, with its fields checked again; anything else is dropped,
// never shown as it was stored.
func TestCitedSources(t *testing.T) {
	cited := []string{"KB#1000002", "KB#1000001", "KB#0001"}
	good := `{"chunk_id":"KB#1000001","title":"Synthetic Fever","url":"https://medlineplus.gov/syntheticfever.html",` +
		`"source":"MedlinePlus","attribution":"Source: MedlinePlus, National Library of Medicine."}`
	got := citedSources([]byte(`[`+good+`,`+good+`,{"chunk_id":"KB#1000002","title":"Synthetamol (Boxed warning)",`+
		`"url":null,"source":"openFDA","attribution":null}]`), cited)
	if len(got) != 2 || got[0].ChunkID != "KB#1000001" || got[1].ChunkID != "KB#1000002" {
		t.Fatalf("got %+v", got)
	}
	if got[0].URL == nil || *got[0].URL != "https://medlineplus.gov/syntheticfever.html" || got[0].Attribution == nil ||
		got[0].Title != "Synthetic Fever" || got[0].Source != "MedlinePlus" {
		t.Errorf("first: %+v", got[0])
	}
	if got[1].URL != nil || got[1].Attribution != nil {
		t.Errorf("second: %+v", got[1])
	}
	b, _ := json.Marshal(got[1])
	if string(b) != `{"chunk_id":"KB#1000002","title":"Synthetamol (Boxed warning)","url":null,"source":"openFDA","attribution":null}` {
		t.Errorf("json %s", b)
	}

	for _, c := range []struct{ name, raw string }{
		{"not cited", `[{"chunk_id":"KB#1000009","title":"T","source":"S"}]`},
		{"no title", `[{"chunk_id":"KB#1000001","source":"S"}]`},
		{"empty source", `[{"chunk_id":"KB#1000001","title":"T","source":""}]`},
		{"control character", "[{\"chunk_id\":\"KB#1000001\",\"title\":\"T\\u0007\",\"source\":\"S\"}]"},
		{"format character", "[{\"chunk_id\":\"KB#1000001\",\"title\":\"T\\u202e\",\"source\":\"S\"}]"},
		{"title too long", `[{"chunk_id":"KB#1000001","title":"` + strings.Repeat("x", 301) + `","source":"S"}]`},
		{"not an array", `{"chunk_id":"KB#1000001"}`},
		{"not json", `[`},
		{"null", `null`},
	} {
		if got := citedSources([]byte(c.raw), cited); len(got) != 0 {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
	if got := citedSources(nil, cited); got == nil || len(got) != 0 {
		t.Errorf("nil: %#v", got)
	}

	for _, u := range []string{
		"http://medlineplus.gov/x.html", "javascript:alert(1)", "https://user@medlineplus.gov/",
		"https://medlineplus.gov:8443/x", `https://medlineplus.gov/x"><script>`, "https://medlineplus.gov/x#f",
		"https://medlineplus.gov/" + strings.Repeat("a", 500),
	} {
		raw, _ := json.Marshal([]map[string]any{{"chunk_id": "KB#1000001", "title": "T", "source": "S", "url": u}})
		got := citedSources(raw, cited)
		if len(got) != 1 || got[0].URL != nil {
			t.Errorf("url %q: %+v", u, got)
		}
	}
}
