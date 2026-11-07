package emrurl

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// A5 at save (EMR design §6.4, §17.5): every address a connection names is
// https (sftp for file_export), a DNS name, with no userinfo, query or
// fragment, at most 500 characters. The proxy is the enforcement point for
// what a name resolves to; this is the early, shared check.
func TestCheckURLRefusals(t *testing.T) {
	https := []string{"https"}
	cases := []struct {
		raw  string
		want error
	}{
		{"https://10.0.0.5", ErrAddress},
		{"https://169.254.169.254", ErrAddress},
		{"https://169.254.169.254/latest/meta-data", ErrAddress},
		{"https://[::1]", ErrAddress},
		{"https://[fd00::1]:8443", ErrAddress},
		{"https://127.1", ErrAddress},
		{"https://2130706433", ErrAddress},
		{"https://0x7f.0.0.1", ErrAddress},
		{"https://localhost", ErrAddress},
		{"https://LOCALHOST.", ErrAddress},
		{"https://api.localhost", ErrAddress},
		{"https://metadata.google.internal", ErrAddress},
		{"https://printer.local", ErrAddress},
		{"https://postgres", ErrAddress}, // a single label: a compose service name
		{"https://user:pw@fhir.example.org", ErrShape},
		{"https://user@fhir.example.org", ErrShape},
		{"http://fhir.example.org", ErrScheme},
		{"ftp://fhir.example.org", ErrScheme},
		{"sftp://files.example.org", ErrScheme},
		{"//fhir.example.org", ErrScheme},
		{"fhir.example.org", ErrScheme},
		{"https:fhir.example.org", ErrShape},
		{"https://fhir.example.org/?q=1", ErrShape},
		{"https://fhir.example.org/?", ErrShape},
		{"https://fhir.example.org/#f", ErrShape},
		{"https://fhir.example.org/#", ErrShape},
		{"https://fhir.example.org:0", ErrShape},
		{"https://fhir.example.org:65536", ErrShape},
		{"https://fhir.example.org:https", ErrShape},
		{"https://fhir example.org", ErrShape},
		{"https://fhir.example.org/a\nb", ErrShape},
		{"https://fhir.exa_mple.org", ErrShape},
		{"https://-fhir.example.org", ErrShape},
		{"https://" + strings.Repeat("a", 64) + ".example.org", ErrShape},
		{"https://fhir.example.org/" + strings.Repeat("p", 500-len("https://fhir.example.org/")+1), ErrShape},
		{"", ErrShape},
	}
	for _, tc := range cases {
		if _, _, err := CheckURL(tc.raw, https, false); !errors.Is(err, tc.want) {
			t.Errorf("CheckURL(%q) = %v, want %v", tc.raw, err, tc.want)
		}
	}
	// Errors never carry the address.
	if _, _, err := CheckURL("https://user:secret-pw@fhir.example.org", https, false); err == nil ||
		strings.Contains(err.Error(), "secret-pw") || strings.Contains(err.Error(), "example") {
		t.Fatalf("error carries the input: %v", err)
	}

	// Exactly 500 characters is allowed.
	long := "https://fhir.example.org/" + strings.Repeat("p", 500-len("https://fhir.example.org/"))
	if _, _, err := CheckURL(long, https, false); err != nil {
		t.Fatalf("500 characters: %v", err)
	}

	// Normalised: lower case, no trailing dot, IDNA, the port explicit.
	for raw, want := range map[string]Origin{
		"https://HOST.Example.ORG.":           "https://host.example.org:443",
		"https://fhir.example.org:8443/r4":    "https://fhir.example.org:8443",
		"https://bücher.example.org/fhir":     "https://xn--bcher-kva.example.org:443",
		"https://fhir.example.org/r4/Patient": "https://fhir.example.org:443",
	} {
		u, o, err := CheckURL(raw, https, false)
		if err != nil || o != want {
			t.Errorf("CheckURL(%q) = %q, %v; want %q", raw, o, err, want)
			continue
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Hostname() != strings.TrimPrefix(strings.Split(string(want), ":")[1], "//") {
			t.Errorf("CheckURL(%q) returned %v", raw, u)
		}
	}
	if _, o, err := CheckURL("sftp://files.example.org/drop", []string{"sftp"}, false); err != nil || o != "sftp://files.example.org:22" {
		t.Errorf("sftp: %q %v", o, err)
	}

	// Development allows an IP literal (httptest servers), nothing else more.
	if _, o, err := CheckURL("https://127.0.0.1:8443", https, true); err != nil || o != "https://127.0.0.1:8443" {
		t.Errorf("development IP: %q %v", o, err)
	}
	for _, raw := range []string{"http://127.0.0.1", "https://u:p@127.0.0.1", "https://127.0.0.1/?q",
		"https://169.254.169.254", "https://10.0.0.5", "https://[fd00::1]", "https://192.168.1.10"} {
		if _, _, err := CheckURL(raw, https, true); err == nil {
			t.Errorf("development accepted %q", raw)
		}
	}
}

// A token_url on another host joins the credential origin (design §4.1,
// §6.3); the origin is a sorted set.
func TestOriginsOfFHIR(t *testing.T) {
	got, err := OriginsOf("fhir_r4", "https://fhir.example.org/r4",
		map[string]string{"token_url": "https://auth.example.org/oauth/token", "client_id": "synthetic-client"}, false)
	want := []Origin{"https://auth.example.org:443", "https://fhir.example.org:443"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("OriginsOf = %v, %v; want %v", got, err, want)
	}
	// The same host once.
	got, err = OriginsOf("fhir_r4", "https://fhir.example.org/r4",
		map[string]string{"token_url": "https://FHIR.example.org/token"}, false)
	if err != nil || !slices.Equal(got, []Origin{"https://fhir.example.org:443"}) {
		t.Fatalf("same host: %v %v", got, err)
	}
	// A token_url gets the base address's checks.
	for _, bad := range []string{"https://169.254.169.254/token", "http://auth.example.org/token", "https://auth.example.org/t?x=1"} {
		if _, err := OriginsOf("fhir_r4", "https://fhir.example.org", map[string]string{"token_url": bad}, false); err == nil {
			t.Errorf("token_url %q accepted", bad)
		}
	}
	// The base address too, and by type: file_export is sftp only.
	if _, err := OriginsOf("fhir_r4", "https://10.1.2.3", nil, false); !errors.Is(err, ErrAddress) {
		t.Errorf("private base: %v", err)
	}
	if o, err := OriginsOf("file_export", "sftp://files.example.org/drop", nil, false); err != nil ||
		!slices.Equal(o, []Origin{"sftp://files.example.org:22"}) {
		t.Errorf("file_export: %v %v", o, err)
	}
	if _, err := OriginsOf("file_export", "https://files.example.org", nil, false); !errors.Is(err, ErrScheme) {
		t.Errorf("file_export over https: %v", err)
	}
	if _, err := OriginsOf("document_archive", "sftp://files.example.org", nil, false); !errors.Is(err, ErrScheme) {
		t.Errorf("document_archive over sftp: %v", err)
	}
	if _, err := OriginsOf("hl7v2_feed", "https://fhir.example.org", nil, false); !errors.Is(err, ErrType) {
		t.Errorf("unknown type: %v", err)
	}
	// token_url is a FHIR setting: another type's token_url is not an
	// origin, and is refused rather than ignored.
	if _, err := OriginsOf("document_archive", "https://archive.example.org",
		map[string]string{"token_url": "https://auth.example.org"}, false); !errors.Is(err, ErrShape) {
		t.Errorf("token_url on document_archive: %v", err)
	}
}

// A record link must be on the base address's host (design §4.1, E16).
func TestRecordLinkMustShareHost(t *testing.T) {
	base := "https://fhir.example.org/r4"
	ok := map[string]string{"record_link": "https://fhir.example.org/chart/{id}"}
	if _, err := OriginsOf("fhir_r4", base, ok, false); err != nil {
		t.Fatalf("same host: %v", err)
	}
	for _, link := range []string{
		"https://evil.example.org/chart/{id}",
		"https://fhir.example.org.evil.example/chart/{id}",
		"http://fhir.example.org/chart/{id}",
		"javascript:alert(1)",
		"https://fhir.example.org/chart?id={id}",
	} {
		if _, err := OriginsOf("fhir_r4", base, map[string]string{"record_link": link}, false); err == nil {
			t.Errorf("record_link %q accepted", link)
		}
	}
	if _, err := OriginsOf("fhir_r4", base, map[string]string{"record_link": "https://other.example.org/{id}"}, false); !errors.Is(err, ErrLinkHost) {
		t.Errorf("other host: want ErrLinkHost")
	}
	// The link's host is not a credential origin.
	o, _ := OriginsOf("fhir_r4", base, ok, false)
	if !slices.Equal(o, []Origin{"https://fhir.example.org:443"}) {
		t.Errorf("origins %v", o)
	}
}
