package recordlink

import "testing"

// A25: the "open record" link is https only, on the base address's host,
// with the source patient id path-escaped into the one {id} of the path;
// anything else is no link.
func TestRecordLink(t *testing.T) {
	base := "https://fhir.hospital-a.example/r4"
	ok := map[string]struct{ tmpl, id, want string }{
		"plain":           {"https://fhir.hospital-a.example/patients/{id}", "123", "https://fhir.hospital-a.example/patients/123"},
		"escaped slash":   {"https://fhir.hospital-a.example/patients/{id}", "../admin", "https://fhir.hospital-a.example/patients/..%2Fadmin"},
		"escaped query":   {"https://fhir.hospital-a.example/p/{id}/view", "1?x=2", "https://fhir.hospital-a.example/p/1%3Fx=2/view"},
		"explicit port":   {"https://fhir.hospital-a.example:443/p/{id}", "a-1", "https://fhir.hospital-a.example:443/p/a-1"},
		"upper-case host": {"https://FHIR.hospital-a.example/p/{id}", "7", "https://fhir.hospital-a.example/p/7"},
	}
	for name, c := range ok {
		got, fine := Build(c.tmpl, base, c.id)
		if !fine || got != c.want {
			t.Errorf("%s: %q %v, want %q", name, got, fine, c.want)
		}
	}
	refused := map[string]struct{ tmpl, base, id string }{
		"other host":       {"https://evil.example.org/patients/{id}", base, "1"},
		"subdomain":        {"https://x.fhir.hospital-a.example/patients/{id}", base, "1"},
		"other port":       {"https://fhir.hospital-a.example:8443/patients/{id}", base, "1"},
		"javascript":       {"javascript:alert(1)//{id}", base, "1"},
		"http":             {"http://fhir.hospital-a.example/patients/{id}", base, "1"},
		"query template":   {"https://fhir.hospital-a.example/patients?id={id}", base, "1"},
		"fragment":         {"https://fhir.hospital-a.example/patients/{id}#x", base, "1"},
		"userinfo":         {"https://u:p@fhir.hospital-a.example/patients/{id}", base, "1"},
		"no placeholder":   {"https://fhir.hospital-a.example/patients/", base, "1"},
		"two placeholders": {"https://fhir.hospital-a.example/{id}/{id}", base, "1"},
		"host placeholder": {"https://{id}.hospital-a.example/p", base, "1"},
		"dot-dot id":       {"https://fhir.hospital-a.example/patients/{id}", base, ".."},
		"dot id":           {"https://fhir.hospital-a.example/patients/{id}", base, "."},
		"empty id":         {"https://fhir.hospital-a.example/patients/{id}", base, ""},
		"control in id":    {"https://fhir.hospital-a.example/patients/{id}", base, "1\n2"},
		"long id":          {"https://fhir.hospital-a.example/patients/{id}", base, string(make([]byte, 257))},
		"empty template":   {"", base, "1"},
		"http base":        {"https://fhir.hospital-a.example/patients/{id}", "http://fhir.hospital-a.example", "1"},
		"backslash":        {`https://fhir.hospital-a.example\@evil.example.org/{id}`, base, "1"},
		"dot segment":      {"https://fhir.hospital-a.example/a/../{id}", base, "1"},
		"protocol-rel":     {"//fhir.hospital-a.example/patients/{id}", base, "1"},
	}
	for name, c := range refused {
		if got, fine := Build(c.tmpl, c.base, c.id); fine || got != "" {
			t.Errorf("%s: built %q", name, got)
		}
	}
}
